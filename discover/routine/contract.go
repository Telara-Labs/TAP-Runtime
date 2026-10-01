package routine

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"

	"gitlab.com/telara-labs/tap-runtime/discover/model"
	"gitlab.com/telara-labs/tap-runtime/discover/redact"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// SplitKey is what a request's run of the group's common steps must agree
// on to be the same procedure: the operation each tool call named and,
// when a step writes, the authority scope it wrote to.
func SplitKey(steps []trace.Step, common map[string]bool) string {
	var parts []string
	seen := map[string]bool{}
	for _, st := range steps {
		if !common[st.Label] || seen[st.Label] {
			continue
		}
		seen[st.Label] = true
		write := trace.StepEffect(st) == "write"
		for _, sl := range st.Slots {
			if sl.Sub {
				continue
			}
			if !strings.HasPrefix(st.Label, "sh:") && trace.SelectorKeys[sl.Key] {
				parts = append(parts, st.Label+"|"+sl.Key+"="+sl.Value)
			}
			if write && trace.IsScopeSlot(st, sl) {
				parts = append(parts, st.Label+"|"+sl.Key+sl.Value)
			}
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x1f")
}

// SplitGroup separates a group's requests by splitKey over the steps at
// least half of them ran. Subgroups keep corpus order.
func SplitGroup(corpus []trace.NormSession, inst []ReqInstance, g []int) [][]int {
	present := map[string]int{}
	for _, i := range g {
		s := corpus[inst[i].Session]
		seen := map[string]bool{}
		for _, si := range inst[i].Steps {
			if l := s.Steps[si].Label; trace.Replayable(l) && !seen[l] {
				seen[l] = true
				present[l]++
			}
		}
	}
	common := map[string]bool{}
	for l, n := range present {
		if 2*n >= len(g) {
			common[l] = true
		}
	}
	byKey := map[string][]int{}
	var order []string
	for _, i := range g {
		s := corpus[inst[i].Session]
		steps := make([]trace.Step, 0, len(inst[i].Steps))
		for _, si := range inst[i].Steps {
			steps = append(steps, s.Steps[si])
		}
		k := SplitKey(steps, common)
		if _, ok := byKey[k]; !ok {
			order = append(order, k)
		}
		byKey[k] = append(byKey[k], i)
	}
	out := make([][]int, 0, len(order))
	for _, k := range order {
		out = append(out, byKey[k])
	}
	return out
}

var GoalWord = regexp.MustCompile(`[a-z][a-z]+`)

// GoalTokens are the words of a request that are not the routine's input
// values: the template the caller filled in.
func GoalTokens(text string, values []string) map[string]bool {
	t := strings.ToLower(text)
	for _, v := range values {
		if v = strings.ToLower(strings.TrimSpace(v)); len(v) >= 2 {
			t = strings.ReplaceAll(t, v, " ")
		}
	}
	out := map[string]bool{}
	for _, w := range GoalWord.FindAllString(t, -1) {
		out[w] = true
	}
	return out
}

func Jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	inter := 0
	for w := range a {
		if b[w] {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}

// SharesRun reports whether v and out share a run of at least n bytes.
func SharesRun(v, out string, n int) bool {
	if len(v) < n || len(out) < n {
		return false
	}
	for i := 0; i+n <= len(v); i += 4 {
		if strings.Contains(out, v[i:i+n]) {
			return true
		}
	}
	return false
}

// ContractRun is one run of a routine with the evidence the contract needs.
type ContractRun struct {
	Steps     []trace.Step `json:"-"`
	All       []trace.Step `json:"-"` // every call of the request, in order
	Text      string       `json:"-"`
	Approvals int          `json:"-"`
}

// BuildContract fills rt.Contract from the draft and its runs, and sets the
// remaining dimensions and the decision.
func BuildContract(rt *model.Routine, d *model.Draft, runs []ContractRun, loops []string) {
	c := &rt.Contract
	c.Inputs = nil
	// Effect and output.
	effect := "read"
	if len(runs) > 0 {
		for _, st := range runs[0].Steps {
			switch trace.StepEffect(st) {
			case "write":
				effect = "write"
			case "unknown":
				if effect != "write" {
					effect = "unknown"
				}
			}
		}
	} else {
		effect = "unknown"
	}
	switch effect {
	case "read":
		c.Effect, c.Output = model.EffectReadOnly, "report"
	case "write":
		c.Effect, c.Output = model.EffectWrites, "state_change"
	default:
		c.Effect, c.Output = model.EffectUnknown, "unknown"
	}
	// Scope: authority constants.
	scope := map[string]bool{}
	if len(runs) > 0 {
		for k, st := range runs[0].Steps {
			for _, sl := range st.Slots {
				if !trace.IsScopeSlot(st, sl) {
					continue
				}
				same := true
				for _, r := range runs[1:] {
					if k >= len(r.Steps) || SlotValue(r.Steps[k], sl.Key) != sl.Value {
						same = false
					}
				}
				if same {
					prog := strings.Fields(strings.TrimPrefix(st.Label, "sh:"))[0]
					if !strings.HasPrefix(st.Label, "sh:") {
						prog = strings.TrimPrefix(st.Label, "mcp:")
					}
					key := strings.SplitN(sl.Key, "#", 2)[0]
					if !strings.HasSuffix(key, "=") {
						key += "="
					}
					scope[prog+" "+key+sl.Value] = true
				}
			}
		}
	}
	c.Scope = nil
	for s := range scope {
		c.Scope = append(c.Scope, s)
	}
	sort.Strings(c.Scope)
	// Approvals.
	c.Approvals = 0
	for _, r := range runs {
		c.Approvals += r.Approvals
	}
	// Inputs and their sources.
	var callerValues []string
	unresolvedRead, unresolvedWrite, composed := 0, 0, 0
	stepEff := func(pos int) string {
		if len(runs) == 0 || pos < 0 || pos >= len(runs[0].Steps) {
			return "unknown"
		}
		return trace.StepEffect(runs[0].Steps[pos])
	}
	for n, in := range d.Inputs {
		ci := model.ContractInput{Name: in.Name, Type: in.Type}
		vals := DraftInputValues(d, n)
		switch {
		case in.List:
			// Only lists the request gave become list inputs (loopSpecs).
			ci.Source = InputCaller
		case in.Sensitive:
			ci.Source = "credential"
		case in.DerivedFrom > 0:
			ci.Source, ci.From = InputPriorOutput, in.DerivedFrom
		default:
			hit, comp, total := 0, 0, 0
			for j, v := range vals {
				if v == "" || j >= len(runs) {
					continue
				}
				total++
				if trace.InRequest(v, runs[j].Text) || ComposedFromRequest(v, runs[j].Text) {
					hit++
					continue
				}
				for _, st := range runs[j].All {
					if st.Output != "" && SharesRun(v, st.Output, 24) {
						comp++
						break
					}
				}
			}
			switch {
			case total > 0 && 2*hit >= total:
				ci.Source = InputCaller
				callerValues = append(callerValues, vals[0])
				for _, v := range vals {
					callerValues = append(callerValues, v)
				}
			case total > 0 && 2*comp >= total:
				ci.Source = InputComposed
				composed++
			default:
				// Nobody stated it and no earlier result held it. On a step
				// that only reads, the agent chose what to look at: the
				// selection rule is missing. On a write it is the action's
				// parameter, which a caller supplies when the goal is stated.
				ci.Source = InputUnresolved
				if stepEff(in.Pos) == "write" {
					unresolvedWrite++
				} else {
					unresolvedRead++
				}
			}
		}
		if in.List {
			ci.Type = "list"
		}
		c.Inputs = append(c.Inputs, ci)
	}
	// Judgment, and whether it only sits at the end: a procedure that runs
	// its steps and then hands back to the agent for the last one (write
	// the summary, decide the fix) names that boundary; judgment in the
	// middle means the steps after it depend on a decision no program made.
	c.Judgment = nil
	firstJudg, lastPlain := -1, -1
	for k, st := range d.Steps {
		if st.Kind == KindHuman {
			c.Judgment = append(c.Judgment, st.Label)
			if firstJudg < 0 {
				firstJudg = k
			}
		} else if st.Kind != KindSkipped {
			lastPlain = k
		}
	}
	for n, in := range c.Inputs {
		if in.Source == InputComposed {
			c.Judgment = append(c.Judgment, "composed:"+in.Name)
			// A composed value's step is where judgment enters.
			if k := d.Inputs[n].Pos; k >= 0 {
				if s := d.PosStep[k]; s > 0 && (firstJudg < 0 || s-1 < firstJudg) {
					firstJudg = s - 1
				}
			}
		}
	}
	c.Boundary = ""
	// Only a human step (drafted as an explicit stop) can be the boundary.
	// A composed value would be drafted as an input to a call, hiding the
	// judgment inside it (plan section 3, item 6).
	if len(c.Judgment) > 0 && composed == 0 && firstJudg > lastPlain && firstJudg >= 2 {
		c.Boundary = "hands back to the agent before: " + strings.Join(c.Judgment, ", ")
	}
	// Goal.
	var toks []map[string]bool
	for k, r := range runs {
		if k == 25 {
			break
		}
		toks = append(toks, GoalTokens(r.Text, callerValues))
	}
	var sims []float64
	for a := 0; a < len(toks); a++ {
		for b := a + 1; b < len(toks); b++ {
			sims = append(sims, Jaccard(toks[a], toks[b]))
		}
	}
	sort.Float64s(sims)
	hasCaller := false
	for _, in := range c.Inputs {
		if in.Source == InputCaller {
			hasCaller = true
		}
	}
	switch {
	case len(sims) > 0 && sims[len(sims)/2] >= 0.5:
		c.Goal = model.GoalStated
	case unresolvedRead == 0 && unresolvedWrite == 0 && (len(c.Judgment) == 0 || c.Boundary != "") && hasCaller:
		c.Goal = model.GoalSelfContained
	default:
		c.Goal = model.GoalUnknown
	}
	// Family: role, goal template, effect and output.
	fam := map[string]int{}
	for _, t := range toks {
		for w := range t {
			fam[w]++
		}
	}
	var words []string
	for w, k := range fam {
		if 2*k >= len(toks) {
			words = append(words, w)
		}
	}
	sort.Strings(words)
	if c.Goal == model.GoalUnknown {
		rt.Family = "unknown:" + rt.ID
	} else {
		h := sha256.Sum256([]byte(rt.SourceRole + "\x00" + strings.Join(words, " ") + "\x00" + c.Effect + "\x00" + c.Output))
		rt.Family = "fam_" + hex.EncodeToString(h[:6])
	}

	Decide(rt, d, loops, unresolvedRead, unresolvedWrite)
}

// Decide applies the suitability rules in order, then the draft status,
// outcome and value dimensions, and derives the legacy decision.
func Decide(rt *model.Routine, d *model.Draft, loops []string, unresolvedRead, unresolvedWrite int) {
	c := &rt.Contract
	// A loop is bounded when its list's source is known: the request, or an
	// earlier result. Otherwise the agent chose the items as it went; over
	// reads that is exploration, over writes it is unknown selection.
	var openReadLoops, openOtherLoops, priorLoops []string
	for _, l := range loops {
		switch {
		case d != nil && d.ListLoop[l]:
		case d != nil && d.PriorLoop[l]:
			priorLoops = append(priorLoops, l)
		default:
			eff := "unknown"
			if d != nil {
				for p, st := range rt.Steps {
					if st.Label == l && len(d.FirstRun) > p {
						eff = trace.StepEffect(d.FirstRun[p])
					}
				}
			}
			if eff == "read" || eff == "unknown" {
				openReadLoops = append(openReadLoops, l)
			} else {
				openOtherLoops = append(openOtherLoops, l)
			}
		}
	}
	rt.Reasons = nil
	reason := func(s string) { rt.Reasons = append(rt.Reasons, s) }
	nIn := len(c.Inputs)
	unresolvedNames := func() {
		for _, in := range c.Inputs {
			if in.Source == InputUnresolved {
				reason("unresolved_input:" + in.Name)
			}
		}
	}
	switch {
	case rt.SourceRole == model.RoleHarness:
		rt.Suitability = model.SuitInvalid
		reason("harness_request")
	case rt.SourceRole == model.RoleInfrastructure:
		rt.Suitability = model.SuitInsufficient
		reason("infrastructure_only")
	case len(openReadLoops) > 0 && c.Goal != model.GoalStated:
		// Different asks, and the agent picked what to read as it went.
		rt.Suitability = model.SuitInvestigation
		reason("loop_unbounded:" + strings.Join(openReadLoops, ","))
	case len(openReadLoops) > 0:
		// One stated task whose loop items came from somewhere the history
		// does not show (a page, a file beyond what was recorded): abstain.
		rt.Suitability = model.SuitInsufficient
		reason("loop_source_unknown:" + strings.Join(openReadLoops, ","))
	case len(openOtherLoops) > 0:
		rt.Suitability = model.SuitInsufficient
		reason("loop_selection_unknown:" + strings.Join(openOtherLoops, ","))
	case nIn > 0 && 2*unresolvedRead > nIn && c.Goal != model.GoalStated:
		rt.Suitability = model.SuitInvestigation
		reason("values_chosen_during_run")
	case unresolvedRead > 0:
		rt.Suitability = model.SuitInsufficient
		unresolvedNames()
	case len(c.Judgment) > 0 && c.Boundary == "":
		rt.Suitability = model.SuitInsufficient
		reason("judgment_step")
	case unresolvedWrite > 0 && c.Goal != model.GoalStated:
		rt.Suitability = model.SuitInsufficient
		unresolvedNames()
	case c.Goal == model.GoalUnknown:
		rt.Suitability = model.SuitInsufficient
		reason("goal_unknown")
	case rt.Consistency < 0.5:
		rt.Suitability = model.SuitInsufficient
		reason("inconsistent_order")
	case EphemeralConstant(d):
		// Fixed to a temporary directory (a test scratchpad): it cannot be
		// rerun anywhere else.
		rt.Suitability = model.SuitInsufficient
		reason("ephemeral_constant")
	case !HasParam(c) && rt.Coverage < 0.5:
		// Nothing to parameterize and a small part of what the requests
		// did: the constant opening of varying work (open the browser,
		// print the directory), not the procedure.
		rt.Suitability = model.SuitInsufficient
		reason("constant_part_of_larger_work")
	default:
		rt.Suitability = model.SuitUseful
		reason("contract_complete:" + c.Goal)
	}
	// Draft status.
	rt.Blockers = nil
	switch {
	case rt.Suitability != model.SuitUseful:
		rt.DraftStatus = model.DraftNotAttempted
	case d == nil:
		rt.DraftStatus = model.DraftNotAttempted
	case len(d.Blocked) > 0:
		rt.DraftStatus = model.DraftBlocked
	default:
		for _, in := range d.Inputs {
			if in.DerivedFrom > 0 && in.Extract == "" {
				rt.Blockers = append(rt.Blockers, "output_binding:"+in.Name)
			}
		}
		if c.Approvals > 0 {
			rt.Blockers = append(rt.Blockers, "approval_between_steps")
		}
		for _, l := range priorLoops {
			rt.Blockers = append(rt.Blockers, "loop_collection_binding:"+l)
		}
		rt.Blockers = append(rt.Blockers, d.RuntimeUnsupported...)
		if len(d.Problems) > 0 {
			rt.Blockers = append(rt.Blockers, "manifest_problems")
		}
		if len(rt.Blockers) > 0 {
			rt.DraftStatus = model.DraftNeedsAuthor
		} else {
			rt.DraftStatus = model.DraftComplete
		}
	}
	// Legacy single decision, kept for callers that list primitives.
	switch {
	case rt.Suitability == model.SuitUseful && rt.SourceRole == model.RoleScheduled:
		rt.Decision = "baseline"
	case rt.Suitability == model.SuitUseful && rt.DraftStatus == model.DraftComplete:
		rt.Decision = "primitive"
	case rt.Suitability == model.SuitUseful:
		rt.Decision = "needs_authoring"
	default:
		rt.Decision = "removed"
	}
	if rt.Decision == "removed" {
		rt.Failed = strings.SplitN(rt.Reasons[0], ":", 2)[0]
	} else {
		rt.Failed = ""
	}
	rt.Why = strings.Join(rt.Reasons, "; ")
	if len(rt.Blockers) > 0 {
		rt.Why += "; blockers: " + strings.Join(rt.Blockers, ", ")
	}
}

// HasParam reports an input a caller or an earlier result supplies.
func HasParam(c *model.Contract) bool {
	for _, in := range c.Inputs {
		if in.Source == InputCaller || in.Source == InputPriorOutput || in.Source == InputUnresolved {
			return true
		}
	}
	return false
}

var CompositeSep = regexp.MustCompile(`\.\.\.?|[=:,]`)

// ComposedFromRequest reports a value built from what the request said:
// split at the operators that join values (a range a..b, a selector k=v, a
// list a,b), some part is in the request and every other part is a short
// fixed word (a key such as involvedObject.name). A path is not split, so
// a file under a directory the request named is still the agent's choice.
func ComposedFromRequest(v, text string) bool {
	if !CompositeSep.MatchString(v) || strings.ContainsAny(v, " /") {
		return false
	}
	parts := CompositeSep.Split(v, -1)
	in := 0
	for _, p := range parts {
		p = strings.TrimSpace(p)
		switch {
		case p == "":
		case len(p) >= 3 && trace.InRequest(p, text):
			in++
		case !strings.ContainsAny(p, "0123456789") && len(p) <= 40:
			// a fixed key or keyword
		default:
			return false
		}
	}
	return in > 0
}

// EphemeralPath matches an agent session's own scratch locations, which do
// not exist outside that session: a client's per-session temp directory or
// a scratchpad. An ordinary /tmp file (a log a procedure writes) is fine.
var EphemeralPath = regexp.MustCompile(`(/private)?/tmp/claude-|/var/folders/|/scratchpad/`)

// EphemeralConstant reports a value the same in every drafted run that
// points into a temporary location.
func EphemeralConstant(d *model.Draft) bool {
	if d == nil || len(d.FirstRun) == 0 {
		return false
	}
	varying := map[string]bool{}
	for _, in := range d.Inputs {
		varying[in.Example] = true
	}
	for _, st := range d.FirstRun {
		for _, sl := range st.Slots {
			if EphemeralPath.MatchString(sl.Value) && !varying[redact.Redact(sl.Value)] {
				return true
			}
		}
	}
	return false
}
