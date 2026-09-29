package discover

import (
	"math"
	"sort"
)

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

// skillProcedures compares, for every skill loaded in at least two sessions,
// the kept patterns' presence in that skill's sessions against sessions of
// the same client and similar length. It reuses the patterns the main search
// kept, so it adds no search.
func skillProcedures(corpus []normSession, seqs [][]int, ps []pattern, names []string, idf []float64, o Options) []SkillReport {
	skillSessions := map[string][]int{}
	for i, s := range corpus {
		for sk := range s.Skills {
			skillSessions[sk] = append(skillSessions[sk], i)
		}
	}
	// patternsOf[s] lists the patterns session s contains.
	patternsOf := make([][]int, len(corpus))
	for pi, p := range ps {
		for _, s := range p.sessions {
			patternsOf[s] = append(patternsOf[s], pi)
		}
	}
	// Sessions that load a skill differ from the rest in client and in
	// length (automation runs are long), and a long session contains more of
	// everything. So the comparison is stratified: sessions are grouped by
	// client and by length decile within that client, and the pattern's count
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
		next := 0
		for _, c := range clients {
			idx := byClient[c]
			sort.SliceStable(idx, func(a, b int) bool { return len(seqs[idx[a]]) < len(seqs[idx[b]]) })
			for r, i := range idx {
				stratum[i] = next + r*10/len(idx)
			}
			next += 10
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
		for _, s := range p.sessions {
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
			k := len(ps[pi].sessions)
			tests = append(tests, test{sk, pi, a, k - a, p})
		}
	}
	pv := make([]float64, len(tests))
	for i, t := range tests {
		pv[i] = t.p
	}
	q := benjaminiHochberg(pv)

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
	out := make([]SkillReport, len(skills))
	parallelFor(len(skills), func(si int) {
		sk := skills[si]
		ns := len(skillSessions[sk])
		ts := sig[sk]
		sort.SliceStable(ts, func(a, b int) bool {
			if ts[a].a != ts[b].a {
				return ts[a].a > ts[b].a
			}
			la, lb := len(ps[ts[a].pattern].items), len(ps[ts[b].pattern].items)
			if la != lb {
				return la > lb
			}
			return float64(ts[a].a)/float64(ts[a].c+1) > float64(ts[b].a)/float64(ts[b].c+1)
		})
		rep := SkillReport{Skill: sk, Sessions: ns, Significant: len(ts)}
		for _, t := range ts {
			if len(rep.Procedures) >= o.PerSkill {
				break
			}
			c := describe(ps[t.pattern], corpus, seqs, names, idf, o.Window)
			if c.Specificity == 0 {
				continue
			}
			lift := 0.0
			if outRate := float64(t.c) / float64(n-ns); outRate > 0 {
				lift = (float64(t.a) / float64(ns)) / outRate
			}
			c.Qualified = true
			c.sessionSet = nil
			rep.Procedures = append(rep.Procedures, SkillProcedure{Candidate: c, InSkill: t.a, Coverage: float64(t.a) / float64(ns), Outside: t.c, Lift: lift, OnlyInSkill: t.c == 0, EnrichmentQ: t.p})
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

// hypergeomUpper is the one-sided Fisher exact p-value: the chance that a
// random draw of n sessions out of total holds a or more of the k sessions
// containing the pattern, i.e. P(X >= a) for X ~ Hypergeometric(total, k, n).
func hypergeomUpper(a, k, n, total int) float64 {
	hi := min(k, n)
	if a > hi {
		return 0
	}
	lchoose := func(x, y int) float64 { return lgamma(float64(x)+1) - lgamma(float64(y)+1) - lgamma(float64(x-y)+1) }
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
