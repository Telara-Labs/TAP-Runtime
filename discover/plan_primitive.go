package discover

import (
	"sort"
	"strings"

	"gitlab.com/telara-labs/tap-runtime/discover/codegen"
	"gitlab.com/telara-labs/tap-runtime/discover/primitive"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// planPrimitiveFamilies checks the multi-continuation API before the menu is
// drawn. The same branch compiler is used on Accept. Patterns that cannot
// compile remain visible for refinement, but cannot be presented as an API.
func planPrimitiveFamilies(res *primitive.Result, sessions []trace.Session) {
	cp := make([]trace.Session, len(sessions))
	for i, s := range sessions {
		s.Calls = append([]trace.Call(nil), s.Calls...)
		cp[i] = s
	}
	trace.DropCopiedCalls(cp)
	bySession := map[string]*trace.Session{}
	for i := range cp {
		bySession[cp[i].Client+"\x00"+cp[i].ID] = &cp[i]
	}
	byID := map[string]primitive.Primitive{}
	for _, p := range res.Primitives {
		byID[p.ID] = p
	}
	for i := range res.Families {
		f := &res.Families[i]
		for j := range f.FollowUps {
			fu := &f.FollowUps[j]
			if len(fu.Members) == 0 {
				continue
			}
			fu.APIMode = "exact_chain"
			for _, id := range fu.Members {
				mode, why := assessContinuation(byID[id], bySession)
				if mode != "exact_chain" {
					fu.APIMode, fu.APIReason = mode, why
					break
				}
			}
		}
		if len(f.FollowUps) <= 1 {
			f.APIMode = "exact_flow"
			continue
		}
		members := make([]primitive.Primitive, 0, len(f.Members))
		for _, id := range f.Members {
			members = append(members, byID[id])
		}
		g, reason := branchGraph(*f, members, bySession)
		if g != nil {
			if _, err := codegen.GenerateProgramPackage(g); err != nil {
				reason = err.Error()
			} else {
				f.APIMode = "caller_choice"
				for _, in := range g.Inputs {
					if in.Name == "action" {
						f.APIChoices = append([]string(nil), in.Allowed...)
						break
					}
				}
				continue
			}
		}
		f.APIMode = "needs_refinement"
		f.APIReason = reason
	}
	// Put flows with a defined interface before unresolved patterns, while
	// retaining Discover's existing ranking within each group.
	sort.SliceStable(res.Families, func(i, j int) bool {
		a, b := res.Families[i].APIMode != "needs_refinement", res.Families[j].APIMode != "needs_refinement"
		return a && !b
	})
}

func assessContinuation(p primitive.Primitive, by map[string]*trace.Session) (string, string) {
	if len(p.Steps) != 2 {
		return "needs_refinement", "this continuation contains more than one downstream operation"
	}
	if len(p.Loops) > 0 {
		return "needs_refinement", "this continuation repeats over a result and needs a selection rule"
	}
	if len(p.Unresolved) > 0 {
		for _, issue := range p.Unresolved {
			if strings.Contains(issue, "source ambiguous") || strings.Contains(issue, "disagree with the majority source") {
				return "needs_refinement", issue
			}
		}
		return "needs_refinement", p.Unresolved[0]
	}
	if p.Confidence.Readiness == "needs_decision" {
		return "needs_refinement", "its recorded arguments need a decision"
	}
	g, why := directGraph(p, by)
	if g == nil {
		if strings.HasPrefix(why, "fallback:") {
			return "synthesis_pending", "the exact chain needs the shell or authored-code synthesizer"
		}
		return "needs_refinement", why
	}
	if _, err := codegen.GenerateProgramPackage(g); err != nil {
		return "needs_refinement", err.Error()
	}
	return "exact_chain", ""
}
