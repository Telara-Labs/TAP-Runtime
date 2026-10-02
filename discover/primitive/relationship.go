package primitive

import (
	"fmt"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// RelationshipEvidence measures whether a continuation really acts on the
// head created or found in the same request: a follow-up step reads the
// head's result, and every follow-up step reads an earlier step's result or
// has its own result read by a later one. A high frequency of calls never
// substitutes for a structured result binding. The score describes recorded
// support, not a probability of correctness.
func RelationshipEvidence(p Primitive, by map[string]*trace.Session) (score int, support, reason string) {
	if len(p.Steps) < 2 {
		return 0, "0/0", "the continuation has no downstream operation"
	}
	total, linked, sameRequest := 0, 0, 0
	for _, ex := range p.Executions {
		if ex.Overlaps != "" {
			continue
		}
		total++
		s := by[ex.Client+"\x00"+ex.Session]
		if s == nil || len(ex.Calls) < 2 {
			continue
		}
		within := true
		for _, c := range ex.Calls {
			if c.Index < 0 || c.Index >= len(s.Calls) || s.Calls[c.Index].Request != ex.Request {
				within = false
				break
			}
		}
		if !within {
			continue
		}
		sameRequest++
		head := false
		connected := map[int]bool{}
		for _, o := range ex.Observed {
			if o.Source != "step" || o.Label != Explicit || o.Selector == "" || o.From < 1 || o.From >= o.Step {
				continue
			}
			head = head || o.From == 1
			connected[o.Step] = true
			if o.From > 1 {
				connected[o.From] = true
			}
		}
		bound := head
		for st := 2; st <= len(p.Steps); st++ {
			bound = bound && connected[st]
		}
		if bound {
			linked++
		}
	}
	if total == 0 {
		return 0, "0/0", "no usable recorded executions"
	}
	score = 100 * linked / total
	support = fmt.Sprintf("%d/%d explicit same-request head-result bindings", linked, total)
	switch {
	case sameRequest < total:
		reason = fmt.Sprintf("%d of %d executions cross a request boundary or have unavailable calls", total-sameRequest, total)
	case linked < total:
		reason = fmt.Sprintf("%d of %d executions do not bind a structured head result to the follow-up", total-linked, total)
	}
	return score, support, reason
}
