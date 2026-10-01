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
	// Color draws accents when the output is a terminal.
	Color bool
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

// WriteSummary prints what was read and found, as a table.
func WriteSummary(out io.Writer, res Result, clients string) {
	writeSummary(out, style{}, res, clients)
}

func writeSummary(out io.Writer, s style, res Result, clients string) {
	sm := res.Summary
	section(out, s, "Summary")
	var saved trace.Usage
	for _, f := range res.Families {
		saved = saved.Add(f.Saved)
	}
	tot, _, eq := tokenCells(sm.Tokens)
	_, _, seq := tokenCells(saved)
	table{widths: []int{52, 12}, right: map[int]bool{1: true}, rows: [][]string{
		{"Sessions read (" + clients + ")", count(sm.Sessions)},
		{"Tool calls", count(sm.ToolCalls)},
		{"Replayable operations", count(sm.Operations)},
		{"Edits and inline scripts (judgment, never replayed)", count(sm.JudgmentCalls)},
		{"Failed calls", count(sm.FailedCalls)},
		{"Decisions built from an earlier output", count(sm.DecisionCalls)},
		{"Gateway calls matched to their direct tool", count(sm.RouteMerged)},
		{"Exact chains found", count(sm.Primitives)},
		{"Proposed primitives", count(sm.Families)},
	}}.render(out, s)
	turns := 0
	for _, f := range res.Families {
		turns += f.TurnsSaved
	}
	section(out, s, "Estimated savings")
	table{head: []string{"", "Tool calls", "Est. tokens (input-equivalent)"}, widths: []int{46, 12, 30}, right: map[int]bool{1: true, 2: true}, rows: [][]string{
		{"Your history", count(sm.ToolCalls), eq},
		{"Follow-up calls the proposed primitives remove", count(turns), seq},
	}}.render(out, s)
	fmt.Fprintln(out, s.dim("  A follow-up call waits on an earlier result, so each one is a model turn you no longer wait for."))
	fmt.Fprintln(out, s.dim(fmt.Sprintf("  Tokens are priced as fresh input: most of a turn re-reads the conversation from the prompt cache at about")))
	fmt.Fprintln(out, s.dim(fmt.Sprintf("  %.1f× that price, and output costs about %.0f×. Raw counts are about ten times larger (%s for the history).", cachedRatio, outputRatio, tot)))
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
	s := style{on: cfg.Color}
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
	banner(out, s, fmt.Sprintf("%s · %s sessions · %s tool calls", cfg.Clients, count(res.Summary.Sessions), count(res.Summary.ToolCalls)))
	writeSummary(out, s, res, cfg.Clients)
	if hidden := len(res.Families) - len(shown); hidden > 0 {
		fmt.Fprintln(out, s.dim(fmt.Sprintf("  %d denied earlier and hidden.", hidden)))
	}
	if len(shown) == 0 {
		return nil
	}
	choice := make([]string, len(shown))
	list := func() {
		section(out, s, "Proposed primitives (largest estimated saving first)")
		t := table{head: []string{"#", "Primitive", "Runs", "Sessions", "Turns saved", "Est. tokens saved", "Values traced", "Open questions", "Choice"},
			widths: []int{3, 38, 5, 8, 11, 17, 13, 14, 7}, right: map[int]bool{0: true, 2: true, 3: true, 4: true, 5: true, 6: true, 7: true}}
		for i, f := range shown {
			c := choice[i]
			if c == "" {
				c = s.dim(done[f.ID])
			} else {
				c = s.choice(c)
			}
			t.rows = append(t.rows, []string{fmt.Sprint(i + 1), title(f), count(f.ExecutionCount), count(f.SessionCount),
				count(f.TurnsSaved), "≈" + tokensText(inputEquivalent(f.Saved)), fmt.Sprintf("%d of %d", f.Traced, f.Values), fmt.Sprint(f.OpenQuestions), c})
		}
		t.render(out, s)
	}
	list()
	if cfg.All {
		for i := range choice {
			choice[i] = "accept"
		}
		return submit(out, s, shown, choice, byID, cfg)
	}
	sc := bufio.NewScanner(in)
	read := func(prompt string) (string, bool) {
		fmt.Fprint(out, "\n"+prompt+s.accent(" › "))
		if !sc.Scan() {
			return "", false
		}
		return strings.ToLower(strings.TrimSpace(sc.Text())), true
	}
	pos := 0
	inspect := func() bool {
		for pos < len(shown) {
			card(out, s, pos+1, len(shown), shown[pos], byID, choice[pos], done[shown[pos].ID])
			k, ok := read(keys(s, "a", "accept", "d", "deny", "e", "agent eval", "n", "next", "p", "previous", "s", "review", "q", "quit"))
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
				fmt.Fprintln(out, s.dim("  Choose a, d, e, n, p, s or q."))
			}
		}
		pos = len(shown) - 1 // back from the review returns to the last card
		return true
	}
	for {
		k, ok := read(keys(s, "i", "inspect each", "a", "approve all", "q", "quit"))
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
			fmt.Fprintln(out, s.dim("  Choose i, a or q."))
			continue
		}
		for {
			section(out, s, "Review")
			t := table{head: []string{"#", "Choice", "Primitive"}, widths: []int{3, 7, 70}, right: map[int]bool{0: true}}
			n := 0
			for i, f := range shown {
				if choice[i] != "" {
					n++
					t.rows = append(t.rows, []string{fmt.Sprint(i + 1), s.choice(choice[i]), title(f)})
				}
			}
			if n > 0 {
				t.render(out, s)
			} else {
				fmt.Fprintln(out, "  No choices made.")
			}
			fmt.Fprintf(out, "  %d of %d undecided (left as they are). Nothing is written until you submit.\n", len(shown)-n, len(shown))
			k, ok := read(keys(s, "s", "submit", "b", "back", "q", "quit without saving"))
			if !ok || k == "q" {
				fmt.Fprintln(out, "Nothing saved.")
				return nil
			}
			if k == "s" {
				return submit(out, s, shown, choice, byID, cfg)
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
		if len(fu) > 2 {
			fu = append(fu[:2], fmt.Sprintf("+%d more", len(fu)-2))
		}
		t += " → " + strings.Join(fu, " / ")
	}
	return t
}

// card shows one proposed primitive: where it was found, its structure, the
// tools it uses, and its metadata.
func card(out io.Writer, s style, n, total int, f Family, byID map[string]Primitive, pending, earlier string) {
	fmt.Fprintln(out)
	head := fmt.Sprintf(" %d of %d ", n, total)
	fmt.Fprintln(out, s.accent("━━"+head+strings.Repeat("━", screen-2-width(head))))
	fmt.Fprintln(out, " "+s.bold(title(f)))
	fmt.Fprintln(out, " "+s.dim(fmt.Sprintf("Found in %s runs across %s sessions · %s", count(f.ExecutionCount), count(f.SessionCount), f.ID)))

	section(out, s, "Structure")
	st := table{head: []string{"Step", "Operation", "When", "Runs"}, widths: []int{6, 62, 9, 6}, right: map[int]bool{3: true}}
	headOp := short(f.Head)
	if len(f.Sources) > 0 {
		var alts []string
		for _, x := range f.Sources {
			alts = append(alts, short(x))
		}
		headOp += " (or " + strings.Join(alts, ", ") + ")"
	}
	st.rows = append(st.rows, []string{"1", headOp, "always", count(f.ExecutionCount)})
	for _, fu := range f.FollowUps {
		var steps []string
		for _, x := range fu.Steps {
			steps = append(steps, short(x))
		}
		when := "always"
		if fu.Optional {
			when = "optional"
		}
		st.rows = append(st.rows, []string{"then", strings.Join(steps, " > "), when, count(fu.Runs)})
	}
	st.render(out, s)

	tools, piped := map[string]bool{}, map[string]bool{}
	open := 0
	for _, id := range f.Members {
		p := byID[id]
		for _, x := range p.Steps {
			tools[short(headKey(x))] = true
			if parts := strings.Split(x, "+"); len(parts) > 1 {
				for _, y := range parts[1:] {
					piped[short(y)] = true
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
	section(out, s, "Tools")
	tt := table{widths: []int{22, 66}, rows: [][]string{{"Called", sorted(tools)}}}
	if len(piped) > 0 {
		tt.rows = append(tt.rows, []string{"Output piped through", sorted(piped) + " (varies by run)"})
	}
	tt.render(out, s)

	if len(f.Inputs) > 0 {
		section(out, s, "Inputs")
		it := table{head: []string{"Command", "Inputs the caller supplies"}, widths: []int{26, 62}}
		for _, in := range f.Inputs {
			cmd, args, _ := strings.Cut(in, ": ")
			it.rows = append(it.rows, []string{cmd, args})
		}
		it.render(out, s)
	}

	section(out, s, "Metadata")
	tot, cached, eq := tokenCells(f.Saved)
	per := 0.0
	if f.ExecutionCount > 0 {
		per = inputEquivalent(f.Saved) / float64(f.ExecutionCount)
	}
	conf := fmt.Sprintf("%s; %d of %d exact chains need a decision", f.Readiness, f.NeedsDecision, len(f.Members))
	mt := table{widths: []int{26, 62}, rows: [][]string{
		{"Effect", f.Effect + " (unknown is treated as write; the runner asks before each call)"},
		{"Estimated saving", fmt.Sprintf("%s model turns · %s tokens (input-equivalent; %s per run)", count(f.TurnsSaved), eq, tokensText(per))},
		{"How that is priced", fmt.Sprintf("raw %s tokens, %s of them cache re-reads of the conversation (about %.1f× the price of fresh input), %s fresh input, %s output", tot, cached, cachedRatio, tokensText(f.Saved.Fresh), tokensText(f.Saved.Output))},
		{"Values traced", fmt.Sprintf("%d of %d (the source of each value is known in every run: an earlier result, or the caller)", f.Traced, f.Values)},
		{"Open questions", fmt.Sprintf("%d (agent eval writes them out with the evidence)", f.OpenQuestions)},
		{"Status", conf},
		{"Run consistency (detail)", fmt.Sprintf("%d/100 weighted by runs, weakest chain %d (%s; describes the evidence, not a probability)", f.Confidence, f.Weakest, Rubric)},
	}}
	if earlier != "" {
		mt.rows = append(mt.rows, []string{"Earlier decision", earlier})
	}
	if pending != "" {
		mt.rows = append(mt.rows, []string{"Your choice so far", s.choice(pending)})
	}
	mt.render(out, s)
}

// submit writes every choice: accept keeps the primitive, deny hides it,
// agent eval writes its handoff.
func submit(out io.Writer, s style, shown []Family, choice []string, byID map[string]Primitive, cfg MenuConfig) error {
	section(out, s, "Submitted")
	for i, f := range shown {
		var err error
		var msg string
		switch choice[i] {
		case "accept":
			err = acceptFamily(cfg.StateDir, f, byID)
			msg = "accepted → " + filepath.Join(cfg.StateDir, "accepted", "families", f.ID+".json")
		case "deny":
			err = record(cfg.StateDir, Decision{f.ID, "deny"})
			msg = "denied; it will not be shown again"
		case "eval":
			where := filepath.Join(cfg.StateDir, "eval", f.ID)
			err = WriteFamilyHandoff(where, cfg.Home, f, byID, cfg.Sessions, cfg.Skill)
			if err == nil {
				err = record(cfg.StateDir, Decision{f.ID, "eval"})
			}
			msg = "agent eval → " + where + " (give your coding agent HANDOFF.md)"
		default:
			continue
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "  %3d. %s %s\n", i+1, s.choice(choice[i]), msg)
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
	case t >= 1e9:
		return fmt.Sprintf("%.1fB", t/1e9)
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
	fmt.Fprintf(&b, "\nSupport: %d runs across %d sessions; tokens the follow-up turns cost: %s. Confidence: %d/100 (%s, weighted by runs; weakest chain %d); %s.\n\n",
		f.ExecutionCount, f.SessionCount, tokensText(f.SavedTokens), f.Confidence, Rubric, f.Weakest, f.Readiness)
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
