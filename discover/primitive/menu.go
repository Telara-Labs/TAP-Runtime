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

// MenuConfig says where decisions and accepted primitives live.
type MenuConfig struct {
	StateDir string
	// All accepts every runnable proposed primitive without asking.
	All bool
	// Clients names what was read, for the summary.
	Clients string
	// Color draws accents when the output is a terminal.
	Color bool
	// Install generates a family's primitive and installs it for the
	// person's agent. Supplied by the command, which may reach the code
	// generator; nil leaves accepting as saving only.
	Install func(f Family, members []Primitive) (InstallResult, error)
	// Home, Sessions and Skill feed the agent-eval handoff: transcripts are
	// located under Home, request text comes from Sessions.
	Home     string
	Sessions []trace.Session
	Skill    Skill
}

// InstallResult says what accepting produced.
type InstallResult struct {
	Installed bool
	Name      string
	Where     string
	// Reason says why nothing was installed (the program could not be
	// determined from the recorded uses).
	Reason string
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

// WriteSummary prints what was read and found, as a table.
func WriteSummary(out io.Writer, res Result, clients string) {
	writeSummary(out, style{}, res, clients)
}

func writeSummary(out io.Writer, s style, res Result, clients string) {
	sm := res.Summary
	section(out, s, "Summary")
	rows := [][]string{
		{"Sessions read (" + clients + ")", count(sm.Sessions)},
	}
	if p := period(sm); p != "" {
		rows = append(rows, []string{"Period", p})
	}
	rows = append(rows,
		[]string{"Tool calls", count(sm.ToolCalls)},
		[]string{"Calls that can be replayed", count(sm.Operations)},
		[]string{"Edits and scripts the agent wrote (never replayed)", count(sm.JudgmentCalls)},
		[]string{"Failed calls (not used)", count(sm.FailedCalls)},
		[]string{"Exact-flow candidates and patterns", count(sm.Families)})
	table{widths: []int{52, 24}, right: map[int]bool{1: true}, rows: rows}.render(out, s)
	var saved trace.Usage
	turns := 0
	for _, f := range res.Families {
		if f.APIMode == "needs_refinement" {
			continue
		}
		saved = saved.Add(f.Saved)
		turns += f.TurnsSaved
	}
	section(out, s, "Estimated savings")
	table{head: []string{"", "Model turns", "Tokens (estimate)"}, widths: []int{46, 12, 18}, right: map[int]bool{1: true, 2: true}, rows: [][]string{
		{"Your history", count(sm.ToolCalls), "about " + tokensText(inputEquivalent(sm.Tokens))},
		{"Potential from exact-flow candidates", count(turns), "about " + tokensText(inputEquivalent(saved))},
	}}.render(out, s)
	fmt.Fprintln(out, s.dim("  Each removed turn is a round-trip to the model you no longer wait for. Token figures are priced as fresh"))
	fmt.Fprintln(out, s.dim("  input; most of a turn is the conversation re-read from cache, which costs about a tenth as much."))
}

// mark is TAP's own badge.
func mark(s style) string {
	if !s.on {
		return "[TAP]"
	}
	return s.wrap("1;7;38;5;209", " TAP ")
}

func header(out io.Writer, s style, res Result, clients string) {
	sm := res.Summary
	line := fmt.Sprintf("%s  %s  %s · %s sessions", mark(s), s.bold("Discover"), clients, count(sm.Sessions))
	if p := period(sm); p != "" {
		line += " · " + p
	}
	fmt.Fprintln(out, line)
	fmt.Fprintln(out, s.dim("Repeated work from your agent history, checked for an executable API before review."))
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
	byID := map[string]Primitive{}
	for _, p := range res.Primitives {
		byID[p.ID] = p
	}
	shown, hidden := Triage(res, LoadLedger(cfg.StateDir))
	header(out, s, res, cfg.Clients)
	writeSummary(out, s, res, cfg.Clients)
	if n := hidden["accept"] + hidden[decisionAcceptDesign] + hidden["deny"] + hidden["eval"]; n > 0 {
		fmt.Fprintln(out, "  "+s.dim(fmt.Sprintf("Already decided and not shown again: %d installed, %d accepted for API design, %d dismissed, %d evidence handoffs exported. Change one with: tap discover --revisit",
			hidden["accept"], hidden[decisionAcceptDesign], hidden["deny"], hidden["eval"])))
	}
	if len(shown) == 0 {
		fmt.Fprintln(out, "\nNothing new to review.")
		return nil
	}
	choice := make([]string, len(shown))
	list := func() {
		section(out, s, "Discovered flows and patterns (largest estimated saving first)")
		listTable(out, s, shown, choice, -1)
	}
	list()
	if cfg.All {
		for i := range choice {
			if shown[i].APIMode != "needs_refinement" {
				choice[i] = "accept"
			}
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
			card(out, s, pos+1, len(shown), shown[pos], byID, choice[pos], res.Summary)
			actions := []string{"d", "dismiss", "e", "export evidence", "n", "next", "p", "previous", "s", "review choices", "q", "quit"}
			acceptLabel := "accept and install"
			if shown[pos].APIMode == "needs_refinement" {
				acceptLabel = "accept for API design"
			}
			actions = append([]string{"a", acceptLabel}, actions...)
			k, ok := read(keys(s, actions...))
			if !ok || k == "q" {
				return false
			}
			switch k {
			case "a", "d", "e":
				decision := map[string]string{"a": "accept", "d": "deny", "e": "eval"}[k]
				if k == "a" && shown[pos].APIMode == "needs_refinement" {
					decision = decisionAcceptDesign
				}
				choice[pos] = decision
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
				fmt.Fprintln(out, s.dim("  Choose one of the actions shown above."))
			}
		}
		pos = len(shown) - 1 // back from the review returns to the last card
		return true
	}
	for {
		k, ok := read(keys(s, "i", "inspect each", "a", "install all runnable", "q", "quit"))
		if !ok || k == "q" {
			fmt.Fprintln(out, "Nothing saved.")
			return nil
		}
		switch k {
		case "a":
			for i := range choice {
				if shown[i].APIMode != "needs_refinement" {
					choice[i] = "accept"
				}
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
			fmt.Fprintln(out, "  Choices are pending. Submitting installs runnable accepts, writes API-design and evidence handoffs, and hides dismissed patterns.")
			fmt.Fprintln(out, "  Design means accepted for API design; no TAP is installed for that choice.")
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
			k, ok := read(keys(s, "s", "submit choices", "b", "back", "q", "quit without saving"))
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

// title names a family the way a person would: its first step and the
// number of observed continuations.
func title(f Family) string {
	t := display(f.Head)
	n := len(f.FollowUps)
	switch {
	case f.APIMode == "optional_followups":
		t += fmt.Sprintf(" → up to %d related follow-ups", len(f.APIInputs))
	case f.APIMode == "caller_choice":
		t += fmt.Sprintf(" → choose 1 of %d actions", len(f.APIChoices))
	case f.APIMode == "needs_refinement":
		t += fmt.Sprintf(" · %d observed continuation", n)
		if n != 1 {
			t += "s"
		}
		t += " · API undefined"
	case n == 1:
		t += " → " + followUpText(f.FollowUps[0])
	case n > 1:
		t += fmt.Sprintf(" (+%d observed continuations)", n)
	}
	return t
}

func followUpText(fu FollowUp) string {
	var steps []string
	for _, x := range fu.Steps {
		steps = append(steps, display(x))
	}
	return strings.Join(steps, ", then ")
}

func period(sm Summary) string {
	if sm.First.IsZero() {
		return ""
	}
	return sm.First.Format("Jan 2") + " – " + sm.Last.Format("Jan 2, 2006")
}

// card shows one proposed primitive, most decision-relevant first: what it
// does, what it saves, what you provide, what needs attention; then tools
// and evidence detail.
func card(out io.Writer, s style, n, total int, f Family, byID map[string]Primitive, pending string, sm Summary) {
	fmt.Fprintln(out)
	head := fmt.Sprintf(" %d of %d ", n, total)
	fmt.Fprintln(out, s.accent("━━"+head+strings.Repeat("━", max(0, min(s.cols(), screen)-2-width(head)))))
	fmt.Fprintln(out, " "+s.bold(title(f)))
	used := fmt.Sprintf("Used %s times in %s sessions", count(f.ExecutionCount), count(f.SessionCount))
	if p := period(sm); p != "" {
		used += " (" + p + ")"
	}
	fmt.Fprintln(out, " "+s.dim(used+" · "+effectText(f.Effect)))
	switch f.Status {
	case StatusNewSince:
		fmt.Fprintln(out, " "+s.accent(fmt.Sprintf("Only what is new since your last decision (%s) is shown.", f.Earlier)))
	case StatusReevaluated:
		fmt.Fprintln(out, " "+s.accent("Shown again: the discovery rules changed since you decided on this."))
	}
	if pending != "" {
		fmt.Fprintln(out, " Your choice: "+s.choice(pending))
	}
	if f.APIMode != "" {
		section(out, s, "API status")
		switch f.APIMode {
		case "optional_followups":
			fmt.Fprintln(out, "  Runs the first operation once. Supply none, one, or several independent follow-up lists.")
			fmt.Fprintln(out, "  Optional inputs: "+strings.Join(f.APIInputs, ", "))
			fmt.Fprintf(out, "  API evidence: weakest included path %d/100; each path's relationship and tool-route support is shown below.\n", f.APIConfidence)
			fmt.Fprintln(out, "  Evidence scores measure recorded support, not a predicted success rate.")
			fmt.Fprintln(out, "  Calls run in listed order. A later failure reports completed results; earlier writes may remain.")
		case "caller_choice":
			fmt.Fprintln(out, "  Input: action (required). Runs the first operation once, then exactly one selected continuation.")
			fmt.Fprintln(out, "  Choices: "+strings.Join(f.APIChoices, ", "))
		case "needs_refinement":
			fmt.Fprintln(out, "  No runnable API exists for this pattern yet.")
			fmt.Fprintln(out, "  [a] Accept for API design. Submit records acceptance and writes a design handoff.")
			fmt.Fprintln(out, "  [e] Export evidence without accepting. Neither action installs a TAP or runs an agent.")
			if f.APIReason != "" {
				fmt.Fprintln(out, "  Reason: "+f.APIReason)
			}
		default:
			fmt.Fprintln(out, "  One exact recorded flow. The generated package is checked on submission.")
		}
	}

	structureTitle := "What it does"
	if f.APIMode == "needs_refinement" {
		structureTitle = "Observed calls (not an API)"
	}
	section(out, s, structureTitle)
	st := table{head: []string{"", "Step", "Used"}, widths: []int{16, 66, 6}, right: map[int]bool{2: true}, flex: 2}
	headOp := display(f.Head)
	if len(f.Sources) > 0 {
		var alts []string
		for _, x := range f.Sources {
			alts = append(alts, display(x))
		}
		headOp += " (or " + strings.Join(alts, ", ") + ")"
	}
	st.rows = append(st.rows, []string{"Starts with", headOp, count(f.ExecutionCount)})
	for i, fu := range f.FollowUps {
		label := ""
		if i == 0 {
			label = "Observed next"
			if len(f.FollowUps) == 1 && !fu.Optional {
				label = "Then"
			}
		}
		st.rows = append(st.rows, []string{label, followUpText(fu), count(fu.Runs)})
	}
	st.render(out, s)
	if len(f.FollowUps) > 1 && f.APIMode != "caller_choice" && f.APIMode != "optional_followups" {
		fmt.Fprintln(out, s.dim("  These are observed continuations, not an inferred combination rule. An installable program must expose an exact choice and result bindings."))
	}

	savingsTitle := "Potential savings"
	if f.APIMode == "needs_refinement" {
		savingsTitle = "Historical cost of this pattern (not savings yet)"
	} else if f.APIMode == "optional_followups" {
		savingsTitle = "Historical opportunity in included paths"
	}
	section(out, s, savingsTitle)
	per := 0.0
	if f.ExecutionCount > 0 {
		per = inputEquivalent(f.Saved) / float64(f.ExecutionCount)
	}
	turnLabel := fmt.Sprintf("%s model round-trips the agent no longer makes", count(f.TurnsSaved))
	tokenLabel := fmt.Sprintf("about %s in total, about %s per use (estimate)", tokensText(inputEquivalent(f.Saved)), tokensText(per))
	if f.APIMode == "needs_refinement" {
		turnLabel = fmt.Sprintf("%s observed follow-up turns; no executable program yet", count(f.TurnsSaved))
		tokenLabel = fmt.Sprintf("about %s spent in those turns (estimate)", tokensText(inputEquivalent(f.Saved)))
	} else if f.APIMode == "optional_followups" {
		var eligibleTokens float64
		eligibleTurns := 0
		for _, fu := range f.FollowUps {
			if fu.APIMode != "exact_chain" {
				continue
			}
			eligibleTokens += fu.PotentialTokens * float64(fu.ShapeScore) / 100
			eligibleTurns += fu.PotentialTurns * fu.ShapeScore / 100
		}
		turnLabel = fmt.Sprintf("up to %s recorded follow-up turns on included call shapes", count(eligibleTurns))
		tokenLabel = fmt.Sprintf("about %s spent in those turns (estimate, conditional on reuse)", tokensText(eligibleTokens))
	}
	table{widths: []int{16, 72}, flex: 2, rows: [][]string{
		{"Turns", turnLabel},
		{"Tokens", tokenLabel},
	}}.render(out, s)
	if f.ReadToDecide > 0 {
		fmt.Fprintln(out, s.dim(fmt.Sprintf("  In %d of %d uses the agent built its next step from this output, so it needed to see it; those uses save less.", f.ReadToDecide, f.ExecutionCount)))
	}
	if f.APIMode == "needs_refinement" || f.APIMode == "optional_followups" {
		section(out, s, "Opportunity by continuation")
		for _, fu := range f.FollowUps {
			assessment := "exact chain"
			shape := fu.ShapeSupport
			if shape == "" {
				shape = "not assessed"
			}
			switch fu.APIMode {
			case "needs_refinement":
				assessment = "needs refinement"
			case "synthesis_pending":
				assessment = "synthesis pending"
			case "":
				assessment = "not assessed"
			}
			line := fmt.Sprintf("%s: %d runs · API evidence %d/100 (relationship %d/100; tool route %s) · ~%s potential tokens · %s", followUpText(fu), fu.Runs, fu.Confidence, fu.RelationshipScore, shape, tokensText(fu.PotentialTokens), assessment)
			for _, part := range wrapText(line, max(20, min(s.cols(), screen)-6)) {
				fmt.Fprintln(out, "  "+part)
			}
			if fu.APIReason != "" {
				for _, part := range wrapText("Reason: "+fu.APIReason, max(20, min(s.cols(), screen)-8)) {
					fmt.Fprintln(out, "    "+s.dim(part))
				}
			}
		}
		fmt.Fprintln(out, s.dim("  Token figures are modeled from recorded follow-up turns; they are conditional opportunity, not measured savings. Options may share a first call."))
	}

	if len(f.Inputs) > 0 {
		section(out, s, "What you provide")
		it := table{head: []string{"Step", "Inputs"}, widths: []int{26, 62}, flex: 2}
		for _, in := range f.Inputs {
			cmd, args, _ := strings.Cut(in, ": ")
			it.rows = append(it.rows, []string{display(cmd), inputsText(args)})
		}
		it.render(out, s)
	}

	section(out, s, "Questions in the recorded evidence")
	fmt.Fprintln(out, s.dim("  These are uncertain value links or decisions found in past calls; they are not executable steps."))
	if len(f.Questions) == 0 {
		fmt.Fprintln(out, "  No open evidence questions.")
	} else {
		for i, q := range f.Questions {
			if i == 5 {
				fmt.Fprintf(out, "  …and %d more (included in the exported handoff)\n", len(f.Questions)-5)
				break
			}
			for j, l := range wrapText(q, max(20, min(s.cols(), screen)-6)) {
				lead := "  • "
				if j > 0 {
					lead = "    "
				}
				fmt.Fprintln(out, lead+l)
			}
		}
	}

	tools, piped := map[string]bool{}, map[string]bool{}
	for _, id := range f.Members {
		p := byID[id]
		for _, x := range p.Steps {
			tools[display(headKey(x))] = true
			if parts := strings.Split(x, "+"); len(parts) > 1 {
				for _, y := range parts[1:] {
					piped[display(y)] = true
				}
			}
		}
	}
	sorted := func(m map[string]bool) string {
		var ts []string
		for t := range m {
			ts = append(ts, t)
		}
		sort.Strings(ts)
		return strings.Join(ts, ", ")
	}
	section(out, s, "Details")
	dt := table{widths: []int{20, 68}, flex: 2, rows: [][]string{{"Tools", sorted(tools)}}}
	if len(piped) > 0 {
		dt.rows = append(dt.rows, []string{"Output piped to", sorted(piped) + " (varies by use)"})
	}
	dt.rows = append(dt.rows,
		[]string{"Variants", fmt.Sprintf("%d recorded variations of this flow", len(f.Members))},
		[]string{"Values traced", fmt.Sprintf("%d of %d (where each value comes from is known in every use)", f.Traced, f.Values)},
		[]string{"Raw tokens", fmt.Sprintf("%s, %.0f%% of them the conversation re-read from cache (priced at about a tenth)", tokensText(f.Saved.Total()), cachedShare(f.Saved))})
	dt.render(out, s)
}

// submit writes every choice and says what happened and what to do next:
// accept generates and installs (or, when the program cannot be determined,
// writes the agent handoff instead), decline hides, agent eval writes the
// handoff.
func submit(out io.Writer, s style, shown []Family, choice []string, byID map[string]Primitive, cfg MenuConfig) error {
	var installed, designed, handed, declined []string
	for i, f := range shown {
		switch choice[i] {
		case "accept":
			if f.APIMode == "needs_refinement" {
				return fmt.Errorf("%s has no runnable API: %s; export a refinement handoff instead", title(f), f.APIReason)
			}
			if cfg.Install == nil {
				if err := acceptFamily(cfg.StateDir, f, byID); err != nil {
					return err
				}
				if err := appendLedger(cfg.StateDir, entryFor(f, "accept")); err != nil {
					return err
				}
				installed = append(installed, title(f)+" (candidate saved; no installer available here)")
				continue
			}
			var members []Primitive
			for _, id := range f.Members {
				members = append(members, byID[id])
			}
			r, err := cfg.Install(f, members)
			if err != nil {
				r.Reason = err.Error()
			}
			if r.Installed {
				if err := acceptFamily(cfg.StateDir, f, byID); err != nil {
					return err
				}
				if err := appendLedger(cfg.StateDir, entryFor(f, "accept")); err != nil {
					return err
				}
				line := fmt.Sprintf("%s → installed as %q (%s)", title(f), r.Name, r.Where)
				installed = append(installed, line)
				continue
			}
			where := filepath.Join(cfg.StateDir, "eval", f.Fingerprintdir())
			if err := WriteFamilyHandoff(where, cfg.Home, f, byID, cfg.Sessions, cfg.Skill); err != nil {
				return err
			}
			if err := appendLedger(cfg.StateDir, entryFor(f, "eval")); err != nil {
				return err
			}
			handed = append(handed, fmt.Sprintf("%s: not installed (%s) → %s", title(f), r.Reason, where))
		case "deny":
			if err := appendLedger(cfg.StateDir, entryFor(f, "deny")); err != nil {
				return err
			}
			declined = append(declined, title(f))
		case decisionAcceptDesign:
			if f.APIMode != "needs_refinement" {
				return fmt.Errorf("%s has a runnable API; accept it for installation instead", title(f))
			}
			where := filepath.Join(cfg.StateDir, "design", f.Fingerprintdir())
			if err := WriteFamilyHandoff(where, cfg.Home, f, byID, cfg.Sessions, cfg.Skill); err != nil {
				return err
			}
			if err := appendLedger(cfg.StateDir, entryFor(f, decisionAcceptDesign)); err != nil {
				return err
			}
			designed = append(designed, fmt.Sprintf("%s → %s", title(f), where))
		case "eval":
			where := filepath.Join(cfg.StateDir, "eval", f.Fingerprintdir())
			if err := WriteFamilyHandoff(where, cfg.Home, f, byID, cfg.Sessions, cfg.Skill); err != nil {
				return err
			}
			if err := appendLedger(cfg.StateDir, entryFor(f, "eval")); err != nil {
				return err
			}
			handed = append(handed, fmt.Sprintf("%s → %s", title(f), where))
		}
	}
	if len(installed)+len(designed)+len(handed)+len(declined) == 0 {
		fmt.Fprintln(out, "Nothing chosen; nothing saved.")
		return nil
	}
	if len(installed) > 0 {
		section(out, s, fmt.Sprintf("Installed (%d)", len(installed)))
		for _, l := range installed {
			fmt.Fprintln(out, "  "+s.good("✓")+" "+l)
		}
		fmt.Fprintln(out, s.dim("  Your agent can run these now. Anything that may change data asks you first."))
	}
	if len(designed) > 0 {
		section(out, s, fmt.Sprintf("Accepted for API design (%d)", len(designed)))
		for _, l := range designed {
			fmt.Fprintln(out, "  "+s.good("✓")+" "+l)
		}
		fmt.Fprintln(out, s.dim("  Acceptance is recorded and a design handoff is written. No runnable TAP is installed."))
		fmt.Fprintln(out, "  Start your coding agent on one with: "+agentCommand(cfg.Clients, "<folder above>/HANDOFF.md"))
	}
	if len(handed) > 0 {
		section(out, s, fmt.Sprintf("Refinement handoffs exported (%d)", len(handed)))
		for _, l := range handed {
			fmt.Fprintln(out, "  "+s.info("→")+" "+l)
		}
		fmt.Fprintln(out, s.dim("  Start your agent on one with:"))
		fmt.Fprintln(out, "    "+agentCommand(cfg.Clients, "<folder above>/HANDOFF.md"))
	}
	if len(declined) > 0 {
		section(out, s, fmt.Sprintf("Declined (%d)", len(declined)))
		for _, l := range declined {
			fmt.Fprintln(out, "  "+s.bad("✗")+" "+l)
		}
		fmt.Fprintln(out, s.dim("  Hidden until something new appears under them. Change with: tap discover --revisit"))
	}
	return nil
}

// agentCommand is how to start the person's coding agent on a handoff.
func agentCommand(clients, handoff string) string {
	prompt := fmt.Sprintf("Read %s and follow it.", handoff)
	switch strings.Split(clients, ",")[0] {
	case "codex":
		return fmt.Sprintf("codex %q", prompt)
	case "claude-code":
		return fmt.Sprintf("claude %q", prompt)
	}
	return "Ask your coding agent: " + prompt
}

// Fingerprintdir is a folder name for a family's handoff.
func (f Family) Fingerprintdir() string {
	return strings.NewReplacer("|", "-", ":", "-", "/", "-", " ", "-", "#", "-").Replace(f.Fingerprint)
}

func perRun(f Family) float64 {
	if f.ExecutionCount == 0 {
		return 0
	}
	return f.SavedTokens / float64(f.ExecutionCount)
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
	return nil
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
	return os.WriteFile(filepath.Join(dir, p.ID+".json"), b, 0o600)
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

// listTable is the one-line-per-primitive list; cursor marks the selected
// row (-1 for none).
func listTable(out io.Writer, s style, shown []Family, choice []string, cursor int) {
	if s.cols() < 105 {
		t := table{head: []string{" ", "#", "Proposal", "Uses", "Est.*", "API", "Choice"},
			widths: []int{1, 3, 23, 5, 8, 6, 6}, right: map[int]bool{1: true, 3: true, 4: true}, flex: 2, oneLine: true}
		for i, f := range shown {
			sel := " "
			if i == cursor {
				sel = s.accent("▶")
			}
			api := "exact"
			switch f.APIMode {
			case "optional_followups":
				api = "bundle"
			case "caller_choice":
				api = "choice"
			case "needs_refinement":
				api = "refine"
			}
			t.rows = append(t.rows, []string{sel, fmt.Sprint(i + 1), title(f), count(f.ExecutionCount),
				"~" + tokensText(inputEquivalent(f.Saved)), api, s.choice(choice[i])})
		}
		t.render(out, s)
		fmt.Fprintln(out, s.dim("  * Modeled from historical follow-up turns; refine rows are conditional, not measured savings."))
		return
	}
	t := table{head: []string{" ", "#", "Primitive", "Uses", "Turns*", "Est. tokens*", "Open", "Choice"},
		widths: []int{1, 3, 46, 5, 11, 12, 4, 8}, right: map[int]bool{1: true, 3: true, 4: true, 5: true, 6: true}, flex: 3, oneLine: true}
	for i, f := range shown {
		sel := " "
		if i == cursor {
			sel = s.accent("▶")
		}
		name := title(f)
		switch f.Status {
		case StatusNewSince:
			name += " · new since you " + pastTense(f.Earlier)
		case StatusReevaluated:
			name += " · re-evaluated"
		}
		c := s.choice(choice[i])
		t.rows = append(t.rows, []string{sel, fmt.Sprint(i + 1), name, count(f.ExecutionCount), count(f.TurnsSaved),
			"~" + tokensText(inputEquivalent(f.Saved)), fmt.Sprint(f.OpenQuestions), c})
	}
	t.render(out, s)
	fmt.Fprintln(out, s.dim("  * Modeled from historical follow-up turns; refine rows are conditional, not measured savings."))
}

func pastTense(d string) string {
	switch d {
	case "accept":
		return "accepted it"
	case decisionAcceptDesign:
		return "accepted it for API design"
	case "deny":
		return "declined it"
	case "eval":
		return "exported its handoff"
	}
	return "decided"
}
