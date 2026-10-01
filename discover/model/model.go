// Package model holds the data types a discover run produces: the report, its candidates,
// routines, contracts, drafts and span groups, with their enums.
package model

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/redact"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
	"gitlab.com/telara-labs/tap-runtime/discover/util"
)

// BriefStatus is every brief's status. Handing a task to an agent says
// nothing about whether it is a useful procedure; the agent establishes that.
const BriefStatus = "unassessed_proposal"

// SpanComposition is a structural retrieval bucket. It describes observed
// operations and result dependencies, not a safe or useful primitive contract.
// Repetition is evidence about this trace; the key omits its observed count.
type SpanComposition struct {
	Key        string       `json:"key"`
	Actions    []string     `json:"actions"`
	Edges      []string     `json:"edges,omitempty"`
	Repetition []SpanRepeat `json:"repetition,omitempty"`
}

type SpanRepeat struct {
	Action string `json:"action"`
	Count  int    `json:"count"`
	Kind   string `json:"kind"` // for_each, repeated, or dependent_repeat
}

type SpanCompositionGroup struct {
	Key       string       `json:"key"`
	Proposals int          `json:"proposals"`
	Sessions  int          `json:"sessions"`
	Example   SpanProposal `json:"example"`
	Members   []string     `json:"members"`
}

// Options control a run. Window, MinSupport, MaxLen and MaxPatterns bound the
// search (compute), Permutations sets how well the null is estimated, Alpha
// is the false discovery rate. None of them is a quality threshold on a
// candidate: significance and stability are measured and reported.
type Options struct {
	Readers      []trace.Reader
	Since        time.Time
	Window       int
	MinSupport   int
	MaxLen       int
	MaxPatterns  int
	Permutations int
	Alpha        float64
	Seed         int64
	Now          func() time.Time
	// PerSkill is how many procedures are described per skill in the
	// report. It bounds the report's size; it does not decide significance.
	PerSkill int
	// Patterns also runs the pattern search (fragments, families, skill
	// comparison): slower, and not needed for the request-level result.
	Patterns bool
	// Spans computes unassessed bounded-call proposals from task context and
	// result provenance. It is opt-in while its review cost is measured.
	Spans bool
	// Progress, when set, receives one line per phase.
	Progress io.Writer
}

type ClientStats struct {
	Client            string    `json:"client"`
	Sessions          int       `json:"sessions"`
	Calls             int       `json:"calls"`
	Steps             int       `json:"steps"`
	DuplicateSessions int       `json:"duplicate_sessions"`
	Earliest          time.Time `json:"earliest,omitempty"`
	Latest            time.Time `json:"latest,omitempty"`
	Error             string    `json:"error,omitempty"`
}

type StepTemplate struct {
	Label     string       `json:"label"`
	Template  string       `json:"template"`
	Params    []trace.Slot `json:"params,omitempty"`
	Stability float64      `json:"stability"`
	// Weight is the step's inverse document frequency over sessions.
	Weight float64 `json:"weight"`
	// Fixed is true when the step pins something beyond the tool: a shell
	// subcommand, or an argument whose value is the same every time.
	Fixed bool `json:"fixed"`
}

type Candidate struct {
	Steps         []StepTemplate `json:"steps"`
	Sessions      int            `json:"sessions"`
	ByClient      map[string]int `json:"by_client"`
	NullMean      float64        `json:"null_mean,omitempty"`
	P             float64        `json:"p,omitempty"`
	Q             float64        `json:"q,omitempty"`
	OrderQ        float64        `json:"order_q,omitempty"`
	NecessityQ    float64        `json:"necessity_q,omitempty"`
	Qualified     bool           `json:"qualified,omitempty"`
	Ordered       bool           `json:"ordered,omitempty"`
	Stability     float64        `json:"stability,omitempty"`
	Specificity   float64        `json:"specificity,omitempty"`
	CallsSaved    int            `json:"calls_saved,omitempty"`
	Score         float64        `json:"score,omitempty"`
	Weeks         int            `json:"weeks"`
	FirstSeen     time.Time      `json:"first_seen"`
	LastSeen      time.Time      `json:"last_seen"`
	MedianGapDays float64        `json:"median_gap_days"`
	Examples      []string       `json:"examples"`
	// Token cost, from the usage the clients recorded. PerRun is what one
	// occurrence cost through the agent (median over measured occurrences),
	// SavedPerRun what a primitive would save (all of it but one turn, the
	// one that invokes the primitive), SavedTotal that saving summed over
	// every measured occurrence. Measured counts those occurrences; Cursor
	// records no usage, so its occurrences are not measured.
	PerRun      trace.Usage `json:"tokens_per_run"`
	SavedPerRun trace.Usage `json:"tokens_saved_per_run"`
	SavedTotal  trace.Usage `json:"tokens_saved_total"`
	Measured    int         `json:"measured_runs"`
	// Family is the rank (index) of the qualified candidate this one is a
	// variant of; a representative is its own family.
	Family     int          `json:"family,omitempty"`
	SessionSet map[int]bool `json:"-"`
	Items      []int        `json:"-"`
}

type SkillMatch struct {
	Pattern   string  `json:"pattern"`
	Qualified bool    `json:"qualified,omitempty"`
	Precision float64 `json:"precision"`
	Recall    float64 `json:"recall"`
	F1        float64 `json:"f1"`
}

type SkillRecall struct {
	Skill         string      `json:"skill"`
	Sessions      int         `json:"sessions"`
	BestQualified *SkillMatch `json:"best_qualified,omitempty"`
	BestTested    *SkillMatch `json:"best_tested,omitempty"`
}

type Report struct {
	RulesVersion   string        `json:"rules_version"`
	GeneratedAt    time.Time     `json:"generated_at"`
	Options        OptionsOut    `json:"options"`
	Clients        []ClientStats `json:"clients"`
	Labels         int           `json:"labels"`
	Examined       int           `json:"examined"`
	Mined          int           `json:"kept"`
	Truncated      bool          `json:"truncated"`
	MinSupportUsed int           `json:"min_support"`
	Tested         int           `json:"tested"`
	Qualified      int           `json:"qualified,omitempty"`
	Families       int           `json:"families"`
	Candidates     []Candidate   `json:"candidates"`
	Recall         []SkillRecall `json:"recall"`
	Skills         []SkillReport `json:"skills"`
	// Funnel and Routines are the request-level result: what was read, how
	// many requests recurred as routines, and which passed every check.
	Funnel   Funnel    `json:"funnel"`
	Routines []Routine `json:"routines"`
	// Opportunities are the requests the selection pass (select.go)
	// recommends from the whole history, whether or not they recurred.
	// Each is an unassessed proposal for an authoring agent.
	Opportunities []Opportunity `json:"opportunities,omitempty"`
	// OpportunityGroups are the opportunities grouped by contract, ranked.
	OpportunityGroups []OpportunityGroup `json:"opportunity_groups,omitempty"`
	// SpanProposals are bounded pieces of work, including pieces of long
	// requests and single calls. They are retrieval candidates, never a
	// useful-procedure recommendation or a Gate D true positive.
	SpanProposals     []SpanProposal         `json:"span_proposals,omitempty"`
	SpanGroups        []SpanGroup            `json:"span_groups,omitempty"`
	CompositionGroups []SpanCompositionGroup `json:"composition_groups,omitempty"`
	// LogicCandidates are recurring parameterized execution shapes. They are
	// the authoring queue; task-completeness checks below are diagnostics only.
	LogicCandidates []LogicCandidate       `json:"logic_candidates,omitempty"`
	LogicFunnels    []LogicFunnel          `json:"logic_funnels,omitempty"`
	ReviewSpans     []SpanProposal         `json:"review_spans,omitempty"`
	ReviewGroups    []SpanCompositionGroup `json:"review_groups,omitempty"`
	ComponentSpans  []SpanProposal         `json:"component_spans,omitempty"`
	ComponentGroups []SpanCompositionGroup `json:"component_groups,omitempty"`

	// Kept in memory so a candidate can be drafted from its real
	// occurrences; never written out.
	Corpus []trace.NormSession `json:"-"`
	Seqs   [][]int             `json:"-"`
	Names  []string            `json:"-"`
	Window int                 `json:"-"`
}

type OptionsOut struct {
	Since        time.Time `json:"since,omitempty"`
	Window       int       `json:"window"`
	MinSupport   int       `json:"min_support"`
	MaxLen       int       `json:"max_len"`
	MaxPatterns  int       `json:"max_patterns"`
	Permutations int       `json:"permutations"`
	Alpha        float64   `json:"alpha"`
	Seed         int64     `json:"seed"`
	Spans        bool      `json:"spans"`
}

// Describe builds the report entry for a pattern: a template per step from
// the occurrences whose skeleton is the most common one, the slots that vary
// as typed parameters, and when the work happened.
func Describe(p Pattern, corpus []trace.NormSession, seqs [][]int, names []string, idf []float64, window int) Candidate {
	c := Candidate{ByClient: map[string]int{}, Sessions: len(p.Sessions), SessionSet: map[int]bool{}, Items: p.Items}
	occ := make([][]trace.Step, 0, len(p.Sessions))
	var times []time.Time
	var runs [][2]trace.Usage
	weeks := map[string]bool{}
	for _, s := range p.Sessions {
		c.SessionSet[s] = true
		ns := corpus[s]
		c.ByClient[ns.Client]++
		idx := trace.MatchAt(seqs[s], p.Items, window)
		steps := make([]trace.Step, len(idx))
		for i, j := range idx {
			steps[i] = ns.Steps[j]
		}
		occ = append(occ, steps)
		if run, saved, ok := RunCost(steps); ok {
			c.Measured++
			c.SavedTotal = c.SavedTotal.Add(saved)
			runs = append(runs, [2]trace.Usage{run, saved})
		}
		t := ns.Start
		if t.IsZero() && len(steps) > 0 {
			t = steps[0].Time
		}
		if !t.IsZero() {
			times = append(times, t)
			y, w := t.ISOWeek()
			weeks[fmt.Sprintf("%d-%02d", y, w)] = true
		}
		if len(c.Examples) < 3 {
			c.Examples = append(c.Examples, ns.Client+"/"+ns.ID)
		}
	}
	var stab, weighted float64
	specific := 0
	for i := range p.Items {
		st := TemplateOf(names[p.Items[i]], occ, i)
		st.Weight = idf[p.Items[i]]
		stab += st.Stability
		if st.Fixed {
			specific++
			weighted += st.Weight * st.Stability
		}
		c.Steps = append(c.Steps, st)
	}
	c.Stability = stab / float64(len(p.Items))
	if len(runs) > 0 {
		sort.Slice(runs, func(a, b int) bool { return runs[a][0].Total() < runs[b][0].Total() })
		c.PerRun, c.SavedPerRun = runs[len(runs)/2][0], runs[len(runs)/2][1]
	}
	c.Specificity = float64(specific) / float64(len(p.Items))
	c.CallsSaved = c.Sessions * len(p.Items)
	// Score: sessions times the weight of the steps that fix something. A
	// step whose every argument varies (read <path>) carries the judgment
	// of which argument, not a procedure, so it adds nothing.
	c.Score = float64(c.Sessions) * weighted
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
	return c
}

func TemplateOf(label string, occ [][]trace.Step, i int) StepTemplate {
	skel := map[string]int{}
	for _, o := range occ {
		skel[o[i].Skeleton]++
	}
	modal, n := "", -1
	for k, v := range skel {
		if v > n || (v == n && k < modal) {
			modal, n = k, v
		}
	}
	st := StepTemplate{Label: label, Stability: float64(n) / float64(len(occ))}
	// A shell subcommand (git commit) or an MCP tool (telara_task_create)
	// names one specific action; that is the fixed part even when every
	// argument varies.
	st.Fixed = (strings.HasPrefix(label, "sh:") && strings.Contains(label, " ")) || strings.HasPrefix(label, "mcp:")
	// Slots are compared by key among occurrences with the modal skeleton.
	vals := map[string]map[string]bool{}
	types := map[string]map[string]int{}
	var keys []string
	for _, o := range occ {
		if o[i].Skeleton != modal {
			continue
		}
		for _, sl := range o[i].Slots {
			if sl.Sub {
				continue
			}
			if vals[sl.Key] == nil {
				vals[sl.Key], types[sl.Key] = map[string]bool{}, map[string]int{}
				keys = append(keys, sl.Key)
			}
			vals[sl.Key][sl.Value] = true
			types[sl.Key][sl.Type]++
		}
	}
	// Keys are shown in the order they first appear (the command's own
	// order); tool arguments were already sorted by name.
	parts := []string{label}
	for _, k := range keys {
		tp, best := "", -1
		for t, v := range types[k] {
			if v > best || (v == best && t < tp) {
				tp, best = t, v
			}
		}
		if len(vals[k]) == 1 && n > 1 {
			for v := range vals[k] {
				parts = append(parts, FmtSlot(label, k, v))
			}
			// A constant flag (tail -n) or numeric option (offset=0) says
			// how a tool is used, not what it acts on; only a constant
			// word, path, text, id or URL fixes the work.
			if tp != trace.SlotFlag && tp != trace.SlotNumber {
				st.Fixed = true
			}
			continue
		}
		parts = append(parts, FmtSlot(label, k, "<"+tp+">"))
		st.Params = append(st.Params, trace.Slot{Key: k, Type: tp})
	}
	// A run of the same parameter type (git add <path> <path> <path>) is one
	// variadic parameter.
	var collapsed []string
	for _, pt := range parts {
		if n := len(collapsed); n > 0 && strings.HasPrefix(pt, "<") && strings.TrimSuffix(collapsed[n-1], "…") == pt {
			collapsed[n-1] = pt + "…"
			continue
		}
		collapsed = append(collapsed, pt)
	}
	st.Template = redact.Redact(strings.Join(collapsed, " "))
	return st
}

func FmtSlot(label, key, v string) string {
	if strings.HasPrefix(label, "sh:") {
		if len(v) > 60 {
			v = trace.TruncateUTF8(v, 60) + "…"
		}
		return v
	}
	if len(v) > 40 {
		v = trace.TruncateUTF8(v, 40) + "…"
	}
	return key + "=" + v
}

func LabelsOf(c Candidate) string {
	ls := make([]string, len(c.Steps))
	for i, s := range c.Steps {
		ls[i] = s.Label
	}
	return strings.Join(ls, " → ")
}

func JaccardInts(a, b map[int]bool) float64 {
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}

// LabelIndex maps each label to the sessions containing it.
func LabelIndex(seqs [][]int) map[int]map[int]bool {
	index := map[int]map[int]bool{}
	for s, seq := range seqs {
		for _, x := range seq {
			if index[x] == nil {
				index[x] = map[int]bool{}
			}
			index[x][s] = true
		}
	}
	return index
}

// SupportIn counts sessions containing items (gapped, within window), looking
// only at sessions that hold every one of its labels.
func SupportIn(seqs [][]int, index map[int]map[int]bool, items []int, window int) int {
	var smallest map[int]bool
	for _, x := range items {
		if smallest == nil || len(index[x]) < len(smallest) {
			smallest = index[x]
		}
	}
	n := 0
next:
	for s := range smallest {
		for _, x := range items {
			if !index[x][s] {
				continue next
			}
		}
		if trace.MatchAt(seqs[s], items, window) != nil {
			n++
		}
	}
	return n
}

// RunCost is what one occurrence cost through the agent (the steps' shares
// of their turns) and what a primitive would save: everything except one
// turn's worth, the turn that calls the primitive. ok is false unless every
// step was measured.
func RunCost(steps []trace.Step) (run, saved trace.Usage, ok bool) {
	// Only steps a primitive can replay count: an edit whose content was
	// decided per run, or the agent's own bookkeeping, stays with the agent,
	// and so does what it costs.
	turns := map[int]bool{}
	merged := 0
	for _, st := range steps {
		if !st.Measured {
			return trace.Usage{}, trace.Usage{}, false
		}
		if !trace.Replayable(st.Label) {
			continue
		}
		run = run.Add(st.Tokens)
		turns[st.Turn] = true
		merged += max(st.Turns, 1) - 1
	}
	if len(turns) == 0 {
		return trace.Usage{}, trace.Usage{}, false
	}
	one := run.Scale(1 / float64(len(turns)+merged))
	return run, trace.Usage{Fresh: run.Fresh - one.Fresh, Cached: run.Cached - one.Cached, Output: run.Output - one.Output}, true
}

// DraftStep is one step as the review shows it.
type DraftStep struct {
	N      int    `json:"n"`
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	Line   string `json:"line"`
	Effect string `json:"effect,omitempty"`
	Note   string `json:"note,omitempty"`
}

// DraftInput is one argument of the drafted primitive: $Position in main.sh.
type DraftInput struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// Raw inputs are passed as JSON (a number, a boolean), not as a string.
	Raw bool `json:"raw,omitempty"`
	// Example is one recorded value, redacted, for the person reviewing on
	// this machine. It is never written into a generated file.
	Example string `json:"-"`
	// Sensitive inputs are credentials: the caller supplies them from its
	// own configuration and no recorded value is kept.
	Sensitive bool `json:"sensitive,omitempty"`
	// DerivedFrom is the step (1-based) whose output held this value in most
	// runs: the program must take it from there, not from the caller.
	DerivedFrom int `json:"derived_from,omitempty"`
	// Extract, when set, is how the program takes this value from step
	// DerivedFrom's output: a grep pattern for the text before it and the
	// value itself. The caller does not supply it and it has no Position.
	Extract string `json:"extract,omitempty"`
	// Binding says how Extract reads the value: "json_path" (a jq path into
	// a JSON result) or "text_anchor" (the text before it).
	Binding string `json:"binding,omitempty"`
	// List marks a caller-given list the program loops over (a JSON array).
	List bool `json:"list,omitempty"`
	// Position is the argument number the caller passes it as ($1, $2...);
	// 0 for an extracted value.
	Position int    `json:"position"`
	From     string `json:"from"`
	// strip is what Extract's match starts with, removed to leave the value.
	Strip string `json:"-"`
	// pos is the template position of the step that takes it.
	Pos int `json:"-"`
}

// Draft is a drafted package and what the review needs to show it.
type Draft struct {
	Name      string            `json:"name"`
	Publisher string            `json:"publisher"`
	Inputs    []DraftInput      `json:"inputs"`
	Steps     []DraftStep       `json:"steps"`
	Files     map[string][]byte `json:"-"`
	// Blocked lists every place a generated file still looks like it holds a
	// credential. While it is non-empty the draft cannot be saved, packaged
	// or published.
	Blocked []string `json:"blocked,omitempty"`
	// Problems is what manifest.PublishProblems reports; empty means the
	// package passes the same checks the registry runs before accepting it.
	Problems   []string `json:"problems"`
	HumanSteps int      `json:"human_steps"`
	// Derived counts inputs an earlier step's output supplied in the
	// recorded runs that the draft could not extract itself: they need
	// authoring. Extracted counts those it takes from that output.
	Derived   int `json:"derived"`
	Extracted int `json:"extracted"`
	// FixedSteps counts steps that pin something: a subcommand, an MCP tool,
	// a constant argument or browser object. None means every command and
	// argument varied: exploration, not a procedure.
	FixedSteps int `json:"fixed_steps"`
	// FixedShare is how much of the routine is fixed: each replayed step's
	// tool or command plus every argument that never changed, over that plus
	// the inputs. A procedure is mostly fixed; an investigation, where the
	// agent chose most values as it went, is mostly inputs.
	FixedShare float64          `json:"fixed_share"`
	Values     []map[int]string `json:"-"`
	// listLoop names the steps drafted as a loop over a caller list.
	ListLoop map[string]bool `json:"-"`
	// humanPos are template positions drafted as human steps.
	HumanPos map[int]bool `json:"-"`
	// priorLoop names steps that loop over a list an earlier result held.
	PriorLoop map[string]bool `json:"-"`
	// firstRun is the first drafted run's steps, for evidence lookups.
	FirstRun []trace.Step `json:"-"`
	PosStep  map[int]int  `json:"-"`
	// RuntimeUnsupported names steps the TAP guest runtime would not run
	// as recorded (a built-in that ignores file arguments, a file read the
	// manifest cannot declare). They block a structurally complete draft.
	RuntimeUnsupported []string `json:"runtime_unsupported,omitempty"`
}

// DraftOptions are the review's answers.
type DraftOptions struct {
	Publisher string
	// ReadOnly lists step numbers (1-based) the user confirmed change nothing.
	ReadOnly map[int]bool
	// loops are steps each run made once per item of a list the request
	// gave: the varying argument and, per occurrence, its values in order.
	Loops map[string]LoopSpec `json:"-"`
}

type LoopSpec struct {
	Key    string     `json:"-"`
	Values [][]string `json:"-"` // per occurrence (index into occ)
	// source is where the list came from: "caller" (the request gave it)
	// or "prior_output" (an earlier step's result held every item).
	Source string `json:"-"`
}

// ErrBlocked is returned when a draft still holds something credential-shaped.
var ErrBlocked = errors.New("the draft still contains credential-shaped values; it cannot be saved or published until they are removed")

// LogicCandidate is a recurring, parameterized execution shape that an agent
// can author as a primitive. It is evidence of repeated logic, not evidence
// that replaying the recorded calls verbatim is safe or useful.
type LogicCandidate struct {
	ID         string       `json:"id"`
	Key        string       `json:"key"`
	Actions    []string     `json:"actions"`
	Edges      []string     `json:"edges,omitempty"`
	Parameters []string     `json:"parameters,omitempty"`
	Proposals  int          `json:"proposals"`
	Executions int          `json:"executions"`
	Sessions   int          `json:"sessions"`
	Evidence   []string     `json:"evidence"`
	Cautions   []string     `json:"cautions,omitempty"`
	Example    SpanProposal `json:"example"`
	Members    []string     `json:"members"`
}

// LogicFunnel is an observed result-flow root with its distinct follow-up
// operations. Branches may occur in different requests and in different
// orders; they are not a claim that all branches belong in one package.
type LogicFunnel struct {
	ID           string              `json:"id"`
	Root         string              `json:"root"`
	Sessions     int                 `json:"sessions"`
	Branches     []LogicFunnelBranch `json:"branches"`
	CandidateIDs []string            `json:"candidate_ids"`
	Members      []string            `json:"members"`
}

type LogicFunnelBranch struct {
	Action       string   `json:"action"`
	Slots        []string `json:"slots"`
	Sessions     int      `json:"sessions"`
	ForEach      bool     `json:"for_each,omitempty"`
	CandidateIDs []string `json:"candidate_ids"`
}

// Pattern is an ordered list of step labels (as ids) and the sessions that
// contain it, each step within window steps of the one before.
type Pattern struct {
	Items    []int `json:"-"`
	Sessions []int `json:"-"` // indexes into the corpus, ascending
}

func (p Pattern) Key() string {
	var b strings.Builder
	for i, x := range p.Items {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(util.Itoa(x))
	}
	return b.String()
}

// Check names, in the order they run.
const (
	CheckReplays = "replays"
	CheckSameWay = "same way"
	CheckWorth   = "worth it"
)

// Funnel is what the request-level run found at each stage.
type Funnel struct {
	Sessions          int            `json:"sessions"`
	Calls             int            `json:"calls"`
	Requests          int            `json:"requests"`
	RequestsWithSteps int            `json:"requests_with_steps"`
	Groups            int            `json:"groups"`
	Routines          int            `json:"routines"`
	Removed           map[string]int `json:"removed"`
	Primitives        int            `json:"primitives"`
	// NeedsAuthoring counts routines that recur and replay but need a step
	// or a value's source written by hand before they can run.
	NeedsAuthoring int `json:"needs_authoring"`
	// Merged counts routines folded into another (same kind, same steps).
	Merged int `json:"merged"`
	// ByKind splits the primitives by who the work is for.
	ByKind map[string]int `json:"primitives_by_kind"`
	// Savings says how the token figures were obtained.
	Savings string `json:"savings"`
	// Per-dimension counts over routines not merged into another:
	// source role -> suitability, suitability -> draft status, and outcome,
	// validation and value. Each sums to the same total.
	ByRole       map[string]map[string]int `json:"by_role"`
	ByDraft      map[string]map[string]int `json:"by_suitability_draft"`
	ByOutcome    map[string]int            `json:"by_outcome"`
	ByValidation map[string]int            `json:"by_validation"`
	ByValue      map[string]int            `json:"by_value"`
}

// Routine is one group of requests that recurred, with its template and the
// verdict of the checks.
type Routine struct {
	Candidate
	Requests    int     `json:"requests"`
	Consistency float64 `json:"consistency"`
	// Coverage is how much of its requests the routine is: its steps over
	// all the replayable steps each request ran (median). A procedure is
	// most of its request; recurring calls inside a long investigation are
	// a small part of it.
	Coverage float64 `json:"coverage"`
	// Inputs lists each input and whether its values were in the request.
	Inputs []RoutineInput `json:"inputs"`
	// CoveredBy names the skill most of its requests loaded, if any.
	CoveredBy string `json:"covered_by,omitempty"`
	// Decision is "primitive" (ready to save), "needs_authoring" (it
	// recurs and replays, but a step's content or a value's source must be
	// written by hand) or "removed" (Failed names the check, Why says how).
	Decision string `json:"decision"`
	// ID is stable across runs for the same kind and step sequence.
	ID string `json:"id"`
	// Kind is who the work is for: "user" (asked for by a person),
	// "automated" (a program sent the same prompt each time), "scheduled"
	// (a Codex automation) or "bookkeeping" (Telara's own recording and
	// tool-discovery calls, which instructions make every agent do).
	Kind string `json:"kind"`
	// Statistics is "not_run": recurrence here is 3+ requests in 2+
	// sessions, not a significance test.
	Statistics string `json:"statistics"`
	// Validation is "not_run": discovery never executes a draft. A primitive
	// is validated by running it on held-out inputs with independent
	// checks; recurrence and passing the publish checks are not that.
	Validation string `json:"validation"`
	// MergedInto is the id of the routine this one duplicated (same kind,
	// same set of steps); a merged routine is not counted again.
	MergedInto string `json:"merged_into,omitempty"`
	// Loops lists steps most runs made several times with different values.
	Loops  []string `json:"loops,omitempty"`
	Failed string   `json:"failed,omitempty"`
	Why    string   `json:"why,omitempty"`
	// Runs counts the requests that ran its sequence; FailedRuns those where
	// a step failed (not evidence); UnknownRuns those whose client recorded
	// no result.
	Runs        int `json:"runs"`
	FailedRuns  int `json:"failed_runs"`
	UnknownRuns int `json:"unknown_runs"`
	// Example is one request's text, shortened.
	Example string `json:"example"`
	// The separate dimensions (see states.go).
	SourceRole      string   `json:"source_role"`
	Suitability     string   `json:"suitability"`
	DraftStatus     string   `json:"draft_status"`
	Blockers        []string `json:"blockers,omitempty"`
	OutcomeEvidence string   `json:"outcome_evidence"`
	Value           string   `json:"value"`
	// Reasons are stable codes for each dimension's value.
	Reasons []string `json:"reasons,omitempty"`
	// Family is the task family (goal and outcome); ID is this procedure.
	Family   string   `json:"family"`
	Contract Contract `json:"contract"`
	// Baseline names existing automation of the same work, if any.
	Baseline string `json:"baseline,omitempty"`
	// Parent is the routine this one is a bounded part of: a procedure
	// found inside requests whose whole was not one.
	Parent string `json:"parent,omitempty"`
	// Sources are the requests the routine was found in. They point into
	// this machine's history and are for local review only.
	Sources   []SourceRef         `json:"sources,omitempty"`
	Draft     *Draft              `json:"-"`
	Occ       [][]trace.Step      `json:"-"`
	LoopSpecs map[string]LoopSpec `json:"-"`
}

// SourceRef names one request in a client's session history. Ran is true
// when that request ran the routine's steps in its order and succeeded.
type SourceRef struct {
	Client  string `json:"client"`
	Session string `json:"session"`
	Request int    `json:"request"`
	Ran     bool   `json:"ran"`
}

type RoutineInput struct {
	Name      string  `json:"name"`
	Type      string  `json:"type"`
	Explained float64 `json:"explained"` // share of runs whose request contained the value
}

// Opportunity is the selection pass's judgment of one request.
type Opportunity struct {
	ID          string   `json:"id"`
	Client      string   `json:"client"`
	Session     string   `json:"session"`
	Request     int      `json:"request"`
	Recommended bool     `json:"recommended"`
	Route       string   `json:"route,omitempty"`
	Reasons     []string `json:"reasons"`
	// Task is the reference `tap discover brief --task` takes.
	Task string `json:"task"`
	// Contract is what the procedure is, as far as the evidence shows:
	// the grouping key. Start is when the request began.
	Contract string    `json:"contract,omitempty"`
	Start    time.Time `json:"start,omitempty"`
}

// Selection routes.
const (
	RouteStatedTemplate = "stated_template"
	RouteRerunCheck     = "rerun_check"
	RouteParamLoop      = "parametric_loop"
	RouteNamedObject    = "named_object"
)

// OpportunityGroup is every recommended request with the same contract.
type OpportunityGroup struct {
	Contract string    `json:"contract"`
	Route    string    `json:"route"`
	Requests int       `json:"requests"`
	Sessions int       `json:"sessions"`
	First    time.Time `json:"first"`
	Last     time.Time `json:"last"`
	// Example is the most recent member: the one to brief.
	Example Opportunity `json:"example"`
	Members []string    `json:"members"`
}

// SkillProcedure is a pattern that sessions loading one skill run far more
// often than sessions that do not: a candidate for what that skill's
// recurring work actually is, step by step.
type SkillProcedure struct {
	Candidate
	// InSkill is how many of the skill's sessions contain the pattern, and
	// Coverage that as a share of them.
	InSkill  int     `json:"in_skill"`
	Coverage float64 `json:"coverage"`
	// Outside is how many other sessions contain it.
	Outside int `json:"outside"`
	// Lift is the pattern's rate in the skill's sessions over its rate
	// elsewhere. It is 0 when OnlyInSkill: the pattern never occurs outside
	// the skill, so the ratio has no finite value (and JSON has no infinity).
	Lift        float64 `json:"lift"`
	OnlyInSkill bool    `json:"only_in_skill"`
	// EnrichmentQ is the Fisher exact test's q-value, FDR-controlled over
	// every skill and pattern pair tested.
	EnrichmentQ float64 `json:"enrichment_q"`
}

// SkillReport lists, for one skill, the procedures its sessions share.
type SkillReport struct {
	Skill    string `json:"skill"`
	Sessions int    `json:"sessions"`
	// Significant is how many patterns were enriched in this skill's
	// sessions; Procedures holds the best of them that fix something.
	Significant int              `json:"significant"`
	Procedures  []SkillProcedure `json:"procedures"`
}

// SpanProposal is a causal slice of a task's recorded calls. It is deliberately
// unassessed: a data-flow or shared-input shape does not prove the task was
// useful, safe to replay, or complete. Calls are one-based within Request and
// may be non-contiguous; a brief includes exactly these calls.
type SpanProposal struct {
	ID             string          `json:"id"`
	Client         string          `json:"client"`
	Session        string          `json:"session"`
	Request        int             `json:"request"`
	Task           string          `json:"task"`
	Status         string          `json:"status"`
	Kind           string          `json:"kind"`
	CodeShape      string          `json:"code_shape,omitempty"`  // normalized inline code, retrieval only
	CodeFamily     string          `json:"code_family,omitempty"` // broad call-order motif, not equivalence
	CodeScope      string          `json:"code_scope,omitempty"`  // direct or embedded shell snippet
	Calls          []int           `json:"calls"`
	CallHashes     []string        `json:"call_hashes"`
	Tools          []string        `json:"tools"`
	Inputs         []SpanInput     `json:"inputs,omitempty"`
	Effect         string          `json:"effect"`
	GoalKey        string          `json:"goal_key"`
	ShapeKey       string          `json:"shape_key"`
	Composition    SpanComposition `json:"composition"`
	Review         SpanTaskReview  `json:"review"`
	ContextRequest int             `json:"context_request,omitempty"`
	EvidenceScore  int             `json:"evidence_score"`
	Start          time.Time       `json:"start,omitempty"`
}

// SpanInput records provenance, not a potentially secret value. FromCall is
// one-based within the request; FromRequest identifies a previous user turn.
type SpanInput struct {
	Key         string `json:"key"`
	Type        string `json:"type"`
	Source      string `json:"source"`
	FromCall    int    `json:"from_call,omitempty"`
	FromRequest int    `json:"from_request,omitempty"`
}

// SpanGroup is a review queue bucket, not a claim that its members implement
// one interchangeable procedure. The key includes goal, ordered call shape,
// input provenance and observed effect; it does not use tool-label overlap.
type SpanGroup struct {
	ShapeKey  string       `json:"shape_key"`
	Proposals int          `json:"proposals"`
	Sessions  int          `json:"sessions"`
	Example   SpanProposal `json:"example"`
	Members   []string     `json:"members"`
}

// Source roles.
const (
	RoleUser           = "user_task"
	RoleScheduled      = "scheduled"
	RoleInfrastructure = "infrastructure"
	RoleHarness        = "harness"
	RoleUnknown        = "unknown"
)

// Suitability values.
const (
	SuitUseful        = "useful_procedure"
	SuitInvestigation = "unbounded_investigation"
	SuitInsufficient  = "insufficient_evidence"
	SuitInvalid       = "invalid_source"
)

// Draft statuses.
const (
	DraftNotAttempted = "not_attempted"
	DraftNeedsAuthor  = "needs_authoring"
	DraftComplete     = "structurally_complete"
	DraftBlocked      = "blocked_export"
)

const OutcomeReported = "reported"

const OutcomeToolOK = "observed_tool_success"

const OutcomeVerified = "independently_verified"

const OutcomeEvFailed = "failed"

const OutcomeEvUnknown = "unknown"

const ValueUnmeasured = "unmeasured"

const ValueEstimated = "estimated"

const ValidationNotRun = "not_run"

const EffectReadOnly = "read_only"

const EffectWrites = "writes"

const EffectUnknown = "unknown"

const GoalStated = "stated_template"

const GoalSelfContained = "self_contained"

const GoalUnknown = "unknown"

// Contract is what the evidence establishes about a routine's task.
type Contract struct {
	// Goal says how the goal is evidenced: the requests share a template
	// (stated_template), the procedure's inputs and output define it on
	// their own (self_contained), or neither (unknown).
	Goal   string          `json:"goal"`
	Inputs []ContractInput `json:"inputs"`
	// Scope lists authority-bearing constants (a kube context, a namespace,
	// a host) that are part of the procedure's identity.
	Scope  []string `json:"scope,omitempty"`
	Effect string   `json:"effect"`
	// Output is "report" (what the steps print) or "state_change".
	Output string `json:"output"`
	// Judgment names steps whose content the agent decided per run.
	Judgment []string `json:"judgment,omitempty"`
	// Boundary is set when all judgment comes after the replayable steps:
	// the procedure runs them and hands back to the agent there.
	Boundary string `json:"boundary,omitempty"`
	// Approvals counts user approvals observed between the steps of the
	// recorded runs; none transfers to a new run.
	Approvals int `json:"approvals,omitempty"`
}

// ContractInput is one input and where its value comes from.
type ContractInput struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Source string `json:"source"`
	// From is the step (1-based) whose output supplied a prior_output value.
	From int `json:"from,omitempty"`
}

// SpanTaskReview is a deterministic admission check for the human review
// queue. Ready means the recorded trace has a task-shaped contract, not that
// it is useful, safe, or a validated primitive.
type SpanTaskReview struct {
	Ready     bool     `json:"ready"`
	Component bool     `json:"component"`
	Source    string   `json:"source"`
	Input     string   `json:"input"`
	Output    string   `json:"output"`
	Stop      string   `json:"stop"`
	Reasons   []string `json:"reasons,omitempty"`
}

// Validation states beyond not_run. Passed names the digest it passed for.
const (
	ValidationPassed = "passed"
	ValidationFailed = "failed"
)
