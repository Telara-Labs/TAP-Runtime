// Package pipeline runs a discover pass end to end: read the history, mine and
// score patterns, group them into candidates, and assemble the report.
package pipeline

import (
	"fmt"
	"math"
	"math/rand"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/model"
	"gitlab.com/telara-labs/tap-runtime/discover/retrieval"
	"gitlab.com/telara-labs/tap-runtime/discover/routine"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
	"gitlab.com/telara-labs/tap-runtime/discover/util"
)

// RulesVersion changes whenever a rule changes, so two reports are only
// compared when they were produced by the same rules.
const RulesVersion = "tap-discover/4"

// DefaultOptions are the rules RulesVersion names.
func DefaultOptions() model.Options {
	return model.Options{Window: 8, MinSupport: 3, MaxLen: 5, MaxPatterns: 2000000, PerSkill: 10, Permutations: 20, Alpha: 0.05, Seed: 1, Now: time.Now}
}

// Run reads every client, mines, tests and ranks. It makes no network call
// and writes nothing.
func Run(o model.Options) (*model.Report, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	rep := &model.Report{RulesVersion: RulesVersion, GeneratedAt: o.Now().UTC(), Options: model.OptionsOut{Since: o.Since, Window: o.Window, MinSupport: o.MinSupport, MaxLen: o.MaxLen, MaxPatterns: o.MaxPatterns, Permutations: o.Permutations, Alpha: o.Alpha, Seed: o.Seed, Spans: o.Spans}}

	var raw []trace.Session
	for _, r := range o.Readers {
		st := model.ClientStats{Client: r.Client()}
		var ss []trace.Session
		var err error
		if sr, ok := r.(trace.StatReader); ok {
			var rs trace.ReadStats
			ss, rs, err = sr.ReadWithStats(o.Since)
			st.UnreadableFiles = rs.UnreadableFiles
		} else {
			ss, err = r.Read(o.Since)
		}
		if err != nil {
			st.Error = err.Error()
		}
		for _, s := range ss {
			st.Calls += len(s.Calls)
			st.SkippedRecords += s.Skipped
		}
		raw = append(raw, ss...)
		rep.Clients = append(rep.Clients, st)
		OptionsLog(o, "%s: %d sessions, %d calls %s", st.Client, len(ss), st.Calls, st.Error)
	}
	OptionsLog(o, "read %d sessions", len(raw))
	// Later passes take sessions in order (grouping picks each group's first
	// request), so the report must not depend on the order readers return.
	sort.SliceStable(raw, func(i, j int) bool {
		a, b := raw[i], raw[j]
		if a.Client != b.Client {
			return a.Client < b.Client
		}
		if !a.Start.Equal(b.Start) {
			return a.Start.Before(b.Start)
		}
		return a.ID < b.ID
	})
	trace.DropCopiedCalls(raw)
	for _, op := range retrieval.SelectOpportunities(raw) {
		if op.Recommended {
			rep.Opportunities = append(rep.Opportunities, op)
		}
	}
	rep.OpportunityGroups = retrieval.GroupOpportunities(rep.Opportunities)
	if o.Spans {
		rep.SpanProposals = retrieval.SelectSpanProposals(raw)
		rep.SpanGroups = retrieval.GroupSpanProposals(rep.SpanProposals)
		rep.CompositionGroups = retrieval.GroupSpanCompositions(rep.SpanProposals)
		rep.LogicCandidates = retrieval.GroupLogicCandidates(rep.SpanProposals)
		rep.LogicFunnels = retrieval.GroupLogicFunnels(rep.LogicCandidates, rep.SpanProposals)
		rep.ReviewSpans = retrieval.ReviewSpanProposals(rep.SpanProposals)
		rep.ReviewGroups = retrieval.GroupSpanCompositions(rep.ReviewSpans)
		rep.ComponentSpans = retrieval.ReviewSpanComponents(rep.SpanProposals)
		rep.ComponentGroups = retrieval.GroupSpanCompositions(rep.ComponentSpans)
	}
	sessions := trace.Normalize(raw)

	// Identical step sequences are one piece of work run twice (a replayed
	// test harness, a re-sent prompt), not recurrence.
	seen := map[string]bool{}
	var corpus []trace.NormSession
	stats := map[string]*model.ClientStats{}
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
			rep.Clients = append(rep.Clients, model.ClientStats{Client: s.Client})
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
	rep.Funnel, rep.Routines = routine.RequestRoutines(sessions, ids, names, o, totalCalls)
	OptionsLog(o, "requests: %d with replayable steps, %d routines, %d primitives", rep.Funnel.RequestsWithSteps, rep.Funnel.Routines, rep.Funnel.Primitives)
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
	index := model.LabelIndex(seqs)
	keep := func(items []int, k, nPrefix int) bool {
		// The last step first: exact and free (the prefix's support is known).
		if routine.BinomialUpper(k, max(nPrefix, k), share[items[len(items)-1]]) > o.Alpha {
			return false
		}
		// Then every other step, stopping at the first that fails.
		return NecessityP(items, k, seqs, index, df, share, o.Window, o.Alpha) <= o.Alpha
	}
	mined, examined, truncated := routine.MinePatterns(seqs, routine.MineLimits{Window: o.Window, MinSupport: o.MinSupport, MaxLen: o.MaxLen, MaxOut: o.MaxPatterns, CanQualify: canQualify, Keep: keep})
	rep.Mined, rep.Examined, rep.Truncated, rep.MinSupportUsed = len(mined), examined, truncated, o.MinSupport
	OptionsLog(o, "examined %d patterns at support >= %d over %d sessions and %d labels; %d kept", examined, o.MinSupport, len(seqs), len(names), len(mined))

	idf := make([]float64, len(names))
	for x, d := range df {
		idf[x] = math.Log(float64(len(seqs)) / float64(d))
	}

	// Every step must be necessary (see necessity), with FDR over all
	// patterns examined. Only patterns passing it go on to the shuffle tests,
	// which cost a pass over the corpus per permutation.
	closed := routine.ClosedOnly(mined)
	pNeedAll := make([]float64, len(closed))
	util.ParallelFor(len(closed), func(i int) {
		pNeedAll[i] = Necessity(closed[i], seqs, index, df, share, o.Window)
	})
	qNeedAll := routine.BenjaminiHochbergOf(pNeedAll, examined)
	var cands []model.Pattern
	var qNeed []float64
	for i, p := range closed {
		if qNeedAll[i] <= o.Alpha {
			cands = append(cands, p)
			qNeed = append(qNeed, qNeedAll[i])
		}
	}
	rep.Tested = len(cands)
	OptionsLog(o, "%d closed patterns; %d pass the per-step test and go to %d permutations per shuffle test", len(closed), len(cands), o.Permutations)

	across := PermuteCounts(seqs, group, cands, o, routine.ShuffleAcross)
	OptionsLog(o, "between-session null done")
	within := PermuteCounts(seqs, group, cands, o, routine.ShuffleWithin)
	OptionsLog(o, "within-session null done")
	pAcross := make([]float64, len(cands))
	pWithin := make([]float64, len(cands))
	for i, p := range cands {
		pAcross[i] = routine.PValue(len(p.Sessions), across[i])
		pWithin[i] = routine.PValue(len(p.Sessions), within[i])
	}
	qAcross := routine.BenjaminiHochberg(pAcross)
	qWithin := routine.BenjaminiHochberg(pWithin)

	for i, p := range cands {
		c := model.Describe(p, corpus, seqs, names, idf, o.Window)
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
	rep.Recall = Recall(corpus, rep.Candidates)
	rep.Skills = routine.SkillProcedures(corpus, seqs, closed, names, idf, o)
	rep.Corpus, rep.Seqs, rep.Names, rep.Window = corpus, seqs, names, o.Window
	OptionsLog(o, "skill comparison: %d skills with enriched procedures", len(rep.Skills))
	rep.Families = AssignFamilies(rep.Candidates)
	return rep, nil
}

func PermuteCounts(seqs [][]int, group []int, ps []model.Pattern, o model.Options, permute func([][]int, []int, *rand.Rand) [][]int) [][]int {
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
			counts := routine.NullSupport(permute(seqs, group, rng), ps, o.Window)
			for i, n := range counts {
				out[i][k] = n
			}
		}(k)
	}
	wg.Wait()
	return out
}

// Recall scores the miner against work known to recur: every skill loaded in
// at least two sessions. For each, the candidate whose sessions best match the
// skill's sessions (F1) is reported, among qualified and among all tested.
func Recall(corpus []trace.NormSession, cands []model.Candidate) []model.SkillRecall {
	skillSessions := map[string]map[int]bool{}
	for i, s := range corpus {
		for sk := range s.Skills {
			if skillSessions[sk] == nil {
				skillSessions[sk] = map[int]bool{}
			}
			skillSessions[sk][i] = true
		}
	}
	var out []model.SkillRecall
	for sk, truth := range skillSessions {
		if len(truth) < 2 {
			continue
		}
		r := model.SkillRecall{Skill: sk, Sessions: len(truth)}
		for _, c := range cands {
			inter := 0
			for s := range c.SessionSet {
				if truth[s] {
					inter++
				}
			}
			if inter == 0 {
				continue
			}
			m := model.SkillMatch{Pattern: model.LabelsOf(c), Qualified: c.Qualified, Precision: float64(inter) / float64(len(c.SessionSet)), Recall: float64(inter) / float64(len(truth))}
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

// AssignFamilies groups qualified candidates that describe the same work: a
// candidate joins the family of a higher-ranked one when they share at least
// half their sessions and half their labels (Jaccard). This only folds the
// report; it does not change what qualified. It returns the family count.
func AssignFamilies(cs []model.Candidate) int {
	var reps []int
	for i := range cs {
		cs[i].Family = i
		if !cs[i].Qualified {
			continue
		}
		joined := false
		for _, r := range reps {
			if model.JaccardInts(cs[i].SessionSet, cs[r].SessionSet) >= 0.5 && JaccardStrings(LabelSet(cs[i]), LabelSet(cs[r])) >= 0.5 {
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

func LabelSet(c model.Candidate) map[string]bool {
	m := map[string]bool{}
	for _, s := range c.Steps {
		m[s.Label] = true
	}
	return m
}

func JaccardStrings(a, b map[string]bool) float64 {
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}

// Necessity asks, for each step x of a pattern, whether the pattern holds in
// more of the sessions that hold the pattern without x than x's own
// frequency explains: a one-sided binomial test with n = support without x,
// k = support with x, and p = the largest share of sessions containing x in
// any one client. That p
// ignores position and so overstates chance, which makes the test
// conservative. The largest p over the steps is returned: every step must be
// necessary. Without this, any significant core plus one unrelated common
// step would qualify on the core's strength.
func Necessity(p model.Pattern, seqs [][]int, index map[int]map[int]bool, df []int, share []float64, window int) float64 {
	return NecessityP(p.Items, len(p.Sessions), seqs, index, df, share, window, 1)
}

// NecessityP is necessity for items with support k. It returns as soon as a
// step's p exceeds stopAbove (pass 1 to compute the exact maximum).
func NecessityP(items []int, k int, seqs [][]int, index map[int]map[int]bool, df []int, share []float64, window int, stopAbove float64) float64 {
	if len(items) < 2 {
		return 1
	}
	worst := 0.0
	for i, x := range items {
		rest := append(append([]int{}, items[:i]...), items[i+1:]...)
		n := k
		if len(rest) > 1 {
			n = model.SupportIn(seqs, index, rest, window*2)
		} else {
			n = df[rest[0]]
		}
		pv := routine.BinomialUpper(k, max(n, k), share[x])
		if pv > worst {
			worst = pv
			if worst > stopAbove {
				return worst
			}
		}
	}
	return worst
}

func OptionsLog(o model.Options, format string, a ...any) {
	if o.Progress != nil {
		fmt.Fprintf(o.Progress, "%s  "+format+"\n", append([]any{time.Now().Format("15:04:05")}, a...)...)
	}
}
