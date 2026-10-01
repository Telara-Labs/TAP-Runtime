package discover

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/redact"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// Request-level discovery turns recurring work into a short list of
// primitives. The unit is one request: a user message and every call the
// agent made to answer it. Requests are grouped by the replayable steps they
// ran, a group's template is the steps most of its requests share, and a
// group becomes a primitive only if it passes four checks, in order:
//
//  1. whole request   it is the work for a request, not a window cut from one
//  2. replays         every step is a command, tool call, browser call, read
//     or fetch: no edit decided per run, no value computed by
//     the rest of a script
//  3. same way        most requests of the group ran the same sequence of its steps
//  4. worth it        it recurs over more than one week, and where token use
//     was recorded, a primitive saves turns
//
// A skill the requests already load is reported (covered_by), not removed.
//
// The funnel counts what each check removed; nothing is dropped silently.

// Check names, in the order they run.
const (
	CheckReplays = "replays"
	CheckSameWay = "same way"
	CheckWorth   = "worth it"
)

// CheckOrder is the order the checks run in (whole request is by construction).
// "Inputs given" is not among them: a primitive is called by an agent, which
// supplies its inputs, so a value the agent chose (the files to commit, the
// package to test) is a normal input. Whether each input's values appeared
// in the request is still reported per input.
// A skill the requests already loaded is not a reason to remove a routine:
// the skill is the baseline a primitive would be measured against. It is
// reported as CoveredBy.
var CheckOrder = []string{CheckReplays, CheckSameWay, CheckWorth}

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
	Sources []SourceRef `json:"sources,omitempty"`
	draft   *Draft
	occ     [][]trace.Step
	loops   map[string]loopSpec
}

// DraftAs redrafts the routine under a publisher, with the steps the user
// marked read-only.
func (r *Routine) DraftAs(publisher string, readOnly map[int]bool) *Draft {
	if r.occ == nil {
		return r.draft
	}
	return buildDraft(r.Candidate, r.occ, DraftOptions{Publisher: publisher, ReadOnly: readOnly, loops: r.loops})
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

// Draft returns the package drafted for the routine.
func (r *Routine) Draft() *Draft { return r.draft }

type reqInstance struct {
	session int
	request int
	steps   []int // indexes into the session's steps
	labels  map[int]float64
	// keys are labels' keys in order, so sums over them come out the same
	// every run (a float sum in map order can land either side of 0.5).
	keys []int
	text string
}

// requestRoutines runs the request-level pass over a normalized corpus.
func requestRoutines(corpus []trace.NormSession, ids map[string]int, names []string, o Options, rawCalls int) (Funnel, []Routine) {
	f := Funnel{Sessions: len(corpus), Calls: rawCalls, Removed: map[string]int{},
		Savings: "estimated from recorded token use (mostly cached input); no primitive run was measured"}
	// Requests and their replayable steps.
	var inst []reqInstance
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
			inst = append(inst, reqInstance{session: si, request: r, steps: steps, labels: labels, text: text})
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
		for x := range inst[i].labels {
			// Smoothed, so a step every request runs still weighs something.
			inst[i].labels[x] = math.Log(1 + float64(len(inst))/float64(df[x]))
			inst[i].keys = append(inst[i].keys, x)
		}
		sort.Ints(inst[i].keys)
	}

	groups := groupRequests(inst)
	f.Groups = len(groups)
	var routines []Routine
	var split [][]int
	for _, g := range groups {
		split = append(split, splitGroup(corpus, inst, g)...)
	}
	// How many requests carry each request text (digits aside): a routine
	// whose goal is a stated text must be how most of them were done.
	instByText := map[string][]int{}
	for i := range inst {
		if k := trace.TextKey(inst[i].text); k != "" {
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
			sessions[inst[i].session] = true
		}
		// Recurrence: at least minSupport requests, in more than one session.
		if len(g) < o.MinSupport || len(sessions) < 2 {
			continue
		}
		rt, ok := buildRoutine(corpus, inst, g, names, o)
		if !ok {
			continue
		}
		goalShare(&rt, corpus, inst, g, byText, instByText)
		f.Routines++
		routines = append(routines, rt)
		if core, ok := goalCore(corpus, inst, names, o, &rt, byText, instByText, coreSeen); ok {
			f.Routines++
			routines = append(routines, core)
		}
		if sub, ok := boundedPart(corpus, inst, g, names, o, &rt); ok {
			f.Routines++
			routines = append(routines, sub)
		}
	}
	rank := map[string]int{"primitive": 0, "needs_authoring": 1, "baseline": 2, "removed": 3}
	// Primitives first. Among them, procedures before investigations: the
	// tokens a routine saves weighted by its coverage (how much of its
	// requests it is). Routines whose clients recorded no token use follow,
	// by coverage times requests. Nothing is removed; this only orders.
	share := func(r Routine) float64 { return r.Coverage }
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
	f.Merged = mergeDuplicates(routines)
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

// groupRequests puts each request in the group whose first request it is
// most like, when they share at least half their weighted steps (weighted
// Jaccard); otherwise it starts a group. Requests are taken in corpus order
// and a tie goes to the earlier group, so the result is the same every run.
func groupRequests(inst []reqInstance) [][]int {
	var leaders []int
	var groups [][]int
	byLabel := map[int][]int{} // label -> groups whose leader has it
	for i := range inst {
		in := &inst[i]
		var cands []int
		for _, x := range in.keys {
			cands = append(cands, byLabel[x]...)
		}
		sort.Ints(cands)
		best, bestSim := -1, 0.0
		for k, g := range cands {
			if k > 0 && cands[k-1] == g {
				continue
			}
			if sim := weightedJaccard(in, &inst[leaders[g]]); sim >= 0.5 && (best < 0 || sim > bestSim) {
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
		for _, x := range in.keys {
			byLabel[x] = append(byLabel[x], g)
		}
	}
	return groups
}

// weightedJaccard sums in key order, so equal inputs give equal bits.
func weightedJaccard(a, b *reqInstance) float64 {
	var inter, union float64
	i, j := 0, 0
	for i < len(a.keys) || j < len(b.keys) {
		switch {
		case j == len(b.keys) || (i < len(a.keys) && a.keys[i] < b.keys[j]):
			union += a.labels[a.keys[i]]
			i++
		case i == len(a.keys) || b.keys[j] < a.keys[i]:
			union += b.labels[b.keys[j]]
			j++
		default:
			w, v := a.labels[a.keys[i]], b.labels[b.keys[j]]
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

// buildRoutine makes a group's template (the steps at least half its
// requests ran together, in a recorded order), finds each request's run of
// it, drafts it and runs the checks.
func buildRoutine(corpus []trace.NormSession, inst []reqInstance, g []int, names []string, o Options) (Routine, bool) {
	// The steps that belong to the routine: the largest set of replayable
	// steps that at least half its requests ran together. Steps are taken in
	// order of how many requests ran them, and one is kept only if half the
	// requests still ran every step kept so far; steps each present in half
	// the requests separately can otherwise make a set no request ran whole.
	has := make([]map[string]bool, len(g))
	present := map[string]int{}
	for k, i := range g {
		s := corpus[inst[i].session]
		has[k] = map[string]bool{}
		for _, si := range inst[i].steps {
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
		s := corpus[inst[g[k]].session]
		seen := map[string]bool{}
		var labels []string
		for _, si := range inst[g[k]].steps {
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
			for _, si := range inst[i].steps {
				if corpus[inst[i].session].Steps[si].Label == only {
					n++
				}
			}
			if n >= 2 {
				multi++
			}
		}
		if 2*multi < len(g) {
			return Routine{}, false
		}
	}
	if len(inSet) == 0 {
		return Routine{}, false
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
		s := corpus[inst[i].session]
		r := run{inst: i, repeats: map[string]bool{}}
		seen := map[string]bool{}
		var all, first []string
		for _, si := range inst[i].steps {
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

	c := Candidate{ByClient: map[string]int{}, Sessions: 0, sessionSet: map[int]bool{}}
	for _, l := range tmpl {
		c.Steps = append(c.Steps, StepTemplate{Label: l})
	}
	rt := Routine{Requests: len(g), Consistency: float64(len(occ)) / float64(len(g))}
	var covs []float64
	for _, i := range occReq {
		n := 0
		for _, si := range inst[i].steps {
			if trace.Replayable(corpus[inst[i].session].Steps[si].Label) {
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
	rt.Example = trace.OneLine(redact.Redact(inst[g[0]].text), 140)
	ran := map[int]bool{}
	for _, i := range occReq {
		ran[i] = true
	}
	for _, i := range g {
		s := corpus[inst[i].session]
		rt.Sources = append(rt.Sources, SourceRef{Client: s.Client, Session: s.ID, Request: inst[i].request, Ran: ran[i]})
	}
	weeks := map[string]bool{}
	var times []time.Time
	for _, i := range g {
		s := corpus[inst[i].session]
		if !c.sessionSet[inst[i].session] {
			c.sessionSet[inst[i].session] = true
			c.ByClient[s.Client]++
			c.Sessions++
		}
		t := s.Start
		if st := s.Steps[inst[i].steps[0]].Time; !st.IsZero() {
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
		if run, saved, ok := runCost(steps); ok {
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
		rt.Failed, rt.Why = CheckSameWay, fmt.Sprintf("only %d of %d requests ran the same sequence of its steps and succeeded", len(occ), len(g))
		rt.Decision = "removed"
		rt.Statistics, rt.Validation = "not_run", "not_run"
		rt.Kind = routineKind(corpus, inst, g, tmpl)
		rt.legacyStates(nil)
		sum := sha256.Sum256([]byte(rt.SourceRole + "\x00" + strings.Join(tmpl, "\x1f")))
		rt.ID = hex.EncodeToString(sum[:6])
		rt.Family = "unknown:" + rt.ID
		rt.Suitability, rt.Reasons = SuitInsufficient, []string{"inconsistent_order"}
		if rt.SourceRole == RoleHarness {
			rt.Suitability, rt.Reasons = SuitInvalid, []string{"harness_request"}
		}
		rt.Failed = strings.SplitN(rt.Reasons[0], ":", 2)[0]
		return rt, true
	}
	c.items = make([]int, len(tmpl))
	rt.Candidate = c
	specs := loopSpecs(loops, occAll, occReq, inst)
	d := buildDraft(c, occ, DraftOptions{Publisher: DefaultPublisher, loops: specs})
	rt.draft, rt.occ, rt.loops = d, occ, specs

	// Per input: how often its value appeared in the request (reported, not a check).
	for n, in := range d.Inputs {
		hit, total := 0, 0
		for j, v := range d.inputValues(n) {
			if v == "" {
				continue
			}
			total++
			if trace.InRequest(v, inst[occReq[j]].text) {
				hit++
			}
		}
		share := 0.0
		if total > 0 {
			share = float64(hit) / float64(total)
		}
		rt.Inputs = append(rt.Inputs, RoutineInput{Name: in.Name, Type: in.Type, Explained: share})
	}
	// Not covered (computed now, checked last)
	skills := map[string]int{}
	for _, i := range g {
		for sk := range corpus[inst[i].session].RequestSkills[inst[i].request] {
			skills[sk]++
		}
	}
	for sk, n := range skills {
		if 2*n >= len(g) && (rt.CoveredBy == "" || sk < rt.CoveredBy) {
			rt.CoveredBy = sk
		}
	}

	rt.Runs, rt.FailedRuns, rt.UnknownRuns = seqRuns, failedRuns, unknownRuns
	rt.Kind = routineKind(corpus, inst, g, tmpl)
	// Role, outcome and value; suitability is decided by the contract.
	rt.legacyStates(d)
	switch {
	case rt.SourceRole == RoleScheduled:
		rt.Baseline = "scheduled_automation"
	case rt.CoveredBy != "":
		rt.Baseline = "skill:" + rt.CoveredBy
	}
	common := map[string]bool{}
	for _, l := range tmpl {
		common[l] = true
	}
	sum := sha256.Sum256([]byte(rt.SourceRole + "\x00" + strings.Join(tmpl, "\x1f") + "\x00" + splitKey(occ[0], common)))
	rt.ID = hex.EncodeToString(sum[:6])
	cruns := make([]contractRun, len(occ))
	for j := range occ {
		in := inst[occReq[j]]
		s := corpus[in.session]
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
			if a.Request == in.request && a.AfterCall > first && a.AfterCall <= last {
				n++
			}
		}
		var all []trace.Step
		for _, si := range in.steps {
			all = append(all, s.Steps[si])
		}
		cruns[j] = contractRun{steps: occ[j], all: all, text: in.text, approvals: n}
	}
	buildContract(&rt, d, cruns, loops)
	return rt, true
}

// loopSpecs finds, for each step most runs made several times, whether it
// loops over a list the request gave: exactly one argument varies between
// its occurrences, and in most runs every value of it is in the request.
// Anything else stays an open loop.
func loopSpecs(loops []string, occAll [][]trace.Step, occReq []int, inst []reqInstance) map[string]loopSpec {
	out := map[string]loopSpec{}
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
		spec := loopSpec{key: key, values: make([][]string, len(occAll))}
		given, prior := 0, 0
		for j := range occAll {
			vs := per[j][key]
			spec.values[j] = vs
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
				if !trace.InRequest(v, inst[occReq[j]].text) {
					inReq = false
				}
				found := false
				for _, st := range before {
					if trace.InResult(v, st) {
						found = true
						break
					}
				}
				if !found && !trace.InRequest(v, inst[occReq[j]].text) {
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
			spec.source = "caller"
			out[l] = spec
		case 2*(given+prior) >= len(occAll):
			spec.source = "prior_output"
			out[l] = spec
		}
	}
	return out
}

// routineKind says who a routine's work is for. Scheduled: most requests are
// Codex automation prompts. Automated: most requests carry the same prompt
// (digits aside), each alone in its session, so a program sent it.
// Bookkeeping: every step is a Telara recording or discovery call.
func routineKind(corpus []trace.NormSession, inst []reqInstance, g []int, tmpl []string) string {
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
		t := strings.TrimSpace(inst[i].text)
		if strings.HasPrefix(t, "Automation:") {
			scheduled++
		}
		if trace.IsHarness(t) || t == "" {
			harness++
		}
		if len(t) >= 120 {
			long++
		}
		if len(corpus[inst[i].session].Requests) == 1 {
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

// mergeDuplicates folds a routine into an earlier one (in report order)
// that is the same procedure: the same source role and the same steps in the
// same order and multiplicity, with the same operations and scope (the
// routine ID covers those, and so does Family's contract). An unordered set
// of labels is never enough: a different order or count is a different
// procedure.
func mergeDuplicates(rs []Routine) int {
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

// boundedPart looks inside a routine that is not a useful procedure as a
// whole for the longest run of its steps (two or more, in order) whose
// every input has a known source and no judgment: a bounded procedure
// inside a larger investigation, such as collecting a namespace's pod logs
// before diagnosing. It is judged on its own contract and reported with its
// parent. It is kept only when useful; the parent is never claimed.
func boundedPart(corpus []trace.NormSession, inst []reqInstance, g []int, names []string, o Options, rt *Routine) (Routine, bool) {
	d := rt.draft
	// Only inside a person's varying work: a scheduled automation is
	// already automated (its baseline covers its parts), and harness or
	// bookkeeping sources carry no user procedure.
	if d == nil || rt.Suitability == SuitUseful || rt.SourceRole != RoleUser || len(rt.Steps) < 3 {
		return Routine{}, false
	}
	bad := map[int]bool{}
	for p := range d.humanPos {
		bad[p] = true
	}
	for k, in := range d.Inputs {
		if k >= len(rt.Contract.Inputs) {
			break
		}
		switch rt.Contract.Inputs[k].Source {
		case InputUnresolved, InputComposed:
			bad[in.pos] = true
		}
	}
	for p, st := range rt.Steps {
		for _, l := range rt.Loops {
			if st.Label == l && !d.listLoop[l] {
				bad[p] = true
			}
		}
	}
	if len(bad) == 0 {
		return Routine{}, false
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
		return Routine{}, false
	}
	keep := map[string]bool{}
	for p := bestLo; p < bestHi; p++ {
		keep[rt.Steps[p].Label] = true
	}
	for p := range bad {
		if p < len(rt.Steps) && keep[rt.Steps[p].Label] {
			return Routine{}, false // a kept label also sits at a bad position
		}
	}
	var sub []reqInstance
	var gs []int
	sessions := map[int]bool{}
	for _, i := range g {
		in := inst[i]
		var steps []int
		for _, si := range inst[i].steps {
			if keep[corpus[inst[i].session].Steps[si].Label] {
				steps = append(steps, si)
			}
		}
		if len(steps) < 2 {
			continue
		}
		in.steps = steps
		gs = append(gs, len(sub))
		sub = append(sub, in)
		sessions[in.session] = true
	}
	// The part must recur on its own.
	if len(gs) < o.MinSupport || len(sessions) < 2 {
		return Routine{}, false
	}
	part, ok := buildRoutine(corpus, sub, gs, names, o)
	if !ok || part.Suitability != SuitUseful {
		return Routine{}, false
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
		return Routine{}, false
	}
	part.Parent = rt.ID
	part.Reasons = append(part.Reasons, "bounded_part_of:"+rt.ID)
	return part, true
}

// goalShare checks a routine whose goal is its requests' shared text: if
// most requests with that text did something else, these steps are not the
// procedure for the goal (a few runs happened to share incidental calls).
// A request with the text counts as doing it this way when it ran every
// step of the routine, whichever group its other calls put it in.
func goalShare(rt *Routine, corpus []trace.NormSession, inst []reqInstance, g []int, byText map[string]int, instByText map[string][]int) {
	if rt.Suitability != SuitUseful || rt.Contract.Goal != GoalStated {
		return
	}
	count := map[string]int{}
	for _, i := range g {
		if k := trace.TextKey(inst[i].text); k != "" {
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
		for _, si := range inst[i].steps {
			have[corpus[inst[i].session].Steps[si].Label]++
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
	rt.Suitability = SuitInsufficient
	rt.Reasons = []string{fmt.Sprintf("goal_usually_done_differently:%d_of_%d", ran, byText[top])}
	rt.DraftStatus, rt.Blockers = DraftNotAttempted, nil
	rt.Decision, rt.Failed = "removed", "goal_usually_done_differently"
	rt.Why = strings.Join(rt.Reasons, "; ")
}

// goalCore rebuilds a routine that failed the goal-share check from the
// steps most requests with its text ran, over all of those requests. When
// incidental calls split one task's requests into several groups, each
// group fails the check on its own; the steps they share are the procedure
// for the goal. Built once per text.
func goalCore(corpus []trace.NormSession, inst []reqInstance, names []string, o Options, rt *Routine, byText map[string]int, instByText map[string][]int, seen map[string]bool) (Routine, bool) {
	if firstReason(*rt) == "" || !strings.HasPrefix(firstReason(*rt), "goal_usually_done_differently") {
		return Routine{}, false
	}
	top := ""
	// The routine's dominant text.
	count := map[string]int{}
	for _, src := range rt.Sources {
		if k := textKeyOf(corpus, src); k != "" {
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
		return Routine{}, false
	}
	seen[top] = true
	members := instByText[top]
	has := make([]map[string]bool, len(members))
	present := map[string]int{}
	for k, i := range members {
		has[k] = map[string]bool{}
		for _, si := range inst[i].steps {
			if l := corpus[inst[i].session].Steps[si].Label; trace.Replayable(l) && !has[k][l] {
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
		return Routine{}, false
	}
	var sub []reqInstance
	var gs []int
	sessions := map[int]bool{}
	for _, i := range members {
		in := inst[i]
		var steps []int
		for _, si := range inst[i].steps {
			if keep[corpus[inst[i].session].Steps[si].Label] {
				steps = append(steps, si)
			}
		}
		if len(steps) < 2 {
			continue
		}
		in.steps = steps
		gs = append(gs, len(sub))
		sub = append(sub, in)
		sessions[in.session] = true
	}
	if len(gs) < o.MinSupport || len(sessions) < 2 {
		return Routine{}, false
	}
	core, ok := buildRoutine(corpus, sub, gs, names, o)
	if !ok {
		return Routine{}, false
	}
	core.Reasons = append(core.Reasons, "core_of_goal")
	return core, true
}

// textKeyOf is the text key of the request a source names.
func textKeyOf(corpus []trace.NormSession, src SourceRef) string {
	for _, s := range corpus {
		if s.Client == src.Client && s.ID == src.Session {
			if src.Request < len(s.Requests) {
				return trace.TextKey(s.Requests[src.Request])
			}
		}
	}
	return ""
}
