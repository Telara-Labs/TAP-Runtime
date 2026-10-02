package primitive

import (
	"fmt"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// RelationshipEvidence measures whether a two-step continuation really acts
// on the head created or found in the same request. A high frequency of calls
// never substitutes for a structured result binding. The score describes
// recorded support, not a probability of correctness.
func RelationshipEvidence(p Primitive, by map[string]*trace.Session) (score int, support, reason string) {
	if len(p.Steps) != 2 {
		return 0, "0/0", "the continuation has more than one downstream operation"
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
		bound := false
		for _, o := range ex.Observed {
			if o.Step == 2 && o.Source == "step" && o.From == 1 && o.Label == Explicit && o.Selector != "" {
				bound = true
			}
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
