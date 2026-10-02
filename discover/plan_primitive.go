package discover

import (
	"fmt"
	"os"
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
		members := make([]primitive.Primitive, 0, len(f.Members))
		for _, id := range f.Members {
			members = append(members, byID[id])
		}
		if len(f.FollowUps) <= 1 {
			f.APIMode = "exact_flow"
			if len(members) > 0 {
				main := members[0]
				for _, m := range members[1:] {
					if m.ExecutionCount > main.ExecutionCount {
						main = m
					}
				}
				switch mode, why := assessContinuation(main, bySession); mode {
				case "needs_refinement":
					f.APIMode, f.APIReason = mode, why
				case "synthesis_pending":
					// Shell and authored-code flows go through the older
					// synthesizer: build once into a throwaway folder, so
					// review only offers what Accept can install.
					if why := dryInstall(*f, members, sessions); why != "" {
						f.APIMode, f.APIReason = "needs_refinement", why
					}
				}
			}
			continue
		}
		keep, kept, left := executableFamily(*f, members, bySession)
		if len(keep.FollowUps) == 0 {
			f.APIMode, f.APIReason = "needs_refinement", "none of its continuations compiles as recorded"
			continue
		}
		if len(keep.FollowUps) == 1 {
			f.APIMode = "exact_flow"
			f.APIReason = fmt.Sprintf("installs %s; %d other continuation(s) need refinement", strings.Join(keep.FollowUps[0].Steps, " > "), len(left))
			continue
		}
		g, reason := branchGraph(keep, kept, bySession)
		if g != nil {
			if _, err := codegen.GenerateProgramPackage(g); err != nil {
				reason = err.Error()
			} else {
				f.APIMode = "caller_choice"
				if len(left) > 0 {
					f.APIReason = fmt.Sprintf("%d continuation(s) need refinement and are not included", len(left))
				}
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

// continuationIssue says why a continuation cannot be compiled as recorded,
// or "". Its open decisions are the confidence report's: a value taken from
// different steps, a list selection with no consistent position. A repeat
// over a result is compiled with caller-picked positions, and notes such as
// long gaps or values only mentioned in text (caller inputs by rule) do not
// block.
func continuationIssue(p primitive.Primitive) string {
	if len(p.Steps) != 2 {
		return "this continuation contains more than one downstream operation"
	}
	if p.Confidence.Readiness == "needs_decision" {
		if len(p.Confidence.NeedsReview) > 0 {
			return p.Confidence.NeedsReview[0]
		}
		return "its recorded arguments need a decision"
	}
	return ""
}

// executableFamily keeps the continuations that compile as recorded and
// names the ones left out. Planning and installing use it alike, so what
// review promises is what Accept installs.
func executableFamily(f primitive.Family, members []primitive.Primitive, by map[string]*trace.Session) (primitive.Family, []primitive.Primitive, []string) {
	byID := map[string]primitive.Primitive{}
	for _, p := range members {
		byID[p.ID] = p
	}
	keep := f
	keep.FollowUps = nil
	var kept []primitive.Primitive
	var left []string
	for _, fu := range f.FollowUps {
		ok := len(fu.Members) > 0
		for _, id := range fu.Members {
			if mode, _ := assessContinuation(byID[id], by); mode != "exact_chain" {
				ok = false
				break
			}
		}
		if !ok {
			left = append(left, strings.Join(fu.Steps, " > "))
			continue
		}
		keep.FollowUps = append(keep.FollowUps, fu)
		for _, id := range fu.Members {
			kept = append(kept, byID[id])
		}
	}
	return keep, kept, left
}

func assessContinuation(p primitive.Primitive, by map[string]*trace.Session) (string, string) {
	if why := continuationIssue(p); why != "" {
		return "needs_refinement", why
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

// dryInstall builds a family exactly as Accept would, into a folder that is
// removed afterwards; it returns why nothing would install, or "".
func dryInstall(f primitive.Family, members []primitive.Primitive, sessions []trace.Session) string {
	dir, err := os.MkdirTemp("", "tap-discover-plan")
	if err != nil {
		return ""
	}
	defer os.RemoveAll(dir)
	r, err := primitiveInstaller(sessions, "claude-code", dir, dir)(f, members)
	if err != nil {
		return err.Error()
	}
	if !r.Installed {
		return r.Reason
	}
	return ""
}
