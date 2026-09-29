package discover

import (
	"fmt"
	"io"
	"math"
	"math/rand"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// RulesVersion changes whenever a rule changes, so two reports are only
// compared when they were produced by the same rules.
const RulesVersion = "tap-discover/2"

// Options control a run. Window, MinSupport, MaxLen and MaxPatterns bound the
// search (compute), Permutations sets how well the null is estimated, Alpha
// is the false discovery rate. None of them is a quality threshold on a
// candidate: significance and stability are measured and reported.
type Options struct {
	Readers      []Reader
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
	// Progress, when set, receives one line per phase.
	Progress io.Writer
}

// DefaultOptions are the rules RulesVersion names.
func DefaultOptions() Options {
	return Options{Window: 8, MinSupport: 3, MaxLen: 5, MaxPatterns: 2000000, PerSkill: 10, Permutations: 20, Alpha: 0.05, Seed: 1, Now: time.Now}
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
	Label     string  `json:"label"`
	Template  string  `json:"template"`
	Params    []Slot  `json:"params,omitempty"`
	Stability float64 `json:"stability"`
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
	NullMean      float64        `json:"null_mean"`
	P             float64        `json:"p"`
	Q             float64        `json:"q"`
	OrderQ        float64        `json:"order_q"`
	NecessityQ    float64        `json:"necessity_q"`
	Qualified     bool           `json:"qualified"`
	Ordered       bool           `json:"ordered"`
	Stability     float64        `json:"stability"`
	Specificity   float64        `json:"specificity"`
	CallsSaved    int            `json:"calls_saved"`
	Score         float64        `json:"score"`
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
	PerRun      Usage `json:"tokens_per_run"`
	SavedPerRun Usage `json:"tokens_saved_per_run"`
	SavedTotal  Usage `json:"tokens_saved_total"`
	Measured    int   `json:"measured_runs"`
	// Family is the rank (index) of the qualified candidate this one is a
	// variant of; a representative is its own family.
	Family     int `json:"family"`
	sessionSet map[int]bool
	items      []int
}

type SkillMatch struct {
	Pattern   string  `json:"pattern"`
	Qualified bool    `json:"qualified"`
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
	Options        optionsOut    `json:"options"`
	Clients        []ClientStats `json:"clients"`
	Labels         int           `json:"labels"`
	Examined       int           `json:"examined"`
	Mined          int           `json:"kept"`
	Truncated      bool          `json:"truncated"`
	MinSupportUsed int           `json:"min_support"`
	Tested         int           `json:"tested"`
	Qualified      int           `json:"qualified"`
	Families       int           `json:"families"`
	Candidates     []Candidate   `json:"candidates"`
	Recall         []SkillRecall `json:"recall"`
	Skills         []SkillReport `json:"skills"`
	// Funnel and Routines are the request-level result: what was read, how
	// many requests recurred as routines, and which passed every check.
	Funnel   Funnel    `json:"funnel"`
	Routines []Routine `json:"routines"`

	// Kept in memory so a candidate can be drafted from its real
	// occurrences; never written out.
	corpus []normSession
	seqs   [][]int
	names  []string
	window int
}

type optionsOut struct {
	Since        time.Time `json:"since,omitempty"`
	Window       int       `json:"window"`
	MinSupport   int       `json:"min_support"`
	MaxLen       int       `json:"max_len"`
	MaxPatterns  int       `json:"max_patterns"`
	Permutations int       `json:"permutations"`
	Alpha        float64   `json:"alpha"`
	Seed         int64     `json:"seed"`
}

// Run reads every client, mines, tests and ranks. It makes no network call
// and writes nothing.
func Run(o Options) (*Report, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	rep := &Report{RulesVersion: RulesVersion, GeneratedAt: o.Now().UTC(), Options: optionsOut{o.Since, o.Window, o.MinSupport, o.MaxLen, o.MaxPatterns, o.Permutations, o.Alpha, o.Seed}}

	var raw []Session
	for _, r := range o.Readers {
		st := ClientStats{Client: r.Client()}
		ss, err := r.Read(o.Since)
		if err != nil {
			st.Error = err.Error()
		}
		for _, s := range ss {
			st.Calls += len(s.Calls)
		}
		raw = append(raw, ss...)
		rep.Clients = append(rep.Clients, st)
		o.log("%s: %d sessions, %d calls %s", st.Client, len(ss), st.Calls, st.Error)
	}
	o.log("read %d sessions", len(raw))
	sessions := normalize(raw)

	// Identical step sequences are one piece of work run twice (a replayed
	// test harness, a re-sent prompt), not recurrence.
	seen := map[string]bool{}
	var corpus []normSession
	stats := map[string]*ClientStats{}
	for i := range rep.Clients {
		stats[rep.Clients[i].Client] = &rep.Clients[i]
	}
	for _, s := range sessions {
		labels := make([]string, len(s.Steps))
		for i, st := range s.Steps {
			labels[i] = st.Label
		}
		k := strings.Join(labels, "\x1f")
		cs := stats[s.Client]
		if cs == nil {
			// A reader returned sessions under another client's name.
			rep.Clients = append(rep.Clients, ClientStats{Client: s.Client})
			for i := range rep.Clients {
				stats[rep.Clients[i].Client] = &rep.Clients[i]
			}
			cs = stats[s.Client]
		}
		if seen[k] {
			cs.DuplicateSessions++
			continue
		}
		seen[k] = true
		corpus = append(corpus, s)
		cs.Sessions++
		cs.Steps += len(s.Steps)
		if cs.Earliest.IsZero() || s.Start.Before(cs.Earliest) {
			cs.Earliest = s.Start
		}
		if s.Start.After(cs.Latest) {
			cs.Latest = s.Start
		}
	}

	ids := map[string]int{}
	var names []string
	seqs := make([][]int, len(corpus))
	for i, s := range corpus {
		for _, st := range s.Steps {
			id, ok := ids[st.Label]
			if !ok {
				id = len(names)
				ids[st.Label] = id
				names = append(names, st.Label)
			}
			seqs[i] = append(seqs[i], id)
		}
	}
	rep.Labels = len(names)
	totalCalls := 0
	for _, c := range rep.Clients {
		totalCalls += c.Calls
	}
	// Identical sessions count here: an automation that runs the same way
	// every week is exactly the recurrence this pass is for, and the "worth
	// it" check still removes a burst replayed inside one week.
	for _, ns := range sessions {
		for _, st := range ns.Steps {
			if _, ok := ids[st.Label]; !ok {
				ids[st.Label] = len(names)
				names = append(names, st.Label)
			}
		}
	}
	rep.Funnel, rep.Routines = requestRoutines(sessions, ids, names, o, totalCalls)
	o.log("requests: %d with replayable steps, %d routines, %d primitives", rep.Funnel.RequestsWithSteps, rep.Funnel.Routines, rep.Funnel.Primitives)
	if !o.Patterns {
		return rep, nil
	}
	groupOf := map[string]int{}
	group := make([]int, len(corpus))
	for i, s := range corpus {
		g, ok := groupOf[s.Client]
		if !ok {
			g = len(groupOf)
			groupOf[s.Client] = g
		}
		group[i] = g
	}

	// df[x] is how many sessions contain label x. It feeds the necessity
	// bound below and the IDF weights in the ranking.
	df := make([]int, len(names))
	dfGroup := make([][]int, len(groupOf))
	nGroup := make([]int, len(groupOf))
	for g := range dfGroup {
		dfGroup[g] = make([]int, len(names))
	}
	for i, seq := range seqs {
		nGroup[group[i]]++
		seen := map[int]bool{}
		for _, x := range seq {
			if !seen[x] {
				seen[x] = true
				df[x]++
				dfGroup[group[i]][x]++
			}
		}
	}
	// share[x] is the largest share of sessions containing x in any one
	// client. Each client names its tools differently, so a share over all
	// clients would understate how common a client-specific tool is there;
	// the largest per-client share is the conservative choice.
	share := make([]float64, len(names))
	for g := range dfGroup {
		for x, d := range dfGroup[g] {
			if f := float64(d) / float64(nGroup[g]); f > share[x] {
				share[x] = f
			}
		}
	}
	// A pattern of support k that contains a step present in a share f of all
	// sessions can do no better on the necessity test than p = f^k (every
	// session holding the rest also holds that step). If f^k is above alpha
	// it cannot qualify, and extensions only lower k, so the branch is cut.
	// This is the qualification rule applied early, not a new threshold.
	canQualify := func(items []int, k int) bool {
		for _, x := range items {
			if math.Pow(share[x], float64(k)) > o.Alpha {
				return false
			}
		}
		return true
	}

	// The search keeps only patterns that can still qualify. A pattern's
	// last step is tested for necessity the moment the pattern is found, and
	// exactly: removing the last step leaves the prefix whose support the
	// search just counted. Failing it means the pattern cannot qualify, so it
	// is not kept. Every pattern examined still counts toward the FDR
	// correction, so discarding early does not make the test more lenient.
	index := labelIndex(seqs)
	keep := func(items []int, k, nPrefix int) bool {
		// The last step first: exact and free (the prefix's support is known).
		if binomialUpper(k, max(nPrefix, k), share[items[len(items)-1]]) > o.Alpha {
			return false
		}
		// Then every other step, stopping at the first that fails.
		return necessityP(items, k, seqs, index, df, share, o.Window, o.Alpha) <= o.Alpha
	}
	mined, examined, truncated := minePatterns(seqs, mineLimits{window: o.Window, minSupport: o.MinSupport, maxLen: o.MaxLen, maxOut: o.MaxPatterns, canQualify: canQualify, keep: keep})
	rep.Mined, rep.Examined, rep.Truncated, rep.MinSupportUsed = len(mined), examined, truncated, o.MinSupport
	o.log("examined %d patterns at support >= %d over %d sessions and %d labels; %d kept", examined, o.MinSupport, len(seqs), len(names), len(mined))

	idf := make([]float64, len(names))
	for x, d := range df {
		idf[x] = math.Log(float64(len(seqs)) / float64(d))
	}

	// Every step must be necessary (see necessity), with FDR over all
	// patterns examined. Only patterns passing it go on to the shuffle tests,
	// which cost a pass over the corpus per permutation.
	closed := closedOnly(mined)
	pNeedAll := make([]float64, len(closed))
	parallelFor(len(closed), func(i int) {
		pNeedAll[i] = necessity(closed[i], seqs, index, df, share, o.Window)
	})
	qNeedAll := benjaminiHochbergOf(pNeedAll, examined)
	var cands []pattern
	var qNeed []float64
	for i, p := range closed {
		if qNeedAll[i] <= o.Alpha {
			cands = append(cands, p)
			qNeed = append(qNeed, qNeedAll[i])
		}
	}
	rep.Tested = len(cands)
	o.log("%d closed patterns; %d pass the per-step test and go to %d permutations per shuffle test", len(closed), len(cands), o.Permutations)

	across := permuteCounts(seqs, group, cands, o, shuffleAcross)
	o.log("between-session null done")
	within := permuteCounts(seqs, group, cands, o, shuffleWithin)
	o.log("within-session null done")
	pAcross := make([]float64, len(cands))
	pWithin := make([]float64, len(cands))
	for i, p := range cands {
		pAcross[i] = pValue(len(p.sessions), across[i])
		pWithin[i] = pValue(len(p.sessions), within[i])
	}
	qAcross := benjaminiHochberg(pAcross)
	qWithin := benjaminiHochberg(pWithin)

	for i, p := range cands {
		c := describe(p, corpus, seqs, names, idf, o.Window)
		var sum float64
		for _, n := range across[i] {
			sum += float64(n)
		}
		c.NullMean = sum / float64(len(across[i]))
		c.P, c.Q, c.OrderQ = pAcross[i], qAcross[i], qWithin[i]
		// Beyond chance under both nulls: the steps travel together more than
		// their frequencies explain, and in this order and proximity more than
		// each session's own mix of calls explains.
		c.Ordered = c.OrderQ <= o.Alpha
		c.NecessityQ = qNeed[i]
		// Every step earned its place (necessity, above), and at least one
		// step fixes something: a sequence whose every argument varies is the
		// agent's judgment about what to touch, not a procedure to replay.
		c.Qualified = c.Q <= o.Alpha && c.Ordered && c.NecessityQ <= o.Alpha && c.Specificity > 0
		if c.Qualified {
			rep.Qualified++
		}
		rep.Candidates = append(rep.Candidates, c)
	}
	sort.SliceStable(rep.Candidates, func(a, b int) bool {
		ca, cb := rep.Candidates[a], rep.Candidates[b]
		if ca.Qualified != cb.Qualified {
			return ca.Qualified
		}
		return ca.Score > cb.Score
	})
	rep.Recall = recall(corpus, rep.Candidates)
	rep.Skills = skillProcedures(corpus, seqs, closed, names, idf, o)
	rep.corpus, rep.seqs, rep.names, rep.window = corpus, seqs, names, o.Window
	o.log("skill comparison: %d skills with enriched procedures", len(rep.Skills))
	rep.Families = assignFamilies(rep.Candidates)
	return rep, nil
}

func permuteCounts(seqs [][]int, group []int, ps []pattern, o Options, permute func([][]int, []int, *rand.Rand) [][]int) [][]int {
	out := make([][]int, len(ps))
	for i := range out {
		out[i] = make([]int, o.Permutations)
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	for k := 0; k < o.Permutations; k++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(k int) {
			defer wg.Done()
			defer func() { <-sem }()
			rng := rand.New(rand.NewSource(o.Seed + int64(k)*7919))
			counts := nullSupport(permute(seqs, group, rng), ps, o.Window)
			for i, n := range counts {
				out[i][k] = n
			}
		}(k)
	}
	wg.Wait()
	return out
}

// describe builds the report entry for a pattern: a template per step from
// the occurrences whose skeleton is the most common one, the slots that vary
// as typed parameters, and when the work happened.
func describe(p pattern, corpus []normSession, seqs [][]int, names []string, idf []float64, window int) Candidate {
	c := Candidate{ByClient: map[string]int{}, Sessions: len(p.sessions), sessionSet: map[int]bool{}, items: p.items}
	occ := make([][]Step, 0, len(p.sessions))
	var times []time.Time
	var runs [][2]Usage
	weeks := map[string]bool{}
	for _, s := range p.sessions {
		c.sessionSet[s] = true
		ns := corpus[s]
		c.ByClient[ns.Client]++
		idx := matchAt(seqs[s], p.items, window)
		steps := make([]Step, len(idx))
		for i, j := range idx {
			steps[i] = ns.Steps[j]
		}
		occ = append(occ, steps)
		if run, saved, ok := runCost(steps); ok {
			c.Measured++
			c.SavedTotal = c.SavedTotal.add(saved)
			runs = append(runs, [2]Usage{run, saved})
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
	for i := range p.items {
		st := templateOf(names[p.items[i]], occ, i)
		st.Weight = idf[p.items[i]]
		stab += st.Stability
		if st.Fixed {
			specific++
			weighted += st.Weight * st.Stability
		}
		c.Steps = append(c.Steps, st)
	}
	c.Stability = stab / float64(len(p.items))
	if len(runs) > 0 {
		sort.Slice(runs, func(a, b int) bool { return runs[a][0].Total() < runs[b][0].Total() })
		c.PerRun, c.SavedPerRun = runs[len(runs)/2][0], runs[len(runs)/2][1]
	}
	c.Specificity = float64(specific) / float64(len(p.items))
	c.CallsSaved = c.Sessions * len(p.items)
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

func templateOf(label string, occ [][]Step, i int) StepTemplate {
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
				parts = append(parts, fmtSlot(label, k, v))
			}
			// A constant flag (tail -n) or numeric option (offset=0) says
			// how a tool is used, not what it acts on; only a constant
			// word, path, text, id or URL fixes the work.
			if tp != SlotFlag && tp != SlotNumber {
				st.Fixed = true
			}
			continue
		}
		parts = append(parts, fmtSlot(label, k, "<"+tp+">"))
		st.Params = append(st.Params, Slot{Key: k, Type: tp})
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
	st.Template = strings.Join(collapsed, " ")
	return st
}

func fmtSlot(label, key, v string) string {
	if strings.HasPrefix(label, "sh:") {
		if len(v) > 60 {
			v = v[:60] + "…"
		}
		return v
	}
	if len(v) > 40 {
		v = v[:40] + "…"
	}
	return key + "=" + v
}

// recall scores the miner against work known to recur: every skill loaded in
// at least two sessions. For each, the candidate whose sessions best match the
// skill's sessions (F1) is reported, among qualified and among all tested.
func recall(corpus []normSession, cands []Candidate) []SkillRecall {
	skillSessions := map[string]map[int]bool{}
	for i, s := range corpus {
		for sk := range s.Skills {
			if skillSessions[sk] == nil {
				skillSessions[sk] = map[int]bool{}
			}
			skillSessions[sk][i] = true
		}
	}
	var out []SkillRecall
	for sk, truth := range skillSessions {
		if len(truth) < 2 {
			continue
		}
		r := SkillRecall{Skill: sk, Sessions: len(truth)}
		for _, c := range cands {
			inter := 0
			for s := range c.sessionSet {
				if truth[s] {
					inter++
				}
			}
			if inter == 0 {
				continue
			}
			m := SkillMatch{Pattern: labelsOf(c), Qualified: c.Qualified, Precision: float64(inter) / float64(len(c.sessionSet)), Recall: float64(inter) / float64(len(truth))}
			m.F1 = 2 * m.Precision * m.Recall / (m.Precision + m.Recall)
			if r.BestTested == nil || m.F1 > r.BestTested.F1 {
				mm := m
				r.BestTested = &mm
			}
			if c.Qualified && (r.BestQualified == nil || m.F1 > r.BestQualified.F1) {
				mm := m
				r.BestQualified = &mm
			}
		}
		out = append(out, r)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Sessions != out[b].Sessions {
			return out[a].Sessions > out[b].Sessions
		}
		return out[a].Skill < out[b].Skill
	})
	return out
}

func labelsOf(c Candidate) string {
	ls := make([]string, len(c.Steps))
	for i, s := range c.Steps {
		ls[i] = s.Label
	}
	return strings.Join(ls, " → ")
}

// assignFamilies groups qualified candidates that describe the same work: a
// candidate joins the family of a higher-ranked one when they share at least
// half their sessions and half their labels (Jaccard). This only folds the
// report; it does not change what qualified. It returns the family count.
func assignFamilies(cs []Candidate) int {
	var reps []int
	for i := range cs {
		cs[i].Family = i
		if !cs[i].Qualified {
			continue
		}
		joined := false
		for _, r := range reps {
			if jaccardInts(cs[i].sessionSet, cs[r].sessionSet) >= 0.5 && jaccardStrings(labelSet(cs[i]), labelSet(cs[r])) >= 0.5 {
				cs[i].Family = r
				joined = true
				break
			}
		}
		if !joined {
			reps = append(reps, i)
		}
	}
	return len(reps)
}

func labelSet(c Candidate) map[string]bool {
	m := map[string]bool{}
	for _, s := range c.Steps {
		m[s.Label] = true
	}
	return m
}

func jaccardInts(a, b map[int]bool) float64 {
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}

func jaccardStrings(a, b map[string]bool) float64 {
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}

// necessity asks, for each step x of a pattern, whether the pattern holds in
// more of the sessions that hold the pattern without x than x's own
// frequency explains: a one-sided binomial test with n = support without x,
// k = support with x, and p = the largest share of sessions containing x in
// any one client. That p
// ignores position and so overstates chance, which makes the test
// conservative. The largest p over the steps is returned: every step must be
// necessary. Without this, any significant core plus one unrelated common
// step would qualify on the core's strength.
func necessity(p pattern, seqs [][]int, index map[int]map[int]bool, df []int, share []float64, window int) float64 {
	return necessityP(p.items, len(p.sessions), seqs, index, df, share, window, 1)
}

// necessityP is necessity for items with support k. It returns as soon as a
// step's p exceeds stopAbove (pass 1 to compute the exact maximum).
func necessityP(items []int, k int, seqs [][]int, index map[int]map[int]bool, df []int, share []float64, window int, stopAbove float64) float64 {
	if len(items) < 2 {
		return 1
	}
	worst := 0.0
	for i, x := range items {
		rest := append(append([]int{}, items[:i]...), items[i+1:]...)
		n := k
		if len(rest) > 1 {
			n = supportIn(seqs, index, rest, window*2)
		} else {
			n = df[rest[0]]
		}
		pv := binomialUpper(k, max(n, k), share[x])
		if pv > worst {
			worst = pv
			if worst > stopAbove {
				return worst
			}
		}
	}
	return worst
}

// labelIndex maps each label to the sessions containing it.
func labelIndex(seqs [][]int) map[int]map[int]bool {
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

// supportIn counts sessions containing items (gapped, within window), looking
// only at sessions that hold every one of its labels.
func supportIn(seqs [][]int, index map[int]map[int]bool, items []int, window int) int {
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
		if matchAt(seqs[s], items, window) != nil {
			n++
		}
	}
	return n
}

// parallelFor runs f(0..n-1) on every CPU.
func parallelFor(n int, f func(int)) {
	var wg sync.WaitGroup
	next := make(chan int)
	for w := 0; w < runtime.GOMAXPROCS(0); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				f(i)
			}
		}()
	}
	for i := 0; i < n; i++ {
		next <- i
	}
	close(next)
	wg.Wait()
}

func (o Options) log(format string, a ...any) {
	if o.Progress != nil {
		fmt.Fprintf(o.Progress, "%s  "+format+"\n", append([]any{time.Now().Format("15:04:05")}, a...)...)
	}
}

// runCost is what one occurrence cost through the agent (the steps' shares
// of their turns) and what a primitive would save: everything except one
// turn's worth, the turn that calls the primitive. ok is false unless every
// step was measured.
func runCost(steps []Step) (run, saved Usage, ok bool) {
	// Only steps a primitive can replay count: an edit whose content was
	// decided per run, or the agent's own bookkeeping, stays with the agent,
	// and so does what it costs.
	turns := map[int]bool{}
	merged := 0
	for _, st := range steps {
		if !st.Measured {
			return Usage{}, Usage{}, false
		}
		if !replayable(st.Label) {
			continue
		}
		run = run.add(st.Tokens)
		turns[st.Turn] = true
		merged += max(st.Turns, 1) - 1
	}
	if len(turns) == 0 {
		return Usage{}, Usage{}, false
	}
	one := run.scale(1 / float64(len(turns)+merged))
	return run, Usage{run.Fresh - one.Fresh, run.Cached - one.Cached, run.Output - one.Output}, true
}
