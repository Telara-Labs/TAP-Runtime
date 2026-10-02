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
// drawn. The same bundle compiler is used on Accept. Patterns that cannot
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
		f.RelationshipConfidence = 100
		for j := range f.FollowUps {
			fu := &f.FollowUps[j]
			fu.RelationshipScore = 100
			fu.RelationshipSupport = "0/0"
			fu.ShapeScore = 100
			fu.ShapeSupport = "not assessed"
			for _, id := range fu.Members {
				score, support, why := primitive.RelationshipEvidence(byID[id], bySession)
				if fu.RelationshipSupport == "0/0" || score < fu.RelationshipScore {
					fu.RelationshipScore, fu.RelationshipSupport = score, support
				}
				if why != "" && fu.APIReason == "" {
					fu.APIReason = why
				}
			}
			if len(fu.Members) == 0 {
				fu.RelationshipScore = 0
				fu.ShapeScore = 0
				fu.Confidence = 0
				continue
			}
			fu.APIMode = "exact_chain"
			if fu.RelationshipScore < 100 && len(f.FollowUps) > 1 {
				fu.APIMode = "needs_refinement"
				continue
			}
			if len(f.FollowUps) > 1 {
				var one []primitive.Primitive
				for _, id := range fu.Members {
					one = append(one, byID[id])
				}
				if len(one) == 1 {
					_, kept, total := modalFollowUpShape(one[0], bySession, headRoute(one, bySession))
					if total > 0 {
						fu.ShapeScore = 100 * kept / total
						fu.ShapeSupport = fmt.Sprintf("%d/%d uses share the selected tool route; optional arguments are checked separately", kept, total)
					}
					if kept < total {
						fu.APIReason = fmt.Sprintf("%d of %d uses have another tool route and are excluded from this API", total-kept, total)
					}
				}
				oneFamily := *f
				oneFamily.FollowUps = []primitive.FollowUp{*fu}
				g, why := bundleGraph(oneFamily, one, bySession)
				if g != nil {
					if _, err := codegen.GenerateProgramPackage(g); err != nil {
						why = err.Error()
					}
				}
				if why != "" {
					fu.APIMode, fu.APIReason = "needs_refinement", why
				}
				continue
			}
			for _, id := range fu.Members {
				mode, why := assessContinuation(byID[id], bySession)
				if mode != "exact_chain" {
					fu.APIMode, fu.APIReason = mode, why
					break
				}
			}
		}
		for j := range f.FollowUps {
			fu := &f.FollowUps[j]
			if fu.ShapeScore < fu.RelationshipScore {
				fu.Confidence = fu.ShapeScore
			} else {
				fu.Confidence = fu.RelationshipScore
			}
			for _, id := range fu.Members {
				if c := byID[id].Confidence; c.Rubric != "" && c.Overall < fu.Confidence {
					fu.Confidence = c.Overall
				}
			}
		}
		for _, fu := range f.FollowUps {
			if fu.RelationshipScore < f.RelationshipConfidence {
				f.RelationshipConfidence = fu.RelationshipScore
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
		f.APIConfidence = 100
		if len(keep.FollowUps) == 0 {
			f.APIConfidence = 0
		}
		for _, fu := range keep.FollowUps {
			if fu.Confidence < f.APIConfidence {
				f.APIConfidence = fu.Confidence
			}
		}
		for j := range f.FollowUps {
			prefix := strings.Join(f.FollowUps[j].Steps, " > ") + ": "
			for _, omitted := range left {
				if strings.HasPrefix(omitted, prefix) {
					f.FollowUps[j].APIMode = "needs_refinement"
					f.FollowUps[j].APIReason = strings.TrimPrefix(omitted, prefix)
				}
			}
		}
		if len(keep.FollowUps) == 0 {
			f.APIMode, f.APIReason = "needs_refinement", "none of its continuations compiles as recorded"
			continue
		}
		g, reason := bundleGraph(keep, kept, bySession)
		if g != nil {
			if _, err := codegen.GenerateProgramPackage(g); err != nil {
				reason = err.Error()
			} else {
				f.APIMode = "optional_followups"
				if len(left) > 0 {
					f.APIReason = fmt.Sprintf("%d continuation(s) need refinement and are not included", len(left))
				}
				for _, in := range g.Inputs {
					if in.Optional && in.List {
						f.APIInputs = append(f.APIInputs, in.Name)
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
			if score, _, _ := primitive.RelationshipEvidence(byID[id], by); score != 100 {
				ok = false
				break
			}
		}
		if fu.APIMode != "exact_chain" {
			ok = false
		}
		if !ok {
			left = append(left, strings.Join(fu.Steps, " > "))
			continue
		}
		candidate := keep
		candidate.FollowUps = append(append([]primitive.FollowUp(nil), keep.FollowUps...), fu)
		candidateMembers := append([]primitive.Primitive(nil), kept...)
		for _, id := range fu.Members {
			candidateMembers = append(candidateMembers, byID[id])
		}
		g, why := bundleGraph(candidate, candidateMembers, by)
		if g == nil {
			left = append(left, strings.Join(fu.Steps, " > ")+": "+why)
			continue
		}
		if _, err := codegen.GenerateProgramPackage(g); err != nil {
			left = append(left, strings.Join(fu.Steps, " > ")+": "+err.Error())
			continue
		}
		keep.FollowUps, kept = candidate.FollowUps, candidateMembers
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
