package routine

import (
	"sort"
	"sync"
	"sync/atomic"

	"github.com/Telara-Labs/TAP-Runtime/discover/model"
	"github.com/Telara-Labs/TAP-Runtime/discover/util"
)

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
