// Package routine finds recurring work in normalized sessions and decides what it is:
// mining, significance, request-level routines, their task contracts and states,
// drafting a TAP package, and the review of what passed.
package routine

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"math/rand"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"

	"gitlab.com/telara-labs/tap-runtime/discover/model"
	"gitlab.com/telara-labs/tap-runtime/discover/pack"
	"gitlab.com/telara-labs/tap-runtime/discover/redact"
	"gitlab.com/telara-labs/tap-runtime/discover/shellparse"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
	"gitlab.com/telara-labs/tap-runtime/discover/util"
	"gitlab.com/telara-labs/tap-runtime/contract/manifest"
	"gopkg.in/yaml.v3"
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

// DefaultPublisher names a draft that has not been given a namespace yet.
const DefaultPublisher = "local.draft"

// Step kinds.
const (
	KindCommand = "command" // a host program, declared in commands[]
	KindTool    = "tool"    // an MCP tool, declared in tools[] and capabilities[]
	KindBrowser = "browser" // awaited calls in a browser script, one tool call
	KindFetch   = "fetch"   // a page fetch, declared in fetch[]
	KindHuman   = "human"   // content decided per run: written as a marked stop
	KindSkipped = "skipped" // the agent's own bookkeeping: nothing to replay
)

// inputValues is input n's value in each drafted run (by run index).
func DraftInputValues(d *model.Draft, n int) map[int]string {
	if n < 0 || n >= len(d.Values) {
		return nil
	}
	return d.Values[n]
}

// Draft builds the package for Candidates[idx].
func ReportDraft(r *model.Report, idx int, opt model.DraftOptions) (*model.Draft, error) {
	if idx < 0 || idx >= len(r.Candidates) || r.Corpus == nil {
		return nil, errors.New("no such candidate in this run")
	}
	c := r.Candidates[idx]
	if len(c.Items) == 0 {
		return nil, errors.New("this candidate cannot be drafted")
	}
	var occ [][]trace.Step
	sessions := make([]int, 0, len(c.SessionSet))
	for s := range c.SessionSet {
		sessions = append(sessions, s)
	}
	sort.Ints(sessions)
	for _, s := range sessions {
		idxs := trace.MatchAt(r.Seqs[s], c.Items, r.Window)
		if idxs == nil {
			continue
		}
		steps := make([]trace.Step, len(idxs))
		for i, j := range idxs {
			steps[i] = r.Corpus[s].Steps[j]
		}
		occ = append(occ, steps)
	}
	if len(occ) == 0 {
		return nil, errors.New("no occurrence of this candidate could be read back")
	}
	return BuildDraft(c, occ, opt), nil
}

// SlotPlan is one argument position of one step: a fixed value, or an input.
type SlotPlan struct {
	Slot  trace.Slot `json:"-"`
	Fixed bool       `json:"-"`
	Input int        `json:"-"` // index into inputs when not fixed
}

type Drafter struct {
	Opt      model.DraftOptions `json:"-"`
	Occ      [][]trace.Step     `json:"-"`
	Inputs   []model.DraftInput `json:"-"`
	Vectors  []map[int]string   `json:"-"` // per input: occurrence -> value
	Names    map[string]bool    `json:"-"`
	Mf       manifest.Manifest  `json:"-"`
	Cmds     map[string]bool    `json:"-"`
	Files    map[string]bool    `json:"-"`
	Origins  map[string]bool    `json:"-"`
	Tools    map[string]bool    `json:"-"`
	Lines    []string           `json:"-"`
	Steps    []model.DraftStep  `json:"-"`
	HumanCnt int                `json:"-"`
	FixedCnt int                `json:"-"`
	FixedArg int                `json:"-"` // arguments that had the same value in every run
	// posStep maps a pattern position to the draft step that replays it.
	PosStep    map[int]int     `json:"-"`
	DerivedCnt int             `json:"-"`
	Extracted  int             `json:"-"`
	ListLoop   map[string]bool `json:"-"`
	PriorLoop  map[string]bool `json:"-"`
	UsesJSON   bool            `json:"-"`
	UsesGrep   bool            `json:"-"`
	// unsupported names steps the runtime cannot run as recorded.
	Unsupported []string `json:"-"`
}

// GuestReadsStdinOnly are the guest shell's own built-ins that read only
// standard input and ignore file arguments (tap-runtime guest-sh): a
// recorded "wc -l file" would count nothing there.
var GuestReadsStdinOnly = map[string]bool{"wc": true, "head": true}

// checkGuest records a step the guest runtime would not run as recorded.
func (d *Drafter) CheckGuest(prog string, plans []SlotPlan, n int) {
	if prog == "cat" {
		for _, p := range plans {
			if !p.Slot.Sub && p.Slot.Type != trace.SlotFlag {
				d.FileAccess(p, n)
			}
		}
		return
	}
	if !GuestReadsStdinOnly[prog] {
		return
	}
	for _, p := range plans {
		// "wc -l file" parses as the flag -l taking "file"; only a number
		// after a flag (head -n 20) is an option's value.
		if p.Slot.Sub || p.Slot.Type == trace.SlotFlag || p.Slot.Type == trace.SlotNumber {
			continue
		}
		d.Unsupported = append(d.Unsupported, fmt.Sprintf("guest_builtin_ignores_files:step%d:%s", n, prog))
		return
	}
}

// fileAccess declares a fixed file a step reads, so the host lets the
// guest open it; a path that varies cannot be declared ahead of time.
func (d *Drafter) FileAccess(p SlotPlan, n int) {
	if !p.Fixed {
		d.Unsupported = append(d.Unsupported, fmt.Sprintf("file_access_undeclared:step%d:%s", n, d.Inputs[p.Input].Name))
		return
	}
	if !d.Files[p.Slot.Value] {
		d.Files[p.Slot.Value] = true
		d.Mf.Files = append(d.Mf.Files, manifest.File{Path: p.Slot.Value, Access: "read"})
	}
}

// ref is a placeholder for input k in a generated line. finish replaces it
// with the argument ("${3}") or, for a value taken from an earlier step's
// output, that shell variable.
func (d *Drafter) Ref(k int) string { return "\x01" + util.Itoa(k) + "\x01" }

func BuildDraft(c model.Candidate, occ [][]trace.Step, opt model.DraftOptions) *model.Draft {
	if opt.Publisher == "" {
		opt.Publisher = DefaultPublisher
	}
	d := &Drafter{Opt: opt, Occ: occ, ListLoop: map[string]bool{}, PriorLoop: map[string]bool{}, PosStep: map[int]int{}, Names: map[string]bool{}, Cmds: map[string]bool{}, Files: map[string]bool{}, Origins: map[string]bool{}, Tools: map[string]bool{}}
	for i := 0; i < len(c.Items); {
		label := c.Steps[i].Label
		d.PosStep[i] = len(d.Steps) + 1
		if strings.HasPrefix(label, "js:") {
			// Consecutive browser calls were one script: they stay one call.
			j := i
			for j < len(c.Items) && strings.HasPrefix(c.Steps[j].Label, "js:") {
				d.PosStep[j] = len(d.Steps) + 1
				j++
			}
			d.Browser(i, j)
			i = j
			continue
		}
		if strings.HasPrefix(label, "sh:") && d.Occ[0][i].Compound {
			// One recorded command line (a pipeline, a chain, a heredoc)
			// that holds this step and possibly the next ones.
			j := i + 1
			for j < len(c.Items) && SameCall(d.Occ, i, j) {
				d.PosStep[j] = len(d.Steps) + 1
				j++
			}
			d.Compound(i, j)
			i = j
			continue
		}
		d.Step(i, label)
		i++
	}

	d.Finish()
	name := DraftName(c)
	desc := fmt.Sprintf("Unvalidated draft (never executed). Recurring routine found by tap discover in %d sessions over %d weeks (%s). Steps: %s.",
		c.Sessions, c.Weeks, ClientList(c.ByClient), model.LabelsOf(c))
	props := map[string]any{}
	var required []string
	for _, in := range d.Inputs {
		if in.Extract != "" {
			continue
		}
		props[in.Name] = map[string]any{
			"type":        "string",
			"description": fmt.Sprintf("Argument %d ($%d): %s, from %s.", in.Position, in.Position, in.Type, in.From),
		}
		required = append(required, in.Name)
	}
	in := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		in["required"] = required
	}
	d.Mf.APIVersion, d.Mf.Kind = manifest.APIVersion, "Primitive"
	d.Mf.Metadata = manifest.Metadata{Publisher: opt.Publisher, Name: name, Version: "0.1.0", Description: desc,
		OutputDescription: "What the steps print, in order."}
	d.Mf.Interface = &manifest.Interface{InputSchema: in, OutputSchema: map[string]any{"type": "object"}}
	d.Mf.Execution = manifest.Execution{Runtime: manifest.RuntimeWasm, Entrypoint: "main.sh"}
	d.Mf.Provenance = &manifest.Provenance{Source: "main.sh", Toolchain: "interpreter obtained by the runner, pinned by sha256", Build: "none: the entrypoint is the source"}

	yml, _ := yaml.Marshal(&d.Mf)
	var problems []string
	if m, err := manifest.Parse(yml); err != nil {
		problems = []string{err.Error()}
	} else {
		problems = m.PublishProblems()
	}

	var sh strings.Builder
	fmt.Fprintf(&sh, "# %s\n# Drafted by telara tap discover. Review every line before running or publishing.\n", desc)
	for _, in := range d.Inputs {
		if in.Sensitive {
			fmt.Fprintf(&sh, "# $%d  %s: a credential; supply it from your own configuration (no recorded value is kept)\n", in.Position, in.Name)
		} else if in.Extract != "" {
			fmt.Fprintf(&sh, "# %s (%s): taken from step %d's output\n", in.Name, in.Type, in.DerivedFrom)
		} else if in.DerivedFrom > 0 {
			fmt.Fprintf(&sh, "# $%d  %s (%s): in the recorded runs this came from step %d's output; take it from there (authoring needed)\n", in.Position, in.Name, in.Type, in.DerivedFrom)
		} else {
			fmt.Fprintf(&sh, "# $%d  %s (%s)\n", in.Position, in.Name, in.Type)
		}
	}
	// A failing step stops the program, including one inside a pipeline, so
	// a check that fails never lets a later write run.
	sh.WriteString("set -eo pipefail\n")
	if d.UsesJSON {
		sh.WriteString(JsonHelpers + "\n")
	}
	for _, l := range d.Lines {
		sh.WriteString(l)
		sh.WriteByte('\n')
	}

	files := map[string][]byte{
		"primitive.yaml": yml,
		"main.sh":        []byte(sh.String()),
		"README.md":      []byte(DraftReadme(name, desc, d)),
	}
	return &model.Draft{
		Blocked: redact.ScanArtifacts(files),
		Name:    name, Publisher: opt.Publisher, Inputs: d.Inputs, Steps: d.Steps,
		RuntimeUnsupported: d.Unsupported,
		ListLoop:           d.ListLoop, PriorLoop: d.PriorLoop, HumanPos: d.HumanPositions(), FirstRun: occ[0], PosStep: d.PosStep,
		Problems: problems, HumanSteps: d.HumanCnt, Derived: d.DerivedCnt, Extracted: d.Extracted, FixedSteps: d.FixedCnt, FixedShare: FixedShare(d), Values: d.Vectors,
		Files: files,
	}
}

func (d *Drafter) Effect(n int) string {
	if d.Opt.ReadOnly[n] {
		return "read"
	}
	return "write"
}

// plan decides, for step i, which arguments are fixed and which are inputs.
// It uses the occurrences with the most common skeleton and argument keys,
// and compares values key by key, so arguments given in a different order
// still line up.
func (d *Drafter) Plan(i int, stepName string) []SlotPlan {
	sig := func(st trace.Step) string {
		var ks []string
		for _, sl := range st.Slots {
			if !trace.Derived(sl.Key) {
				ks = append(ks, sl.Key)
			}
		}
		sort.Strings(ks)
		return st.Skeleton + "#" + strings.Join(ks, ",")
	}
	count := map[string]int{}
	for _, o := range d.Occ {
		count[sig(o[i])]++
	}
	modal, best := "", -1
	for k, v := range count {
		if v > best || (v == best && k < modal) {
			modal, best = k, v
		}
	}
	var reps []int
	for j, o := range d.Occ {
		if sig(o[i]) == modal {
			reps = append(reps, j)
		}
	}
	valueOf := func(st trace.Step, key string) string {
		for _, sl := range st.Slots {
			if sl.Key == key {
				return sl.Value
			}
		}
		return ""
	}
	first := d.Occ[reps[0]][i]
	var plans []SlotPlan
	for _, sl := range first.Slots {
		if trace.Derived(sl.Key) {
			continue
		}
		p := SlotPlan{Slot: sl}
		if sl.Sub {
			p.Fixed = true
			plans = append(plans, p)
			continue
		}
		vec := map[int]string{}
		same := true
		for _, j := range reps {
			v := valueOf(d.Occ[j][i], sl.Key)
			vec[j] = v
			if v != sl.Value {
				same = false
			}
		}
		// A value that looks like a credential in any recorded run is never
		// written, however constant: it becomes a credential input.
		sensitive := false
		for _, j := range reps {
			for _, other := range d.Occ[j][i].Slots {
				if other.Key == sl.Key && redact.SensitiveSlot(d.Occ[j][i].Label, other) {
					sensitive = true
				}
				// A tool argument some run sent as JSON (an object, an
				// array, a boolean) is JSON, even where another client
				// recorded it as text.
				if other.Key == sl.Key && other.Raw && !sl.Raw && json.Valid([]byte(other.Value)) {
					sl.Raw, sl.Value = true, other.Value
				}
			}
		}
		if t, ok := ArgSchema(sl)["type"].(string); ok && sl.Raw {
			sl.Type = t
		}
		p.Slot = sl
		switch {
		case sensitive:
			p.Input = d.Input(stepName, sl, vec, true, i)
		case same && len(reps) > 1:
			p.Fixed = true
			d.FixedArg++
		case sl.Key == "recv":
			// A JS variable the author named differently each run is not an
			// input anyone would pass; the browser step names it itself.
			p.Input = -1
		default:
			p.Input = d.Input(stepName, sl, vec, false, i)
		}
		plans = append(plans, p)
	}
	return plans
}

// input returns the input for a varying slot, reusing an earlier input that
// held the same value in every occurrence both appear in (a path passed to
// gofmt and then to go test is one input, not two).
func (d *Drafter) Input(stepName string, sl trace.Slot, vec map[int]string, sensitive bool, pos int) int {
	for n, other := range d.Vectors {
		if d.Inputs[n].Sensitive != sensitive {
			continue
		}
		common, agree := 0, true
		for j, v := range vec {
			if w, ok := other[j]; ok {
				common++
				if v != w {
					agree = false
					break
				}
			}
		}
		if agree && common >= 2 {
			return n
		}
	}
	base := stepName
	key := strings.SplitN(sl.Key, "#", 2)[0]
	switch {
	case strings.HasSuffix(key, "="):
		base += "_" + strings.TrimLeft(strings.TrimSuffix(key, "="), "-")
	case len(key) > 1 && key[0] == 'p' && IsDigits(key[1:]):
		base += "_arg" + key[1:]
	case key == "recv":
		base += "_object"
	default:
		base += "_" + strings.TrimLeft(key, "-")
	}
	name := SanitizeName(base)
	for k := 2; d.Names[name]; k++ {
		name = fmt.Sprintf("%s_%d", SanitizeName(base), k)
	}
	d.Names[name] = true
	in := model.DraftInput{Name: name, Type: sl.Type, Raw: sl.Raw, Example: redact.Redact(sl.Value), From: stepName, Pos: pos}
	if !sensitive {
		if h := d.DerivedFrom(pos, vec); h >= 0 {
			in.DerivedFrom = d.PosStep[h]
			if pat, strip, binding, ok := d.Extraction(h, vec); ok && d.Capturable(in.DerivedFrom) {
				in.Extract, in.Strip, in.Binding = pat, strip, binding
				d.Extracted++
			} else {
				d.DerivedCnt++
			}
		}
	}
	if sensitive {
		in.Sensitive, in.Type, in.Example = true, trace.SlotSecret, ""
	}
	d.Inputs = append(d.Inputs, in)
	d.Vectors = append(d.Vectors, vec)
	return len(d.Inputs) - 1
}

func (d *Drafter) Step(i int, label string) {
	n := len(d.Steps) + 1
	switch {
	case strings.HasPrefix(label, "sh:"):
		d.Command(i, n, label)
	case strings.HasPrefix(label, "mcp:"):
		d.Tool(i, n, strings.TrimPrefix(label, "mcp:"), label)
	case strings.HasPrefix(label, "patch:"):
		d.Human(i, n, label, "file")
	default:
		d.Builtin(i, n, label)
	}
}

func (d *Drafter) Command(i, n int, label string) {
	fields := strings.Fields(strings.TrimPrefix(label, "sh:"))
	prog := fields[0]
	stepName := strings.Join(fields, "_")
	plans := d.Plan(i, stepName)
	// A step run once per item of a list the request gave becomes a loop
	// over a list input.
	spec, isLoop := d.Opt.Loops[label]
	loopIn := -1
	if isLoop && spec.Source == "prior_output" {
		// The list came from an earlier result; binding a collection out of
		// a result is not drafted, so this step is marked for authoring.
		d.PriorLoop[label] = true
		isLoop = false
	}
	if isLoop {
		for _, p := range plans {
			if p.Slot.Key == spec.Key && !p.Fixed && p.Input >= 0 {
				loopIn = p.Input
			}
		}
	}
	if loopIn >= 0 {
		in := &d.Inputs[loopIn]
		in.List, in.Raw, in.Type, in.Example = true, true, "list", ""
		vec := map[int]string{}
		for j, vs := range spec.Values {
			if vs == nil {
				continue
			}
			b, _ := json.Marshal(vs)
			vec[j] = string(b)
		}
		d.Vectors[loopIn] = vec
		d.ListLoop[label] = true
	}
	d.CheckGuest(prog, plans, n)
	words := []string{prog}
	var globals []string
	subSeen, sub := false, ""
	for k, p := range plans {
		switch {
		case p.Slot.Sub:
			subSeen, sub = true, p.Slot.Value
			words = append(words, p.Slot.Value)
		case p.Fixed:
			words = append(words, ShellQuote(p.Slot.Value))
		case p.Input == loopIn:
			words = append(words, `"$item"`)
		default:
			words = append(words, `"`+d.Ref(p.Input)+`"`)
		}
		// Flags before the subcommand are the program's global flags; the
		// word keyed "<flag>=" after one is its value.
		if !subSeen && len(fields) > 1 && p.Slot.Type == trace.SlotFlag {
			g := p.Slot.Value
			if k+1 < len(plans) && plans[k+1].Slot.Key == p.Slot.Key+"=" {
				if plans[k+1].Fixed {
					g += " " + plans[k+1].Slot.Value
				} else {
					g += " <any>"
				}
			}
			globals = append(globals, g)
		}
	}
	args := []string{"*"}
	if sub != "" {
		args = []string{sub, "*"}
	}
	eff := d.Effect(n)
	key := prog + "\x00" + strings.Join(globals, "\x00") + "\x00" + strings.Join(args, "\x00") + "\x00" + eff
	if !d.Cmds[key] {
		d.Cmds[key] = true
		d.Mf.Commands = append(d.Mf.Commands, manifest.Command{Command: prog, Globals: globals, Args: args, Effect: eff})
	}
	line := strings.Join(words, " ")
	if loopIn >= 0 {
		// Each item on its own line from the JSON array; an empty list runs
		// nothing; a malformed list fails the pipeline (pipefail).
		line = fmt.Sprintf(`printf '%%s' "%s" | jq -r '.[]' | while IFS= read -r item; do %s; done`, d.Ref(loopIn), line)
		d.Lines = append(d.Lines, fmt.Sprintf("# %d. %s (once per item of the list; an empty list runs nothing)", n, label), line)
	} else {
		d.Lines = append(d.Lines, fmt.Sprintf("# %d. %s", n, label), line)
	}
	fixed := len(fields) > 1
	for _, p := range plans {
		if p.Fixed && !p.Slot.Sub && p.Slot.Type != trace.SlotFlag && p.Slot.Type != trace.SlotNumber {
			fixed = true
		}
	}
	if fixed {
		d.FixedCnt++
	}
	d.Steps = append(d.Steps, model.DraftStep{N: n, Kind: KindCommand, Label: label, Line: line, Effect: eff})
}

var NonAlias = regexp.MustCompile(`[^a-z0-9_]+`)

// JsIdentifier matches an argument that is only a variable (url1, row.href):
// a value the recorded script computed, not one written into the call.
var JsIdentifier = regexp.MustCompile(`^[A-Za-z_$][\w$]*(\.[A-Za-z_$][\w$]*)*$`)

func (d *Drafter) Tool(i, n int, tool, label string) {
	alias := strings.Trim(NonAlias.ReplaceAllString(strings.ToLower(tool), "_"), "_")
	plans := d.Plan(i, alias)
	props := map[string]any{}
	var keys []string
	var pre []string
	var obj JsonObject
	for _, p := range plans {
		keys = append(keys, p.Slot.Key)
		props[p.Slot.Key] = ArgSchema(p.Slot)
		if p.Fixed {
			obj.Fixed(p.Slot.Key, JsonLiteral(p.Slot))
			continue
		}
		v := fmt.Sprintf("a%d_%d", n, p.Input+1)
		pre = append(pre, d.JsonAssign(v, p.Input))
		obj.Variable(p.Slot.Key, v)
	}
	sort.Strings(keys)
	args := map[string]any{"type": "object", "properties": props, "required": AnySlice(keys)}
	result := map[string]any{"type": "object"}
	question := fmt.Sprintf("Run %s with %s, as the recorded sessions did.", tool, OrNone(keys))
	capLabel := d.Opt.Publisher + "/" + CapName(tool) + "@1"
	eff := d.Effect(n)
	if !d.Tools[alias] {
		d.Tools[alias] = true
		d.Mf.Capabilities = append(d.Mf.Capabilities, manifest.Capability{Label: capLabel, ID: manifest.CapabilityID(question, args, result), Question: question, Args: args, Result: result})
		d.Mf.Tools = append(d.Mf.Tools, manifest.Tool{Alias: alias, Capability: capLabel, Effect: eff})
	}
	line := fmt.Sprintf(`tap call %s %s`, alias, obj.Word())
	d.Lines = append(d.Lines, fmt.Sprintf("# %d. %s", n, label))
	d.Lines = append(d.Lines, pre...)
	d.Lines = append(d.Lines, line)
	d.FixedCnt++ // an MCP tool names one action in one system
	d.Steps = append(d.Steps, model.DraftStep{N: n, Kind: KindTool, Label: label, Line: line, Effect: eff})
}

// browser writes steps i..j-1 (awaited calls of one script) as one call to
// the browser tool, rebuilding the script with this run's inputs.
func (d *Drafter) Browser(i, j int) {
	n := len(d.Steps) + 1
	var labels, pre []string
	// The script is built as one shell word: fixed JavaScript in single
	// quotes, each input spliced in as a JSON value (a JS literal).
	var code strings.Builder
	varyingObject, browserFixed := false, false
	var scriptVars []string
	for k := i; k < j; k++ {
		method := strings.TrimPrefix(d.Occ[0][k].Label, "js:")
		labels = append(labels, method)
		recv, arg := "tab", ""
		for _, p := range d.Plan(k, strings.ReplaceAll(method, ".", "_")) {
			switch {
			case p.Slot.Key == "recv" && p.Fixed:
				recv = p.Slot.Value
			case p.Slot.Key == "recv":
				varyingObject = true
			case p.Slot.Key != "0":
			case p.Fixed && p.Slot.Raw:
				browserFixed = true
				if JsIdentifier.MatchString(p.Slot.Value) {
					scriptVars = append(scriptVars, p.Slot.Value)
				}
				arg = ShellSingle(p.Slot.Value)
			case p.Fixed:
				browserFixed = true
				b, _ := json.Marshal(p.Slot.Value)
				arg = ShellSingle(string(b))
			default:
				v := fmt.Sprintf("a%d_%d", n, p.Input+1)
				if p.Slot.Raw {
					if JsIdentifier.MatchString(p.Slot.Value) {
						scriptVars = append(scriptVars, p.Slot.Value)
					}
					// A JavaScript expression the caller supplies, as is.
					pre = append(pre, fmt.Sprintf(`%s="%s"`, v, d.Ref(p.Input)))
				} else {
					d.UsesJSON = true
					pre = append(pre, fmt.Sprintf(`%s=$(json_str "%s")`, v, d.Ref(p.Input)))
				}
				arg = `"$` + v + `"`
			}
		}
		code.WriteString(ShellSingle("await "+recv+"."+method+"(") + arg + ShellSingle("); "))
	}
	question := "Run a browser script in the user's open browser session and return what it reports."
	args := map[string]any{"type": "object", "properties": map[string]any{"code": map[string]any{"type": "string"}}, "required": []any{"code"}}
	result := map[string]any{"type": "object"}
	capLabel := d.Opt.Publisher + "/browser.script@1"
	eff := d.Effect(n)
	if !d.Tools["browser"] {
		d.Tools["browser"] = true
		d.Mf.Capabilities = append(d.Mf.Capabilities, manifest.Capability{Label: capLabel, ID: manifest.CapabilityID(question, args, result), Question: question, Args: args, Result: result})
		d.Mf.Tools = append(d.Mf.Tools, manifest.Tool{Alias: "browser", Capability: capLabel, Effect: eff})
	}
	cv := fmt.Sprintf("code%d", n)
	pre = append(pre, cv+"="+code.String())
	d.UsesJSON = true
	pre = append(pre, fmt.Sprintf(`%s_json=$(json_str "$%s")`, cv, cv))
	var obj JsonObject
	obj.Variable("code", cv+"_json")
	line := fmt.Sprintf(`tap call browser %s`, obj.Word())
	var notes []string
	if varyingObject {
		notes = append(notes, "Some calls were made on a variable the author named differently each run; the draft calls it tab. Open or select that tab first.")
	}
	if len(scriptVars) > 0 {
		// goto(url1): the value was a variable the original script computed
		// before the call. Extracting the call cannot recover it.
		notes = append(notes, "Uses values the original script computed before these calls ("+strings.Join(scriptVars, ", ")+"); this step needs the rest of that script before it can run.")
		d.HumanCnt++
	}
	note := strings.Join(notes, " ")
	label := "browser: " + strings.Join(labels, " → ")
	d.Lines = append(d.Lines, fmt.Sprintf("# %d. %s", n, label))
	if note != "" {
		d.Lines = append(d.Lines, "# "+note)
	}
	d.Lines = append(d.Lines, pre...)
	d.Lines = append(d.Lines, line)
	if browserFixed {
		d.FixedCnt++
	}
	d.Steps = append(d.Steps, model.DraftStep{N: n, Kind: KindBrowser, Label: label, Line: line, Effect: eff, Note: note})
}

func (d *Drafter) Human(i, n int, label, fileKey string) {
	target := ""
	for _, p := range d.Plan(i, "edit") {
		if p.Slot.Key == fileKey && p.Fixed {
			target = p.Slot.Value
		}
	}
	note := "The change was different every run, so it is not replayed. Make it by hand, or give it to the agent as this step."
	line := fmt.Sprintf(`echo "HUMAN STEP %d: %s%s" >&2`, n, label, map[bool]string{true: " " + target, false: ""}[target != ""])
	if target != "" && !d.Files[target] {
		d.Files[target] = true
		d.Mf.Files = append(d.Mf.Files, manifest.File{Path: target, Access: "write"})
	}
	d.HumanCnt++
	d.Lines = append(d.Lines, fmt.Sprintf("# %d. %s", n, label), "# "+note, line)
	d.Steps = append(d.Steps, model.DraftStep{N: n, Kind: KindHuman, Label: label, Line: line, Note: note})
}

func (d *Drafter) Builtin(i, n int, label string) {
	switch {
	case trace.ReadTools[label]:
		for _, p := range d.Plan(i, "read") {
			if p.Slot.Type != trace.SlotPath {
				continue
			}
			arg := ShellQuote(p.Slot.Value)
			if !p.Fixed {
				arg = `"` + d.Ref(p.Input) + `"`
			}
			d.FileAccess(p, n)
			key := "cat\x00\x00*\x00read"
			if !d.Cmds[key] {
				d.Cmds[key] = true
				d.Mf.Commands = append(d.Mf.Commands, manifest.Command{Command: "cat", Args: []string{"*"}, Effect: d.Effect(n)})
			}
			line := "cat " + arg
			d.Lines = append(d.Lines, fmt.Sprintf("# %d. %s (read a file)", n, label), line)
			if p.Fixed {
				d.FixedCnt++
			}
			d.Steps = append(d.Steps, model.DraftStep{N: n, Kind: KindCommand, Label: label, Line: line, Effect: d.Effect(n)})
			return
		}
	case trace.EditTools[label]:
		key := "file_path"
		if label == "edit_file_v2" {
			key = "relativeWorkspacePath"
		}
		d.Human(i, n, label, key)
		return
	case trace.FetchTools[label]:
		for _, p := range d.Plan(i, "fetch") {
			if p.Slot.Key != "url" {
				continue
			}
			u, err := url.Parse(p.Slot.Value)
			if err != nil || u.Host == "" {
				break
			}
			origin := u.Scheme + "://" + u.Host
			if !d.Origins[origin] {
				d.Origins[origin] = true
				d.Mf.Fetch = append(d.Mf.Fetch, manifest.Fetch{Origin: origin})
			}
			arg := ShellQuote(p.Slot.Value)
			if !p.Fixed {
				arg = `"` + d.Ref(p.Input) + `"`
			}
			line := "tap fetch " + arg
			note := ""
			if !p.Fixed {
				note = "Only " + origin + " is declared; a URL on another site will be refused."
			}
			d.Lines = append(d.Lines, fmt.Sprintf("# %d. %s", n, label), line)
			d.Steps = append(d.Steps, model.DraftStep{N: n, Kind: KindFetch, Label: label, Line: line, Effect: "read", Note: note})
			return
		}
	}
	note := "The agent's own bookkeeping (planning, searching its tools, typing into a terminal it opened): nothing to replay."
	d.Lines = append(d.Lines, fmt.Sprintf("# %d. %s: not replayed. %s", n, label, note))
	d.Steps = append(d.Steps, model.DraftStep{N: n, Kind: KindSkipped, Label: label, Note: note})
}

func DraftReadme(name, desc string, d *Drafter) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n**Status: unvalidated.** This package has never been executed. Its steps were drafted from recorded sessions; passing publish checks is not validation. Run it on fresh inputs with an independent check of the result before relying on it.\n\n%s\n\nDrafted by `telara tap discover` from recorded sessions on this machine. Review it before you run or publish it.\n\n", name, desc)
	b.WriteString("## Inputs\n\n")
	if len(d.Inputs) == 0 {
		b.WriteString("None: every value was the same in every recorded run.\n")
	}
	for _, in := range d.Inputs {
		if in.Sensitive {
			fmt.Fprintf(&b, "- `$%d` **%s**: a credential, from %s. Supply it from your own configuration; its recorded value was not kept.\n", in.Position, in.Name, in.From)
		} else if in.Extract != "" {
			fmt.Fprintf(&b, "- **%s** (%s), from %s: not an argument. In every recorded run step %d's output held it after the same text, so the program takes it from there (%s `%s`, exactly one match or it stops).\n", in.Name, in.Type, in.From, in.DerivedFrom, in.Binding, in.Extract)
		} else if in.DerivedFrom > 0 {
			fmt.Fprintf(&b, "- `$%d` **%s** (%s), from %s: in the recorded runs step %d's output supplied it. Take it from there; the caller should not have to.\n", in.Position, in.Name, in.Type, in.From, in.DerivedFrom)
		} else {
			fmt.Fprintf(&b, "- `$%d` **%s** (%s), from %s.\n", in.Position, in.Name, in.Type, in.From)
		}
	}
	b.WriteString("\n## Steps\n\n")
	for _, s := range d.Steps {
		eff := ""
		if s.Effect != "" {
			eff = " (" + s.Effect + ")"
		}
		fmt.Fprintf(&b, "%d. **%s** %s%s\n", s.N, s.Kind, s.Label, eff)
		if s.Line != "" {
			fmt.Fprintf(&b, "   ```\n   %s\n   ```\n", s.Line)
		}
		if s.Note != "" {
			fmt.Fprintf(&b, "   %s\n", s.Note)
		}
	}
	if d.HumanCnt > 0 {
		fmt.Fprintf(&b, "\n%d step(s) are human steps: the content differed every run, so the draft stops there instead of guessing.\n", d.HumanCnt)
	}
	b.WriteString("\nEffects are `write` (the runner asks before each) unless a step was marked read-only in review.\n")
	return b.String()
}

var NameWord = regexp.MustCompile(`[a-z0-9]+`)

// DraftName joins the steps' words (git add commit push), keeping the first
// time each appears, into a manifest name.
func DraftName(c model.Candidate) string {
	seen := map[string]bool{}
	var words []string
	for _, s := range c.Steps {
		l := strings.ToLower(s.Label)
		for _, p := range []string{"sh:", "mcp:", "js:", "patch:", "telara_"} {
			l = strings.ReplaceAll(l, p, "")
		}
		for _, w := range NameWord.FindAllString(l, -1) {
			if !seen[w] {
				seen[w] = true
				words = append(words, w)
			}
		}
	}
	name := strings.Join(words, "-")
	if len(name) > 48 {
		name = strings.TrimRight(name[:48], "-")
	}
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		name = "routine-" + name
	}
	return name
}

func ClientList(m map[string]int) string {
	var out []string
	for k, v := range m {
		out = append(out, fmt.Sprintf("%s %d", k, v))
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// ArgSchema is the JSON schema of one recorded tool argument: its recorded
// JSON type, so the contract asks for what the tool was actually sent.
func ArgSchema(sl trace.Slot) map[string]any {
	if !sl.Raw {
		return map[string]any{"type": "string"}
	}
	var v any
	if json.Unmarshal([]byte(sl.Value), &v) == nil {
		switch v.(type) {
		case float64:
			return map[string]any{"type": "number"}
		case bool:
			return map[string]any{"type": "boolean"}
		case []any:
			return map[string]any{"type": "array"}
		case map[string]any:
			return map[string]any{"type": "object"}
		}
	}
	return map[string]any{}
}

// JsonLiteral writes a fixed argument as the JSON it was recorded as.
func JsonLiteral(sl trace.Slot) string {
	if sl.Raw && json.Valid([]byte(sl.Value)) {
		var c bytes.Buffer
		if json.Compact(&c, []byte(sl.Value)) == nil {
			return c.String()
		}
		return sl.Value
	}
	b, _ := json.Marshal(sl.Value)
	return string(b)
}

var JqIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func JqKey(k string) string {
	if JqIdent.MatchString(k) {
		return k
	}
	return strconv.Quote(k)
}

var CapChars = regexp.MustCompile(`[^a-z0-9_.-]+`)

// CapName writes a tool's name as the provider.resource.verb capability the
// runner binds (tap-runtime bind.Candidates): the first word is the
// provider, the first word that is a known verb (else the last) the verb,
// and the words between the resource (the provider again when none are
// left). gmail_search_emails -> gmail.emails.search.
func CapName(tool string) string {
	n := strings.Trim(CapChars.ReplaceAllString(strings.ToLower(tool), "_"), "_.-")
	words := strings.FieldsFunc(n, func(r rune) bool { return r == '_' || r == '.' || r == '-' })
	if len(words) == 0 {
		return "tool.tool.run"
	}
	if words[0][0] < 'a' || words[0][0] > 'z' {
		words[0] = "t" + words[0]
	}
	provider := words[0]
	rest := words[1:]
	verbAt := -1
	for k, w := range rest {
		if trace.ReadVerbs[w] || trace.WriteVerbs[w] {
			verbAt = k
			break
		}
	}
	if verbAt < 0 {
		verbAt = len(rest) - 1
	}
	if verbAt < 0 {
		return provider + "." + provider + ".run"
	}
	verb := rest[verbAt]
	var res []string
	res = append(res, rest[:verbAt]...)
	res = append(res, rest[verbAt+1:]...)
	resource := strings.Join(res, "_")
	if resource == "" {
		resource = provider
	}
	return provider + "." + resource + "." + verb
}

var SafeShell = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

func ShellQuote(s string) string {
	if SafeShell.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var NonName = regexp.MustCompile(`[^a-z0-9_]+`)

func SanitizeName(s string) string {
	n := strings.Trim(NonName.ReplaceAllString(strings.ToLower(s), "_"), "_")
	if n == "" || n[0] < 'a' || n[0] > 'z' {
		n = "in_" + n
	}
	return n
}

func IsDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func OrNone(keys []string) string {
	if len(keys) == 0 {
		return "no arguments"
	}
	return strings.Join(keys, ", ")
}

func AnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func FixedShare(d *Drafter) float64 {
	fixed := d.FixedArg
	for _, s := range d.Steps {
		if s.Kind != KindSkipped && s.Kind != KindHuman {
			fixed++
		}
	}
	if fixed+len(d.Inputs) == 0 {
		return 0
	}
	return float64(fixed) / float64(fixed+len(d.Inputs))
}

// SameCall reports that steps i and j came from the same recorded call in
// every occurrence.
func SameCall(occ [][]trace.Step, i, j int) bool {
	for _, o := range occ {
		if o[j].Raw == "" || o[j].Call != o[i].Call {
			return false
		}
	}
	return true
}

func SlotValue(st trace.Step, key string) string {
	for _, sl := range st.Slots {
		if sl.Key == key {
			return sl.Value
		}
	}
	return ""
}

// compound drafts steps i..j-1, which every run made as one command line,
// as that line. Each run's line is cut into words at their exact positions;
// runs whose lines have the same structure (the same text between the same
// number of words) are compared word by word, and a word that differs
// becomes an input: a cd target, a redirect target and an environment value
// included. A heredoc body that differs cannot be an input. If fewer than
// half the runs share one structure, or a body differs, the step is written
// as needing authoring rather than guessed.
func (d *Drafter) Compound(i, j int) {
	n := len(d.Steps) + 1
	var labels []string
	for k := i; k < j; k++ {
		labels = append(labels, d.Occ[0][k].Label)
	}
	label := strings.Join(labels, " + ")
	human := func(why string) {
		note := "Recorded as one command line, but " + why + ". Write this step by hand."
		line := fmt.Sprintf(`echo "HUMAN STEP %d: %s" >&2`, n, label)
		d.HumanCnt++
		d.Lines = append(d.Lines, fmt.Sprintf("# %d. %s", n, label), "# "+note, line)
		d.Steps = append(d.Steps, model.DraftStep{N: n, Kind: KindHuman, Label: label, Line: line, Note: note})
	}
	type cut struct {
		raw   string
		words []shellparse.Span
	}
	byShape := map[string][]int{}
	cuts := make([]cut, len(d.Occ))
	for o := range d.Occ {
		raw := d.Occ[o][i].Raw
		ws := shellparse.WordSpans(raw)
		cuts[o] = cut{raw, ws}
		byShape[shellparse.ShapeOf(raw, ws)] = append(byShape[shellparse.ShapeOf(raw, ws)], o)
	}
	best := ""
	for k, os := range byShape {
		if len(os) > len(byShape[best]) || (len(os) == len(byShape[best]) && k < best) {
			best = k
		}
	}
	reps := byShape[best]
	if 2*len(reps) < len(d.Occ) {
		human(fmt.Sprintf("only %d of %d runs share its structure", len(reps), len(d.Occ)))
		return
	}
	// A line is the routine's only when separate sessions wrote it: runs that
	// agree only within one session are one piece of work repeated there.
	sessions := map[string]bool{}
	for _, o := range reps {
		sessions[d.Occ[o][i].Session] = true
	}
	if len(sessions) < 2 {
		human(fmt.Sprintf("the %d runs that share its structure all come from one session", len(reps)))
		return
	}
	first := cuts[reps[0]]
	prog := strings.ReplaceAll(strings.TrimPrefix(labels[0], "sh:"), " ", "_")
	var out strings.Builder
	last := 0
	for p, w := range first.words {
		vec := map[int]string{}
		same := true
		sensitive := false
		for _, o := range reps {
			v := cuts[o].words[p].Text
			vec[o] = v
			if v != w.Text {
				same = false
			}
			if redact.SecretShape(v) != "" {
				sensitive = true
			}
		}
		if p > 0 && redact.SensitiveName.MatchString(strings.TrimSuffix(first.words[p-1].Text, "=")) {
			sensitive = true
		}
		if same && !sensitive {
			continue
		}
		if w.Body {
			human("its heredoc body differs between runs")
			return
		}
		name := prog + "_arg" + util.Itoa(p)
		if p > 0 && strings.HasPrefix(first.words[p-1].Text, "-") {
			name = prog + "_" + strings.TrimLeft(first.words[p-1].Text, "-")
		}
		in := d.Input(name, trace.Slot{Key: "w" + util.Itoa(p), Type: trace.TypeOf(shellparse.Word{Text: w.Text, Quoted: w.Quoted}), Value: w.Text}, vec, sensitive, i)
		out.WriteString(first.raw[last:w.S])
		out.WriteString(`"` + d.Ref(in) + `"`)
		last = w.E
	}
	out.WriteString(first.raw[last:])
	line := out.String()

	eff := d.Effect(n)
	for _, ws := range shellparse.SimpleCommands(line) {
		prog := ws[0].Text
		if !ManifestCommand.MatchString(prog) {
			continue
		}
		key := prog + "\x00\x00*\x00" + eff
		if !d.Cmds[key] {
			d.Cmds[key] = true
			d.Mf.Commands = append(d.Mf.Commands, manifest.Command{Command: prog, Args: []string{"*"}, Effect: eff})
		}
	}
	note := ""
	if len(reps) < len(d.Occ) {
		note = fmt.Sprintf("%d of %d runs used a command line of exactly this structure.", len(reps), len(d.Occ))
	}
	d.FixedCnt++
	d.Lines = append(d.Lines, fmt.Sprintf("# %d. %s (one recorded command line)", n, label), line)
	d.Steps = append(d.Steps, model.DraftStep{N: n, Kind: KindCommand, Label: label, Line: line, Effect: eff, Note: note})
}

var ManifestCommand = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._+-]{0,63}$`)

// derivedFrom returns the position of the earlier step whose output held
// this value in at least half the runs, or -1.
func (d *Drafter) DerivedFrom(pos int, vec map[int]string) int {
	count := map[int]int{}
	n := 0
	for j, v := range vec {
		if v == "" || j >= len(d.Occ) {
			continue
		}
		n++
	found:
		for h := 0; h < pos && h < len(d.Occ[j]); h++ {
			for _, id := range d.Occ[j][h].OutIDs {
				if id == v {
					count[h]++
					break found
				}
			}
			// A name that is not identifier-shaped (a pod, a branch) is
			// still taken from a result when the result shows it.
			if trace.InResult(v, d.Occ[j][h]) {
				count[h]++
				break found
			}
		}
	}
	best, bestN := -1, 0
	for h, c := range count {
		if c > bestN || (c == bestN && h < best) {
			best, bestN = h, c
		}
	}
	if n == 0 || 2*bestN < n {
		return -1
	}
	return best
}

// extraction finds how to take a value back out of step h's output: the text
// just before it, common to every run where step h's output held it, and the
// character that ended it. It returns a grep -o pattern matching that text
// and the value, and the text to strip from the match.
func (d *Drafter) Extraction(h int, vec map[int]string) (pattern, strip, binding string, ok bool) {
	// A JSON result is read by its path. When every run found the value at
	// one and the same path, bind that path. When runs found it at
	// different paths (the first result one time, the second another) the
	// choice was the agent's: bind nothing, and do not fall back to text.
	paths := map[string]bool{}
	located, total := 0, 0
	for j, v := range vec {
		if v == "" || j >= len(d.Occ) || h >= len(d.Occ[j]) {
			continue
		}
		st := d.Occ[j][h]
		for k, id := range st.OutIDs {
			if id != v {
				continue
			}
			total++
			if k < len(st.OutPaths) && st.OutPaths[k] != "" && st.OutPaths[k] != "*" {
				paths[st.OutPaths[k]] = true
				located++
			}
			break
		}
	}
	if located >= 2 && len(paths) > 1 {
		return "", "", "", false
	}
	if located >= 2 && located == total && len(paths) == 1 {
		for p := range paths {
			if !strings.ContainsAny(p, "'\\") {
				return p, "", "json_path", true
			}
		}
	}
	var befores []string
	after, first := "", true
	for j, v := range vec {
		if v == "" || j >= len(d.Occ) || h >= len(d.Occ[j]) {
			continue
		}
		st := d.Occ[j][h]
		for k, id := range st.OutIDs {
			if id != v || k >= len(st.OutCtx) {
				continue
			}
			b, a, _ := strings.Cut(st.OutCtx[k], "\x00")
			if !first && a != after {
				return "", "", "", false
			}
			after, first = a, false
			befores = append(befores, b)
			break
		}
	}
	if len(befores) < 2 {
		return "", "", "", false
	}
	common := befores[0]
	for _, b := range befores[1:] {
		n := 0
		for n < len(common) && n < len(b) && common[len(common)-1-n] == b[len(b)-1-n] {
			n++
		}
		common = common[len(common)-n:]
	}
	for len(common) > 0 && !utf8.RuneStart(common[0]) {
		common = common[1:]
	}
	// The anchor must say something ("Task ID: `", "\"id\":\"") and fit in
	// a single-quoted shell word; the value must end at a delimiter. Text
	// with a backslash was recorded escaped (a JSON-encoded result) and will
	// not read the same in the live output.
	if len(strings.TrimSpace(common)) < 3 || strings.ContainsAny(common+after, "'\n\\") {
		return "", "", "", false
	}
	if after != "" {
		if r, _ := utf8.DecodeRuneInString(after); unicode.IsLetter(r) || unicode.IsDigit(r) {
			return "", "", "", false
		}
	}
	class := "[^[:space:]]*"
	if after != "" {
		class = "[^" + after + "[:space:]]*"
		if after == "]" {
			class = "[^][:space:]]*"
		}
	}
	return BreQuote(common) + class, common, "text_anchor", true
}

// BreQuote escapes s for a basic regular expression.
func BreQuote(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\.*[]^$`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// capturable reports whether draft step n runs something whose output a later
// step can read: a command, a tool call, a browser script or a fetch.
func (d *Drafter) Capturable(n int) bool {
	if n < 1 || n > len(d.Steps) {
		return false
	}
	switch d.Steps[n-1].Kind {
	case KindCommand, KindTool, KindBrowser, KindFetch:
		return true
	}
	return false
}

// finish numbers the caller's arguments, resolves every input placeholder,
// and makes each step whose output supplies a later value keep that output
// and take the value from it.
func (d *Drafter) Finish() {
	pos := 0
	refs := make([]string, len(d.Inputs))
	byStep := map[int][]int{}
	for k := range d.Inputs {
		in := &d.Inputs[k]
		if in.Extract != "" {
			refs[k] = "${" + in.Name + "}"
			byStep[in.DerivedFrom] = append(byStep[in.DerivedFrom], k)
			continue
		}
		pos++
		in.Position = pos
		refs[k] = "${" + util.Itoa(pos) + "}"
	}
	resolve := func(l string) string {
		if !strings.Contains(l, "\x01") {
			return l
		}
		parts := strings.Split(l, "\x01")
		for i := 1; i < len(parts); i += 2 {
			if k, err := strconv.Atoi(parts[i]); err == nil && k < len(refs) {
				parts[i] = refs[k]
			}
		}
		return strings.Join(parts, "")
	}
	for i := range d.Steps {
		d.Steps[i].Line = resolve(d.Steps[i].Line)
	}
	var out []string
	step := 0
	for _, l := range d.Lines {
		l = resolve(l)
		if strings.HasPrefix(l, "# ") {
			if n, err := strconv.Atoi(strings.SplitN(l[2:], ".", 2)[0]); err == nil {
				step = n
			}
		}
		ks := byStep[step]
		if len(ks) == 0 || step < 1 || l != d.Steps[step-1].Line {
			out = append(out, l)
			continue
		}
		v := "out" + util.Itoa(step)
		out = append(out, v+"=$("+l+")", `printf '%s\n' "$`+v+`"`)
		for _, k := range ks {
			in := d.Inputs[k]
			fail := func(why string) string {
				return fmt.Sprintf(`{ echo "step %d's output %s %s" >&2; exit 1; }`, step, why, in.Name)
			}
			if in.Binding == "json_path" {
				// The guest's jq takes -r and a filter only: an absent value
				// prints "null" (or nothing), which stops the program.
				out = append(out,
					fmt.Sprintf(`%s=$(printf '%%s\n' "$%s" | jq -r '%s')`, in.Name, v, in.Extract),
					fmt.Sprintf(`[ -n "$%s" ] && [ "$%s" != null ] || %s`, in.Name, in.Name, fail("has no")))
			} else {
				// The anchor must occur exactly once: a missing, repeated or
				// quoted anchor never selects a value by accident.
				d.UsesGrep = true
				m := "m_" + in.Name
				out = append(out,
					fmt.Sprintf(`%s=$(printf '%%s\n' "$%s" | grep -o -e '%s' || true)`, m, v, in.Extract),
					fmt.Sprintf(`[ -n "$%s" ] && [ "$(( $(printf '%%s\n' "$%s" | wc -l) ))" = 1 ] || %s`, m, m, fail("did not hold exactly one")),
					fmt.Sprintf(`%s=${%s#%s}`, in.Name, m, ShellQuote(in.Strip)))
			}
			// Whatever was read must look like an identifier before any
			// later step uses it.
			d.UsesGrep = true
			out = append(out, fmt.Sprintf(`printf '%%s' "$%s" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9._:/@+=-]*$' || %s`, in.Name, fail("held no identifier-shaped")))
		}
		delete(byStep, step)
	}
	d.Lines = out
	// grep is a host command in the guest: declare it (read-only) when the
	// program uses it to read a value out of a result.
	if d.UsesGrep && !d.Cmds["grep\x00\x00*\x00read"] {
		d.Cmds["grep\x00\x00*\x00read"] = true
		d.Mf.Commands = append(d.Mf.Commands, manifest.Command{Command: "grep", Args: []string{"*"}, Effect: "read"})
	}
}

// humanPositions are the template positions whose draft step is a human
// step.
func (d *Drafter) HumanPositions() map[int]bool {
	out := map[int]bool{}
	for pos, n := range d.PosStep {
		if n >= 1 && n <= len(d.Steps) && d.Steps[n-1].Kind == KindHuman {
			out[pos] = true
		}
	}
	return out
}

// JsonHelpers are defined once at the top of a draft that needs them.
const JsonHelpers = `json_str() { s=$1; s=${s//\\/\\\\}; s=${s//\"/\\\"}; s=${s//$'\n'/\\n}; s=${s//$'\t'/\\t}; s=${s//$'\r'/\\r}; printf '"%s"' "$s"; }
json_raw() { printf '%s' "$1" | jq .; }`

// jsonAssign encodes input k into shell variable v: a JSON string, or for a
// value recorded as JSON, the value checked by jq (a malformed one stops
// the program).
func (d *Drafter) JsonAssign(v string, k int) string {
	d.UsesJSON = true
	if d.Inputs[k].Raw {
		return fmt.Sprintf(`%s=$(json_raw "%s")`, v, d.Ref(k))
	}
	return fmt.Sprintf(`%s=$(json_str "%s")`, v, d.Ref(k))
}

// JsonObject collects the members of a JSON object built in the shell.
type JsonObject struct {
	Parts []string `json:"-"`
}

func (o *JsonObject) Fixed(key, literal string) {
	o.Parts = append(o.Parts, "'"+strings.ReplaceAll(JsonKeyText(key)+":"+literal, "'", `'\''`)+"'")
}

func (o *JsonObject) Variable(key, v string) {
	o.Parts = append(o.Parts, "'"+strings.ReplaceAll(JsonKeyText(key)+":", "'", `'\''`)+"'"+`"$`+v+`"`)
}

// word is the object as one shell word: quoted fragments joined with ','.
func (o *JsonObject) Word() string {
	if len(o.Parts) == 0 {
		return "'{}'"
	}
	return "'{'" + strings.Join(o.Parts, "','") + "'}'"
}

func JsonKeyText(k string) string {
	b, _ := json.Marshal(k)
	return string(b)
}

// ShellSingle quotes s as one single-quoted shell word.
func ShellSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

type MineLimits struct {
	Window     int `json:"-"` // max steps between consecutive pattern items
	MinSupport int `json:"-"` // a compute bound, not a quality threshold
	MaxLen     int `json:"-"`
	MaxOut     int `json:"-"` // stop growing once this many patterns exist
	// canQualify, when set, reports whether a pattern with these items and
	// this support could still pass qualification. It must be anti-monotone:
	// false for a pattern means false for every extension of it, so the
	// branch is skipped without losing anything that could qualify.
	CanQualify func(items []int, support int) bool `json:"-"`
	// keep, when set, decides whether a found pattern is stored. It gets the
	// pattern's support and its prefix's. A pattern not kept is still grown.
	Keep func(items []int, support, prefixSupport int) bool `json:"-"`
}

// MinePatterns is PrefixSpan with a gap constraint. A projection maps each
// supporting session to the positions where the current prefix can end.
// It returns the patterns of length >= 2 with at least two distinct labels
// and support >= minSupport that keep accepts, how many such patterns it
// examined, and whether it stopped because maxOut patterns were kept.
//
// Patterns starting with different labels are independent, so each starting
// label is searched on its own worker; keep and canQualify must be safe to
// call concurrently. The result is sorted, so it does not depend on
// scheduling.
func MinePatterns(seqs [][]int, lim MineLimits) ([]model.Pattern, int, bool) {
	var (
		mu        sync.Mutex
		out       []model.Pattern
		examined  atomic.Int64
		truncated atomic.Bool
	)
	first := map[int]map[int][]int{}
	for s, seq := range seqs {
		for i, x := range seq {
			if first[x] == nil {
				first[x] = map[int][]int{}
			}
			first[x][s] = append(first[x][s], i)
		}
	}
	var grow func(prefix []int, proj map[int][]int)
	grow = func(prefix []int, proj map[int][]int) {
		if len(prefix) >= lim.MaxLen || truncated.Load() {
			return
		}
		ext := map[int]map[int][]int{}
		for s, ends := range proj {
			seq := seqs[s]
			seen := map[int]int{} // label -> last position recorded, to dedupe
			for _, e := range ends {
				for j := e + 1; j < len(seq) && j <= e+lim.Window; j++ {
					x := seq[j]
					if last, ok := seen[x]; ok && last >= j {
						continue
					}
					if ext[x] == nil {
						ext[x] = map[int][]int{}
					}
					ext[x][s] = append(ext[x][s], j)
					seen[x] = j
				}
			}
		}
		labels := make([]int, 0, len(ext))
		for x, m := range ext {
			if len(m) >= lim.MinSupport {
				labels = append(labels, x)
			}
		}
		sort.Ints(labels)
		for _, x := range labels {
			np := append(append([]int{}, prefix...), x)
			if lim.CanQualify != nil && !lim.CanQualify(np, len(ext[x])) {
				continue
			}
			if Distinct(np) >= 2 {
				examined.Add(1)
				if lim.Keep == nil || lim.Keep(np, len(ext[x]), len(proj)) {
					ss := make([]int, 0, len(ext[x]))
					for s := range ext[x] {
						ss = append(ss, s)
					}
					sort.Ints(ss)
					mu.Lock()
					out = append(out, model.Pattern{Items: np, Sessions: ss})
					if len(out) >= lim.MaxOut {
						truncated.Store(true)
					}
					mu.Unlock()
				}
			}
			grow(np, ext[x])
		}
	}
	roots := make([]int, 0, len(first))
	for x, m := range first {
		if len(m) >= lim.MinSupport {
			roots = append(roots, x)
		}
	}
	sort.Ints(roots)
	util.ParallelFor(len(roots), func(i int) {
		grow([]int{roots[i]}, first[roots[i]])
	})
	sort.Slice(out, func(a, b int) bool { return LessItems(out[a].Items, out[b].Items) })
	return out, int(examined.Load()), truncated.Load()
}

func LessItems(a, b []int) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

func Distinct(xs []int) int {
	m := map[int]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return len(m)
}

// ClosedOnly drops a pattern when a pattern one step longer that contains it
// has the same support: the longer one says everything the shorter one does.
func ClosedOnly(ps []model.Pattern) []model.Pattern {
	sup := make(map[string]int, len(ps))
	for _, p := range ps {
		sup[p.Key()] = len(p.Sessions)
	}
	notClosed := map[string]bool{}
	for _, q := range ps {
		for i := range q.Items {
			sub := model.Pattern{Items: append(append([]int{}, q.Items[:i]...), q.Items[i+1:]...)}
			if k := sub.Key(); sup[k] == len(q.Sessions) {
				notClosed[k] = true
			}
		}
	}
	out := ps[:0:0]
	for _, p := range ps {
		if !notClosed[p.Key()] {
			out = append(out, p)
		}
	}
	return out
}

// WriteText prints a report for a person: what was read, the qualified
// candidates with their templates, and how well known skills were recovered.
func WriteText(w io.Writer, r *model.Report, top int) {
	fmt.Fprintf(w, "Rules %s, window %d, min support %d, max length %d, %d permutations, FDR %.2f, seed %d\n\n",
		r.RulesVersion, r.Options.Window, r.Options.MinSupport, r.Options.MaxLen, r.Options.Permutations, r.Options.Alpha, r.Options.Seed)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CLIENT\tSESSIONS\tDUPLICATES\tCALLS\tSTEPS\tFROM\tTO\tNOTE")
	for _, c := range r.Clients {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%s\t%s\t%s\n", c.Client, c.Sessions, c.DuplicateSessions, c.Calls, c.Steps, Day(c.Earliest), Day(c.Latest), c.Error)
	}
	tw.Flush()
	trunc := ""
	if r.Truncated {
		trunc = " (stopped at the kept-pattern limit; raise --max-patterns)"
	}
	fmt.Fprintf(w, "\n%d distinct step labels. %d patterns examined at support >= %d, %d kept%s; %d passed the per-step test and were shuffle-tested; %d qualified, in %d families.\n\n",
		r.Labels, r.Examined, r.MinSupportUsed, r.Mined, trunc, r.Tested, r.Qualified, r.Families)

	variants := map[int]int{}
	for i, c := range r.Candidates {
		if c.Qualified && c.Family != i {
			variants[c.Family]++
		}
	}
	shown := 0
	for i, c := range r.Candidates {
		if !c.Qualified || c.Family != i || shown >= top {
			continue
		}
		shown++
		clients := make([]string, 0, len(c.ByClient))
		for k, v := range c.ByClient {
			clients = append(clients, fmt.Sprintf("%s %d", k, v))
		}
		ordered := "order not significant"
		if c.Ordered {
			ordered = "ordered"
		}
		fmt.Fprintf(w, "#%d  %d sessions (%s), null mean %.1f, q %.1e, %s, stability %.2f, specificity %.2f, %d weeks, median gap %.1f days, %s → %s\n",
			shown, c.Sessions, strings.Join(clients, ", "), c.NullMean, c.Q, ordered, c.Stability, c.Specificity, c.Weeks, c.MedianGapDays, Day(c.FirstSeen), Day(c.LastSeen))
		for i, s := range c.Steps {
			fmt.Fprintf(w, "    %d. %s   [stability %.2f, weight %.2f]\n", i+1, s.Template, s.Stability, s.Weight)
		}
		if c.Measured > 0 {
			fmt.Fprintf(w, "    tokens per run %s; a primitive saves %s per run, %s over %d measured runs\n",
				FmtUsage(c.PerRun), FmtUsage(c.SavedPerRun), FmtUsage(c.SavedTotal), c.Measured)
		}
		if n := variants[i]; n > 0 {
			fmt.Fprintf(w, "    +%d variant patterns over mostly the same sessions\n", n)
		}
		fmt.Fprintf(w, "    e.g. %s\n\n", strings.Join(c.Examples, ", "))
	}
	if shown == 0 {
		fmt.Fprintln(w, "No candidate recurs beyond chance.")
	}

	if len(r.Skills) > 0 {
		fmt.Fprintln(w, "\nProcedures specific to a skill (sessions that loaded it vs. those that did not):")
		for i, sk := range r.Skills {
			if i >= top {
				break
			}
			fmt.Fprintf(w, "\n  %s — loaded in %d sessions, %d enriched patterns\n", sk.Skill, sk.Sessions, sk.Significant)
			for j, p := range sk.Procedures {
				if j >= 3 {
					break
				}
				where := fmt.Sprintf("%d elsewhere, lift %.1f", p.Outside, p.Lift)
				if p.OnlyInSkill {
					where = "never elsewhere"
				}
				fmt.Fprintf(w, "    in %d of its sessions (%.0f%%), %s, q %.1e:\n", p.InSkill, 100*p.Coverage, where, p.EnrichmentQ)
				for k, st := range p.Steps {
					fmt.Fprintf(w, "      %d. %s\n", k+1, st.Template)
				}
			}
		}
	}

	if len(r.Recall) > 0 {
		fmt.Fprintln(w, "\nKnown skills (loaded in 2+ sessions) and the best-matching candidate:")
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "SKILL\tSESSIONS\tBEST QUALIFIED F1\tBEST TESTED F1\tBEST TESTED PATTERN")
		for _, s := range r.Recall {
			q, t, pat := "-", "-", ""
			if s.BestQualified != nil {
				q = fmt.Sprintf("%.2f", s.BestQualified.F1)
			}
			if s.BestTested != nil {
				t = fmt.Sprintf("%.2f (P %.2f R %.2f)", s.BestTested.F1, s.BestTested.Precision, s.BestTested.Recall)
				pat = s.BestTested.Pattern
			}
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", s.Skill, s.Sessions, q, t, pat)
		}
		tw.Flush()
	}
}

func Day(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("2006-01-02")
}

// FmtUsage prints fresh input, cached input and output separately: cached
// input costs a small fraction of fresh.
func FmtUsage(u trace.Usage) string {
	return fmt.Sprintf("%s (%s fresh, %s cached, %s out)", HumanTokens(u.Total()), HumanTokens(u.Fresh), HumanTokens(u.Cached), HumanTokens(u.Output))
}

func HumanTokens(t float64) string {
	switch {
	case t >= 1e6:
		return fmt.Sprintf("%.1fM", t/1e6)
	case t >= 1e3:
		return fmt.Sprintf("%.1fk", t/1e3)
	}
	return fmt.Sprintf("%.0f", t)
}

// WriteFunnel prints the request-level result: what was read, how it narrowed
// to primitives, and the primitives. With rejected, it also lists what each
// check removed and why.
func WriteFunnel(w io.Writer, r *model.Report, top int, rejected bool) {
	f := r.Funnel
	var clients []string
	for _, c := range r.Clients {
		clients = append(clients, fmt.Sprintf("%s %d", c.Client, c.Sessions))
	}
	fmt.Fprintf(w, "Reviewed %d sessions (%s) and %d tool calls.\n", f.Sessions, strings.Join(clients, ", "), f.Calls)
	fmt.Fprintf(w, "%d requests; %d ran at least two steps a program could replay.\n", f.Requests, f.RequestsWithSteps)
	parts := 0
	for _, rt := range r.Routines {
		if rt.Parent != "" {
			parts++
		}
	}
	fmt.Fprintf(w, "Grouped into %d kinds of request; %d routines recurred (%d+ requests in 2+ sessions), %d of them bounded parts found inside larger work.\n",
		f.Groups, f.Routines, r.Options.MinSupport, parts)
	if f.Merged > 0 {
		fmt.Fprintf(w, "%d routines duplicated another (same role, steps in the same order, scope and task family) and are counted once.\n", f.Merged)
	}
	count := func(m map[string]int, keys ...string) string {
		var out []string
		for _, k := range keys {
			if n := m[k]; n > 0 {
				out = append(out, fmt.Sprintf("%d %s", n, strings.ReplaceAll(k, "_", " ")))
			}
		}
		if len(out) == 0 {
			return "none"
		}
		return strings.Join(out, ", ")
	}
	roles, suits := map[string]int{}, map[string]int{}
	for role, m := range f.ByRole {
		for suit, n := range m {
			roles[role] += n
			suits[suit] += n
		}
	}
	fmt.Fprintf(w, "Who the work is for: %s.\n", count(roles, model.RoleUser, model.RoleScheduled, model.RoleInfrastructure, model.RoleHarness, model.RoleUnknown))
	fmt.Fprintf(w, "Is it a useful procedure: %s.\n", count(suits, model.SuitUseful, model.SuitInsufficient, model.SuitInvestigation, model.SuitInvalid))
	useful := f.ByRole[model.RoleUser][model.SuitUseful]
	fmt.Fprintf(w, "Useful procedures for user tasks: %d; drafts: %s. Scheduled work that is already automated: %d (a baseline, not new automation).\n",
		useful, count(f.ByDraft[model.SuitUseful], model.DraftComplete, model.DraftNeedsAuthor, model.DraftBlocked), f.ByRole[model.RoleScheduled][model.SuitUseful])
	fmt.Fprintf(w, "Outcome evidence: %s. Validation: %s. Value: %s.\n",
		count(f.ByOutcome, model.OutcomeToolOK, model.OutcomeEvUnknown, model.OutcomeEvFailed), count(f.ByValidation, model.ValidationNotRun), count(f.ByValue, model.ValueEstimated, model.ValueUnmeasured))
	fmt.Fprintln(w, "No draft has been executed. \"Structurally complete\" means the package is written and passes publish checks, not that it works: validate it on fresh inputs first.")
	fmt.Fprintln(w, "Savings are estimates from recorded token use, mostly cached input; no primitive run was measured.")
	fmt.Fprintln(w)

	fmt.Fprintln(w, "Recommended (useful procedures for user tasks; unvalidated):")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tDRAFT\tREQUESTS\tSESSIONS\tWEEKS\tEFFECT\tINPUTS\tSTEPS\tEXAMPLE REQUEST")
	shown := 0
	for _, rt := range r.Routines {
		if rt.Suitability != model.SuitUseful || rt.SourceRole != model.RoleUser || rt.MergedInto != "" || (top > 0 && shown >= top) {
			continue
		}
		shown++
		var ins []string
		for _, in := range rt.Contract.Inputs {
			ins = append(ins, in.Source)
		}
		draft := rt.DraftStatus
		if len(rt.Blockers) > 0 {
			draft += " (" + strings.Join(rt.Blockers, ", ") + ")"
		}
		fmt.Fprintf(tw, "%d\t%s\t%d\t%d\t%d\t%s\t%s\t%s\t%s\n", shown, trace.OneLine(draft, 40), rt.Requests, rt.Sessions, rt.Weeks, rt.Contract.Effect,
			trace.OneLine(strings.Join(ins, ","), 30), trace.OneLine(model.LabelsOf(rt.Candidate), 60), trace.OneLine(rt.Example, 50))
	}
	tw.Flush()
	if shown == 0 {
		fmt.Fprintln(w, "  none")
	}
	if n := f.ByRole[model.RoleScheduled][model.SuitUseful]; n > 0 {
		fmt.Fprintln(w, "\nScheduled work (already automated; listed as a baseline):")
		for _, rt := range r.Routines {
			if rt.Suitability == model.SuitUseful && rt.SourceRole == model.RoleScheduled && rt.MergedInto == "" {
				fmt.Fprintf(w, "  %d runs: %s\n", rt.Requests, trace.OneLine(model.LabelsOf(rt.Candidate), 100))
			}
		}
	}
	if !rejected {
		fmt.Fprintln(w, "\n(--rejected lists every other routine by the reason it is not recommended.)")
		return
	}
	byReason := map[string][]model.Routine{}
	var reasons []string
	for _, rt := range r.Routines {
		if rt.Suitability == model.SuitUseful || rt.MergedInto != "" {
			continue
		}
		k := rt.Suitability + ": " + strings.SplitN(FirstReason(rt), ":", 2)[0]
		if _, ok := byReason[k]; !ok {
			reasons = append(reasons, k)
		}
		byReason[k] = append(byReason[k], rt)
	}
	sort.Strings(reasons)
	for _, k := range reasons {
		fmt.Fprintf(w, "\n%s (%d):\n", k, len(byReason[k]))
		for _, rt := range byReason[k] {
			fmt.Fprintf(w, "  %d requests, %s: %s\n      %s\n", rt.Requests, rt.SourceRole, trace.OneLine(model.LabelsOf(rt.Candidate), 90), trace.OneLine(strings.Join(rt.Reasons, "; "), 140))
		}
	}
}

func FirstReason(rt model.Routine) string {
	if len(rt.Reasons) == 0 {
		return "unknown"
	}
	return rt.Reasons[0]
}

// Primitives returns the routines ready to save (decision "primitive"), in
// report order.
func ReportPrimitives(r *model.Report) []*model.Routine {
	var out []*model.Routine
	for i := range r.Routines {
		if r.Routines[i].Decision == "primitive" && r.Routines[i].MergedInto == "" {
			out = append(out, &r.Routines[i])
		}
	}
	return out
}

// WriteOpportunities says how many requests the selection pass surfaced,
// by route, and lists the n highest-ranked contract groups with the id of
// the example an authoring brief takes.
func WriteOpportunities(w io.Writer, r *model.Report, n int) {
	by := map[string]int{}
	for _, o := range r.Opportunities {
		by[o.Route]++
	}
	fmt.Fprintf(w, "\nSurfaced %d opportunities from the whole history by mechanical evidence (%d stated template, %d re-run check, %d parametric loop, %d single pass), in %d contract groups.\n",
		len(r.Opportunities), by[model.RouteStatedTemplate], by[model.RouteRerunCheck], by[model.RouteParamLoop], by[model.RouteNamedObject], len(r.OpportunityGroups))
	fmt.Fprintln(w, "Each is an unassessed proposal, not a verified procedure. Brief a group's example with `tap discover brief --opportunity <id> --report <file>`.")
	for i, g := range r.OpportunityGroups {
		if i == n {
			break
		}
		fmt.Fprintf(w, "  %3d. %-16s %3d sessions %4d requests  last %s  %s\n       %s\n",
			i+1, g.Route, g.Sessions, g.Requests, g.Last.Format("2006-01-02"), g.Example.ID, trace.OneLine(g.Contract, 110))
	}
}

// WriteSpanProposals lists structural retrieval candidates separately from
// recommendations. A group is a review aid, not a certified procedure.
func WriteSpanProposals(w io.Writer, r *model.Report, n int) {
	if len(r.LogicFunnels) > 0 {
		fmt.Fprintf(w, "\nFound %d result-flow funnels. A root's branches are observed follow-up operations; repeated branches can become loops over runtime inputs. Funnels are structural, not validated primitives.\n", len(r.LogicFunnels))
		for i, f := range r.LogicFunnels {
			if i == n {
				break
			}
			var branches []string
			for _, b := range f.Branches {
				name := b.Action
				if b.ForEach {
					name += "[*]"
				}
				branches = append(branches, name)
			}
			fmt.Fprintf(w, "  %3d. %3d sessions  %s  %s -> {%s}\n       raw candidate IDs %s\n", i+1, f.Sessions, f.ID, f.Root, strings.Join(branches, ", "), trace.OneLine(strings.Join(f.CandidateIDs, ", "), 100))
		}
	}
	if len(r.LogicCandidates) > 0 {
		fmt.Fprintf(w, "\nFound %d raw execution-logic candidates from %d diagnostic spans. Concrete inputs and outputs are parameters, not admission gates. Candidates still need authoring and validation.\n", len(r.LogicCandidates), len(r.SpanProposals))
		fmt.Fprintln(w, "Brief a candidate with `tap discover brief --logic <id> --report <file> --out <private-dir>`; the brief compares up to three independent executions.")
		for i, c := range r.LogicCandidates {
			if i == n {
				break
			}
			fmt.Fprintf(w, "  %3d. %3d sessions %4d executions  %s  %s\n       possible parameters %s; evidence %s\n",
				i+1, c.Sessions, c.Executions, c.ID, trace.OneLine(strings.Join(c.Actions, " > "), 85),
				trace.OneLine(strings.Join(c.Parameters, ", "), 85), strings.Join(c.Evidence, ", "))
			if len(c.Cautions) > 0 {
				fmt.Fprintf(w, "       check %s\n", strings.Join(c.Cautions, ", "))
			}
		}
		return
	}
	if len(r.SpanProposals) > 0 && r.SpanProposals[0].Review.Source != "" {
		fmt.Fprintf(w, "\nTask-first queue: %d unassessed spans in %d groups; component queue: %d spans in %d groups (missing task contract); diagnostic inventory: %d spans in %d groups. Queue membership is not a useful-procedure verdict.\n", len(r.ReviewSpans), len(r.ReviewGroups), len(r.ComponentSpans), len(r.ComponentGroups), len(r.SpanProposals), len(r.CompositionGroups))
		fmt.Fprintln(w, "Brief an example with `tap discover brief --span <id> --report <file> --out <private-dir>`.")
		queue := r.ReviewGroups
		if len(queue) == 0 {
			queue = r.ComponentGroups
		}
		for i, g := range queue {
			if i == n {
				break
			}
			p := g.Example
			fmt.Fprintf(w, "  %3d. %3d sessions %4d proposals  %-13s %-7s calls %v  %s  %s\n",
				i+1, g.Sessions, g.Proposals, p.Kind, p.Effect, p.Calls, p.ID, trace.OneLine(strings.Join(p.Composition.Actions, " > "), 85))
		}
		return
	}
	if len(r.CompositionGroups) == 0 && len(r.SpanGroups) > 0 {
		fmt.Fprintf(w, "\nFound %d unassessed bounded-span proposals in %d exact-shape groups (older report). These have not passed the useful-procedure gate.\n", len(r.SpanProposals), len(r.SpanGroups))
		fmt.Fprintln(w, "Brief an example with `tap discover brief --span <id> --report <file> --out <private-dir>`.")
		for i, g := range r.SpanGroups {
			if i == n {
				break
			}
			p := g.Example
			fmt.Fprintf(w, "  %3d. %3d sessions %4d proposals  %-13s %-7s calls %v  %s  %s\n",
				i+1, g.Sessions, g.Proposals, p.Kind, p.Effect, p.Calls, p.ID, trace.OneLine(strings.Join(p.Tools, " > "), 85))
		}
		return
	}
	fmt.Fprintf(w, "\nFound %d unassessed bounded-span proposals in %d composition groups (%d exact-shape groups). These have not passed the useful-procedure gate.\n", len(r.SpanProposals), len(r.CompositionGroups), len(r.SpanGroups))
	fmt.Fprintln(w, "Brief an example with `tap discover brief --span <id> --report <file> --out <private-dir>`.")
	for i, g := range r.CompositionGroups {
		if i == n {
			break
		}
		p := g.Example
		fmt.Fprintf(w, "  %3d. %3d sessions %4d proposals  %-13s %-7s calls %v  %s  %s\n",
			i+1, g.Sessions, g.Proposals, p.Kind, p.Effect, p.Calls, p.ID, trace.OneLine(strings.Join(p.Composition.Actions, " > "), 85))
	}
}

// CheckOrder is the order the checks run in (whole request is by construction).
// "Inputs given" is not among them: a primitive is called by an agent, which
// supplies its inputs, so a value the agent chose (the files to commit, the
// package to test) is a normal input. Whether each input's values appeared
// in the request is still reported per input.
// A skill the requests already loaded is not a reason to remove a routine:
// the skill is the baseline a primitive would be measured against. It is
// reported as CoveredBy.
var CheckOrder = []string{model.CheckReplays, model.CheckSameWay, model.CheckWorth}

// DraftAs redrafts the routine under a publisher, with the steps the user
// marked read-only.
func RoutineDraftAs(r *model.Routine, publisher string, readOnly map[int]bool) *model.Draft {
	if r.Occ == nil {
		return r.Draft
	}
	return BuildDraft(r.Candidate, r.Occ, model.DraftOptions{Publisher: publisher, ReadOnly: readOnly, Loops: r.LoopSpecs})
}

// Draft returns the package drafted for the routine.
func RoutineDraft(r *model.Routine) *model.Draft { return r.Draft }

type ReqInstance struct {
	Session int             `json:"-"`
	Request int             `json:"-"`
	Steps   []int           `json:"-"` // indexes into the session's steps
	Labels  map[int]float64 `json:"-"`
	// keys are labels' keys in order, so sums over them come out the same
	// every run (a float sum in map order can land either side of 0.5).
	Keys []int  `json:"-"`
	Text string `json:"-"`
}

// RequestRoutines runs the request-level pass over a normalized corpus.
func RequestRoutines(corpus []trace.NormSession, ids map[string]int, names []string, o model.Options, rawCalls int) (model.Funnel, []model.Routine) {
	f := model.Funnel{Sessions: len(corpus), Calls: rawCalls, Removed: map[string]int{},
		Savings: "estimated from recorded token use (mostly cached input); no primitive run was measured"}
	// Requests and their replayable steps.
	var inst []ReqInstance
	df := make([]int, len(names))
	for si, s := range corpus {
		byReq := map[int][]int{}
		for i, st := range s.Steps {
			byReq[st.Request] = append(byReq[st.Request], i)
		}
		f.Requests += len(s.Requests)
		reqs := make([]int, 0, len(byReq))
		for r := range byReq {
			reqs = append(reqs, r)
		}
		sort.Ints(reqs)
		for _, r := range reqs {
			steps := byReq[r]
			labels := map[int]float64{}
			nrep := 0
			for _, i := range steps {
				if l := s.Steps[i].Label; trace.Replayable(l) {
					labels[ids[l]] = 1
					nrep++
				}
			}
			// Two replayable steps make a procedure, even of one tool: two
			// diffs, a checksum per file.
			if nrep < 2 {
				continue
			}
			text := ""
			if r < len(s.Requests) {
				text = s.Requests[r]
			}
			inst = append(inst, ReqInstance{Session: si, Request: r, Steps: steps, Labels: labels, Text: text})
			for x := range labels {
				df[x]++
			}
		}
	}
	f.RequestsWithSteps = len(inst)
	if len(inst) == 0 {
		return f, nil
	}
	// A step that nearly every request runs says little about which task a
	// request is: steps are weighted by inverse document frequency.
	for i := range inst {
		for x := range inst[i].Labels {
			// Smoothed, so a step every request runs still weighs something.
			inst[i].Labels[x] = math.Log(1 + float64(len(inst))/float64(df[x]))
			inst[i].Keys = append(inst[i].Keys, x)
		}
		sort.Ints(inst[i].Keys)
	}

	groups := GroupRequests(inst)
	f.Groups = len(groups)
	var routines []model.Routine
	var split [][]int
	for _, g := range groups {
		split = append(split, SplitGroup(corpus, inst, g)...)
	}
	// How many requests carry each request text (digits aside): a routine
	// whose goal is a stated text must be how most of them were done.
	instByText := map[string][]int{}
	for i := range inst {
		if k := trace.TextKey(inst[i].Text); k != "" {
			instByText[k] = append(instByText[k], i)
		}
	}
	coreSeen := map[string]bool{}
	byText := map[string]int{}
	for _, s := range corpus {
		asked := map[int]bool{}
		for _, st := range s.Steps {
			asked[st.Request] = true
		}
		for r := range asked {
			if r < len(s.Requests) {
				if k := trace.TextKey(s.Requests[r]); k != "" {
					byText[k]++
				}
			}
		}
	}
	for _, g := range split {
		sessions := map[int]bool{}
		for _, i := range g {
			sessions[inst[i].Session] = true
		}
		// Recurrence: at least minSupport requests, in more than one session.
		if len(g) < o.MinSupport || len(sessions) < 2 {
			continue
		}
		rt, ok := BuildRoutine(corpus, inst, g, names, o)
		if !ok {
			continue
		}
		GoalShare(&rt, corpus, inst, g, byText, instByText)
		f.Routines++
		routines = append(routines, rt)
		if core, ok := GoalCore(corpus, inst, names, o, &rt, byText, instByText, coreSeen); ok {
			f.Routines++
			routines = append(routines, core)
		}
		if sub, ok := BoundedPart(corpus, inst, g, names, o, &rt); ok {
			f.Routines++
			routines = append(routines, sub)
		}
	}
	rank := map[string]int{"primitive": 0, "needs_authoring": 1, "baseline": 2, "removed": 3}
	// Primitives first. Among them, procedures before investigations: the
	// tokens a routine saves weighted by its coverage (how much of its
	// requests it is). Routines whose clients recorded no token use follow,
	// by coverage times requests. Nothing is removed; this only orders.
	share := func(r model.Routine) float64 { return r.Coverage }
	sort.SliceStable(routines, func(a, b int) bool {
		ra, rb := routines[a], routines[b]
		if rank[ra.Decision] != rank[rb.Decision] {
			return rank[ra.Decision] < rank[rb.Decision]
		}
		if (ra.Measured > 0) != (rb.Measured > 0) {
			return ra.Measured > 0
		}
		if ra.Measured > 0 {
			return share(ra)*ra.SavedTotal.Total() > share(rb)*rb.SavedTotal.Total()
		}
		return share(ra)*float64(ra.Requests) > share(rb)*float64(rb.Requests)
	})
	// One job found as two groups is counted once, under the first.
	f.Merged = MergeDuplicates(routines)
	f.ByKind = map[string]int{}
	f.ByRole, f.ByDraft = map[string]map[string]int{}, map[string]map[string]int{}
	f.ByOutcome, f.ByValidation, f.ByValue = map[string]int{}, map[string]int{}, map[string]int{}
	for i := range routines {
		if routines[i].MergedInto != "" {
			continue
		}
		r := &routines[i]
		if f.ByRole[r.SourceRole] == nil {
			f.ByRole[r.SourceRole] = map[string]int{}
		}
		f.ByRole[r.SourceRole][r.Suitability]++
		if f.ByDraft[r.Suitability] == nil {
			f.ByDraft[r.Suitability] = map[string]int{}
		}
		f.ByDraft[r.Suitability][r.DraftStatus]++
		f.ByOutcome[r.OutcomeEvidence]++
		f.ByValidation[r.Validation]++
		f.ByValue[r.Value]++
		switch routines[i].Decision {
		case "removed":
			f.Removed[routines[i].Failed]++
		case "needs_authoring":
			f.NeedsAuthoring++
		case "primitive":
			f.Primitives++
			f.ByKind[routines[i].Kind]++
		}
	}
	return f, routines
}

// GroupRequests puts each request in the group whose first request it is
// most like, when they share at least half their weighted steps (weighted
// Jaccard); otherwise it starts a group. Requests are taken in corpus order
// and a tie goes to the earlier group, so the result is the same every run.
func GroupRequests(inst []ReqInstance) [][]int {
	var leaders []int
	var groups [][]int
	byLabel := map[int][]int{} // label -> groups whose leader has it
	for i := range inst {
		in := &inst[i]
		var cands []int
		for _, x := range in.Keys {
			cands = append(cands, byLabel[x]...)
		}
		sort.Ints(cands)
		best, bestSim := -1, 0.0
		for k, g := range cands {
			if k > 0 && cands[k-1] == g {
				continue
			}
			if sim := WeightedJaccard(in, &inst[leaders[g]]); sim >= 0.5 && (best < 0 || sim > bestSim) {
				best, bestSim = g, sim
			}
		}
		if best >= 0 {
			groups[best] = append(groups[best], i)
			continue
		}
		g := len(groups)
		leaders = append(leaders, i)
		groups = append(groups, []int{i})
		for _, x := range in.Keys {
			byLabel[x] = append(byLabel[x], g)
		}
	}
	return groups
}

// WeightedJaccard sums in key order, so equal inputs give equal bits.
func WeightedJaccard(a, b *ReqInstance) float64 {
	var inter, union float64
	i, j := 0, 0
	for i < len(a.Keys) || j < len(b.Keys) {
		switch {
		case j == len(b.Keys) || (i < len(a.Keys) && a.Keys[i] < b.Keys[j]):
			union += a.Labels[a.Keys[i]]
			i++
		case i == len(a.Keys) || b.Keys[j] < a.Keys[i]:
			union += b.Labels[b.Keys[j]]
			j++
		default:
			w, v := a.Labels[a.Keys[i]], b.Labels[b.Keys[j]]
			inter += math.Min(w, v)
			union += math.Max(w, v)
			i++
			j++
		}
	}
	if union == 0 {
		return 0
	}
	return inter / union
}

// BuildRoutine makes a group's template (the steps at least half its
// requests ran together, in a recorded order), finds each request's run of
// it, drafts it and runs the checks.
func BuildRoutine(corpus []trace.NormSession, inst []ReqInstance, g []int, names []string, o model.Options) (model.Routine, bool) {
	// The steps that belong to the routine: the largest set of replayable
	// steps that at least half its requests ran together. Steps are taken in
	// order of how many requests ran them, and one is kept only if half the
	// requests still ran every step kept so far; steps each present in half
	// the requests separately can otherwise make a set no request ran whole.
	has := make([]map[string]bool, len(g))
	present := map[string]int{}
	for k, i := range g {
		s := corpus[inst[i].Session]
		has[k] = map[string]bool{}
		for _, si := range inst[i].Steps {
			if l := s.Steps[si].Label; trace.Replayable(l) && !has[k][l] {
				has[k][l] = true
				present[l]++
			}
		}
	}
	byPresence := make([]string, 0, len(present))
	for l := range present {
		byPresence = append(byPresence, l)
	}
	sort.Slice(byPresence, func(a, b int) bool {
		if present[byPresence[a]] != present[byPresence[b]] {
			return present[byPresence[a]] > present[byPresence[b]]
		}
		return byPresence[a] < byPresence[b]
	})
	inSet := map[string]bool{}
	covering := make([]int, len(g))
	for k := range covering {
		covering[k] = k
	}
	// firstOrder is request k's order of first appearance of the steps in set.
	firstOrder := func(k int, set map[string]bool) string {
		s := corpus[inst[g[k]].Session]
		seen := map[string]bool{}
		var labels []string
		for _, si := range inst[g[k]].Steps {
			if l := s.Steps[si].Label; set[l] && !seen[l] {
				seen[l] = true
				labels = append(labels, l)
			}
		}
		return strings.Join(labels, "\x1f")
	}
	for _, l := range byPresence {
		if 2*present[l] < len(g) {
			break
		}
		var next []int
		for _, k := range covering {
			if has[k][l] {
				next = append(next, k)
			}
		}
		if 2*len(next) < len(g) {
			continue
		}
		// Kept only if half the requests also ran the kept steps in one
		// order: a procedure recurs in an order, not just as a set.
		trial := map[string]bool{l: true}
		for x := range inSet {
			trial[x] = true
		}
		orders := map[string][]int{}
		for _, k := range next {
			o := firstOrder(k, trial)
			orders[o] = append(orders[o], k)
		}
		var best []int
		for _, ks := range orders {
			if len(ks) > len(best) {
				best = ks
			}
		}
		if 2*len(best) >= len(g) {
			inSet[l] = true
			covering = next
		}
	}
	if len(inSet) == 1 {
		// One tool can still be a procedure when most requests ran it more
		// than once (two diffs, a checksum per file).
		var only string
		for l := range inSet {
			only = l
		}
		multi := 0
		for _, i := range g {
			n := 0
			for _, si := range inst[i].Steps {
				if corpus[inst[i].Session].Steps[si].Label == only {
					n++
				}
			}
			if n >= 2 {
				multi++
			}
		}
		if 2*multi < len(g) {
			return model.Routine{}, false
		}
	}
	if len(inSet) == 0 {
		return model.Routine{}, false
	}
	// Each request's run of those steps in recorded order. Two readings:
	// the exact sequence with repeats, and the order in which each step
	// first appeared. If one exact sequence is shared by at least half the
	// requests it is used as recorded. Otherwise the first-appearance order
	// most requests share is used (a real order, never rearranged), and a
	// step that most of those requests ran several times with different
	// values is flagged as a loop to be written by hand.
	type run struct {
		inst    int
		all     []trace.Step
		first   []trace.Step
		repeats map[string]bool
	}
	byFull := map[string][]run{}
	byFirst := map[string][]run{}
	for _, i := range g {
		s := corpus[inst[i].Session]
		r := run{inst: i, repeats: map[string]bool{}}
		seen := map[string]bool{}
		var all, first []string
		for _, si := range inst[i].Steps {
			st := s.Steps[si]
			if !inSet[st.Label] {
				continue
			}
			r.all = append(r.all, st)
			all = append(all, st.Label)
			if seen[st.Label] {
				r.repeats[st.Label] = true
				continue
			}
			seen[st.Label] = true
			r.first = append(r.first, st)
			first = append(first, st.Label)
		}
		if len(seen) < len(inSet) {
			continue
		}
		if len(r.all) <= 24 {
			byFull[strings.Join(all, "\x1f")] = append(byFull[strings.Join(all, "\x1f")], r)
		}
		byFirst[strings.Join(first, "\x1f")] = append(byFirst[strings.Join(first, "\x1f")], r)
	}
	modalOf := func(m map[string][]run) string {
		best := ""
		for k, rs := range m {
			if len(rs) > len(m[best]) || (len(rs) == len(m[best]) && k < best) {
				best = k
			}
		}
		return best
	}
	type chosenRun struct {
		inst  int
		steps []trace.Step
		all   []trace.Step
	}
	var chosen []chosenRun
	var tmpl []string
	var loops []string
	if full := modalOf(byFull); full != "" && 2*len(byFull[full]) >= len(g) {
		tmpl = strings.Split(full, "\x1f")
		for _, r := range byFull[full] {
			chosen = append(chosen, chosenRun{r.inst, r.all, r.all})
		}
	} else if first := modalOf(byFirst); first != "" {
		tmpl = strings.Split(first, "\x1f")
		rep := map[string]int{}
		for _, r := range byFirst[first] {
			chosen = append(chosen, chosenRun{r.inst, r.first, r.all})
			for l := range r.repeats {
				rep[l]++
			}
		}
		for _, l := range tmpl {
			if 2*rep[l] >= len(byFirst[first]) {
				loops = append(loops, l)
			}
		}
	}
	// A run where a step failed is not evidence the procedure works.
	var occ [][]trace.Step
	var occReq []int // index into inst
	var occAll [][]trace.Step
	seqRuns, failedRuns, unknownRuns := 0, 0, 0
	for _, r := range chosen {
		seqRuns++
		failed, unknown := false, false
		for _, st := range r.steps {
			switch st.Outcome {
			case trace.OutcomeFailed:
				failed = true
			case trace.OutcomeUnknown:
				unknown = true
			}
		}
		if failed {
			failedRuns++
			continue
		}
		if unknown {
			unknownRuns++
		}
		occ = append(occ, r.steps)
		occReq = append(occReq, r.inst)
		occAll = append(occAll, r.all)
	}
	if len(tmpl) < 2 && len(inSet) > 1 {
		// No sequence of these steps recurred: the requests share steps but
		// not a procedure. Report the step set for the reader.
		tmpl = nil
		for l := range inSet {
			tmpl = append(tmpl, l)
		}
		sort.Strings(tmpl)
	}

	c := model.Candidate{ByClient: map[string]int{}, Sessions: 0, SessionSet: map[int]bool{}}
	for _, l := range tmpl {
		c.Steps = append(c.Steps, model.StepTemplate{Label: l})
	}
	rt := model.Routine{Requests: len(g), Consistency: float64(len(occ)) / float64(len(g))}
	var covs []float64
	for _, i := range occReq {
		n := 0
		for _, si := range inst[i].Steps {
			if trace.Replayable(corpus[inst[i].Session].Steps[si].Label) {
				n++
			}
		}
		if n > 0 {
			covs = append(covs, math.Min(1, float64(len(tmpl))/float64(n)))
		}
	}
	if len(covs) > 0 {
		sort.Float64s(covs)
		rt.Coverage = covs[len(covs)/2]
	}
	rt.Example = trace.OneLine(redact.Redact(inst[g[0]].Text), 140)
	ran := map[int]bool{}
	for _, i := range occReq {
		ran[i] = true
	}
	for _, i := range g {
		s := corpus[inst[i].Session]
		rt.Sources = append(rt.Sources, model.SourceRef{Client: s.Client, Session: s.ID, Request: inst[i].Request, Ran: ran[i]})
	}
	weeks := map[string]bool{}
	var times []time.Time
	for _, i := range g {
		s := corpus[inst[i].Session]
		if !c.SessionSet[inst[i].Session] {
			c.SessionSet[inst[i].Session] = true
			c.ByClient[s.Client]++
			c.Sessions++
		}
		t := s.Start
		if st := s.Steps[inst[i].Steps[0]].Time; !st.IsZero() {
			t = st
		}
		if !t.IsZero() {
			times = append(times, t)
			y, w := t.ISOWeek()
			weeks[fmt.Sprintf("%d-%02d", y, w)] = true
		}
	}
	c.Weeks = len(weeks)
	sort.Slice(times, func(a, b int) bool { return times[a].Before(times[b]) })
	if len(times) > 0 {
		c.FirstSeen, c.LastSeen = times[0], times[len(times)-1]
	}
	if len(times) > 1 {
		gaps := make([]float64, 0, len(times)-1)
		for i := 1; i < len(times); i++ {
			gaps = append(gaps, times[i].Sub(times[i-1]).Hours()/24)
		}
		sort.Float64s(gaps)
		c.MedianGapDays = gaps[len(gaps)/2]
	}
	var runs [][2]trace.Usage
	for _, steps := range occ {
		if run, saved, ok := model.RunCost(steps); ok {
			c.Measured++
			c.SavedTotal = c.SavedTotal.Add(saved)
			runs = append(runs, [2]trace.Usage{run, saved})
		}
	}
	if len(runs) > 0 {
		sort.Slice(runs, func(a, b int) bool { return runs[a][0].Total() < runs[b][0].Total() })
		c.PerRun, c.SavedPerRun = runs[len(runs)/2][0], runs[len(runs)/2][1]
	}
	// Request routines are not significance-tested: none of the pattern
	// statistics apply, and none is claimed.
	rt.Candidate = c
	rt.Statistics, rt.Validation = "not_run", "not_run"

	// A group whose template no request ran in full cannot be drafted.
	rt.Loops = loops
	if len(occ) < 2 {
		rt.Failed, rt.Why = model.CheckSameWay, fmt.Sprintf("only %d of %d requests ran the same sequence of its steps and succeeded", len(occ), len(g))
		rt.Decision = "removed"
		rt.Statistics, rt.Validation = "not_run", "not_run"
		rt.Kind = RoutineKind(corpus, inst, g, tmpl)
		RoutineLegacyStates(&rt, nil)
		sum := sha256.Sum256([]byte(rt.SourceRole + "\x00" + strings.Join(tmpl, "\x1f")))
		rt.ID = hex.EncodeToString(sum[:6])
		rt.Family = "unknown:" + rt.ID
		rt.Suitability, rt.Reasons = model.SuitInsufficient, []string{"inconsistent_order"}
		if rt.SourceRole == model.RoleHarness {
			rt.Suitability, rt.Reasons = model.SuitInvalid, []string{"harness_request"}
		}
		rt.Failed = strings.SplitN(rt.Reasons[0], ":", 2)[0]
		return rt, true
	}
	c.Items = make([]int, len(tmpl))
	rt.Candidate = c
	specs := LoopSpecs(loops, occAll, occReq, inst)
	d := BuildDraft(c, occ, model.DraftOptions{Publisher: DefaultPublisher, Loops: specs})
	rt.Draft, rt.Occ, rt.LoopSpecs = d, occ, specs

	// Per input: how often its value appeared in the request (reported, not a check).
	for n, in := range d.Inputs {
		hit, total := 0, 0
		for j, v := range DraftInputValues(d, n) {
			if v == "" {
				continue
			}
			total++
			if trace.InRequest(v, inst[occReq[j]].Text) {
				hit++
			}
		}
		share := 0.0
		if total > 0 {
			share = float64(hit) / float64(total)
		}
		rt.Inputs = append(rt.Inputs, model.RoutineInput{Name: in.Name, Type: in.Type, Explained: share})
	}
	// Not covered (computed now, checked last)
	skills := map[string]int{}
	for _, i := range g {
		for sk := range corpus[inst[i].Session].RequestSkills[inst[i].Request] {
			skills[sk]++
		}
	}
	for sk, n := range skills {
		if 2*n >= len(g) && (rt.CoveredBy == "" || sk < rt.CoveredBy) {
			rt.CoveredBy = sk
		}
	}

	rt.Runs, rt.FailedRuns, rt.UnknownRuns = seqRuns, failedRuns, unknownRuns
	rt.Kind = RoutineKind(corpus, inst, g, tmpl)
	// Role, outcome and value; suitability is decided by the contract.
	RoutineLegacyStates(&rt, d)
	switch {
	case rt.SourceRole == model.RoleScheduled:
		rt.Baseline = "scheduled_automation"
	case rt.CoveredBy != "":
		rt.Baseline = "skill:" + rt.CoveredBy
	}
	common := map[string]bool{}
	for _, l := range tmpl {
		common[l] = true
	}
	sum := sha256.Sum256([]byte(rt.SourceRole + "\x00" + strings.Join(tmpl, "\x1f") + "\x00" + SplitKey(occ[0], common)))
	rt.ID = hex.EncodeToString(sum[:6])
	cruns := make([]ContractRun, len(occ))
	for j := range occ {
		in := inst[occReq[j]]
		s := corpus[in.Session]
		first, last := -1, -1
		for _, st := range occAll[j] {
			if first < 0 || st.Call < first {
				first = st.Call
			}
			if st.Call > last {
				last = st.Call
			}
		}
		n := 0
		for _, a := range s.Approvals {
			if a.Request == in.Request && a.AfterCall > first && a.AfterCall <= last {
				n++
			}
		}
		var all []trace.Step
		for _, si := range in.Steps {
			all = append(all, s.Steps[si])
		}
		cruns[j] = ContractRun{Steps: occ[j], All: all, Text: in.Text, Approvals: n}
	}
	BuildContract(&rt, d, cruns, loops)
	return rt, true
}

// LoopSpecs finds, for each step most runs made several times, whether it
// loops over a list the request gave: exactly one argument varies between
// its occurrences, and in most runs every value of it is in the request.
// Anything else stays an open loop.
func LoopSpecs(loops []string, occAll [][]trace.Step, occReq []int, inst []ReqInstance) map[string]model.LoopSpec {
	out := map[string]model.LoopSpec{}
	for _, l := range loops {
		varied := map[string]int{}
		per := make([]map[string][]string, len(occAll))
		for j, all := range occAll {
			per[j] = map[string][]string{}
			var occs []trace.Step
			for _, st := range all {
				if st.Label == l {
					occs = append(occs, st)
				}
			}
			vals := map[string][]string{}
			for _, st := range occs {
				for _, sl := range st.Slots {
					if sl.Sub || sl.Type == trace.SlotFlag || trace.Derived(sl.Key) {
						continue
					}
					vals[sl.Key] = append(vals[sl.Key], sl.Value)
				}
			}
			for k, vs := range vals {
				if len(vs) != len(occs) {
					continue
				}
				per[j][k] = vs
				for _, v := range vs[1:] {
					if v != vs[0] {
						varied[k]++
						break
					}
				}
			}
		}
		key, n := "", 0
		others := 0
		for k, c := range varied {
			if c > n || (c == n && k < key) {
				key, n = k, c
			}
		}
		for k, c := range varied {
			if k != key && 2*c >= len(occAll) {
				others++
			}
		}
		if key == "" || 2*n < len(occAll) || others > 0 {
			continue
		}
		spec := model.LoopSpec{Key: key, Values: make([][]string, len(occAll))}
		given, prior := 0, 0
		for j := range occAll {
			vs := per[j][key]
			spec.Values[j] = vs
			// The steps before the loop's first item, whose results could
			// have listed the items.
			var before []trace.Step
			for _, st := range occAll[j] {
				if st.Label == l {
					break
				}
				before = append(before, st)
			}
			inReq, inOut := len(vs) > 0, len(vs) > 0
			for _, v := range vs {
				if !trace.InRequest(v, inst[occReq[j]].Text) {
					inReq = false
				}
				found := false
				for _, st := range before {
					if trace.InResult(v, st) {
						found = true
						break
					}
				}
				if !found && !trace.InRequest(v, inst[occReq[j]].Text) {
					inOut = false
				}
			}
			switch {
			case inReq:
				given++
			case inOut:
				prior++
			}
		}
		switch {
		case 2*given >= len(occAll):
			spec.Source = "caller"
			out[l] = spec
		case 2*(given+prior) >= len(occAll):
			spec.Source = "prior_output"
			out[l] = spec
		}
	}
	return out
}

// RoutineKind says who a routine's work is for. Scheduled: most requests are
// Codex automation prompts. Automated: most requests carry the same prompt
// (digits aside), each alone in its session, so a program sent it.
// Bookkeeping: every step is a Telara recording or discovery call.
func RoutineKind(corpus []trace.NormSession, inst []ReqInstance, g []int, tmpl []string) string {
	book := true
	for _, l := range tmpl {
		if !trace.BookkeepingTools[l] {
			book = false
		}
	}
	if book {
		return "bookkeeping"
	}
	scheduled, single, harness, long := 0, 0, 0, 0
	texts := map[string]int{}
	for _, i := range g {
		t := strings.TrimSpace(inst[i].Text)
		if strings.HasPrefix(t, "Automation:") {
			scheduled++
		}
		if trace.IsHarness(t) || t == "" {
			harness++
		}
		if len(t) >= 120 {
			long++
		}
		if len(corpus[inst[i].Session].Requests) == 1 {
			single++
		}
		texts[trace.Digits.ReplaceAllString(trace.TruncateUTF8(t, 120), "#")]++
	}
	if 2*harness > len(g) {
		return "harness"
	}
	if 2*scheduled >= len(g) {
		return "scheduled"
	}
	top := 0
	for t, n := range texts {
		if t != "" && n > top {
			top = n
		}
	}
	// A program sends the same long prompt, alone in its session; a person
	// types a short request that merely differs in a number.
	if 5*top >= 4*len(g) && 5*single >= 4*len(g) && 5*long >= 4*len(g) {
		return "automated"
	}
	return "user"
}

// MergeDuplicates folds a routine into an earlier one (in report order)
// that is the same procedure: the same source role and the same steps in the
// same order and multiplicity, with the same operations and scope (the
// routine ID covers those, and so does Family's contract). An unordered set
// of labels is never enough: a different order or count is a different
// procedure.
func MergeDuplicates(rs []model.Routine) int {
	seen := map[string]string{}
	merged := 0
	for i := range rs {
		if rs[i].Decision == "removed" {
			continue
		}
		labels := make([]string, len(rs[i].Steps))
		for k, st := range rs[i].Steps {
			labels[k] = st.Label
		}
		key := rs[i].Kind + "\x00" + rs[i].SourceRole + "\x00" + strings.Join(labels, "\x1f") + "\x00" + strings.Join(rs[i].Contract.Scope, "\x1f") + "\x00" + rs[i].Family
		if rs[i].Family == "" || strings.HasPrefix(rs[i].Family, "unknown:") {
			key += "\x00" + rs[i].ID
		}
		if first, ok := seen[key]; ok {
			rs[i].MergedInto = first
			merged++
			continue
		}
		seen[key] = rs[i].ID
	}
	return merged
}

// BoundedPart looks inside a routine that is not a useful procedure as a
// whole for the longest run of its steps (two or more, in order) whose
// every input has a known source and no judgment: a bounded procedure
// inside a larger investigation, such as collecting a namespace's pod logs
// before diagnosing. It is judged on its own contract and reported with its
// parent. It is kept only when useful; the parent is never claimed.
func BoundedPart(corpus []trace.NormSession, inst []ReqInstance, g []int, names []string, o model.Options, rt *model.Routine) (model.Routine, bool) {
	d := rt.Draft
	// Only inside a person's varying work: a scheduled automation is
	// already automated (its baseline covers its parts), and harness or
	// bookkeeping sources carry no user procedure.
	if d == nil || rt.Suitability == model.SuitUseful || rt.SourceRole != model.RoleUser || len(rt.Steps) < 3 {
		return model.Routine{}, false
	}
	bad := map[int]bool{}
	for p := range d.HumanPos {
		bad[p] = true
	}
	for k, in := range d.Inputs {
		if k >= len(rt.Contract.Inputs) {
			break
		}
		switch rt.Contract.Inputs[k].Source {
		case InputUnresolved, InputComposed:
			bad[in.Pos] = true
		}
	}
	for p, st := range rt.Steps {
		for _, l := range rt.Loops {
			if st.Label == l && !d.ListLoop[l] {
				bad[p] = true
			}
		}
	}
	if len(bad) == 0 {
		return model.Routine{}, false
	}
	bestLo, bestHi := 0, 0
	for lo := 0; lo < len(rt.Steps); lo++ {
		hi := lo
		for hi < len(rt.Steps) && !bad[hi] {
			hi++
		}
		if hi-lo > bestHi-bestLo {
			bestLo, bestHi = lo, hi
		}
	}
	if bestHi-bestLo < 2 {
		return model.Routine{}, false
	}
	keep := map[string]bool{}
	for p := bestLo; p < bestHi; p++ {
		keep[rt.Steps[p].Label] = true
	}
	for p := range bad {
		if p < len(rt.Steps) && keep[rt.Steps[p].Label] {
			return model.Routine{}, false // a kept label also sits at a bad position
		}
	}
	var sub []ReqInstance
	var gs []int
	sessions := map[int]bool{}
	for _, i := range g {
		in := inst[i]
		var steps []int
		for _, si := range inst[i].Steps {
			if keep[corpus[inst[i].Session].Steps[si].Label] {
				steps = append(steps, si)
			}
		}
		if len(steps) < 2 {
			continue
		}
		in.Steps = steps
		gs = append(gs, len(sub))
		sub = append(sub, in)
		sessions[in.Session] = true
	}
	// The part must recur on its own.
	if len(gs) < o.MinSupport || len(sessions) < 2 {
		return model.Routine{}, false
	}
	part, ok := BuildRoutine(corpus, sub, gs, names, o)
	if !ok || part.Suitability != model.SuitUseful {
		return model.Routine{}, false
	}
	// A part with nothing to parameterize (opening a browser, printing the
	// working directory) is scaffolding around the work, not a procedure a
	// caller would run with different inputs.
	param := false
	for _, in := range part.Contract.Inputs {
		if in.Source == InputCaller || in.Source == InputPriorOutput {
			param = true
		}
	}
	if !param {
		return model.Routine{}, false
	}
	part.Parent = rt.ID
	part.Reasons = append(part.Reasons, "bounded_part_of:"+rt.ID)
	return part, true
}

// GoalShare checks a routine whose goal is its requests' shared text: if
// most requests with that text did something else, these steps are not the
// procedure for the goal (a few runs happened to share incidental calls).
// A request with the text counts as doing it this way when it ran every
// step of the routine, whichever group its other calls put it in.
func GoalShare(rt *model.Routine, corpus []trace.NormSession, inst []ReqInstance, g []int, byText map[string]int, instByText map[string][]int) {
	if rt.Suitability != model.SuitUseful || rt.Contract.Goal != model.GoalStated {
		return
	}
	count := map[string]int{}
	for _, i := range g {
		if k := trace.TextKey(inst[i].Text); k != "" {
			count[k]++
		}
	}
	top, n := "", 0
	for k, c := range count {
		if c > n || (c == n && k < top) {
			top, n = k, c
		}
	}
	if top == "" {
		return
	}
	need := map[string]int{}
	for _, st := range rt.Steps {
		need[st.Label]++
	}
	ran := 0
	for _, i := range instByText[top] {
		have := map[string]int{}
		for _, si := range inst[i].Steps {
			have[corpus[inst[i].Session].Steps[si].Label]++
		}
		all := true
		for l, c := range need {
			if have[l] < c {
				all = false
			}
		}
		if all {
			ran++
		}
	}
	if 2*ran >= byText[top] {
		return
	}
	rt.Suitability = model.SuitInsufficient
	rt.Reasons = []string{fmt.Sprintf("goal_usually_done_differently:%d_of_%d", ran, byText[top])}
	rt.DraftStatus, rt.Blockers = model.DraftNotAttempted, nil
	rt.Decision, rt.Failed = "removed", "goal_usually_done_differently"
	rt.Why = strings.Join(rt.Reasons, "; ")
}

// GoalCore rebuilds a routine that failed the goal-share check from the
// steps most requests with its text ran, over all of those requests. When
// incidental calls split one task's requests into several groups, each
// group fails the check on its own; the steps they share are the procedure
// for the goal. Built once per text.
func GoalCore(corpus []trace.NormSession, inst []ReqInstance, names []string, o model.Options, rt *model.Routine, byText map[string]int, instByText map[string][]int, seen map[string]bool) (model.Routine, bool) {
	if FirstReason(*rt) == "" || !strings.HasPrefix(FirstReason(*rt), "goal_usually_done_differently") {
		return model.Routine{}, false
	}
	top := ""
	// The routine's dominant text.
	count := map[string]int{}
	for _, src := range rt.Sources {
		if k := TextKeyOf(corpus, src); k != "" {
			count[k]++
		}
	}
	n := 0
	for k, c := range count {
		if c > n || (c == n && k < top) {
			top, n = k, c
		}
	}
	if top == "" || seen[top] {
		return model.Routine{}, false
	}
	seen[top] = true
	members := instByText[top]
	has := make([]map[string]bool, len(members))
	present := map[string]int{}
	for k, i := range members {
		has[k] = map[string]bool{}
		for _, si := range inst[i].Steps {
			if l := corpus[inst[i].Session].Steps[si].Label; trace.Replayable(l) && !has[k][l] {
				has[k][l] = true
				present[l]++
			}
		}
	}
	keep := map[string]bool{}
	for l, c := range present {
		if 2*c >= byText[top] {
			keep[l] = true
		}
	}
	if len(keep) < 2 {
		return model.Routine{}, false
	}
	var sub []ReqInstance
	var gs []int
	sessions := map[int]bool{}
	for _, i := range members {
		in := inst[i]
		var steps []int
		for _, si := range inst[i].Steps {
			if keep[corpus[inst[i].Session].Steps[si].Label] {
				steps = append(steps, si)
			}
		}
		if len(steps) < 2 {
			continue
		}
		in.Steps = steps
		gs = append(gs, len(sub))
		sub = append(sub, in)
		sessions[in.Session] = true
	}
	if len(gs) < o.MinSupport || len(sessions) < 2 {
		return model.Routine{}, false
	}
	core, ok := BuildRoutine(corpus, sub, gs, names, o)
	if !ok {
		return model.Routine{}, false
	}
	core.Reasons = append(core.Reasons, "core_of_goal")
	return core, true
}

// TextKeyOf is the text key of the request a source names.
func TextKeyOf(corpus []trace.NormSession, src model.SourceRef) string {
	for _, s := range corpus {
		if s.Client == src.Client && s.ID == src.Session {
			if src.Request < len(s.Requests) {
				return trace.TextKey(s.Requests[src.Request])
			}
		}
	}
	return ""
}

// ReviewActions are the side effects the review can take. Publish may be nil
// (the runner alone has no registry); CanPublish then need not be set.
type ReviewActions struct {
	// Save installs a draft and returns where it went.
	Save func(d *model.Draft) (string, error)
	// CanPublish reports why publishing is unavailable ("" when it is).
	CanPublish func() string
	// Publish sends a draft to "user" or "tenant" and returns the
	// registry's answer and whether it accepted.
	Publish func(d *model.Draft, audience string) (string, bool, error)
}

// ReviewConfig limits the list and names the publisher for publishing.
type ReviewConfig struct {
	Top       int
	Publisher string
}

// Pick reads "1 3 5", "2-4", "all" or blank against n items (1-based) and
// returns 0-based indexes, each once, in the order given.
func Pick(answer string, n int) []int {
	answer = strings.ToLower(strings.TrimSpace(answer))
	var out []int
	if answer == "all" {
		for i := 0; i < n; i++ {
			out = append(out, i)
		}
		return out
	}
	seen := map[int]bool{}
	for _, f := range strings.Fields(strings.ReplaceAll(answer, ",", " ")) {
		lo, hi := f, f
		if a, b, ok := strings.Cut(f, "-"); ok {
			lo, hi = a, b
		}
		x, e1 := strconv.Atoi(lo)
		y, e2 := strconv.Atoi(hi)
		if e1 != nil || e2 != nil {
			continue
		}
		for k := x; k <= y; k++ {
			if k >= 1 && k <= n && !seen[k] {
				seen[k] = true
				out = append(out, k-1)
			}
		}
	}
	return out
}

// Review runs the pick-list over rep's primitives.
func Review(in io.Reader, out io.Writer, rep *model.Report, cfg ReviewConfig, act ReviewActions) error {
	sc := bufio.NewScanner(in)
	ask := func(prompt string) (string, bool) {
		fmt.Fprint(out, prompt)
		if !sc.Scan() {
			return "", false
		}
		return strings.TrimSpace(sc.Text()), true
	}
	prims := ReportPrimitives(rep)
	if cfg.Top > 0 && len(prims) > cfg.Top {
		prims = prims[:cfg.Top]
	}
	if len(prims) == 0 {
		fmt.Fprintln(out, "No routine was recognized as a useful procedure with a complete draft, so there is nothing to save.")
		return nil
	}
	fmt.Fprintln(out, "\nRecommended procedures, drafted. UNVALIDATED: none has been executed.")
	for i, p := range prims {
		d := RoutineDraft(p)
		fmt.Fprintf(out, "\n%d. %s  (%d requests, %d weeks", i+1, d.Name, p.Requests, p.Weeks)
		if p.Measured > 0 {
			fmt.Fprintf(out, ", saves %s tokens per run", HumanTokens(p.SavedPerRun.Total()))
		}
		fmt.Fprintf(out, ")\n   asked as: %s\n", trace.OneLine(redact.Redact(p.Example), 120))
		if _, digest, err := pack.PackageDraft(d); err == nil {
			fmt.Fprintf(out, "   status: unvalidated (validation not run for %s)\n", digest)
		}
		var srcs []string
		for _, in := range p.Contract.Inputs {
			srcs = append(srcs, in.Name+"<-"+in.Source)
		}
		fmt.Fprintf(out, "   contract: effect %s, goal %s, inputs %s", p.Contract.Effect, p.Contract.Goal, OrNone(srcs))
		if p.Contract.Boundary != "" {
			fmt.Fprintf(out, "; %s", p.Contract.Boundary)
		}
		fmt.Fprintln(out)
		for _, s := range d.Steps {
			fmt.Fprintf(out, "   %d. [%s] %s\n", s.N, s.Kind, trace.OneLine(redact.Redact(s.Line), 130))
		}
		for _, in := range d.Inputs {
			if in.Sensitive {
				fmt.Fprintf(out, "   input $%d %s: a credential, supplied by the caller (recorded value not kept)\n", in.Position, in.Name)
			} else if in.Extract != "" {
				fmt.Fprintf(out, "   value %s (%s): taken from step %d's output, not an input\n", in.Name, in.Type, in.DerivedFrom)
			} else {
				fmt.Fprintf(out, "   input $%d %s (%s), e.g. %s\n", in.Position, in.Name, in.Type, trace.OneLine(redact.Redact(in.Example), 60))
			}
		}
		if len(d.Blocked) > 0 {
			fmt.Fprintf(out, "   BLOCKED: still credential-shaped (%s); it cannot be saved or published\n", strings.Join(d.Blocked, "; "))
		}
		if len(d.Problems) > 0 {
			fmt.Fprintf(out, "   publish checks: %d problem(s): %s\n", len(d.Problems), trace.OneLine(d.Problems[0], 100))
		}
	}
	fmt.Fprintln(out, "\nEvery step is drafted as a change: the runner asks before running it. Read main.sh in a saved folder before you run it.")

	answer, ok := ask("\nSave which? Numbers (1 3, 2-4), all, or blank for none > ")
	if !ok {
		return nil
	}
	for _, i := range Pick(answer, len(prims)) {
		d := RoutineDraft(prims[i])
		where, err := act.Save(d)
		if err != nil {
			fmt.Fprintf(out, "%d. %s not saved: %v\n", i+1, d.Name, err)
			continue
		}
		fmt.Fprintf(out, "%d. %s saved -> %s\n", i+1, d.Name, where)
	}

	if act.Publish == nil {
		return nil
	}
	if act.CanPublish != nil {
		if why := act.CanPublish(); why != "" {
			fmt.Fprintf(out, "\nPublishing is unavailable: %s\n", why)
			return nil
		}
	}
	answer, ok = ask("\nPublish which? Numbers, all, or blank for none > ")
	chosen := Pick(answer, len(prims))
	if !ok || len(chosen) == 0 {
		return nil
	}
	publisher := cfg.Publisher
	if publisher == "" || publisher == DefaultPublisher {
		if publisher, ok = ask("Publisher namespace your organisation publishes under (reverse-DNS, e.g. com.acme) > "); !ok || publisher == "" {
			fmt.Fprintln(out, "Not published: a publisher namespace is required.")
			return nil
		}
	}
	aud, ok := ask("Audience: [u]ser (only you) or [t]enant (everyone; an admin approves it first) > ")
	if !ok {
		return nil
	}
	audience := "user"
	if strings.HasPrefix(strings.ToLower(aud), "t") {
		audience = "tenant"
	}
	for _, i := range chosen {
		d := RoutineDraftAs(prims[i], publisher, nil)
		if _, err := pack.DraftArtifacts(d); err != nil {
			fmt.Fprintf(out, "%d. %s not published: %v\n", i+1, d.Name, err)
			continue
		}
		if len(d.Problems) > 0 {
			fmt.Fprintf(out, "%d. %s not published: %s\n", i+1, d.Name, strings.Join(d.Problems, "; "))
			continue
		}
		msg, accepted, err := act.Publish(d, audience)
		switch {
		case err != nil:
			fmt.Fprintf(out, "%d. %s not published: %v\n", i+1, d.Name, err)
		case !accepted:
			fmt.Fprintf(out, "%d. %s refused by the registry:\n%s\n", i+1, d.Name, msg)
		default:
			fmt.Fprintf(out, "%d. published %s/%s@0.1.0 to %s. %s\n", i+1, d.Publisher, d.Name, audience, trace.OneLine(msg, 200))
		}
	}
	return nil
}

// NullSupport counts, for each pattern, how many sessions contain it in a
// corpus built by permute. It is run several times; the mean and variance of
// those counts are the null model for the pattern's observed support.
func NullSupport(seqs [][]int, ps []model.Pattern, window int) []int {
	index := model.LabelIndex(seqs)
	out := make([]int, len(ps))
	for i, p := range ps {
		out[i] = model.SupportIn(seqs, index, p.Items, window)
	}
	return out
}

// ShuffleAcross pools the steps of every session of one client and deals
// them back into that client's sessions at their original lengths, for each
// client separately. What survives: how often each label occurs in each
// client, and how long sessions are. What is destroyed: which labels travel
// together. It is the null for "these steps co-occur". Pooling across
// clients would make any two tools that only one client has look related.
func ShuffleAcross(seqs [][]int, group []int, rng *rand.Rand) [][]int {
	out := make([][]int, len(seqs))
	byGroup := map[int][]int{}
	for i, g := range group {
		byGroup[g] = append(byGroup[g], i)
	}
	gs := make([]int, 0, len(byGroup))
	for g := range byGroup {
		gs = append(gs, g)
	}
	sort.Ints(gs)
	for _, g := range gs {
		var pool []int
		for _, i := range byGroup[g] {
			pool = append(pool, seqs[i]...)
		}
		rng.Shuffle(len(pool), func(a, b int) { pool[a], pool[b] = pool[b], pool[a] })
		k := 0
		for _, i := range byGroup[g] {
			out[i] = pool[k : k+len(seqs[i])]
			k += len(seqs[i])
		}
	}
	return out
}

// ShuffleWithin reorders each session's steps. What survives: which labels
// each session has. What is destroyed: their order. It is the null for
// "these steps happen in this order".
func ShuffleWithin(seqs [][]int, _ []int, rng *rand.Rand) [][]int {
	out := make([][]int, len(seqs))
	for i, s := range seqs {
		c := append([]int{}, s...)
		rng.Shuffle(len(c), func(a, b int) { c[a], c[b] = c[b], c[a] })
		out[i] = c
	}
	return out
}

// PValue is the upper-tail probability of observing obs or more under the
// null counts. A Poisson with the null mean is used, add-half smoothed so an
// all-zero null is not treated as impossible; when the null counts are more
// spread than Poisson allows, the normal tail with their own variance is used
// if it is larger. The larger p is the conservative one.
func PValue(obs int, null []int) float64 {
	k := float64(len(null))
	var sum, sq float64
	for _, n := range null {
		sum += float64(n)
		sq += float64(n) * float64(n)
	}
	lambda := (sum + 0.5) / k
	p := PoissonUpper(obs, lambda)
	mean := sum / k
	if v := sq/k - mean*mean; v > mean && v > 0 {
		z := (float64(obs) - 0.5 - mean) / math.Sqrt(v)
		if q := 0.5 * math.Erfc(z/math.Sqrt2); q > p {
			p = q
		}
	}
	return p
}

// PoissonUpper is P(X >= k) for X ~ Poisson(lambda). Above the mean the tail
// is summed directly in log space, so far tails stay accurate instead of
// bottoming out at 1 - (1 - epsilon).
func PoissonUpper(k int, lambda float64) float64 {
	if k <= 0 {
		return 1
	}
	logPMF := func(i int) float64 { return float64(i)*math.Log(lambda) - lambda - Lgamma(float64(i)+1) }
	if float64(k) > lambda {
		sum := 0.0
		for i := k; ; i++ {
			t := math.Exp(logPMF(i))
			sum += t
			if t < sum*1e-17 || i > k+100000 {
				return sum
			}
		}
	}
	cdf := 0.0
	for i := 0; i < k; i++ {
		cdf += math.Exp(logPMF(i))
	}
	return math.Max(0, 1-cdf)
}

// BinomialUpper is P(X >= k) for X ~ Binomial(n, p), summed in log space.
func BinomialUpper(k, n int, p float64) float64 {
	switch {
	case k <= 0:
		return 1
	case k > n:
		return 0
	case p <= 0:
		return 0
	case p >= 1:
		return 1
	}
	lc := func(i int) float64 {
		return Lgamma(float64(n)+1) - Lgamma(float64(i)+1) - Lgamma(float64(n-i)+1) + float64(i)*math.Log(p) + float64(n-i)*math.Log1p(-p)
	}
	sum := 0.0
	for i := k; i <= n; i++ {
		sum += math.Exp(lc(i))
	}
	return math.Min(1, sum)
}

func Lgamma(x float64) float64 { v, _ := math.Lgamma(x); return v }

// BenjaminiHochberg turns p-values into q-values controlling the false
// discovery rate across every candidate tested at once.
func BenjaminiHochberg(ps []float64) []float64 {
	m := len(ps)
	order := make([]int, m)
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return ps[order[a]] < ps[order[b]] })
	q := make([]float64, m)
	minSoFar := 1.0
	for r := m - 1; r >= 0; r-- {
		i := order[r]
		v := ps[i] * float64(m) / float64(r+1)
		if v < minSoFar {
			minSoFar = v
		}
		q[i] = minSoFar
	}
	return q
}

// BenjaminiHochbergOf is benjaminiHochberg when only some of m tests are
// listed and every unlisted test is known to have p above every level of
// interest (it was discarded for failing). A listed p at or below alpha then
// has the same rank among all m as among the listed, so its q is exact.
func BenjaminiHochbergOf(ps []float64, m int) []float64 {
	q := BenjaminiHochberg(ps)
	scale := float64(max(m, len(ps))) / float64(max(len(ps), 1))
	for i := range q {
		q[i] = math.Min(1, q[i]*scale)
	}
	return q
}

// SkillProcedures compares, for every skill loaded in at least two sessions,
// the kept patterns' presence in that skill's sessions against sessions of
// the same client and similar length. It reuses the patterns the main search
// kept, so it adds no search.
func SkillProcedures(corpus []trace.NormSession, seqs [][]int, ps []model.Pattern, names []string, idf []float64, o model.Options) []model.SkillReport {
	skillSessions := map[string][]int{}
	for i, s := range corpus {
		for sk := range s.Skills {
			skillSessions[sk] = append(skillSessions[sk], i)
		}
	}
	// patternsOf[s] lists the patterns session s contains.
	patternsOf := make([][]int, len(corpus))
	for pi, p := range ps {
		for _, s := range p.Sessions {
			patternsOf[s] = append(patternsOf[s], pi)
		}
	}
	// Sessions that load a skill differ from the rest in client and in
	// length (automation runs are long), and a long session contains more of
	// everything. So the comparison is stratified: sessions are grouped by
	// client and by length (doubling bins) within that client, and the pattern's count
	// in the skill's sessions is compared with what its rate in each stratum
	// predicts (Cochran-Mantel-Haenszel, one-sided, continuity-corrected).
	stratum := make([]int, len(corpus))
	{
		byClient := map[string][]int{}
		for i, s := range corpus {
			byClient[s.Client] = append(byClient[s.Client], i)
		}
		clients := make([]string, 0, len(byClient))
		for c := range byClient {
			clients = append(clients, c)
		}
		sort.Strings(clients)
		// Length is binned by doubling (1, 2-3, 4-7, 8-15, ...): coarse enough
		// that a skill's own few extra steps do not put its sessions in a
		// stratum of their own, fine enough to separate a short manual
		// session from a long automation run.
		for ci, c := range clients {
			for _, i := range byClient[c] {
				stratum[i] = ci*64 + bits.Len(uint(len(seqs[i])))
			}
		}
	}
	nStratum := map[int]int{}
	for _, st := range stratum {
		nStratum[st]++
	}
	// patternStrata[pi][stratum] is how many of pattern pi's sessions fall in it.
	patternStrata := make([]map[int]int, len(ps))
	for pi, p := range ps {
		m := map[int]int{}
		for _, s := range p.Sessions {
			m[stratum[s]]++
		}
		patternStrata[pi] = m
	}

	type test struct {
		skill   string
		pattern int
		a, c    int
		p       float64
	}
	var tests []test
	n := len(corpus)
	skills := make([]string, 0, len(skillSessions))
	for sk, ss := range skillSessions {
		if len(ss) >= 2 {
			skills = append(skills, sk)
		}
	}
	sort.Strings(skills)
	for _, sk := range skills {
		ss := skillSessions[sk]
		skillStrata := map[int]int{}
		for _, s := range ss {
			skillStrata[stratum[s]]++
		}
		inSkill := map[int]int{}
		for _, s := range ss {
			for _, pi := range patternsOf[s] {
				inSkill[pi]++
			}
		}
		for pi, a := range inSkill {
			if a < 2 {
				continue
			}
			var e, v float64
			for st, ns := range skillStrata {
				N := float64(nStratum[st])
				K := float64(patternStrata[pi][st])
				x := float64(ns)
				e += x * K / N
				if N > 1 {
					v += x * K * (N - K) * (N - x) / (N * N * (N - 1))
				}
			}
			p := 1.0
			if v > 0 {
				p = 0.5 * math.Erfc(((float64(a)-e-0.5)/math.Sqrt(v))/math.Sqrt2)
			} else if float64(a) > e {
				p = 0
			}
			k := len(ps[pi].Sessions)
			tests = append(tests, test{sk, pi, a, k - a, p})
		}
	}
	pv := make([]float64, len(tests))
	for i, t := range tests {
		pv[i] = t.p
	}
	q := BenjaminiHochberg(pv)

	// Significant pairs are ranked by the cheap numbers first (sessions of
	// the skill covered, then length, then lift), and only the best are fully
	// described, until each skill has perSkill procedures that fix something.
	// Every significant pair still counted toward the FDR correction above.
	sig := map[string][]test{}
	for i, t := range tests {
		if q[i] <= o.Alpha {
			t.p = q[i]
			sig[t.skill] = append(sig[t.skill], t)
		}
	}
	out := make([]model.SkillReport, len(skills))
	util.ParallelFor(len(skills), func(si int) {
		sk := skills[si]
		ns := len(skillSessions[sk])
		ts := sig[sk]
		sort.SliceStable(ts, func(a, b int) bool {
			if ts[a].a != ts[b].a {
				return ts[a].a > ts[b].a
			}
			la, lb := len(ps[ts[a].pattern].Items), len(ps[ts[b].pattern].Items)
			if la != lb {
				return la > lb
			}
			return float64(ts[a].a)/float64(ts[a].c+1) > float64(ts[b].a)/float64(ts[b].c+1)
		})
		rep := model.SkillReport{Skill: sk, Sessions: ns, Significant: len(ts)}
		for _, t := range ts {
			if len(rep.Procedures) >= o.PerSkill {
				break
			}
			c := model.Describe(ps[t.pattern], corpus, seqs, names, idf, o.Window)
			if c.Specificity == 0 {
				continue
			}
			lift := 0.0
			if outRate := float64(t.c) / float64(n-ns); outRate > 0 {
				lift = (float64(t.a) / float64(ns)) / outRate
			}
			c.Qualified = true
			c.SessionSet = nil
			rep.Procedures = append(rep.Procedures, model.SkillProcedure{Candidate: c, InSkill: t.a, Coverage: float64(t.a) / float64(ns), Outside: t.c, Lift: lift, OnlyInSkill: t.c == 0, EnrichmentQ: t.p})
		}
		out[si] = rep
	})
	kept := out[:0]
	for _, r := range out {
		if len(r.Procedures) > 0 {
			kept = append(kept, r)
		}
	}
	sort.SliceStable(kept, func(a, b int) bool { return kept[a].Sessions > kept[b].Sessions })
	return kept
}

// HypergeomUpper is the one-sided Fisher exact p-value: the chance that a
// random draw of n sessions out of total holds a or more of the k sessions
// containing the pattern, i.e. P(X >= a) for X ~ Hypergeometric(total, k, n).
func HypergeomUpper(a, k, n, total int) float64 {
	hi := min(k, n)
	if a > hi {
		return 0
	}
	lchoose := func(x, y int) float64 { return Lgamma(float64(x)+1) - Lgamma(float64(y)+1) - Lgamma(float64(x-y)+1) }
	base := lchoose(total, n)
	sum := 0.0
	for i := a; i <= hi; i++ {
		if n-i > total-k {
			continue
		}
		t := math.Exp(lchoose(k, i) + lchoose(total-k, n-i) - base)
		sum += t
		if i > a && t < sum*1e-17 {
			break
		}
	}
	return math.Min(1, sum)
}

// Outcome evidence values.
const (
	InputCaller      = "caller"
	InputPriorOutput = "prior_output"
	InputConstant    = "constant"
	InputComposed    = "composed"
	InputUnresolved  = "unresolved"
)

// legacyStates fills the dimensions from the single decision the rules made
// before they were separated. It reproduces those claims unchanged:
// needs_authoring was a claim of usefulness.
func RoutineLegacyStates(rt *model.Routine, d *model.Draft) {
	rt.SourceRole = map[string]string{"user": model.RoleUser, "automated": model.RoleScheduled, "scheduled": model.RoleScheduled, "bookkeeping": model.RoleInfrastructure, "harness": model.RoleHarness}[rt.Kind]
	if rt.SourceRole == "" {
		rt.SourceRole = model.RoleUnknown
	}
	switch rt.Decision {
	case "primitive":
		rt.Suitability, rt.DraftStatus = model.SuitUseful, model.DraftComplete
	case "needs_authoring":
		rt.Suitability, rt.DraftStatus = model.SuitUseful, model.DraftNeedsAuthor
	default:
		rt.DraftStatus = model.DraftNotAttempted
		rt.Suitability = model.SuitInsufficient
		if rt.Failed == model.CheckReplays {
			rt.Suitability = model.SuitInvestigation
		}
	}
	if d != nil && len(d.Blocked) > 0 {
		rt.DraftStatus = model.DraftBlocked
	}
	switch {
	case rt.Runs > 0 && rt.FailedRuns == rt.Runs:
		rt.OutcomeEvidence = model.OutcomeEvFailed
	case rt.Runs > 0 && rt.UnknownRuns == 0:
		rt.OutcomeEvidence = model.OutcomeToolOK
	default:
		rt.OutcomeEvidence = model.OutcomeEvUnknown
	}
	rt.Value = model.ValueUnmeasured
	if rt.Measured > 0 {
		rt.Value = model.ValueEstimated
	}
	rt.Family = rt.ID
	rt.Contract.Goal, rt.Contract.Effect, rt.Contract.Output = model.GoalUnknown, model.EffectUnknown, "unknown"
}
