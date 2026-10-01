package discover

import (
	"sort"

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
