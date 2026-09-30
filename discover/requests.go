package discover

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
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
	// Sources are the requests the routine was found in. They point into
	// this machine's history and are for local review only.
	Sources []SourceRef `json:"sources,omitempty"`
	draft   *Draft
	occ     [][]Step
}

// DraftAs redrafts the routine under a publisher, with the steps the user
// marked read-only.
func (r *Routine) DraftAs(publisher string, readOnly map[int]bool) *Draft {
	if r.occ == nil {
		return r.draft
	}
	return buildDraft(r.Candidate, r.occ, DraftOptions{Publisher: publisher, ReadOnly: readOnly})
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
func requestRoutines(corpus []normSession, ids map[string]int, names []string, o Options, rawCalls int) (Funnel, []Routine) {
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
			for _, i := range steps {
				if l := s.Steps[i].Label; replayable(l) {
					labels[ids[l]] = 1
				}
			}
			if len(labels) < 2 {
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
	for _, g := range groups {
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
		f.Routines++
		routines = append(routines, rt)
	}
	rank := map[string]int{"primitive": 0, "needs_authoring": 1, "removed": 2}
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
	for i := range routines {
		if routines[i].MergedInto != "" {
			continue
		}
		switch routines[i].Decision {
		case "removed":
			f.Removed[routines[i].Failed]++
		case "needs_authoring":
			f.NeedsAuthoring++
		default:
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
func buildRoutine(corpus []normSession, inst []reqInstance, g []int, names []string, o Options) (Routine, bool) {
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
			if l := s.Steps[si].Label; replayable(l) && !has[k][l] {
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
	if len(inSet) < 2 {
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
		all     []Step
		first   []Step
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
		steps []Step
	}
	var chosen []chosenRun
	var tmpl []string
	var loops []string
	if full := modalOf(byFull); full != "" && 2*len(byFull[full]) >= len(g) {
		tmpl = strings.Split(full, "\x1f")
		for _, r := range byFull[full] {
			chosen = append(chosen, chosenRun{r.inst, r.all})
		}
	} else if first := modalOf(byFirst); first != "" {
		tmpl = strings.Split(first, "\x1f")
		rep := map[string]int{}
		for _, r := range byFirst[first] {
			chosen = append(chosen, chosenRun{r.inst, r.first})
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
	var occ [][]Step
	var occReq []int // index into inst
	seqRuns, failedRuns, unknownRuns := 0, 0, 0
	for _, r := range chosen {
		seqRuns++
		failed, unknown := false, false
		for _, st := range r.steps {
			switch st.Outcome {
			case OutcomeFailed:
				failed = true
			case OutcomeUnknown:
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
	}
	if len(tmpl) < 2 {
		// No sequence of these steps recurred: the requests share steps but
		// not a procedure. Report the step set for the reader.
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
			if replayable(corpus[inst[i].session].Steps[si].Label) {
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
	rt.Example = oneLine(Redact(inst[g[0]].text), 140)
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
	var runs [][2]Usage
	for _, steps := range occ {
		if run, saved, ok := runCost(steps); ok {
			c.Measured++
			c.SavedTotal = c.SavedTotal.add(saved)
			runs = append(runs, [2]Usage{run, saved})
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
		sum := sha256.Sum256([]byte(rt.Kind + "\x00" + strings.Join(tmpl, "\x1f")))
		rt.ID = hex.EncodeToString(sum[:6])
		return rt, true
	}
	c.items = make([]int, len(tmpl))
	rt.Candidate = c
	d := buildDraft(c, occ, DraftOptions{Publisher: DefaultPublisher})
	rt.draft, rt.occ = d, occ

	// Per input: how often its value appeared in the request (reported, not a check).
	for n, in := range d.Inputs {
		hit, total := 0, 0
		for j, v := range d.inputValues(n) {
			if v == "" {
				continue
			}
			total++
			if inRequest(v, inst[occReq[j]].text) {
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
	switch {
	case d.FixedSteps == 0:
		rt.Failed, rt.Why = CheckReplays, "no step fixes anything (no subcommand, tool or constant value): exploration, not a procedure"
	case rt.Consistency < 0.5:
		rt.Failed, rt.Why = CheckSameWay, fmt.Sprintf("only %.0f%% of its requests ran the same sequence of its steps and succeeded", 100*rt.Consistency)
		if failedRuns > 0 {
			rt.Why += fmt.Sprintf(" (%d of the %d that ran it had a failed step)", failedRuns, seqRuns)
		}
	case c.Weeks < 2:
		rt.Failed, rt.Why = CheckWorth, "all its requests fell in one week"
	case c.Measured > 0 && c.SavedPerRun.Total() == 0:
		rt.Failed, rt.Why = CheckWorth, "a typical run was already a single turn"
	}
	switch {
	case rt.Failed != "":
		rt.Decision = "removed"
	case d.HumanSteps > 0 || d.Derived > 0 || len(loops) > 0:
		rt.Decision = "needs_authoring"
		var why []string
		if len(loops) > 0 {
			why = append(why, "runs "+strings.Join(loops, ", ")+" several times with different values: write the loop")
		}
		if d.HumanSteps > 0 {
			why = append(why, fmt.Sprintf("%d step(s) decided per run", d.HumanSteps))
		}
		if d.Derived > 0 {
			why = append(why, fmt.Sprintf("%d value(s) come from an earlier step's output", d.Derived))
		}
		rt.Why = strings.Join(why, "; ")
	default:
		rt.Decision = "primitive"
	}
	rt.Kind = routineKind(corpus, inst, g, tmpl)
	sum := sha256.Sum256([]byte(rt.Kind + "\x00" + strings.Join(tmpl, "\x1f")))
	rt.ID = hex.EncodeToString(sum[:6])
	return rt, true
}

// unexplained names the first input whose values were mostly not in the
// request, or "".
// inRequest reports whether a value was given in the request: the value
// itself, or for a path its base name, or for a URL its path, appears in the
// text (case-insensitive).
func inRequest(v, text string) bool {
	if text == "" {
		return false
	}
	t := strings.ToLower(text)
	lv := strings.ToLower(strings.TrimSpace(v))
	if lv == "" {
		return false
	}
	if strings.Contains(t, lv) {
		return true
	}
	if b := path.Base(lv); len(b) >= 3 && b != "." && strings.Contains(t, b) {
		return true
	}
	return false
}

// bookkeepingTools are Telara's own recording and tool-discovery calls,
// which agent instructions make every agent run around its work.
var bookkeepingTools = map[string]bool{
	"mcp:telara_task_list": true, "mcp:telara_task_create": true, "mcp:telara_task_resume": true,
	"mcp:telara_task_checkpoint": true, "mcp:telara_task_complete": true, "mcp:telara_task_pause": true,
	"mcp:telara_tool_search": true, "mcp:telara_tool_describe": true, "mcp:telara_annotate": true,
	"mcp:telara_link": true, "get_mcp_tools": true, "ToolSearch": true,
}

var digits = regexp.MustCompile(`\d+`)

// routineKind says who a routine's work is for. Scheduled: most requests are
// Codex automation prompts. Automated: most requests carry the same prompt
// (digits aside), each alone in its session, so a program sent it.
// Bookkeeping: every step is a Telara recording or discovery call.
func routineKind(corpus []normSession, inst []reqInstance, g []int, tmpl []string) string {
	book := true
	for _, l := range tmpl {
		if !bookkeepingTools[l] {
			book = false
		}
	}
	if book {
		return "bookkeeping"
	}
	scheduled, single := 0, 0
	texts := map[string]int{}
	for _, i := range g {
		t := strings.TrimSpace(inst[i].text)
		if strings.HasPrefix(t, "Automation:") {
			scheduled++
		}
		if len(corpus[inst[i].session].Requests) == 1 {
			single++
		}
		texts[digits.ReplaceAllString(truncateUTF8(t, 120), "#")]++
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
	if 5*top >= 4*len(g) && 5*single >= 4*len(g) {
		return "automated"
	}
	return "user"
}

// mergeDuplicates folds a routine into an earlier one (in report order) with
// the same kind and the same set of steps: one job found as two groups.
func mergeDuplicates(rs []Routine) int {
	seen := map[string]string{}
	merged := 0
	for i := range rs {
		if rs[i].Decision == "removed" {
			continue
		}
		set := map[string]bool{}
		for _, st := range rs[i].Steps {
			set[st.Label] = true
		}
		keys := make([]string, 0, len(set))
		for k := range set {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		key := rs[i].Kind + "\x00" + strings.Join(keys, "\x1f")
		if first, ok := seen[key]; ok {
			rs[i].MergedInto = first
			merged++
			continue
		}
		seen[key] = rs[i].ID
	}
	return merged
}
