package retrieval

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"

	"github.com/Telara-Labs/TAP-Runtime/discover/model"
)

// GroupLogicFunnels condenses result-dependent branches across candidate
// records. One create_issue result feeding transitions, links and comments is
// one observed funnel. A repeated branch over distinct sourced items is
// represented as for_each, regardless of the number of observed items. The
// input spans remain available through candidate IDs for contract review.
func GroupLogicFunnels(candidates []model.LogicCandidate, spans []model.SpanProposal) []model.LogicFunnel {
	bySpan := make(map[string]model.SpanProposal, len(spans))
	for _, p := range spans {
		bySpan[p.ID] = p
	}
	type branchState struct {
		slots      map[string]bool
		sessions   map[string]bool
		candidates map[string]bool
		forEach    bool
	}
	type funnelState struct {
		sessions   map[string]bool
		members    map[string]bool
		candidates map[string]bool
		branches   map[string]*branchState
	}
	byRoot := map[string]*funnelState{}
	for _, c := range candidates {
		for _, member := range c.Members {
			p, ok := bySpan[member]
			if !ok {
				continue
			}
			session := p.Client + "/" + p.Session
			for _, edge := range p.Composition.Edges {
				root, action, slot, ok := LogicEdge(edge)
				if !ok || root == action {
					continue
				}
				f := byRoot[root]
				if f == nil {
					f = &funnelState{sessions: map[string]bool{}, members: map[string]bool{}, candidates: map[string]bool{}, branches: map[string]*branchState{}}
					byRoot[root] = f
				}
				b := f.branches[action]
				if b == nil {
					b = &branchState{slots: map[string]bool{}, sessions: map[string]bool{}, candidates: map[string]bool{}}
					f.branches[action] = b
				}
				f.sessions[session], f.members[p.ID], f.candidates[c.ID] = true, true, true
				b.sessions[session], b.candidates[c.ID], b.slots[slot] = true, true, true
				for _, repeat := range p.Composition.Repetition {
					if LogicRole(repeat.Action) == action && repeat.Kind == "for_each" {
						b.forEach = true
					}
				}
			}
		}
	}
	var out []model.LogicFunnel
	for root, f := range byRoot {
		loop := false
		for _, b := range f.branches {
			loop = loop || b.forEach
		}
		if len(f.branches) < 2 && !loop {
			continue
		}
		h := sha256.Sum256([]byte(root))
		g := model.LogicFunnel{ID: "lf_" + hex.EncodeToString(h[:6]), Root: root, Sessions: len(f.sessions), CandidateIDs: LogicSortedKeys(f.candidates), Members: LogicSortedKeys(f.members)}
		for action, b := range f.branches {
			g.Branches = append(g.Branches, model.LogicFunnelBranch{Action: action, Slots: LogicSortedKeys(b.slots), Sessions: len(b.sessions), ForEach: b.forEach, CandidateIDs: LogicSortedKeys(b.candidates)})
		}
		sort.Slice(g.Branches, func(i, j int) bool {
			if g.Branches[i].Sessions != g.Branches[j].Sessions {
				return g.Branches[i].Sessions > g.Branches[j].Sessions
			}
			return g.Branches[i].Action < g.Branches[j].Action
		})
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sessions != out[j].Sessions {
			return out[i].Sessions > out[j].Sessions
		}
		if len(out[i].Branches) != len(out[j].Branches) {
			return len(out[i].Branches) > len(out[j].Branches)
		}
		return out[i].Root < out[j].Root
	})
	return out
}

func LogicEdge(edge string) (root, action, slot string, ok bool) {
	parts := strings.SplitN(edge, " -> ", 2)
	if len(parts) != 2 {
		return "", "", "", false
	}
	target, slot, ok := strings.Cut(parts[1], " (")
	if !ok || !strings.HasSuffix(slot, ")") {
		return "", "", "", false
	}
	return LogicRole(parts[0]), LogicRole(target), strings.TrimSuffix(slot, ")"), true
}

func LogicSortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// GroupLogicCandidates ignores concrete argument and result values. Recurrence
// is across sessions, disjoint executions within a session, or a loop/program
// actually run more than once inside one selected span. No user-task completion
// or fixed input/output value is required for nomination.
func GroupLogicCandidates(ps []model.SpanProposal) []model.LogicCandidate {
	by := map[string][]model.SpanProposal{}
	for _, p := range ps {
		if !LogicHasWork(p) {
			continue
		}
		key, _, _ := ProposalLogicShape(p)
		by[key] = append(by[key], p)
	}
	out := make([]model.LogicCandidate, 0)
	for key, members := range by {
		sort.Slice(members, func(i, j int) bool {
			a, b := members[i], members[j]
			if a.Client != b.Client {
				return a.Client < b.Client
			}
			if a.Session != b.Session {
				return a.Session < b.Session
			}
			if a.Request != b.Request {
				return a.Request < b.Request
			}
			if len(a.Calls) != len(b.Calls) {
				return len(a.Calls) > len(b.Calls)
			}
			return a.ID < b.ID
		})
		_, actions, edges := ProposalLogicShape(members[0])
		h := sha256.Sum256([]byte(key))
		g := model.LogicCandidate{ID: "lc_" + hex.EncodeToString(h[:6]), Key: key, Actions: actions, Edges: edges, Proposals: len(members), Example: members[0]}
		sessions := map[string]bool{}
		usedCalls := map[string]bool{}
		params, cautions := map[string]bool{}, map[string]bool{}
		loops, authoredRepeated := false, false
		codeShapes := map[string]bool{}
		for _, p := range members {
			session := p.Client + "/" + p.Session
			sessions[session] = true
			g.Members = append(g.Members, p.ID)
			if p.Review.Source == "user" && g.Example.Review.Source != "user" ||
				p.Review.Source == g.Example.Review.Source && p.EvidenceScore > g.Example.EvidenceScore {
				g.Example = p
			}
			overlaps := false
			for _, call := range p.Calls {
				if usedCalls[LogicCallID(p, call)] {
					overlaps = true
					break
				}
			}
			if !overlaps {
				g.Executions++
				for _, call := range p.Calls {
					usedCalls[LogicCallID(p, call)] = true
				}
			}
			for _, in := range p.Inputs {
				if in.Source != "prior_result" || in.FromCall == 0 {
					params[in.Key+":"+in.Type] = true
				}
			}
			if p.Kind == "authored_program" && p.CodeShape == "" {
				authoredRepeated = true
			}
			if p.CodeShape != "" {
				codeShapes[p.CodeShape] = true
				cautions["inline_code_family_needs_data_flow_proof"] = true
			}
			if p.CodeScope == "embedded" {
				cautions["inline_program_embedded_in_shell"] = true
			}
			if len(p.Composition.Repetition) > 0 {
				loops = true
			}
			if p.Review.Source != "user" && p.Review.Source != "scheduled" {
				cautions["source_role_uncertain"] = true
			}
			for _, reason := range p.Review.Reasons {
				switch reason {
				case "failed_or_oversized_call", "stop_condition_unproven", "input_provenance_unknown":
					cautions[reason] = true
				}
			}
		}
		g.Sessions = len(sessions)
		if g.Sessions >= 2 {
			g.Evidence = append(g.Evidence, "cross_session")
		}
		if g.Executions >= 2 {
			g.Evidence = append(g.Evidence, "disjoint_executions")
		}
		if loops {
			g.Evidence = append(g.Evidence, "observed_repetition")
		}
		if authoredRepeated {
			g.Evidence = append(g.Evidence, "authored_program_reused")
		}
		if len(g.Evidence) == 0 {
			continue
		}
		if len(codeShapes) > 1 {
			cautions["inline_code_shape_variants"] = true
		}
		for p := range params {
			g.Parameters = append(g.Parameters, p)
		}
		for c := range cautions {
			g.Cautions = append(g.Cautions, c)
		}
		sort.Strings(g.Parameters)
		sort.Strings(g.Cautions)
		sort.Strings(g.Members)
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sessions != out[j].Sessions {
			return out[i].Sessions > out[j].Sessions
		}
		if out[i].Executions != out[j].Executions {
			return out[i].Executions > out[j].Executions
		}
		if len(out[i].Actions) != len(out[j].Actions) {
			return len(out[i].Actions) > len(out[j].Actions)
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func LogicCallID(p model.SpanProposal, call int) string {
	return p.Client + "/" + p.Session + "/" + strconv.Itoa(p.Request) + "/" + strconv.Itoa(call)
}

func LogicHasWork(p model.SpanProposal) bool {
	c := p.Composition
	if p.Kind == "authored_program" || len(c.Repetition) > 0 || len(c.Edges) > 0 || len(c.Actions) >= 2 {
		return true
	}
	return len(c.Actions) == 1 && strings.Contains(c.Actions[0], "+")
}

func ProposalLogicShape(p model.SpanProposal) (string, []string, []string) {
	if p.CodeFamily != "" {
		_, operations, _ := strings.Cut(p.CodeFamily, ":")
		return "inline_python_family=" + p.CodeFamily, []string{"python.inline " + strings.ReplaceAll(operations, ">", " -> ")}, nil
	}
	return LogicShape(p.Composition)
}

func LogicRole(role string) string {
	if i := strings.IndexByte(role, '['); i >= 0 {
		return role[:i]
	}
	return role
}

func LogicShape(c model.SpanComposition) (string, []string, []string) {
	actions := make([]string, 0, len(c.Actions))
	for _, action := range c.Actions {
		actions = append(actions, LogicRole(action))
	}
	edgeSet := map[string]bool{}
	for _, edge := range c.Edges {
		parts := strings.SplitN(edge, " -> ", 2)
		if len(parts) != 2 {
			continue
		}
		target, slot, ok := strings.Cut(parts[1], " (")
		if !ok {
			continue
		}
		edgeSet[LogicRole(parts[0])+" -> "+LogicRole(target)+" ("+slot] = true
	}
	edges := make([]string, 0, len(edgeSet))
	for edge := range edgeSet {
		edges = append(edges, edge)
	}
	sort.Strings(edges)
	// Cardinality is evidence about a run, not primitive identity. A
	// one-item create/link and the same flow over three items share code.
	// Non-foldable repeats (same-target retries or state changes) remain
	// distinct because their ordered actions are still present above.
	key := "actions=" + strings.Join(actions, " -> ") + "|edges=" + strings.Join(edges, ";")
	return key, actions, edges
}
