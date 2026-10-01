// Command discover-eval freezes a corpus of local agent history, runs
// discover on it, and draws the labeling sample. It reads this machine's
// client stores and writes local files only; nothing it writes belongs in
// version control.
//
//	discover-eval freeze -cutoff 2026-09-30T04:00:00Z -out manifest.json
//	discover-eval run    -manifest manifest.json -out report.json
//	discover-eval sample -manifest manifest.json -report report.json -seed 20260930 -dir sample
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/pipeline"

	"gitlab.com/telara-labs/tap-runtime/discover/eval"

	"gitlab.com/telara-labs/tap-runtime/discover/routine"

	"gitlab.com/telara-labs/tap-runtime/discover/primitive"
	"gitlab.com/telara-labs/tap-runtime/discover/retrieval"

	"gitlab.com/telara-labs/tap-runtime/discover/model"

	"gitlab.com/telara-labs/tap-runtime/discover/history"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: discover-eval freeze|run|sample|show|opportunities|spans [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "freeze":
		err = freeze(os.Args[2:])
	case "run":
		err = run(os.Args[2:])
	case "sample":
		err = sample(os.Args[2:])
	case "show":
		err = show(os.Args[2:])
	case "opportunities":
		err = opportunities(os.Args[2:])
	case "primitives":
		err = primitives(os.Args[2:])
	case "spans":
		err = spans(os.Args[2:])
	case "packets":
		err = packets(os.Args[2:])
	case "holdout":
		err = holdout(os.Args[2:])
	case "episodes":
		err = episodes(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "discover-eval:", err)
		os.Exit(1)
	}
}

func readers() ([]trace.Reader, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return history.DefaultReaders([]string{"claude-code", "codex", "cursor"}, home)
}

func readAll(rs []trace.Reader) ([]trace.Session, error) {
	var all []trace.Session
	for _, r := range rs {
		ss, err := r.Read(time.Time{})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", r.Client(), err)
		}
		all = append(all, ss...)
	}
	return all, nil
}

func frozenReaders(path string) ([]trace.Reader, history.Manifest, error) {
	var m history.Manifest
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, m, err
	}
	rs, err := readers()
	if err != nil {
		return nil, m, err
	}
	out := make([]trace.Reader, len(rs))
	for i, r := range rs {
		out[i] = history.FrozenReader{Inner: r, Manifest: m}
	}
	return out, m, nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func freeze(args []string) error {
	fs := flag.NewFlagSet("freeze", flag.ExitOnError)
	cutoff := fs.String("cutoff", "", "freeze sessions whose last activity is before this RFC 3339 time")
	out := fs.String("out", "manifest.json", "manifest to write")
	fs.Parse(args)
	t, err := time.Parse(time.RFC3339, *cutoff)
	if err != nil {
		return fmt.Errorf("-cutoff: %w", err)
	}
	rs, err := readers()
	if err != nil {
		return err
	}
	ss, err := readAll(rs)
	if err != nil {
		return err
	}
	m := history.BuildManifest(ss, t)
	fmt.Printf("froze %d of %d sessions before %s; digest %s\n", len(m.Sessions), len(ss), m.Cutoff.Format(time.RFC3339), m.Digest)
	return writeJSON(*out, m)
}

func run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	manifest := fs.String("manifest", "manifest.json", "frozen corpus")
	out := fs.String("out", "report.json", "report to write")
	text := fs.String("text", "", "also write the printed funnel here")
	fs.Parse(args)
	rs, _, err := frozenReaders(*manifest)
	if err != nil {
		return err
	}
	o := pipeline.DefaultOptions()
	o.Readers = rs
	o.Now = func() time.Time { return time.Time{} } // a run's output must not depend on when it ran
	rep, err := pipeline.Run(o)
	if err != nil {
		return err
	}
	for _, c := range rep.Clients {
		if c.Error != "" {
			return fmt.Errorf("%s: %s", c.Client, c.Error)
		}
	}
	if *text != "" {
		f, err := os.OpenFile(*text, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		routine.WriteFunnel(f, rep, 0, true)
		f.Close()
	}
	return writeJSON(*out, rep)
}

func sample(args []string) error {
	fs := flag.NewFlagSet("sample", flag.ExitOnError)
	manifest := fs.String("manifest", "manifest.json", "frozen corpus")
	report := fs.String("report", "report.json", "report of the run being evaluated")
	seed := fs.Int64("seed", 1, "sampling seed")
	dir := fs.String("dir", "sample", "directory for sample.json and the episode packets")
	perGroup := fs.Int("per-group", 20, "routines drawn from needs_authoring and from each removal check")
	uncovered := fs.Int("uncovered", 42, "episodes drawn that produced no routine")
	fs.Parse(args)
	rs, _, err := frozenReaders(*manifest)
	if err != nil {
		return err
	}
	ss, err := readAll(rs)
	if err != nil {
		return err
	}
	var rep model.Report
	b, err := os.ReadFile(*report)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &rep); err != nil {
		return err
	}
	c := eval.NewCorpus(ss)
	eps := eval.SampleEpisodes(c, &rep, eval.SampleOptions{Seed: *seed, PerReady: 2, PerGroup: *perGroup, Uncovered: *uncovered})
	if err := os.MkdirAll(filepath.Join(*dir, "packets"), 0o700); err != nil {
		return err
	}
	for _, e := range eps {
		if err := os.WriteFile(filepath.Join(*dir, "packets", e.ID+".md"), []byte(c.RenderEpisode(e)), 0o600); err != nil {
			return err
		}
	}
	strata := map[string]int{}
	for _, e := range eps {
		strata[e.Stratum]++
	}
	fmt.Printf("sampled %d episodes: %v\n", len(eps), strata)
	return writeJSON(filepath.Join(*dir, "sample.json"), map[string]any{"seed": *seed, "report": *report, "episodes": eps, "strata": strata})
}

// show reruns discovery on the frozen corpus and writes, for each named
// routine, its record, its drafted files and up to -episodes of its source
// episodes, for reviewing a decision against its evidence.
func show(args []string) error {
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	manifest := fs.String("manifest", "manifest.json", "frozen corpus")
	dir := fs.String("dir", "show", "output directory")
	n := fs.Int("episodes", 4, "source episodes rendered per routine")
	fs.Parse(args)
	ids := map[string]bool{}
	for _, a := range fs.Args() {
		ids[a] = true
	}
	rs, _, err := frozenReaders(*manifest)
	if err != nil {
		return err
	}
	var all []trace.Session
	for _, r := range rs {
		ss, err := r.Read(time.Time{})
		if err != nil {
			return err
		}
		all = append(all, ss...)
	}
	o := pipeline.DefaultOptions()
	o.Readers = []trace.Reader{staticReader(all)}
	o.Now = func() time.Time { return time.Time{} }
	rep, err := pipeline.Run(o)
	if err != nil {
		return err
	}
	c := eval.NewCorpus(all)
	for i := range rep.Routines {
		r := &rep.Routines[i]
		if !ids[r.ID] {
			continue
		}
		d := filepath.Join(*dir, r.ID)
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		rec := *r
		if err := writeJSON(filepath.Join(d, "routine.json"), rec); err != nil {
			return err
		}
		if dr := routine.RoutineDraft(r); dr != nil {
			for name, body := range dr.Files {
				os.WriteFile(filepath.Join(d, name), body, 0o600)
			}
		}
		for k, s := range r.Sources {
			if k == *n {
				break
			}
			e := eval.Episode{ID: trace.EpisodeID(s.Client, s.Session, s.Request), Client: s.Client, Session: s.Session, Request: s.Request}
			for _, ep := range c.Episodes() {
				if ep.ID == e.ID {
					e = ep
				}
			}
			os.WriteFile(filepath.Join(d, fmt.Sprintf("episode-%d-%s.md", k+1, e.ID)), []byte(c.RenderEpisode(e)), 0o600)
		}
		fmt.Printf("%s: %s\n", r.ID, d)
	}
	return nil
}

// staticReader serves sessions already read, as one reader per client.
type staticReaderT struct{ ss []trace.Session }

func staticReader(ss []trace.Session) trace.Reader { return staticReaderT{ss} }
func (s staticReaderT) Client() string             { return "frozen" }
func (s staticReaderT) Read(time.Time) ([]trace.Session, error) {
	return s.ss, nil
}

// episodes judges each sampled episode on its own contract (no recurrence).
func episodes(args []string) error {
	fs := flag.NewFlagSet("episodes", flag.ExitOnError)
	manifest := fs.String("manifest", "manifest.json", "frozen corpus")
	sample := fs.String("sample", "sample/sample.json", "sample to judge")
	out := fs.String("out", "episode-claims.json", "claims to write")
	fs.Parse(args)
	rs, _, err := frozenReaders(*manifest)
	if err != nil {
		return err
	}
	ss, err := readAll(rs)
	if err != nil {
		return err
	}
	var smp struct {
		Episodes []eval.Episode `json:"episodes"`
	}
	b, err := os.ReadFile(*sample)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &smp); err != nil {
		return err
	}
	c := eval.NewCorpus(ss)
	claims := c.AssessEpisodes(smp.Episodes)
	n := map[string]int{}
	for _, cl := range claims {
		n[cl.Suitability]++
	}
	fmt.Println("episode claims:", n)
	return writeJSON(*out, claims)
}

// holdout draws a lineage-separated holdout: uniform over ordinary history,
// leaving out every lineage and template of the episodes in -exclude.
func holdout(args []string) error {
	fs := flag.NewFlagSet("holdout", flag.ExitOnError)
	manifest := fs.String("manifest", "manifest.json", "frozen corpus")
	exclude := fs.String("exclude", "", "comma-separated sample.json files whose lineages are excluded")
	seed := fs.Int64("seed", 1, "sampling seed")
	n := fs.Int("n", 150, "episodes to draw")
	dir := fs.String("dir", "holdout", "directory for sample.json and the episode packets")
	dropChanged := fs.Bool("drop-changed", false, "leave out sessions that changed since the freeze, and list them, instead of failing")
	fs.Parse(args)
	rs, _, err := frozenReaders(*manifest)
	if err != nil {
		return err
	}
	var dropped []string
	if *dropChanged {
		for i, r := range rs {
			fr := r.(history.FrozenReader)
			fr.DropChanged, fr.Dropped = true, &dropped
			rs[i] = fr
		}
	}
	ss, err := readAll(rs)
	if err != nil {
		return err
	}
	var ex []eval.EpisodeKey
	for _, f := range strings.Split(*exclude, ",") {
		if f == "" {
			continue
		}
		var smp struct {
			Episodes []eval.Episode `json:"episodes"`
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &smp); err != nil {
			return err
		}
		for _, e := range smp.Episodes {
			ex = append(ex, eval.EpisodeKey{Client: e.Client, Session: e.Session, Request: e.Request})
		}
	}
	c := eval.NewCorpus(ss)
	eps := eval.SampleHoldout(c, eval.HoldoutOptions{Seed: *seed, N: *n, Exclude: ex})
	if err := os.MkdirAll(filepath.Join(*dir, "packets"), 0o700); err != nil {
		return err
	}
	clients := map[string]int{}
	for _, e := range eps {
		clients[e.Client]++
		if err := os.WriteFile(filepath.Join(*dir, "packets", e.ID+".md"), []byte(c.RenderEpisode(e)), 0o600); err != nil {
			return err
		}
	}
	fmt.Printf("drew %d holdout episodes (excluded %d earlier episodes' lineages and templates; dropped %d changed sessions %v): %v\n", len(eps), len(ex), len(dropped), dropped, clients)
	return writeJSON(filepath.Join(*dir, "sample.json"), map[string]any{"seed": *seed, "manifest": *manifest, "exclude": *exclude, "dropped": dropped, "episodes": eps, "clients": clients})
}

// opportunities runs the selection pass over a frozen corpus and writes one
// judgment per request that made calls.
func opportunities(args []string) error {
	fs := flag.NewFlagSet("opportunities", flag.ExitOnError)
	manifest := fs.String("manifest", "manifest.json", "frozen corpus")
	out := fs.String("out", "opportunities.json", "judgments to write")
	dropChanged := fs.Bool("drop-changed", false, "leave out sessions that changed since the freeze, and list them, instead of failing")
	fs.Parse(args)
	rs, _, err := frozenReaders(*manifest)
	if err != nil {
		return err
	}
	var dropped []string
	if *dropChanged {
		for i, r := range rs {
			fr := r.(history.FrozenReader)
			fr.DropChanged, fr.Dropped = true, &dropped
			rs[i] = fr
		}
	}
	ss, err := readAll(rs)
	if err != nil {
		return err
	}
	ops := retrieval.SelectOpportunities(ss)
	n := map[string]int{}
	for _, o := range ops {
		if o.Recommended {
			n[o.Route]++
		}
	}
	fmt.Printf("judged %d requests; recommended %v; dropped %d changed session(s): %v\n", len(ops), n, len(dropped), dropped)
	return writeJSON(*out, map[string]any{"manifest": *manifest, "dropped": dropped, "opportunities": ops, "groups": retrieval.GroupOpportunities(ops)})
}

// spans runs model-free bounded-span retrieval on a frozen corpus. Its output
// is diagnostic and unassessed; it cannot be counted as Gate D recall.
// primitives replays the condensed primitive discovery over a frozen corpus.
func primitives(args []string) error {
	fs := flag.NewFlagSet("primitives", flag.ExitOnError)
	manifest := fs.String("manifest", "manifest.json", "frozen corpus")
	out := fs.String("out", "primitives.json", "result to write")
	client := fs.String("client", "", "optional single client to replay: claude-code, codex or cursor")
	dropChanged := fs.Bool("drop-changed", false, "leave out sessions changed since the freeze")
	fs.Parse(args)
	rs, _, err := frozenReaders(*manifest)
	if err != nil {
		return err
	}
	if *client != "" {
		var selected []trace.Reader
		for _, r := range rs {
			if r.Client() == *client {
				selected = append(selected, r)
			}
		}
		if len(selected) == 0 {
			return fmt.Errorf("unknown client %q", *client)
		}
		rs = selected
	}
	var dropped []string
	if *dropChanged {
		for i, r := range rs {
			fr := r.(history.FrozenReader)
			fr.DropChanged, fr.Dropped = true, &dropped
			rs[i] = fr
		}
	}
	ss, err := readAll(rs)
	if err != nil {
		return err
	}
	res := primitive.Discover(ss, nil)
	fmt.Printf("read %d frozen sessions (%d dropped as changed); %d primitives (%d multi-step)\n", len(ss), len(dropped), res.Summary.Primitives, res.Summary.MultiStep)
	return writeJSON(*out, map[string]any{"manifest": *manifest, "dropped": dropped, "result": res})
}

func spans(args []string) error {
	fs := flag.NewFlagSet("spans", flag.ExitOnError)
	manifest := fs.String("manifest", "manifest.json", "frozen corpus")
	out := fs.String("out", "spans.json", "proposals to write")
	client := fs.String("client", "", "optional single client to replay: claude-code, codex or cursor")
	dropChanged := fs.Bool("drop-changed", false, "leave out sessions changed since the freeze")
	fs.Parse(args)
	rs, _, err := frozenReaders(*manifest)
	if err != nil {
		return err
	}
	if *client != "" {
		var selected []trace.Reader
		for _, r := range rs {
			if r.Client() == *client {
				selected = append(selected, r)
			}
		}
		if len(selected) == 0 {
			return fmt.Errorf("unknown client %q", *client)
		}
		rs = selected
	}
	var dropped []string
	if *dropChanged {
		for i, r := range rs {
			fr := r.(history.FrozenReader)
			fr.DropChanged, fr.Dropped = true, &dropped
			rs[i] = fr
		}
	}
	ss, err := readAll(rs)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "read %d frozen sessions for span retrieval\n", len(ss))
	ps := retrieval.SelectSpanProposals(ss)
	fmt.Fprintf(os.Stderr, "extracted %d span proposals\n", len(ps))
	groups := retrieval.GroupSpanProposals(ps)
	compositions := retrieval.GroupSpanCompositions(ps)
	logic := retrieval.GroupLogicCandidates(ps)
	funnels := retrieval.GroupLogicFunnels(logic, ps)
	review := retrieval.ReviewSpanProposals(ps)
	reviewGroups := retrieval.GroupSpanCompositions(review)
	components := retrieval.ReviewSpanComponents(ps)
	componentGroups := retrieval.GroupSpanCompositions(components)
	fmt.Printf("found %d unassessed spans in %d composition groups; %d logic funnels and %d recurring logic candidates; dropped %d changed sessions\n", len(ps), len(compositions), len(funnels), len(logic), len(dropped))
	return writeJSON(*out, map[string]any{"manifest": *manifest, "dropped": dropped, "span_proposals": ps, "span_groups": groups, "composition_groups": compositions, "logic_funnels": funnels, "logic_candidates": logic, "review_spans": review, "review_groups": reviewGroups, "component_spans": components, "component_groups": componentGroups})
}

// packets renders the labeler view of the named episodes, for reviewing a
// group of opportunities by its members.
func packets(args []string) error {
	fs := flag.NewFlagSet("packets", flag.ExitOnError)
	manifest := fs.String("manifest", "manifest.json", "frozen corpus")
	ids := fs.String("ids", "", "file with one episode id per line")
	dir := fs.String("dir", "packets", "directory for the rendered packets")
	fs.Parse(args)
	rs, _, err := frozenReaders(*manifest)
	if err != nil {
		return err
	}
	var dropped []string
	for i, r := range rs {
		fr := r.(history.FrozenReader)
		fr.DropChanged, fr.Dropped = true, &dropped
		rs[i] = fr
	}
	ss, err := readAll(rs)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(*ids)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, l := range strings.Fields(string(b)) {
		want[l] = true
	}
	c := eval.NewCorpus(ss)
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return err
	}
	n := 0
	for _, e := range c.Episodes() {
		if want[e.ID] {
			if err := os.WriteFile(filepath.Join(*dir, e.ID+".md"), []byte(c.RenderEpisode(e)), 0o600); err != nil {
				return err
			}
			n++
		}
	}
	fmt.Printf("rendered %d of %d packets (dropped changed sessions: %v)\n", n, len(want), dropped)
	return nil
}
