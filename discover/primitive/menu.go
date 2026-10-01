package primitive

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// Decision is one recorded menu choice.
type Decision struct {
	ID     string `json:"id"`
	Choice string `json:"choice"` // accept | deny | eval
}

// MenuConfig says where decisions and accepted primitives live.
type MenuConfig struct {
	StateDir string
	// All accepts every proposed primitive without asking.
	All bool
	// Clients names what was read, for the summary.
	Clients string
	// Home, Sessions and Skill feed the agent-eval handoff: transcripts are
	// located under Home, request text comes from Sessions.
	Home     string
	Sessions []trace.Session
	Skill    Skill
}

// LoadKnown returns the primitives accepted earlier on this machine, for
// composition.
func LoadKnown(stateDir string) []Known {
	files, _ := filepath.Glob(filepath.Join(stateDir, "accepted", "*.json"))
	var out []Known
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var p Primitive
		if json.Unmarshal(b, &p) == nil && len(p.Steps) > 0 {
			out = append(out, Known{Name: p.ID, Steps: p.Steps})
		}
	}
	return out
}

func loadDecisions(stateDir string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(filepath.Join(stateDir, "decisions.jsonl"))
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		var d Decision
		if json.Unmarshal([]byte(line), &d) == nil && d.ID != "" {
			out[d.ID] = d.Choice
		}
	}
	return out
}

func record(stateDir string, d Decision) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(stateDir, "decisions.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, _ := json.Marshal(d)
	_, err = f.Write(append(b, '\n'))
	return err
}

// WriteSummary prints what was read and found.
func WriteSummary(out io.Writer, res Result, clients string) {
	s := res.Summary
	fmt.Fprintln(out, "Summary")
	fmt.Fprintf(out, "  Read %d sessions (%s) holding %d tool calls.\n", s.Sessions, clients, s.ToolCalls)
	fmt.Fprintf(out, "  %d calls are replayable operations, %d distinct; %d were edits or inline scripts (judgment) and %d failed.\n",
		s.Operations, s.DistinctOps, s.JudgmentCalls, s.FailedCalls)
	fmt.Fprintf(out, "  %d calls through a dispatching route were matched to their direct tool; %d arguments were settings (inputs), not separate operations.\n",
		s.RouteMerged, s.SettingsAsInput)
	fmt.Fprintf(out, "  %d calls were decisions built from an earlier output; each starts a new chain.\n", s.DecisionCalls)
	fmt.Fprintf(out, "  Found %d exact chains (%d pieces of longer chains dropped), grouped into %d proposed primitives.\n",
		s.Primitives, s.Fragments, s.Families)
}

func describe(p Primitive) string {
	loop := map[int]bool{}
	for _, l := range p.Loops {
		loop[l] = true
	}
	steps := make([]string, len(p.Steps))
	for i, s := range p.Steps {
		steps[i] = short(s)
		if loop[i+1] {
			steps[i] = "for each: " + steps[i]
		}
	}
	return strings.Join(steps, " > ")
}

func short(op string) string {
	op = strings.TrimPrefix(op, "mcp:")
	op = strings.TrimPrefix(op, "sh:")
	if len(op) > 70 {
		op = op[:69] + "…"
	}
	return op
}

func inputNames(p Primitive) string {
	var names []string
	seen := map[string]bool{}
	flags := 0
	for _, in := range p.Inputs {
		k := in.Key
		if seen[k] {
			continue
		}
		seen[k] = true
		if strings.HasPrefix(k, "-") {
			flags++
			continue
		}
		names = append(names, k)
	}
	sort.Strings(names)
	if flags > 0 {
		names = append(names, fmt.Sprintf("+%d flags", flags))
	}
	if len(names) == 0 {
		return "none"
	}
	s := strings.Join(names, ", ")
	if len(s) > 110 {
		s = s[:109] + "…"
	}
	return s
}

// Menu shows the summary and every proposed procedure (a family of exact
// chains), ranked by the model-turn tokens it would remove, then takes
// accept, deny or agent-eval choices. Denied families are not shown again.
func Menu(in io.Reader, out io.Writer, res Result, cfg MenuConfig) error {
	decided := loadDecisions(cfg.StateDir)
	byID := map[string]Primitive{}
	for _, p := range res.Primitives {
		byID[p.ID] = p
	}
	var shown []Family
	for _, f := range res.Families {
		if decided[f.ID] != "deny" {
			shown = append(shown, f)
		}
	}
	WriteSummary(out, res, cfg.Clients)
	if hidden := len(res.Families) - len(shown); hidden > 0 {
		fmt.Fprintf(out, "  %d denied earlier and hidden.\n", hidden)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Proposed primitives (most tokens saved first)")
	for i, f := range shown {
		mark := ""
		if c := decided[f.ID]; c != "" {
			mark = " [" + c + "ed]"
		}
		head := short(f.Head)
		if len(f.Sources) > 0 {
			var alts []string
			for _, s := range f.Sources {
				alts = append(alts, short(s))
			}
			head += " (or " + strings.Join(alts, ", ") + ")"
		}
		fmt.Fprintf(out, "%3d. %s%s\n", i+1, head, mark)
		for _, fu := range f.FollowUps {
			var steps []string
			for _, s := range fu.Steps {
				steps = append(steps, short(s))
			}
			opt := ""
			if fu.Optional {
				opt = " (optional)"
			}
			fmt.Fprintf(out, "       then %s%s\n", strings.Join(steps, " > "), opt)
		}
		fmt.Fprintf(out, "     tokens saved: %s (%s per run) · effect: %s · sessionCount: %d · executionCount: %d\n",
			tokensText(f.SavedTokens), tokensText(perRun(f)), f.Effect, f.SessionCount, f.ExecutionCount)
		fmt.Fprintf(out, "     confidence: %d/100 (weakest member; %s) · %s", f.Confidence, Rubric, f.Readiness)
		if f.NeedsDecision > 0 {
			fmt.Fprintf(out, " (%d of %d exact chains need a decision)", f.NeedsDecision, len(f.Members))
		}
		fmt.Fprintln(out)
		if len(f.Inputs) > 0 {
			in := strings.Join(f.Inputs, "; ")
			if len(in) > 160 {
				in = in[:159] + "…"
			}
			fmt.Fprintf(out, "     inputs: %s\n", in)
		}
	}
	if len(shown) == 0 {
		return nil
	}
	if cfg.All {
		for _, f := range shown {
			if err := acceptFamily(cfg.StateDir, f, byID); err != nil {
				return err
			}
		}
		fmt.Fprintf(out, "\nAccepted all %d into %s\n", len(shown), filepath.Join(cfg.StateDir, "accepted"))
		return nil
	}
	sc := bufio.NewScanner(in)
	for {
		fmt.Fprint(out, "\nChoose: <number> a (accept) | d (deny) | e (agent eval); 'all a' accepts every one; q quits > ")
		if !sc.Scan() {
			return sc.Err()
		}
		fs := strings.Fields(strings.ToLower(sc.Text()))
		if len(fs) == 0 || fs[0] == "q" {
			return nil
		}
		if len(fs) != 2 {
			fmt.Fprintln(out, "  Give a number (or 'all') and a, d or e.")
			continue
		}
		var targets []int
		if fs[0] == "all" {
			for i := range shown {
				targets = append(targets, i)
			}
		} else if n, err := strconv.Atoi(fs[0]); err == nil && n >= 1 && n <= len(shown) {
			targets = []int{n - 1}
		} else {
			fmt.Fprintln(out, "  No such primitive.")
			continue
		}
		for _, i := range targets {
			f := shown[i]
			var msg string
			var err error
			switch fs[1] {
			case "a", "accept":
				err = acceptFamily(cfg.StateDir, f, byID)
				msg = "accepted: " + filepath.Join(cfg.StateDir, "accepted", "families", f.ID+".json")
			case "d", "deny":
				err = record(cfg.StateDir, Decision{f.ID, "deny"})
				msg = "denied; it will not be shown again"
			case "e", "eval":
				where := filepath.Join(cfg.StateDir, "eval", f.ID)
				err = WriteFamilyHandoff(where, cfg.Home, f, byID, cfg.Sessions, cfg.Skill)
				if err == nil {
					err = record(cfg.StateDir, Decision{f.ID, "eval"})
				}
				msg = "agent eval handoff: " + where + " (give your coding agent HANDOFF.md)"
			default:
				fmt.Fprintln(out, "  Choose a, d or e.")
				continue
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "  #%d %s\n", i+1, msg)
		}
	}
}

func perRun(f Family) float64 {
	if f.ExecutionCount == 0 {
		return 0
	}
	return f.SavedTokens / float64(f.ExecutionCount)
}

func tokensText(t float64) string {
	switch {
	case t >= 1e6:
		return fmt.Sprintf("%.1fM", t/1e6)
	case t >= 1e3:
		return fmt.Sprintf("%.0fk", t/1e3)
	}
	return fmt.Sprintf("%.0f", t)
}

// acceptFamily keeps the family and its exact chains: the chains are known
// primitives for later composition.
func acceptFamily(stateDir string, f Family, byID map[string]Primitive) error {
	dir := filepath.Join(stateDir, "accepted", "families")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(f, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, f.ID+".json"), b, 0o600); err != nil {
		return err
	}
	for _, id := range f.Members {
		if err := accept(stateDir, byID[id]); err != nil {
			return err
		}
	}
	return record(stateDir, Decision{f.ID, "accept"})
}

// WriteFamilyHandoff writes one handoff per exact chain under dir, and an
// index that presents them as one procedure to refine.
func WriteFamilyHandoff(dir, home string, f Family, byID map[string]Primitive, sessions []trace.Session, skill Skill) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Refine procedure %s\n\nHead: %s", f.ID, f.Head)
	if len(f.Sources) > 0 {
		fmt.Fprintf(&b, " (alternative sources: %s)", strings.Join(f.Sources, ", "))
	}
	b.WriteString("\n\nFollow-ups:\n\n")
	for _, fu := range f.FollowUps {
		opt := ""
		if fu.Optional {
			opt = " (optional)"
		}
		fmt.Fprintf(&b, "- %s%s, %d runs\n", strings.Join(fu.Steps, " > "), opt, fu.Runs)
	}
	fmt.Fprintf(&b, "\nSupport: %d runs across %d sessions; tokens the follow-up turns cost: %s. Confidence: %d/100 (weakest member, %s); %s.\n\n",
		f.ExecutionCount, f.SessionCount, tokensText(f.SavedTokens), f.Confidence, Rubric, f.Readiness)
	b.WriteString("Design one primitive: the head, then each follow-up runs only when the caller supplies its inputs. ")
	b.WriteString("Each exact chain below has its own handoff (prompt, skill, questions, evidence); read them in order:\n\n")
	for _, id := range f.Members {
		p := byID[id]
		sub := filepath.Join(dir, id)
		if err := WriteHandoff(sub, home, p, sessions, skill); err != nil {
			return err
		}
		fmt.Fprintf(&b, "- `%s/HANDOFF.md`: %s (%d runs, %s)\n", id, describe(p), p.ExecutionCount, p.Confidence.Readiness)
	}
	return os.WriteFile(filepath.Join(dir, "HANDOFF.md"), []byte(b.String()), 0o600)
}

func accept(stateDir string, p Primitive) error {
	dir := filepath.Join(stateDir, "accepted")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(p, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, p.ID+".json"), b, 0o600); err != nil {
		return err
	}
	return record(stateDir, Decision{p.ID, "accept"})
}

// bindingSummary counts each argument's evidence level.
func bindingSummary(p Primitive) string {
	n := map[string]int{}
	for _, b := range p.Bindings {
		n[b.Label]++
	}
	if len(n) == 0 {
		return "no value flows"
	}
	var parts []string
	for _, l := range []string{"explicit", "inferred", "ambiguous", "missing"} {
		if n[l] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n[l], l))
		}
	}
	return strings.Join(parts, ", ")
}
