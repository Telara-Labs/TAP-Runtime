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
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: discover-eval freeze|run|sample [flags]")
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
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "discover-eval:", err)
		os.Exit(1)
	}
}

func readers() ([]discover.Reader, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return discover.DefaultReaders([]string{"claude-code", "codex", "cursor"}, home)
}

func readAll(rs []discover.Reader) ([]discover.Session, error) {
	var all []discover.Session
	for _, r := range rs {
		ss, err := r.Read(time.Time{})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", r.Client(), err)
		}
		all = append(all, ss...)
	}
	return all, nil
}

func frozenReaders(path string) ([]discover.Reader, discover.Manifest, error) {
	var m discover.Manifest
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
	out := make([]discover.Reader, len(rs))
	for i, r := range rs {
		out[i] = discover.FrozenReader{Inner: r, Manifest: m}
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
	m := discover.BuildManifest(ss, t)
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
	o := discover.DefaultOptions()
	o.Readers = rs
	o.Now = func() time.Time { return time.Time{} } // a run's output must not depend on when it ran
	rep, err := discover.Run(o)
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
		discover.WriteFunnel(f, rep, 0, true)
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
	var rep discover.Report
	b, err := os.ReadFile(*report)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &rep); err != nil {
		return err
	}
	c := discover.NewCorpus(ss)
	eps := discover.SampleEpisodes(c, &rep, discover.SampleOptions{Seed: *seed, PerReady: 2, PerGroup: *perGroup, Uncovered: *uncovered})
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
