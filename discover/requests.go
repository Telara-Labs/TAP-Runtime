package discover

import (
	"fmt"
	"math"
	"path"
	"sort"
	"strings"
	"time"
)

// Request-level discovery turns recurring work into a short list of
// primitives. The unit is one request: a user message and every call the
// agent made to answer it. Requests are grouped by the replayable steps they
// ran, a group's template is the steps most of its requests share, and a
// group becomes a primitive only if it passes five checks, in order:
//
//  1. whole request   it is the work for a request, not a window cut from one
//  2. replays         every step is a command, tool call, browser call, read
//     or fetch: no edit decided per run, no value computed by
//     the rest of a script
//  3. same way        most requests of the group ran the same sequence of its steps
//  4. worth it        it recurs over more than one week, and where token use
//     was recorded, a primitive saves turns
//  5. not covered     the requests did not already load a skill for it
//
// The funnel counts what each check removed; nothing is dropped silently.

// Check names, in the order they run.
const (
	CheckReplays = "replays"
	CheckSameWay = "same way"
	CheckWorth   = "worth it"
	CheckCovered = "not covered"
)

// CheckOrder is the order the checks run in (whole request is by construction).
// "Inputs given" is not among them: a primitive is called by an agent, which
// supplies its inputs, so a value the agent chose (the files to commit, the
// package to test) is a normal input. Whether each input's values appeared
// in the request is still reported per input.
var CheckOrder = []string{CheckReplays, CheckSameWay, CheckWorth, CheckCovered}

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
	// Failed is the first check it failed ("" for a primitive), Why says how.
	Failed string `json:"failed,omitempty"`
	Why    string `json:"why,omitempty"`
	// Example is one request's text, shortened.
	Example string `json:"example"`
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
	text    string
}

// requestRoutines runs the request-level pass over a normalized corpus.
func requestRoutines(corpus []normSession, ids map[string]int, names []string, o Options, rawCalls int) (Funnel, []Routine) {
	f := Funnel{Sessions: len(corpus), Calls: rawCalls, Removed: map[string]int{}}
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
		}
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
	for i := range routines {
		if routines[i].Failed != "" {
			f.Removed[routines[i].Failed]++
		} else {
			f.Primitives++
		}
	}
	// Primitives first. Among them, procedures before investigations: the
	// tokens a routine saves weighted by its coverage (how much of its
	// requests it is). Routines whose clients recorded no token use follow,
	// by coverage times requests. Nothing is removed; this only orders.
	share := func(r Routine) float64 { return r.Coverage }
	sort.SliceStable(routines, func(a, b int) bool {
		ra, rb := routines[a], routines[b]
		if (ra.Failed == "") != (rb.Failed == "") {
			return ra.Failed == ""
		}
		if (ra.Measured > 0) != (rb.Measured > 0) {
			return ra.Measured > 0
		}
		if ra.Measured > 0 {
			return share(ra)*ra.SavedTotal.Total() > share(rb)*rb.SavedTotal.Total()
		}
		return share(ra)*float64(ra.Requests) > share(rb)*float64(rb.Requests)
	})
	return f, routines
}

// groupRequests puts each request in the group whose first request it is
// most like, when they share at least half their weighted steps (weighted
// Jaccard); otherwise it starts a group. Requests are taken in corpus order,
// so the result does not depend on scheduling.
func groupRequests(inst []reqInstance) [][]int {
	var leaders []int
	var groups [][]int
	byLabel := map[int][]int{} // label -> groups whose leader has it
	for i, in := range inst {
		best, bestSim := -1, 0.5
		seen := map[int]bool{}
		for x := range in.labels {
			for _, g := range byLabel[x] {
				if seen[g] {
					continue
				}
				seen[g] = true
				if sim := weightedJaccard(in.labels, inst[leaders[g]].labels); sim >= bestSim {
					best, bestSim = g, sim
				}
			}
		}
		if best >= 0 {
			groups[best] = append(groups[best], i)
			continue
		}
		g := len(groups)
		leaders = append(leaders, i)
		groups = append(groups, []int{i})
		for x := range in.labels {
			byLabel[x] = append(byLabel[x], g)
		}
	}
	return groups
}

func weightedJaccard(a, b map[int]float64) float64 {
	var inter, union float64
	for x, w := range a {
		if v, ok := b[x]; ok {
			inter += math.Min(w, v)
			union += math.Max(w, v)
		} else {
			union += w
		}
	}
	for x, v := range b {
		if _, ok := a[x]; !ok {
			union += v
		}
	}
	if union == 0 {
		return 0
	}
	return inter / union
}

// buildRoutine makes a group's template (steps at least half its requests
// ran, in their usual order), finds each request's run of it, drafts it and
// runs the checks.
func buildRoutine(corpus []normSession, inst []reqInstance, g []int, names []string, o Options) (Routine, bool) {
	// The steps that belong to the routine: replayable labels at least half
	// its requests ran.
	present := map[string]int{}
	for _, i := range g {
		s := corpus[inst[i].session]
		seen := map[string]bool{}
		for _, si := range inst[i].steps {
			if l := s.Steps[si].Label; replayable(l) && !seen[l] {
				seen[l] = true
				present[l]++
			}
		}
	}
	inSet := map[string]bool{}
	for l, n := range present {
		if 2*n >= len(g) {
			inSet[l] = true
		}
	}
	if len(inSet) < 2 {
		return Routine{}, false
	}
	// Each request's run of those steps, in the order it made them, repeats
	// included. The draft follows the sequence most requests actually ran;
	// it never reorders or combines steps from different runs.
	type run struct {
		inst  int
		steps []Step
	}
	bySeq := map[string][]run{}
	for _, i := range g {
		s := corpus[inst[i].session]
		var r run
		r.inst = i
		has := map[string]bool{}
		var labels []string
		for _, si := range inst[i].steps {
			if st := s.Steps[si]; inSet[st.Label] {
				r.steps = append(r.steps, st)
				labels = append(labels, st.Label)
				has[st.Label] = true
			}
		}
		if len(has) < len(inSet) || len(r.steps) > 24 {
			continue
		}
		key := strings.Join(labels, "\x1f")
		bySeq[key] = append(bySeq[key], r)
	}
	modal := ""
	for k, rs := range bySeq {
		if len(rs) > len(bySeq[modal]) || (len(rs) == len(bySeq[modal]) && k < modal) {
			modal = k
		}
	}
	var tmpl []string
	if modal != "" {
		tmpl = strings.Split(modal, "\x1f")
	}
	var occ [][]Step
	var occReq []int // index into inst
	for _, r := range bySeq[modal] {
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
	c.Qualified = true
	rt.Candidate = c

	// A group whose template no request ran in full cannot be drafted.
	if len(occ) < 2 {
		rt.Failed, rt.Why = CheckSameWay, fmt.Sprintf("only %d of %d requests ran the same sequence of its steps", len(occ), len(g))
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

	switch {
	case d.HumanSteps > 0:
		rt.Failed, rt.Why = CheckReplays, fmt.Sprintf("%d step(s) the agent decided per run", d.HumanSteps)
	case d.FixedSteps == 0:
		rt.Failed, rt.Why = CheckReplays, "no step fixes anything (no subcommand, tool or constant value): exploration, not a procedure"
	case rt.Consistency < 0.5:
		rt.Failed, rt.Why = CheckSameWay, fmt.Sprintf("only %.0f%% of its requests ran the same sequence of its steps", 100*rt.Consistency)
	case c.Weeks < 2:
		rt.Failed, rt.Why = CheckWorth, "all its requests fell in one week"
	case c.Measured > 0 && c.SavedTotal.Total() == 0:
		rt.Failed, rt.Why = CheckWorth, "each run was already a single turn"
	case rt.CoveredBy != "":
		rt.Failed, rt.Why = CheckCovered, "its requests already load the "+rt.CoveredBy+" skill"
	}
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
