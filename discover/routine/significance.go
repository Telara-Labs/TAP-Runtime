package routine

import (
	"math"
	"math/rand"
	"sort"

	"github.com/Telara-Labs/TAP-Runtime/discover/model"
)

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
