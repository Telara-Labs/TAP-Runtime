package primitive

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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

// Menu starts with the summary, then lets the person approve every proposed
// primitive at once or inspect them one by one (next, previous, accept, deny,
// agent eval). Choices are held until a final review and submit: quitting
// before submitting writes nothing. Denied primitives are not shown again.
func Menu(in io.Reader, out io.Writer, res Result, cfg MenuConfig) error {
	done := loadDecisions(cfg.StateDir)
	byID := map[string]Primitive{}
	for _, p := range res.Primitives {
		byID[p.ID] = p
	}
	var shown []Family
	for _, f := range res.Families {
		if done[f.ID] != "deny" {
			shown = append(shown, f)
		}
	}
	WriteSummary(out, res, cfg.Clients)
	if hidden := len(res.Families) - len(shown); hidden > 0 {
		fmt.Fprintf(out, "  %d denied earlier and hidden.\n", hidden)
	}
	if len(shown) == 0 {
		return nil
	}
	fmt.Fprintln(out)
	for i, f := range shown {
		fmt.Fprintf(out, "%3d. %-60s %s saved · %d runs in %d sessions\n", i+1, title(f), tokensText(f.SavedTokens), f.ExecutionCount, f.SessionCount)
	}
	choice := make([]string, len(shown))
	if cfg.All {
		for i := range choice {
			choice[i] = "accept"
		}
		return submit(out, shown, choice, byID, cfg)
	}
	sc := bufio.NewScanner(in)
	read := func(prompt string) (string, bool) {
		fmt.Fprint(out, prompt)
		if !sc.Scan() {
			return "", false
		}
		return strings.ToLower(strings.TrimSpace(sc.Text())), true
	}
	// inspect walks the cards from position i; it returns false when the
	// person quits.
	pos := 0
	inspect := func() bool {
		for pos < len(shown) {
			card(out, pos+1, len(shown), shown[pos], byID, choice[pos], done[shown[pos].ID])
			k, ok := read("[a] accept · [d] deny · [e] agent eval · [n] next · [p] previous · [s] review and submit · [q] quit > ")
			if !ok || k == "q" {
				return false
			}
			switch k {
			case "a", "d", "e":
				choice[pos] = map[string]string{"a": "accept", "d": "deny", "e": "eval"}[k]
				pos++
			case "n":
				pos++
			case "p":
				if pos > 0 {
					pos--
				}
			case "s":
				return true
			default:
				fmt.Fprintln(out, "  Choose a, d, e, n, p, s or q.")
			}
		}
		pos = len(shown) - 1 // back from the review returns to the last card
		return true
	}
	for {
		k, ok := read("\n[i] inspect each · [a] approve all · [q] quit > ")
		if !ok || k == "q" {
			fmt.Fprintln(out, "Nothing saved.")
			return nil
		}
		switch k {
		case "a":
			for i := range choice {
				choice[i] = "accept"
			}
			pos = len(shown) - 1
		case "i":
			pos = 0
			if !inspect() {
				fmt.Fprintln(out, "Nothing saved.")
				return nil
			}
		default:
			fmt.Fprintln(out, "  Choose i, a or q.")
			continue
		}
		// Review before anything is written; back returns to the cards.
		for {
			fmt.Fprintln(out, "\nReview")
			n := 0
			for i, f := range shown {
				if choice[i] != "" {
					n++
					fmt.Fprintf(out, "  %3d. %-8s %s\n", i+1, choice[i], title(f))
				}
			}
			if n == 0 {
				fmt.Fprintln(out, "  No choices made.")
			}
			fmt.Fprintf(out, "  %d of %d undecided (left as they are).\n", len(shown)-n, len(shown))
			k, ok := read("[s] submit · [b] back · [q] quit without saving > ")
			if !ok || k == "q" {
				fmt.Fprintln(out, "Nothing saved.")
				return nil
			}
			if k == "s" {
				return submit(out, shown, choice, byID, cfg)
			}
			if k == "b" && !inspect() {
				fmt.Fprintln(out, "Nothing saved.")
				return nil
			}
		}
	}
}

func title(f Family) string {
	t := short(f.Head)
	if len(f.FollowUps) > 0 {
		var fu []string
		for _, x := range f.FollowUps {
			fu = append(fu, short(headKey(x.Steps[0])))
		}
		fu = dedupeLines(fu)
		if len(fu) > 3 {
			fu = append(fu[:3], fmt.Sprintf("+%d more", len(fu)-3))
		}
		t += " → " + strings.Join(fu, " / ")
	}
	if len(t) > 60 {
		t = t[:59] + "…"
	}
	return t
}

// card shows one proposed primitive: where it was found, its structure, the
// tools it uses, and its metadata.
func card(out io.Writer, n, total int, f Family, byID map[string]Primitive, pending, earlier string) {
	fmt.Fprintf(out, "\n── %d of %d ──────────────────────────────────────────\n", n, total)
	fmt.Fprintf(out, "%s\n", title(f))
	fmt.Fprintf(out, "Found in %d runs across %d sessions.\n", f.ExecutionCount, f.SessionCount)
	fmt.Fprintln(out, "\nStructure")
	head := short(f.Head)
	if len(f.Sources) > 0 {
		var alts []string
		for _, s := range f.Sources {
			alts = append(alts, short(s))
		}
		head += "  (or: " + strings.Join(alts, ", ") + ")"
	}
	fmt.Fprintf(out, "  1. %s\n", head)
	for _, fu := range f.FollowUps {
		var steps []string
		for _, s := range fu.Steps {
			steps = append(steps, short(s))
		}
		opt := "always"
		if fu.Optional {
			opt = "optional"
		}
		fmt.Fprintf(out, "     then %s  (%s; %d runs)\n", strings.Join(steps, " > "), opt, fu.Runs)
	}
	tools, piped := map[string]bool{}, map[string]bool{}
	open := 0
	for _, id := range f.Members {
		p := byID[id]
		for _, s := range p.Steps {
			tools[short(headKey(s))] = true
			if parts := strings.Split(s, "+"); len(parts) > 1 {
				for _, x := range parts[1:] {
					piped[short(x)] = true
				}
			}
		}
		open += len(p.Unresolved)
	}
	sorted := func(m map[string]bool) string {
		var ts []string
		for t := range m {
			ts = append(ts, t)
		}
		sort.Strings(ts)
		return strings.Join(ts, ", ")
	}
	fmt.Fprintf(out, "\nTools\n  %s\n", sorted(tools))
	if len(piped) > 0 {
		fmt.Fprintf(out, "  output piped through: %s (varies by run)\n", sorted(piped))
	}
	fmt.Fprintln(out, "\nMetadata")
	if len(f.Inputs) > 0 {
		fmt.Fprintln(out, "  inputs:")
		for _, in := range f.Inputs {
			fmt.Fprintf(out, "    %s\n", in)
		}
	}
	fmt.Fprintf(out, "  effect: %s (unknown is treated as write; the runner asks before each call)\n", f.Effect)
	fmt.Fprintf(out, "  tokens its follow-up calls cost: %s (%s per run)\n", tokensText(f.SavedTokens), tokensText(perRun(f)))
	fmt.Fprintf(out, "  confidence: %d/100 (weakest of %d exact chains; %s) · %s", f.Confidence, len(f.Members), Rubric, f.Readiness)
	if f.NeedsDecision > 0 {
		fmt.Fprintf(out, ", %d chains need a decision", f.NeedsDecision)
	}
	fmt.Fprintf(out, "\n  open questions: %d (agent eval writes them out with the evidence)\n", open)
	if earlier != "" {
		fmt.Fprintf(out, "  earlier decision: %s\n", earlier)
	}
	if pending != "" {
		fmt.Fprintf(out, "  your choice so far: %s\n", pending)
	}
	fmt.Fprintln(out)
}

// submit writes every choice: accept keeps the primitive, deny hides it,
// agent eval writes its handoff.
func submit(out io.Writer, shown []Family, choice []string, byID map[string]Primitive, cfg MenuConfig) error {
	for i, f := range shown {
		var err error
		var msg string
		switch choice[i] {
		case "accept":
			err = acceptFamily(cfg.StateDir, f, byID)
			msg = "accepted: " + filepath.Join(cfg.StateDir, "accepted", "families", f.ID+".json")
		case "deny":
			err = record(cfg.StateDir, Decision{f.ID, "deny"})
			msg = "denied; it will not be shown again"
		case "eval":
			where := filepath.Join(cfg.StateDir, "eval", f.ID)
			err = WriteFamilyHandoff(where, cfg.Home, f, byID, cfg.Sessions, cfg.Skill)
			if err == nil {
				err = record(cfg.StateDir, Decision{f.ID, "eval"})
			}
			msg = "agent eval handoff: " + where + " (give your coding agent HANDOFF.md)"
		default:
			continue
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "  %d. %s\n", i+1, msg)
	}
	fmt.Fprintln(out, "Submitted.")
	return nil
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
