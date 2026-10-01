package model

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/redact"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

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
