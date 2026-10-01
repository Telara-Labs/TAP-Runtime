// Package retrieval selects, groups and screens causal slices of recorded calls
// as candidates for human review.
package retrieval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gitlab.com/telara-labs/tap-runtime/discover/history"
	"gitlab.com/telara-labs/tap-runtime/discover/model"
	"gitlab.com/telara-labs/tap-runtime/discover/pyparse"
	"gitlab.com/telara-labs/tap-runtime/discover/redact"
	"gitlab.com/telara-labs/tap-runtime/discover/shellparse"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// SpanComposition keeps the operation's provider and explicit state selector,
// then records which selected operation supplied each result-derived input.
// Repeated independent calls and repeated whole motifs share the same action
// skeleton, while dependency edges keep a sequential state machine distinct.
func SpanComposition(nodes []SpanNode, set []int) model.SpanComposition {
	roles := make([]string, len(set))
	byOrdinal := make(map[int]string, len(set))
	for j, i := range set {
		roles[j] = SpanActionRole(nodes[i])
		byOrdinal[nodes[i].Ordinal] = roles[j]
	}
	edgeSet := map[string]bool{}
	for j, i := range set {
		for _, in := range nodes[i].Inputs {
			if in.Source != "prior_result" || in.FromCall == 0 {
				continue
			}
			if parent := byOrdinal[in.FromCall]; parent != "" {
				key := strings.TrimPrefix(in.Key, "params/")
				edgeSet[parent+" -> "+roles[j]+" ("+key+":"+in.Type+")"] = true
			}
		}
	}
	edges := make([]string, 0, len(edgeSet))
	for edge := range edgeSet {
		edges = append(edges, edge)
	}
	sort.Strings(edges)

	// Consecutive identical roles can be a fan-out, but never erase a
	// dependency from one occurrence to the next. A cyclic motif is folded
	// only when its complete role sequence repeats.
	var actions []string
	var actionNodes []int
	for j, role := range roles {
		if j > 0 && role == roles[j-1] && SpanCanFoldRepeat(nodes[set[j-1]], nodes[set[j]]) {
			continue
		}
		actions = append(actions, role)
		actionNodes = append(actionNodes, j)
	}
	for period := 1; period <= len(actions)/2; period++ {
		if len(actions)%period != 0 {
			continue
		}
		// A later instance consuming an earlier instance's result is an
		// observed state transition, not independent fan-out.
		selfDependent := false
		for _, edge := range edges {
			for _, role := range actions[:period] {
				if strings.HasPrefix(edge, role+" -> "+role+" (") {
					selfDependent = true
				}
			}
		}
		if selfDependent {
			continue
		}
		// A repeated motif is a loop only when corresponding iterations
		// demonstrably target different items or produce different objects.
		foldable := true
		for j := period; j < len(actions); j++ {
			if !SpanCanFoldRepeat(nodes[set[actionNodes[j-period]]], nodes[set[actionNodes[j]]]) {
				foldable = false
				break
			}
		}
		if !foldable {
			continue
		}
		matches := true
		for j := period; j < len(actions); j++ {
			if actions[j] != actions[j%period] {
				matches = false
				break
			}
		}
		if matches {
			actions = actions[:period]
			break
		}
	}
	var repeats []model.SpanRepeat
	counts := map[string]int{}
	seenRepeat := map[string]bool{}
	for _, role := range roles {
		counts[role]++
	}
	for _, role := range actions {
		if counts[role] < 2 || seenRepeat[role] {
			continue
		}
		seenRepeat[role] = true
		kind := "repeated"
		for j, i := range set {
			if roles[j] != role {
				continue
			}
			for _, in := range nodes[i].Inputs {
				if in.Source != "prior_result" || in.FromCall == 0 {
					continue
				}
				if byOrdinal[in.FromCall] == role {
					kind = "dependent_repeat"
					break
				}
			}
		}
		if kind != "dependent_repeat" && SpanRepeatedItems(nodes, set, roles, role) {
			kind = "for_each"
		}
		repeats = append(repeats, model.SpanRepeat{Action: role, Count: counts[role], Kind: kind})
	}
	// A readable structural key allows reviewers to see exactly why two
	// traces were bucketed. It is not a hash of prompt text or concrete IDs.
	key := "actions=" + strings.Join(actions, " -> ") + "|edges=" + strings.Join(edges, ";")
	return model.SpanComposition{Key: key, Actions: actions, Edges: edges, Repetition: repeats}
}

func SpanDependsOn(n SpanNode, ordinal int) bool {
	for _, in := range n.Inputs {
		if in.Source == "prior_result" && in.FromCall == ordinal {
			return true
		}
	}
	return false
}

func SpanCanFoldRepeat(a, b SpanNode) bool {
	if SpanActionRole(a) != SpanActionRole(b) || SpanDependsOn(b, a.Ordinal) {
		return false
	}
	aTargets, bTargets := SpanTargetSlots(a), SpanTargetSlots(b)
	distinctTarget := false
	for key, left := range aTargets {
		if right, ok := bTargets[key]; ok {
			// A shared parent is normal in a fan-out: one new issue may
			// be linked to several different issues. Fold when an item
			// target changes, even if another endpoint stays fixed. A
			// retry or two state changes on the same target have no
			// differing item slot and therefore remain visible.
			if left != right {
				distinctTarget = true
			}
		}
	}
	if distinctTarget {
		return true
	}
	if len(a.Call.OutIDs) > 0 && len(b.Call.OutIDs) > 0 {
		return strings.Join(a.Call.OutIDs, "\x00") != strings.Join(b.Call.OutIDs, "\x00")
	}
	return false
}

func SpanTargetSlots(n SpanNode) map[string]string {
	out := map[string]string{}
	for _, st := range n.Steps {
		for _, sl := range SpanExpandedSlots(st.Slots) {
			key := strings.TrimPrefix(strings.ToLower(sl.Key), "params/")
			if sl.Value == "" || (sl.Type != trace.SlotID && sl.Type != trace.SlotPath && sl.Type != trace.SlotURL && sl.Type != trace.SlotNumber) {
				continue
			}
			switch key {
			case "project_id", "project_key", "namespace", "cluster", "context", "transition_id", "priority":
				continue
			}
			out[key] = sl.Value
		}
	}
	return out
}

// A for_each label needs distinct items of the same typed input role, drawn
// from the caller or the same earlier result. Mere repeated tool use is not
// enough to assert an iterable input.
func SpanRepeatedItems(nodes []SpanNode, set []int, roles []string, role string) bool {
	values := map[string]map[string]bool{}
	byOrdinal := map[int]string{}
	for j, i := range set {
		byOrdinal[nodes[i].Ordinal] = roles[j]
	}
	for j, i := range set {
		if roles[j] != role {
			continue
		}
		for _, st := range nodes[i].Steps {
			for _, sl := range SpanExpandedSlots(st.Slots) {
				if sl.Type != trace.SlotID && sl.Type != trace.SlotURL && sl.Type != trace.SlotPath && sl.Type != trace.SlotNumber {
					continue
				}
				for _, in := range nodes[i].Inputs {
					if in.Key != sl.Key || in.Type != sl.Type || (in.Source != "caller" && in.Source != "prior_result") {
						continue
					}
					origin := in.Source
					if in.Source == "prior_result" {
						// Two created issues can feed two transitions. The
						// parent call numbers differ, but their producer role
						// is the same iterable source of item IDs.
						origin += ":" + byOrdinal[in.FromCall]
					}
					key := in.Key + ":" + in.Type + ":" + origin
					if values[key] == nil {
						values[key] = map[string]bool{}
					}
					values[key][sl.Value] = true
				}
			}
		}
	}
	for _, items := range values {
		if len(items) >= 2 {
			return true
		}
	}
	return SpanRepeatedArgumentVariation(nodes, set, roles, role)
}

// A call can be a loop item even when the agent authored a text field that
// was not copied verbatim from the request. At invocation that changing field
// becomes a typed caller input. Require independent operations and a stable
// argument slot; changed output IDs alone could be a retry.
func SpanRepeatedArgumentVariation(nodes []SpanNode, set []int, roles []string, role string) bool {
	var operations []SpanNode
	for j, i := range set {
		if roles[j] == role {
			operations = append(operations, nodes[i])
		}
	}
	if len(operations) < 2 {
		return false
	}
	for i := 1; i < len(operations); i++ {
		if !SpanCanFoldRepeat(operations[i-1], operations[i]) {
			return false
		}
	}
	fields := map[string]map[string]bool{}
	present := map[string]int{}
	for _, op := range operations {
		for path, field := range trace.ObservedArgs(op.Call) {
			if trace.OperationSelector(op.Call, path) {
				continue
			}
			key := path + "\x00" + field.TypeName
			if fields[key] == nil {
				fields[key] = map[string]bool{}
			}
			fields[key][field.Value] = true
			present[key]++
		}
	}
	for key, seen := range fields {
		if present[key] == len(operations) && len(seen) >= 2 {
			return true
		}
	}
	return false
}

func SpanActionRole(n SpanNode) string {
	tool := strings.ToLower(n.Call.Tool)
	role := n.Label
	switch {
	case tool == "mcp:telara_execute_action":
		integration, action := strings.ToLower(n.Call.Args["integration"]), strings.ToLower(n.Call.Args["action"])
		if integration != "" && action != "" {
			role = integration + "." + action
		} else {
			role = "telara.execute_action[unresolved]"
		}
	case tool == "shell":
		role = n.Label
	case tool != "":
		role = tool
	}
	// State and relationship choices can change the operation. Preserve
	// these declared selectors, but never use variable resource IDs or text.
	var selectors []string
	for _, st := range n.Steps {
		for _, sl := range SpanExpandedSlots(st.Slots) {
			if redact.SensitiveSlot(st.Label, sl) {
				continue
			}
			k := strings.ToLower(sl.Key)
			if trace.ScopeFlags[strings.SplitN(k, "#", 2)[0]] && sl.Value != "" && len(sl.Value) <= 40 {
				selectors = append(selectors, k+"="+strings.ToLower(redact.Redact(sl.Value)))
				continue
			}
			switch k {
			case "status", "state", "transition", "transition_id", "resolution":
				if sl.Value != "" && len(sl.Value) <= 40 {
					selectors = append(selectors, k+"="+strings.ToLower(redact.Redact(sl.Value)))
				}
			case "environment", "env", "cluster", "namespace", "context", "profile":
				if sl.Value != "" && len(sl.Value) <= 40 {
					selectors = append(selectors, k+"="+strings.ToLower(redact.Redact(sl.Value)))
				}
			}
		}
	}
	if len(selectors) > 0 {
		sort.Strings(selectors)
		role += "[" + strings.Join(selectors, ",") + "]"
	}
	return role
}

// GroupSpanCompositions is a separate, deliberately broad review index. It
// must not be interpreted as deduplicated validated primitives.
func GroupSpanCompositions(ps []model.SpanProposal) []model.SpanCompositionGroup {
	by := map[string]*model.SpanCompositionGroup{}
	sessions := map[string]map[string]bool{}
	for _, p := range ps {
		key := p.Composition.Key
		if key == "" {
			continue
		}
		g := by[key]
		if g == nil {
			g = &model.SpanCompositionGroup{Key: key, Example: p}
			by[key] = g
			sessions[key] = map[string]bool{}
		}
		g.Proposals++
		g.Members = append(g.Members, p.ID)
		sessions[key][p.Client+"/"+p.Session] = true
		if p.EvidenceScore > g.Example.EvidenceScore {
			g.Example = p
		}
	}
	out := make([]model.SpanCompositionGroup, 0, len(by))
	for key, g := range by {
		g.Sessions = len(sessions[key])
		sort.Strings(g.Members)
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sessions != out[j].Sessions {
			return out[i].Sessions > out[j].Sessions
		}
		if out[i].Proposals != out[j].Proposals {
			return out[i].Proposals > out[j].Proposals
		}
		return out[i].Key < out[j].Key
	})
	return out
}

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

// Thresholds, fixed before the lineage-separated holdout was labeled.
const (
	TemplateMinSessions = 3
	StateMinStepLines   = 3
	StateMinAnchors     = 2
	RerunMinRuns        = 2
	RerunMinProgram     = 120
	NavMaxShare         = 0.25
	EditMaxShare        = 0.30
	MinWorkSteps        = 2
	LoopMinItems        = 2
	SinglePassMaxSteps  = 15
)

var (
	StepLine  = regexp.MustCompile(`(?m)^\s*(?:[-*•]|\d+[.)])\s+\S`)
	AnchorRe  = regexp.MustCompile("`[^`\n]{3,}`|(?:~|\\.{0,2})/[\\w.@-]+(?:/[\\w.@-]+)+|\\b[\\w-]+\\.(?:md|json|jsonl|csv|yaml|yml|go|py|ts|sh|txt|log)\\b")
	PathNoise = regexp.MustCompile(`(?:~|\.{0,2})/[\w.@/-]+`)
	NumNoise  = regexp.MustCompile(`\b\d+\b`)
	StrNoise  = regexp.MustCompile(`'[^'\n]*'|"[^"\n]*"`)
)

// SelectOpportunities judges every request of the corpus that made calls.
func SelectOpportunities(ss []trace.Session) []model.Opportunity {
	ss = append([]trace.Session(nil), ss...)
	for i := range ss {
		ss[i].Calls = append([]trace.Call(nil), ss[i].Calls...)
	}
	trace.DropCopiedCalls(ss)
	raw := map[string][]trace.Call{}
	for _, s := range ss {
		raw[s.Client+"/"+s.ID] = s.Calls
	}
	norm := trace.Normalize(ss)
	sessionsByText := map[string]map[string]bool{}
	for _, ns := range norm {
		for _, r := range ns.Requests {
			k := trace.TextKey(r)
			if k == "" {
				continue
			}
			if sessionsByText[k] == nil {
				sessionsByText[k] = map[string]bool{}
			}
			sessionsByText[k][ns.Client+"/"+ns.ID] = true
		}
	}
	var out []model.Opportunity
	for i := range norm {
		ns := &norm[i]
		byReq := map[int][]trace.Step{}
		for _, st := range ns.Steps {
			byReq[st.Request] = append(byReq[st.Request], st)
		}
		reqs := make([]int, 0, len(byReq))
		for r := range byReq {
			reqs = append(reqs, r)
		}
		sort.Ints(reqs)
		for _, r := range reqs {
			text := ""
			if r < len(ns.Requests) {
				text = ns.Requests[r]
			}
			o := model.Opportunity{ID: trace.EpisodeID(ns.Client, ns.ID, r), Client: ns.Client, Session: ns.ID, Request: r,
				Task: ns.Client + "/" + ns.ID + "/" + strconv.Itoa(r)}
			var shell []string
			for _, c := range raw[ns.Client+"/"+ns.ID] {
				if c.Request == r && c.Tool == "shell" {
					shell = append(shell, c.Command)
				}
			}
			JudgeOpportunity(&o, text, byReq[r], shell, len(sessionsByText[trace.TextKey(text)]))
			if st := byReq[r]; len(st) > 0 {
				o.Start = st[0].Time
			}
			out = append(out, o)
		}
	}
	return out
}

func JudgeOpportunity(o *model.Opportunity, text string, steps []trace.Step, shell []string, textSessions int) {
	reason := func(s string) { o.Reasons = append(o.Reasons, s) }
	if strings.TrimSpace(text) == "" || trace.IsHarness(text) || strings.HasPrefix(strings.TrimSpace(text), "<") {
		reason("no_request_text")
		return
	}
	var work, nav, edits int
	calls := map[int]bool{}
	for _, st := range steps {
		if calls[st.Call] {
			continue // one call can hold several steps
		}
		calls[st.Call] = true
		switch {
		case trace.BookkeepingTools[st.Label]:
		case trace.EditTools[st.Label] || strings.HasPrefix(st.Label, "patch:"):
			edits++
			work++
		case strings.HasPrefix(st.Label, "sh:") && ViewingCall(st.Call, steps):
			// Printing a file or a listing through the shell is navigation
			// too (sed -n, cat, head, rg ...).
			nav++
			work++
		case strings.HasPrefix(st.Label, "sh:") || strings.HasPrefix(st.Label, "mcp:") || strings.HasPrefix(st.Label, "js:"):
			work++
		default:
			// A client's own read, search or listing tool: navigation.
			nav++
			work++
		}
	}
	if work < MinWorkSteps {
		reason("fewer_than_two_work_steps")
		return
	}
	// Route 1: a recurring prompt that states its procedure.
	if textSessions >= TemplateMinSessions {
		lines := len(StepLine.FindAllString(text, -1))
		anchors := StateAnchors(text)
		touched := 0
		for _, a := range anchors {
			if Touches(a, steps) {
				touched++
			}
		}
		switch {
		case lines < StateMinStepLines && len(anchors) < StateMinAnchors:
			reason("template_states_no_procedure")
		case touched == 0:
			reason("template_anchors_untouched")
		default:
			o.Recommended, o.Route = true, model.RouteStatedTemplate
			o.Contract = "template " + TemplateTitle(text)
			reason("template_sessions:" + strconv.Itoa(textSessions))
			reason("step_lines:" + strconv.Itoa(lines))
			reason("anchors_touched:" + strconv.Itoa(touched) + "/" + strconv.Itoa(len(anchors)))
			return
		}
	}
	// Route 2: a program the agent wrote and ran again.
	navShare, editShare := float64(nav)/float64(work), float64(edits)/float64(work)
	// Counted over the recorded calls: normalizing merges identical repeats.
	runs := map[string]int{}
	for _, cmd := range shell {
		if len(cmd) >= RerunMinProgram && WritesProgram(cmd) {
			runs[ProgramKey(cmd)]++
		}
	}
	most, prog := 0, ""
	for k, n := range runs {
		if n > most || (n == most && k < prog) {
			most, prog = n, k
		}
	}
	loopLabel, loopItems, loopArg, loopSourceUnknown := ParametricLoop(text, steps)
	switch {
	case most < RerunMinRuns && loopItems < LoopMinItems:
		reason("no_rerun_program")
		if loopSourceUnknown {
			reason("loop_source_unknown")
		} else {
			reason("no_parametric_loop")
		}
		// Route 4: one short pass over an object the request names: the
		// request is short, a step acts on a named object, and a later step
		// depends on an earlier one. The navigation and edit gates apply to
		// the window from the first such step to the last dependent step.
		named, win, wNav, wEdit, dependent, seq := NamedObject(text, steps)
		switch {
		case named == 0:
			reason("no_named_object_touched")
		case !dependent:
			reason("named_object_without_dependent_steps")
		case work > SinglePassMaxSteps:
			// The whole request must be one short pass. Judging only the
			// window was tried on the diagnostic sets and added a false
			// positive and no true one.
			reason("single_pass_too_long:" + strconv.Itoa(work))
		case float64(wNav) >= NavMaxShare*float64(win):
			reason("navigation_share:" + strconv.FormatFloat(float64(wNav)/float64(win), 'f', 2, 64))
		case float64(wEdit) >= EditMaxShare*float64(win):
			reason("edit_share:" + strconv.FormatFloat(float64(wEdit)/float64(win), 'f', 2, 64))
		default:
			o.Reasons = nil
			o.Recommended, o.Route = true, model.RouteNamedObject
			reason("named_objects_touched:" + strconv.Itoa(named))
			reason("window_steps:" + strconv.Itoa(win))
			o.Contract = "single pass " + strings.Join(seq, " > ")
		}
	case navShare >= NavMaxShare:
		reason("navigation_share:" + strconv.FormatFloat(navShare, 'f', 2, 64))
	case editShare >= EditMaxShare:
		reason("edit_share:" + strconv.FormatFloat(editShare, 'f', 2, 64))
	case most >= RerunMinRuns:
		o.Recommended, o.Route = true, model.RouteRerunCheck
		reason("program_runs:" + strconv.Itoa(most))
		o.Contract = "program " + ShortHash(prog) + ": " + trace.OneLine(prog, 60)
	default:
		o.Recommended, o.Route = true, model.RouteParamLoop
		reason("loop:" + loopLabel + ":" + strconv.Itoa(loopItems))
		o.Contract = "loop " + loopLabel + " over " + loopArg
	}
}

// ParametricLoop finds a replayable step run on two or more items where
// exactly one argument varies. The complete list must be visible in the
// request or in one earlier result, and no later item may have been picked
// from an intermediate result. Otherwise its source and termination rule
// are unknown, even if the calls happen to look like a loop.
func ParametricLoop(text string, steps []trace.Step) (string, int, string, bool) {
	byLabel := map[string][]int{}
	var order []string
	for i, st := range steps {
		if !(strings.HasPrefix(st.Label, "sh:") || strings.HasPrefix(st.Label, "mcp:") || strings.HasPrefix(st.Label, "js:")) {
			continue
		}
		if trace.BookkeepingTools[st.Label] || trace.StepEffect(st) == "write" {
			continue
		}
		if f := strings.Fields(strings.TrimPrefix(st.Label, "sh:")); strings.HasPrefix(st.Label, "sh:") && len(f) > 0 && (ViewPrograms[f[0]] || trace.SearchPrograms[f[0]] || f[0] == "sed") {
			continue // viewing files one after another is navigation
		}
		if _, ok := byLabel[st.Label]; !ok {
			order = append(order, st.Label)
		}
		byLabel[st.Label] = append(byLabel[st.Label], i)
	}
	best, bestN, bestArg, unknownSource := "", 0, "", false
	for _, l := range order {
		idx := byLabel[l]
		if len(idx) < LoopMinItems {
			continue
		}
		// The one varying argument.
		vals := map[string][]string{}
		for _, i := range idx {
			for _, sl := range steps[i].Slots {
				if sl.Sub || sl.Type == trace.SlotFlag || trace.Derived(sl.Key) {
					continue
				}
				vals[sl.Key] = append(vals[sl.Key], sl.Value)
			}
		}
		key := ""
		ok := true
		types := map[string]string{}
		for _, sl := range steps[idx[0]].Slots {
			types[sl.Key] = sl.Type
		}
		for k, vs := range vals {
			if len(vs) != len(idx) {
				continue
			}
			distinct := map[string]bool{}
			for _, v := range vs {
				distinct[v] = true
			}
			if len(distinct) == 1 {
				continue
			}
			if key != "" {
				ok = false // two arguments vary: not one list
				break
			}
			if len(distinct) != len(vs) {
				ok = false // an item repeated: a retry, not a list
				break
			}
			key = k
		}
		// A list a caller gives names things: ids, URLs, paths. A loop over
		// free text (search phrasings, scripts) is the agent trying things.
		// A number of five or more digits is an id (a job, a pipeline); a
		// short one is a count or a page.
		if t := types[key]; t != trace.SlotID && t != trace.SlotURL && t != trace.SlotPath && !(t == trace.SlotNumber && LongNumbers(vals[key])) {
			ok = false
		}
		if !ok || key == "" {
			continue
		}
		// No item may come from what the loop read since the last item.
		for n := 1; n < len(idx) && ok; n++ {
			v := vals[key][n]
			for j := idx[n-1]; j < idx[n]; j++ {
				if trace.InResult(v, steps[j]) {
					ok = false
					break
				}
			}
		}
		source := ""
		if ok {
			source = LoopListSource(text, steps, idx[0], vals[key])
			if source == "" {
				unknownSource = true
			}
		}
		if source != "" && len(idx) > bestN {
			best, bestN, bestArg = l, len(idx), key+" ("+types[key]+") "+ItemShape(types[key], vals[key])+" from "+source
		}
	}
	return best, bestN, bestArg, unknownSource
}

func LoopListSource(text string, steps []trace.Step, before int, items []string) string {
	all := func(has func(string) bool) bool {
		for _, item := range items {
			if !has(item) {
				return false
			}
		}
		return true
	}
	if all(func(item string) bool { return ContainsItem(text, item) }) {
		return "caller"
	}
	for i := 0; i < before; i++ {
		st := steps[i]
		if st.Outcome == trace.OutcomeFailed {
			continue
		}
		if all(func(item string) bool {
			for _, id := range st.OutIDs {
				if id == item {
					return true
				}
			}
			for _, token := range st.OutTokens {
				if token == item {
					return true
				}
			}
			return ContainsItem(st.Output, item)
		}) {
			return "prior_output"
		}
	}
	return ""
}

// ContainsItem requires an item boundary, so ID 81234567 is not mistaken
// for a caller-supplied ID in 1812345679.
func ContainsItem(text, item string) bool {
	if item == "" {
		return false
	}
	word := func(b byte) bool {
		return b >= '0' && b <= '9' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b == '_'
	}
	for offset := 0; offset < len(text); {
		i := strings.Index(text[offset:], item)
		if i < 0 {
			return false
		}
		i += offset
		end := i + len(item)
		if (i == 0 || !word(text[i-1]) || !word(item[0])) &&
			(end == len(text) || !word(text[end]) || !word(item[len(item)-1])) {
			return true
		}
		offset = i + 1
	}
	return false
}

// StateAnchors are the paths, file names and quoted commands a request
// names.
func StateAnchors(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range AnchorRe.FindAllString(text, -1) {
		m = strings.Trim(m, "`")
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// Touches reports whether a step's command or arguments name the anchor (a
// path by its last element, a command by its first word).
func Touches(anchor string, steps []trace.Step) bool {
	key := anchor
	if strings.Contains(anchor, "/") {
		key = anchor[strings.LastIndex(anchor, "/")+1:]
	} else if f := strings.Fields(anchor); len(f) > 1 {
		key = f[0] + " " + f[1]
	}
	if len(key) < 3 {
		return false
	}
	for _, st := range steps {
		if strings.Contains(st.Raw, key) {
			return true
		}
		for _, sl := range st.Slots {
			if strings.Contains(sl.Value, key) {
				return true
			}
		}
	}
	return false
}

// ProgramKey is a shell call with its paths, numbers and quoted strings
// abstracted, so the same program run on other inputs has the same key.
func ProgramKey(raw string) string {
	k := PathNoise.ReplaceAllString(raw, "<p>")
	k = StrNoise.ReplaceAllString(k, "<s>")
	k = NumNoise.ReplaceAllString(k, "<n>")
	return strings.Join(strings.Fields(k), " ")
}

// ViewPrograms print files or listings. A shell call made only of them is
// navigation, whatever its length.
var ViewPrograms = map[string]bool{"cat": true, "head": true, "tail": true, "nl": true, "ls": true, "tree": true, "less": true, "wc": true, "stat": true}

// ViewingCall reports whether every command of a shell call only views:
// a viewer, a search program, or sed printing (-n, no -i).
func ViewingCall(call int, steps []trace.Step) bool {
	any := false
	for _, st := range steps {
		if st.Call != call {
			continue
		}
		f := strings.Fields(strings.TrimPrefix(st.Label, "sh:"))
		if len(f) == 0 {
			return false
		}
		prog := f[0]
		switch {
		case ViewPrograms[prog] || trace.SearchPrograms[prog]:
		case prog == "sed" && trace.StepEffect(st) == "read":
		case prog == "cd" || prog == "echo" || prog == "printf":
		default:
			return false
		}
		any = true
	}
	return any
}

// ScriptMarks show a call carries a program the agent wrote: a heredoc,
// inline interpreter code, or a jq or awk program.
var ScriptMarks = regexp.MustCompile(`<<-?\s*['"]?\w+|\b(?:python3?|node|ruby|perl|deno|bun)\s+(?:-c|-e|-)\s|\bjq\s+(?:-\w+\s+)*'[^']{8,}|\bawk\s+'[^']{8,}`)

// WritesProgram reports whether a shell call runs a program written for it,
// rather than a single named command or a file view.
func WritesProgram(cmd string) bool { return ScriptMarks.MatchString(cmd) }

func LongNumbers(vs []string) bool {
	for _, v := range vs {
		if len(v) < 5 {
			return false
		}
	}
	return true
}

// ObjectRe finds what a request names that a procedure could take as an
// input: an issue key, a URL, a long number, a path or a file name.
var ObjectRe = regexp.MustCompile(`\b[A-Z][A-Z0-9]+-\d+\b|https?://[^\s)>"'` + "`" + `]+|\b\d{5,}\b|(?:~|\.{0,2})/[\w.@-]+(?:/[\w.@-]+)+|\b[\w-]+\.(?:md|json|jsonl|csv|yaml|yml|go|py|ts|sh|txt|log|pdf|html)\b`)

// NamedObject counts the objects the request names that some step acts on,
// and finds the window from the first such step to the last step that used
// a value an earlier step in the window produced. It returns the count, the
// window's work steps, how many of them navigate and edit, and whether any
// dependency was found.
func NamedObject(text string, steps []trace.Step) (named, win, nav, edits int, dependent bool, seq []string) {
	seen := map[string]bool{}
	first := -1
	for _, m := range ObjectRe.FindAllString(text, -1) {
		m = strings.TrimRight(m, ".,;:")
		if len(m) < 4 || seen[m] {
			continue
		}
		seen[m] = true
		if i := FirstTouch(m, steps); i >= 0 {
			named++
			if first < 0 || i < first {
				first = i
			}
		}
	}
	if named == 0 {
		return
	}
	last := -1
	for j := first + 1; j < len(steps); j++ {
		for _, sl := range steps[j].Slots {
			if sl.Sub || sl.Type == trace.SlotFlag || len(sl.Value) < 4 {
				continue
			}
			for i := first; i < j; i++ {
				if steps[i].Call != steps[j].Call && trace.InResult(sl.Value, steps[i]) {
					last = j
				}
			}
		}
	}
	if last < 0 {
		return
	}
	dependent = true
	calls := map[int]bool{}
	for _, st := range steps[first : last+1] {
		if calls[st.Call] || trace.BookkeepingTools[st.Label] {
			continue
		}
		calls[st.Call] = true
		win++
		if len(seq) == 0 || seq[len(seq)-1] != st.Label {
			seq = append(seq, st.Label)
		}
		switch {
		case trace.EditTools[st.Label] || strings.HasPrefix(st.Label, "patch:"):
			edits++
		case strings.HasPrefix(st.Label, "sh:") && ViewingCall(st.Call, steps):
			nav++
		case strings.HasPrefix(st.Label, "sh:") || strings.HasPrefix(st.Label, "mcp:") || strings.HasPrefix(st.Label, "js:"):
		default:
			nav++
		}
	}
	return
}

// FirstTouch is the index of the first step whose command or arguments name
// the object, or -1.
func FirstTouch(obj string, steps []trace.Step) int {
	for i := range steps {
		if Touches(obj, steps[i:i+1]) {
			return i
		}
	}
	return -1
}

func ShortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:4])
}

// GroupOpportunities groups recommended requests by contract and ranks the
// groups: most distinct sessions first, then most requests, then the most
// recently seen. Grouping and ranking change what a person reads, not which
// requests are recommended.
func GroupOpportunities(ops []model.Opportunity) []model.OpportunityGroup {
	by := map[string]*model.OpportunityGroup{}
	sess := map[string]map[string]bool{}
	var order []string
	for _, o := range ops {
		if !o.Recommended {
			continue
		}
		k := o.Route + "\x00" + o.Contract
		g := by[k]
		if g == nil {
			g = &model.OpportunityGroup{Contract: o.Contract, Route: o.Route, First: o.Start, Last: o.Start, Example: o}
			by[k] = g
			sess[k] = map[string]bool{}
			order = append(order, k)
		}
		g.Requests++
		g.Members = append(g.Members, o.ID)
		sess[k][o.Client+"/"+o.Session] = true
		if o.Start.Before(g.First) {
			g.First = o.Start
		}
		if !o.Start.Before(g.Last) {
			g.Last, g.Example = o.Start, o
		}
	}
	out := make([]model.OpportunityGroup, 0, len(order))
	for _, k := range order {
		g := by[k]
		g.Sessions = len(sess[k])
		sort.Strings(g.Members)
		out = append(out, *g)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.Sessions != b.Sessions:
			return a.Sessions > b.Sessions
		case a.Requests != b.Requests:
			return a.Requests > b.Requests
		case !a.Last.Equal(b.Last):
			return a.Last.After(b.Last)
		}
		return a.Contract < b.Contract
	})
	return out
}

// TemplateTitle is a stated prompt's first line, digits aside: the name a
// scheduled prompt keeps while its body is edited, so its versions group
// together. A prompt whose first line is too short to name it falls back to
// a hash of its whole text.
func TemplateTitle(text string) string {
	for _, line := range strings.Split(text, "\n") {
		k := trace.TextKey(line)
		if len(k) >= 12 {
			return trace.OneLine(k, 80)
		}
		if strings.TrimSpace(line) != "" {
			break
		}
	}
	return ShortHash(trace.TextKey(text))
}

// ItemShape says what a loop's items are, so loops of one step over
// different things do not group together: the hosts of URLs, the parent
// directory of paths.
func ItemShape(typ string, vs []string) string {
	set := map[string]bool{}
	for _, v := range vs {
		switch typ {
		case trace.SlotURL:
			h := v
			if i := strings.Index(h, "://"); i >= 0 {
				h = h[i+3:]
			}
			if i := strings.IndexAny(h, "/?#"); i >= 0 {
				h = h[:i]
			}
			set[h] = true
		case trace.SlotPath:
			d := v
			if i := strings.LastIndex(strings.TrimRight(d, "/"), "/"); i >= 0 {
				d = d[:i]
			}
			if i := strings.LastIndex(d, "/"); i >= 0 {
				d = d[i+1:]
			}
			set[d+"/"] = true
		}
	}
	var out []string
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) > 3 {
		out = append(out[:3], "…")
	}
	return strings.Join(out, ",")
}

type SpanNode struct {
	Call    trace.Call        `json:"-"`
	Ordinal int               `json:"-"`
	Steps   []trace.Step      `json:"-"`
	Label   string            `json:"-"`
	Effect  string            `json:"-"`
	Inputs  []model.SpanInput `json:"-"`
	Deps    []int             `json:"-"`
	Anchors []string          `json:"-"`
}

// SelectSpanProposals extracts bounded pieces of work without requiring the
// whole request to be short, multi-call, or to name an object literally. It
// makes no LLM call and does not change the older recommendation score.
func SelectSpanProposals(ss []trace.Session) []model.SpanProposal {
	cp := append([]trace.Session(nil), ss...)
	for i := range cp {
		cp[i].Calls = append([]trace.Call(nil), cp[i].Calls...)
	}
	trace.DropCopiedCalls(cp)
	norm := trace.Normalize(cp)
	normBySession := make(map[string]*trace.NormSession, len(norm))
	for i := range norm {
		normBySession[norm[i].Client+"\x00"+norm[i].ID] = &norm[i]
	}
	type requestNodes struct {
		session int
		request int
		nodes   []SpanNode
	}
	var work []requestNodes
	pairSessions := map[string]map[string]bool{}
	for si := range cp {
		s := &cp[si]
		ns := normBySession[s.Client+"\x00"+s.ID]
		if ns == nil {
			continue
		}
		byCall := map[int][]trace.Step{}
		for _, st := range ns.Steps {
			byCall[st.Call] = append(byCall[st.Call], st)
		}
		byReq := map[int][]int{}
		for ci := range s.Calls {
			byReq[s.Calls[ci].Request] = append(byReq[s.Calls[ci].Request], ci)
		}
		reqs := make([]int, 0, len(byReq))
		for r := range byReq {
			reqs = append(reqs, r)
		}
		sort.Ints(reqs)
		for _, r := range reqs {
			if r >= len(s.Requests) || strings.TrimSpace(s.Requests[r]) == "" || trace.IsHarness(s.Requests[r]) {
				continue
			}
			nodes := BuildSpanNodes(*s, r, byReq[r], byCall)
			if len(nodes) == 0 {
				continue
			}
			work = append(work, requestNodes{session: si, request: r, nodes: nodes})
			for i := 1; i < len(nodes); i++ {
				key := SpanAdjacentPairKey(nodes[i-1], nodes[i])
				if key == "" {
					continue
				}
				if pairSessions[key] == nil {
					pairSessions[key] = map[string]bool{}
				}
				pairSessions[key][s.Client+"\x00"+s.ID] = true
			}
		}
	}
	pairSupport := map[string]bool{}
	for key, sessions := range pairSessions {
		if len(sessions) >= 2 {
			pairSupport[key] = true
		}
	}
	var out []model.SpanProposal
	for _, item := range work {
		out = append(out, ProposalsForRequest(cp[item.session], item.request, item.nodes, pairSupport)...)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Client != b.Client {
			return a.Client < b.Client
		}
		if !a.Start.Equal(b.Start) {
			return a.Start.Before(b.Start)
		}
		if a.Session != b.Session {
			return a.Session < b.Session
		}
		if a.Request != b.Request {
			return a.Request < b.Request
		}
		return a.ID < b.ID
	})
	return out
}

func BuildSpanNodes(s trace.Session, req int, calls []int, byCall map[int][]trace.Step) []SpanNode {
	var nodes []SpanNode
	for ordinal, ci := range calls {
		steps := byCall[ci]
		if len(steps) == 0 {
			continue
		}
		work := false
		effect := "unknown"
		var labels []string
		for _, st := range steps {
			if trace.BookkeepingTools[st.Label] {
				continue
			}
			work = true
			label := st.Label
			for _, sl := range st.Slots {
				if trace.SelectorKeys[sl.Key] && sl.Value != "" {
					label += "#" + sl.Key + "=" + trace.OneLine(strings.ToLower(sl.Value), 40)
				}
			}
			labels = append(labels, label)
			switch trace.StepEffect(st) {
			case "write":
				effect = "write"
			case "read":
				if effect != "write" {
					effect = "read"
				}
			}
		}
		if !work {
			continue
		}
		n := SpanNode{Call: s.Calls[ci], Ordinal: ordinal + 1, Steps: steps,
			Label: strings.Join(labels, "+"), Effect: effect}
		for _, st := range steps {
			for _, slot := range SpanExpandedSlots(st.Slots) {
				if !SpanVariable(slot) {
					continue
				}
				input := model.SpanInput{Key: slot.Key, Type: slot.Type, Source: "unknown"}
				v := slot.Value
				switch {
				case SpanInText(v, slot.Type, s.Requests[req]):
					input.Source = "caller"
				case SpanPriorRequest(v, slot.Type, s.Requests, req) >= 0:
					input.Source = "prior_request"
					input.FromRequest = SpanPriorRequest(v, slot.Type, s.Requests, req) + 1
				default:
					for j := len(nodes) - 1; j >= 0; j-- {
						if SpanResultHas(nodes[j].Call, v) {
							input.Source, input.FromCall = "prior_result", nodes[j].Ordinal
							n.Deps = AppendUniqueInt(n.Deps, j)
							break
						}
					}
					if input.Source == "unknown" {
						for ci := len(s.Calls) - 1; ci >= 0; ci-- {
							if s.Calls[ci].Request >= req {
								continue
							}
							if SpanResultHas(s.Calls[ci], v) {
								input.Source, input.FromRequest = "prior_result", s.Calls[ci].Request+1
								break
							}
						}
					}
				}
				n.Inputs = append(n.Inputs, input)
				if input.Source == "caller" || input.Source == "prior_request" {
					if slot.Type == trace.SlotID || slot.Type == trace.SlotPath || slot.Type == trace.SlotURL || slot.Type == trace.SlotNumber {
						n.Anchors = append(n.Anchors, slot.Type+"\x00"+v)
					}
				}
			}
		}
		nodes = append(nodes, n)
	}
	return nodes
}

// Tool gateways commonly put the real resource parameters inside a JSON
// string argument such as params={"job_id":123}. Flattening only the outer
// argument hides both caller inputs and result-derived dependencies.
func SpanExpandedSlots(slots []trace.Slot) []trace.Slot {
	out := append([]trace.Slot(nil), slots...)
	for _, sl := range slots {
		if !strings.HasPrefix(strings.TrimSpace(sl.Value), "{") {
			continue
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal([]byte(sl.Value), &obj) != nil {
			continue
		}
		SpanFlattenObject(&out, sl.Key, obj, 0)
	}
	return out
}

func SpanFlattenObject(out *[]trace.Slot, prefix string, obj map[string]json.RawMessage, depth int) {
	if depth >= 4 {
		return
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		// Use slash so these new argument fields are not mistaken for the
		// normalizer's derived "path.basename" slots.
		key := prefix + "/" + k
		var nested map[string]json.RawMessage
		if json.Unmarshal(obj[k], &nested) == nil && nested != nil {
			SpanFlattenObject(out, key, nested, depth+1)
			continue
		}
		var v string
		if json.Unmarshal(obj[k], &v) != nil {
			v = string(obj[k])
		}
		if v == "" || strings.HasPrefix(v, "[") || v == "null" {
			continue
		}
		*out = append(*out, trace.Slot{Key: key, Type: trace.TypeOf(shellparse.Word{Text: v}), Value: v})
	}
}

func SpanVariable(s trace.Slot) bool {
	if s.Sub || s.Type == trace.SlotFlag || trace.Derived(s.Key) || s.Key == "recv" {
		return false
	}
	// Shell flags configure an observed command; their literal values are
	// not automatically inputs the eventual primitive must ask for.
	if strings.HasPrefix(s.Key, "-") {
		return false
	}
	v := strings.TrimSpace(s.Value)
	if len(v) < 3 {
		return false
	}
	switch s.Type {
	case trace.SlotWord:
		return strings.Contains(s.Key, "query") || strings.Contains(s.Key, "search") || strings.Contains(s.Key, "name")
	case trace.SlotNumber:
		return len(v) >= 5 || strings.Contains(strings.ToLower(s.Key), "id")
	default:
		return true
	}
}

func SpanInText(value, kind, text string) bool {
	if kind == trace.SlotPath || kind == trace.SlotURL {
		return trace.InRequest(value, text)
	}
	return ContainsItem(strings.ToLower(text), strings.ToLower(value))
}

func SpanPriorRequest(value, kind string, requests []string, before int) int {
	for r := before - 1; r >= 0; r-- {
		if !trace.IsHarness(requests[r]) && !history.IsClaudeContinuationSummary(requests[r]) && SpanInText(value, kind, requests[r]) {
			return r
		}
	}
	return -1
}

func SpanResultHas(c trace.Call, value string) bool {
	if c.Outcome == trace.OutcomeFailed || len(value) < 4 {
		return false
	}
	for i, id := range c.OutIDs {
		// A literal in a shell transcript can be source code, a command echo,
		// or a diagnostic. A uniquely located structured result is evidence
		// that this call actually produced the value used by a later call.
		if id == value && i < len(c.OutPaths) && c.OutPaths[i] != "" && c.OutPaths[i] != "*" {
			return true
		}
	}
	return false
}

func AppendUniqueInt(xs []int, n int) []int {
	for _, x := range xs {
		if x == n {
			return xs
		}
	}
	return append(xs, n)
}

func ProposalsForRequest(s trace.Session, req int, nodes []SpanNode, pairSupport map[string]bool) []model.SpanProposal {
	var sets [][]int
	authored := map[string]bool{}
	repeatedOrder := map[string]bool{}
	siblingChildren := map[int]bool{}
	byParentOperation := map[string][]int{}
	for i, n := range nodes {
		if len(n.Deps) == 1 {
			key := strconv.Itoa(n.Deps[0]) + "\x00" + n.Label
			byParentOperation[key] = append(byParentOperation[key], i)
		}
	}
	for _, children := range byParentOperation {
		if len(children) < 2 {
			continue
		}
		parent := nodes[children[0]].Deps[0]
		if strings.Contains(nodes[parent].Label, "tabs_context_mcp") {
			continue
		}
		set := append([]int{parent}, children...)
		sort.Ints(set)
		sets = append(sets, set)
		for _, child := range children {
			siblingChildren[child] = true
		}
	}
	// Result dependencies supply a causal slice even when the useful work is
	// surrounded by unrelated navigation or editing in a long request.
	for i := range nodes {
		if len(nodes[i].Deps) == 0 || siblingChildren[i] || SpanNavigationDependency(nodes, i) {
			continue
		}
		seen := map[int]bool{}
		var visit func(int)
		visit = func(j int) {
			if seen[j] {
				return
			}
			seen[j] = true
			for _, parent := range nodes[j].Deps {
				visit(parent)
			}
		}
		visit(i)
		var set []int
		for j := range seen {
			set = append(set, j)
		}
		sort.Ints(set)
		sets = append(sets, set)
	}
	// Separate result chains can be iterations of one program even when the
	// calls are interleaved (A1, A2, B1, B2). Retain each chain as evidence,
	// and also propose their disjoint union for loop synthesis.
	sets = append(sets, SpanRepeatedMotifSets(nodes, sets)...)
	// Fixed sequences can share a caller-supplied resource without consuming
	// each other's results (for example, comment then transition an issue).
	byAnchor := map[string][]int{}
	for i, n := range nodes {
		for _, anchor := range n.Anchors {
			kind, _, _ := strings.Cut(anchor, "\x00")
			// A shared file path mostly joins incidental source reads and edits.
			// Resource IDs, URLs and explicit numbers identify a caller's
			// bounded target more reliably for this independent sequence route.
			if kind != trace.SlotID && kind != trace.SlotURL && kind != trace.SlotNumber {
				continue
			}
			byAnchor[anchor] = AppendUniqueInt(byAnchor[anchor], i)
		}
	}
	for _, set := range byAnchor {
		// Re-reading one file or running one command repeatedly against the
		// same path is a common investigation trace, not a fixed sequence.
		if len(set) >= 2 && len(set) <= 8 && SpanDistinctOperations(nodes, set) >= 2 {
			sets = append(sets, set)
		}
	}
	// A request that explicitly names two adjacent command operations is
	// evidence for their sequence even when no result ID or shared resource
	// joins the calls. Keep this narrow: command names and subcommands must
	// appear in the user's request, and the calls must be direct literals.
	for i := 1; i < len(nodes); i++ {
		if SpanExplicitCommand(s.Requests[req], nodes[i-1]) && SpanExplicitCommand(s.Requests[req], nodes[i]) {
			sets = append(sets, []int{i - 1, i})
		}
		if pairSupport[SpanAdjacentPairKey(nodes[i-1], nodes[i])] && !SpanPairCovered(sets, i-1, i) {
			set := []int{i - 1, i}
			sets = append(sets, set)
			repeatedOrder[SpanSetKey(nodes, set)] = true
		}
	}
	for _, set := range SpanAuthoredProgramSets(nodes) {
		sets = append(sets, set)
		authored[SpanSetKey(nodes, set)] = true
	}
	inlineShapes := map[string]string{}
	inlineFamilies := map[string]string{}
	inlineScopes := map[string]string{}
	for i, n := range nodes {
		if n.Call.Tool != "shell" {
			continue
		}
		shape, family, embedded := pyparse.InlinePythonSnippet(n.Call.Command)
		if shape == "" {
			continue
		}
		set := []int{i}
		key := SpanSetKey(nodes, set)
		sets = append(sets, set)
		authored[key] = true
		inlineShapes[key] = shape
		inlineFamilies[key] = family
		inlineScopes[key] = "direct"
		if embedded {
			inlineScopes[key] = "embedded"
		}
	}
	covered := map[int]bool{}
	for _, set := range sets {
		for _, i := range set {
			covered[i] = true
		}
	}
	// A complete single call is eligible for review. It is still unassessed;
	// the caller must establish its output contract and business result.
	for i, n := range nodes {
		if covered[i] {
			continue
		}
		if !SpanDirectIntent(s.Requests, req, n) {
			continue
		}
		if n.Effect == "unknown" && n.Call.Output == "" {
			continue
		}
		if n.Effect == "read" && n.Call.Output == "" {
			continue
		}
		sets = append(sets, []int{i})
	}
	seenSets := map[string]bool{}
	var out []model.SpanProposal
	for _, set := range sets {
		key := SpanSetKey(nodes, set)
		if seenSets[key] {
			continue
		}
		seenSets[key] = true
		kind := "single_call"
		if authored[key] {
			kind = "authored_program"
		} else if repeatedOrder[key] {
			kind = "repeated_order"
		} else if len(set) > 1 {
			kind = "shared_input"
			for _, i := range set {
				if len(nodes[i].Deps) > 0 {
					kind = "result_chain"
					break
				}
			}
		}
		proposal := MakeSpanProposal(s, req, nodes, set, kind)
		proposal.CodeShape = inlineShapes[key]
		proposal.CodeFamily = inlineFamilies[key]
		proposal.CodeScope = inlineScopes[key]
		out = append(out, proposal)
	}
	return out
}

func SpanPairCovered(sets [][]int, first, second int) bool {
	for _, set := range sets {
		a, b := false, false
		for _, i := range set {
			a = a || i == first
			b = b || i == second
		}
		if a && b {
			return true
		}
	}
	return false
}

func SpanAdjacentPairKey(a, b SpanNode) string {
	if a.Effect != "read" && a.Effect != "write" || b.Effect != "read" && b.Effect != "write" || a.Effect != "write" && b.Effect != "write" || a.Call.Outcome == trace.OutcomeFailed || b.Call.Outcome == trace.OutcomeFailed {
		return ""
	}
	// Repeated order alone is not a process: an issue comment followed by a
	// local file read can recur in many sessions without a shared operation.
	// Require the two calls to act on the same concrete resource in each
	// execution. The value is used only to prove this within one execution;
	// it is absent from the cross-session key.
	if !SpanPairSharesResource(a, b) {
		return ""
	}
	first, second := LogicRole(SpanActionRole(a)), LogicRole(SpanActionRole(b))
	if first == second || first == "" || second == "" {
		return ""
	}
	return first + " -> " + second
}

func SpanPairSharesResource(a, b SpanNode) bool {
	values := map[string]bool{}
	for _, st := range a.Steps {
		for _, slot := range SpanExpandedSlots(st.Slots) {
			if !SpanVariable(slot) || !SpanPairResourceSlot(slot) || redact.SensitiveSlot(st.Label, slot) {
				continue
			}
			values[slot.Type+"\x00"+slot.Value] = true
		}
	}
	for _, st := range b.Steps {
		for _, slot := range SpanExpandedSlots(st.Slots) {
			if SpanVariable(slot) && SpanPairResourceSlot(slot) && !redact.SensitiveSlot(st.Label, slot) && values[slot.Type+"\x00"+slot.Value] {
				return true
			}
		}
	}
	return false
}

func SpanPairResourceSlot(slot trace.Slot) bool {
	if len(slot.Value) < 4 || slot.Value == "/dev/null" {
		return false
	}
	switch slot.Type {
	case trace.SlotID, trace.SlotURL, trace.SlotPath:
		return true
	}
	return false
}

func SpanExplicitCommand(request string, n SpanNode) bool {
	if n.Call.Tool != "shell" {
		return false
	}
	words, err := shellparse.ProgramShellWords(n.Call.Command)
	if err != nil || shellparse.ProgramCommandRunsCode(words[0], words[1:]) {
		return false
	}
	mentioned := SpanWords(request)
	if !mentioned[strings.ToLower(words[0])] || len(words) < 2 {
		return false
	}
	first := strings.ToLower(words[1])
	return trace.TypeOf(shellparse.Word{Text: first}) == trace.SlotWord && mentioned[first]
}

func SpanRepeatedMotifSets(nodes []SpanNode, sets [][]int) [][]int {
	byShape := map[string][][]int{}
	seen := map[string]bool{}
	for _, set := range sets {
		if len(set) < 2 {
			continue
		}
		identity := SpanSetKey(nodes, set)
		if seen[identity] {
			continue
		}
		seen[identity] = true
		shape := SpanComposition(nodes, set).Key
		byShape[shape] = append(byShape[shape], set)
	}
	keys := make([]string, 0, len(byShape))
	for key := range byShape {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out [][]int
	for _, key := range keys {
		group := byShape[key]
		if len(group) < 2 {
			continue
		}
		sort.Slice(group, func(i, j int) bool {
			if len(group[i]) != len(group[j]) {
				return len(group[i]) > len(group[j])
			}
			return SpanSetKey(nodes, group[i]) < SpanSetKey(nodes, group[j])
		})
		used := map[int]bool{}
		union := []int{}
		iterations := 0
		for _, set := range group {
			overlaps := false
			for _, node := range set {
				if used[node] {
					overlaps = true
					break
				}
			}
			if overlaps {
				continue
			}
			iterations++
			for _, node := range set {
				used[node] = true
				union = append(union, node)
			}
		}
		if iterations < 2 {
			continue
		}
		sort.Ints(union)
		out = append(out, union)
	}
	return out
}

func SpanNavigationDependency(nodes []SpanNode, i int) bool {
	if len(nodes[i].Deps) != 1 {
		return false
	}
	parent := nodes[nodes[i].Deps[0]].Label
	return strings.Contains(parent, "tabs_context_mcp")
}

func SpanSetKey(nodes []SpanNode, set []int) string {
	var parts []string
	for _, i := range set {
		parts = append(parts, strconv.Itoa(nodes[i].Ordinal))
	}
	return strings.Join(parts, ",")
}

var SpanScriptWriteRe = regexp.MustCompile(`(?:^|[;&\n])\s*(?:cat|tee)\s*>?\s*([^\s<>;&]+)\s*<<`)

// A program authored in the transcript and run twice with different calls is
// a bounded subprocedure even if the containing request was an investigation.
// Only literal script paths count; shell-variable expansion is unresolved.
func SpanAuthoredProgramSets(nodes []SpanNode) [][]int {
	writes := map[string]int{}
	for i, n := range nodes {
		if n.Call.Tool == "shell" {
			for _, m := range SpanScriptWriteRe.FindAllStringSubmatch(n.Call.Command, -1) {
				if SpanProgramPath(m[1]) {
					writes[m[1]] = i
				}
			}
		} else if n.Call.Tool == "Write" || n.Call.Tool == "write_file" || n.Call.Tool == "Edit" {
			path := n.Call.Args["file_path"]
			if path == "" {
				path = n.Call.Args["path"]
			}
			if SpanProgramPath(path) {
				writes[path] = i
			}
		}
	}
	var out [][]int
	for path, write := range writes {
		set := []int{write}
		for i := write + 1; i < len(nodes); i++ {
			if SpanRunsScript(nodes[i].Call, path) {
				set = append(set, i)
			}
		}
		if len(set) >= 3 {
			out = append(out, set)
		}
	}
	return out
}

func SpanProgramPath(path string) bool {
	if len(path) < 5 || strings.ContainsAny(path, "$*`\n") {
		return false
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".sh", ".py", ".go", ".js", ".ts", ".rb":
		return true
	}
	return false
}

func SpanRunsScript(c trace.Call, path string) bool {
	if c.Tool != "shell" {
		return false
	}
	for _, line := range strings.FieldsFunc(c.Command, func(r rune) bool { return r == '\n' || r == ';' }) {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) >= 2 && f[0] == "timeout" {
			f = f[2:]
		}
		if len(f) >= 2 && (f[0] == "bash" || f[0] == "sh" || f[0] == "python" || f[0] == "python3" || f[0] == "node" || f[0] == "ruby") {
			f = f[1:]
		}
		if len(f) >= 3 && f[0] == "go" && f[1] == "run" {
			f = f[2:]
		}
		if len(f) > 0 && f[0] == path {
			return true
		}
	}
	return false
}

func SpanDistinctOperations(nodes []SpanNode, set []int) int {
	seen := map[string]bool{}
	for _, i := range set {
		seen[nodes[i].Label] = true
	}
	return len(seen)
}

// SpanDirectIntent requires a requested operation and a concrete object of
// that operation. A shared product name or tool name alone is not intent.
// This is retrieval evidence, never a useful-procedure judgment.
func SpanDirectIntent(requests []string, req int, n SpanNode) bool {
	if n.Call.Tool == "shell" {
		fields := strings.Fields(n.Call.Command)
		if len(fields) == 0 {
			return false
		}
		switch fields[0] {
		case "git", "kubectl", "gh", "docker", "helm", "tap":
		default:
			return false
		}
	}
	current := SpanWords(requests[req])
	want := current
	if req > 0 && SpanHasReference(current) {
		for r := req - 1; r >= 0; r-- {
			if strings.TrimSpace(requests[r]) != "" && !trace.IsHarness(requests[r]) && !history.IsClaudeContinuationSummary(requests[r]) {
				want = SpanWords(requests[r] + " " + requests[req])
				break
			}
		}
	}
	operation := n.Label
	if n.Call.Tool == "shell" {
		// Include only the command head: arbitrary grep patterns and file
		// arguments often copy words from the request but say nothing about
		// whether the call performs its requested operation.
		for i, w := range strings.Fields(n.Call.Command) {
			if i >= 3 {
				break
			}
			operation += " " + w
		}
	}
	for _, st := range n.Steps {
		for _, slot := range st.Slots {
			if !slot.Sub && !trace.Derived(slot.Key) && trace.SelectorKeys[slot.Key] && len(slot.Value) >= 4 {
				operation += " " + slot.Value
			}
		}
	}
	actual := SpanWords(operation)
	verbMatch := false
	for w := range current {
		if SpanVerb(w) != "" && SpanVerb(w) == SpanOperationVerb(actual, n) {
			verbMatch = true
			break
		}
	}
	if !verbMatch {
		return false
	}
	for w := range actual {
		if SpanVerb(w) == "" && !SpanGenericWord(w) && want[w] {
			return true
		}
	}
	return false
}

func SpanWords(text string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return r < 'a' || r > 'z'
	}) {
		if len(w) < 3 {
			continue
		}
		if strings.HasSuffix(w, "ies") && len(w) > 5 {
			w = strings.TrimSuffix(w, "ies") + "y"
		} else if strings.HasSuffix(w, "s") && len(w) > 5 && !strings.HasSuffix(w, "ss") && w != "status" {
			w = strings.TrimSuffix(w, "s")
		}
		out[w] = true
	}
	return out
}

func SpanHasReference(words map[string]bool) bool {
	for _, w := range []string{"this", "that", "these", "those", "them", "its", "again", "same"} {
		if words[w] {
			return true
		}
	}
	return false
}

func SpanGenericWord(w string) bool {
	switch w {
	case "telara", "jira", "gitlab", "github", "codex", "claude", "mcp", "sh", "shell", "tool", "call", "command", "action", "execute", "file", "repo", "project", "work", "task", "result", "output", "input", "status", "test", "script":
		return true
	}
	return false
}

// Verbs are semantic classes declared in code, rather than a text-similarity
// score. Unknown operations do not qualify for the direct one-call route.
func SpanVerb(w string) string {
	switch w {
	case "list", "show", "report", "get", "read", "fetch", "view", "check", "inspect", "find", "search", "query", "describe", "look", "status", "count", "grep":
		return "read"
	case "add", "create", "make", "write", "post", "send", "insert":
		return "create"
	case "close", "transition", "resolve":
		return "transition"
	case "update", "edit", "change", "set", "modify", "patch":
		return "update"
	case "delete", "remove", "archive":
		return "delete"
	case "run", "execute", "trigger", "start", "test":
		return "run"
	case "push", "publish", "deploy":
		return "publish"
	case "commit":
		return "commit"
	}
	return ""
}

func SpanOperationVerb(words map[string]bool, n SpanNode) string {
	// A tool name can contain both "get" and "status"; prefer its actual
	// operation over nouns that also happen to be verbs.
	for _, w := range []string{"commit", "transition", "close", "delete", "remove", "archive", "publish", "deploy", "push", "update", "edit", "patch", "create", "add", "post", "send", "run", "trigger", "test", "list", "search", "get", "read", "fetch", "view", "check", "show", "status", "grep"} {
		if words[w] {
			return SpanVerb(w)
		}
	}
	if n.Effect == "read" && strings.HasPrefix(n.Label, "sh:") {
		// Shell readers without an explicit operation (for example, ls) still
		// need a named object match below.
		return "read"
	}
	return ""
}

func MakeSpanProposal(s trace.Session, req int, nodes []SpanNode, set []int, kind string) model.SpanProposal {
	var calls []int
	var callHashes []string
	var labels, tools, inputShapes []string
	effect, score := "unknown", 0
	seenInputs := map[string]bool{}
	var inputs []model.SpanInput
	for _, i := range set {
		n := nodes[i]
		calls = append(calls, n.Ordinal)
		callHashes = append(callHashes, SpanCallHash(n.Call))
		labels = append(labels, n.Label)
		for _, st := range n.Steps {
			if !trace.BookkeepingTools[st.Label] {
				tools = append(tools, st.Label)
			}
		}
		if n.Effect == "write" {
			effect = "write"
		} else if n.Effect == "read" && effect != "write" {
			effect = "read"
		}
		if n.Call.Outcome == trace.OutcomeOK {
			score++
		}
		if n.Call.Output != "" {
			score++
		}
		for _, in := range n.Inputs {
			shape := in.Key + ":" + in.Type + ":" + in.Source
			inputShapes = append(inputShapes, shape)
			if in.Source != "unknown" {
				score++
			}
			if !seenInputs[shape] {
				inputs = append(inputs, in)
				seenInputs[shape] = true
			}
		}
	}
	sort.Strings(inputShapes)
	text := s.Requests[req]
	contextRequest := 0
	// Earlier task context distinguishes an elliptical follow-up from the
	// same short phrase in an unrelated session; it is not an authority grant.
	if req > 0 {
		for r := req - 1; r >= 0; r-- {
			if strings.TrimSpace(s.Requests[r]) != "" && !trace.IsHarness(s.Requests[r]) && !SpanSyntheticRequest(s, r) {
				text = s.Requests[r] + "\n" + text
				contextRequest = r + 1
				break
			}
		}
	}
	goal := ObjectRe.ReplaceAllStringFunc(strings.ToLower(text), func(v string) string {
		switch {
		case strings.HasPrefix(v, "http"):
			return "<url>"
		case strings.Contains(v, "/"):
			return "<path>"
		default:
			return "<id>"
		}
	})
	goal = strings.Join(strings.Fields(goal), " ")
	goalHash := sha256.Sum256([]byte(goal))
	var scope []string
	for _, i := range set {
		for _, st := range nodes[i].Steps {
			for _, sl := range st.Slots {
				if !trace.IsScopeSlot(st, sl) {
					continue
				}
				k := strings.ToLower(sl.Key)
				if strings.Contains(k, "context") || strings.Contains(k, "environment") || k == "env" || strings.Contains(k, "cluster") || strings.Contains(k, "profile") || strings.Contains(k, "namespace") {
					scope = append(scope, k+"="+strings.ToLower(sl.Value))
				}
			}
		}
	}
	sort.Strings(scope)
	shape := strings.Join([]string{hex.EncodeToString(goalHash[:6]), strings.Join(labels, ">"), strings.Join(inputShapes, ","), effect, strings.Join(scope, ",")}, "|")
	shapeHash := sha256.Sum256([]byte(shape))
	// The ID depends only on source identity and the selected call ordinals.
	idHash := sha256.Sum256([]byte(trace.EpisodeID(s.Client, s.ID, req) + "/" + strings.Join(IntsToStrings(calls), ",")))
	start := nodes[set[0]].Call.Time
	return model.SpanProposal{ID: "sp_" + hex.EncodeToString(idHash[:6]), Client: s.Client, Session: s.ID, Request: req,
		Task: s.Client + "/" + s.ID + "/" + strconv.Itoa(req), Status: model.BriefStatus, Kind: kind, Calls: calls, CallHashes: callHashes, Tools: tools,
		Inputs: inputs, Effect: effect, GoalKey: hex.EncodeToString(goalHash[:6]), ContextRequest: contextRequest,
		ShapeKey: "shape_" + hex.EncodeToString(shapeHash[:8]), Composition: SpanComposition(nodes, set), Review: AssessSpanTask(s, req, nodes, set, inputs), EvidenceScore: score, Start: start}
}

// SpanCallHash lets a brief match selected calls against the source session
// even when resumed-session copy removal shifted their numeric positions.
func SpanCallHash(c trace.Call) string {
	b, _ := json.Marshal(struct {
		ID, Tool, MCPServer, MCPTool, Command, Output string
		Args                                          map[string]string
		Outcome                                       trace.Outcome
		OutCollections                                []trace.ResultCollection `json:",omitempty"`
	}{c.ID, c.Tool, c.MCPServer, c.MCPTool, c.Command, c.Output, c.Args, c.Outcome, c.OutCollections})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:12])
}

func IntsToStrings(xs []int) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = strconv.Itoa(x)
	}
	return out
}

// GroupSpanProposals reduces review load without treating similar tool use
// as proof of one procedure. Exact shape keys must agree before grouping.
func GroupSpanProposals(ps []model.SpanProposal) []model.SpanGroup {
	by := map[string]*model.SpanGroup{}
	sessions := map[string]map[string]bool{}
	for _, p := range ps {
		g := by[p.ShapeKey]
		if g == nil {
			g = &model.SpanGroup{ShapeKey: p.ShapeKey, Example: p}
			by[p.ShapeKey] = g
			sessions[p.ShapeKey] = map[string]bool{}
		}
		g.Proposals++
		g.Members = append(g.Members, p.ID)
		sessions[p.ShapeKey][p.Client+"/"+p.Session] = true
		if p.EvidenceScore > g.Example.EvidenceScore || p.EvidenceScore == g.Example.EvidenceScore && p.Start.After(g.Example.Start) {
			g.Example = p
		}
	}
	out := make([]model.SpanGroup, 0, len(by))
	for key, g := range by {
		g.Sessions = len(sessions[key])
		sort.Strings(g.Members)
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sessions != out[j].Sessions {
			return out[i].Sessions > out[j].Sessions
		}
		if out[i].Proposals != out[j].Proposals {
			return out[i].Proposals > out[j].Proposals
		}
		return out[i].ShapeKey < out[j].ShapeKey
	})
	return out
}

func SpanSyntheticRequest(s trace.Session, req int) bool {
	return req >= 0 && req < len(s.Requests) &&
		(req < len(s.RequestRoles) && s.RequestRoles[req] == "synthetic_context" ||
			history.IsClaudeContinuationSummary(s.Requests[req]))
}

func AssessSpanTask(s trace.Session, req int, nodes []SpanNode, set []int, inputs []model.SpanInput) model.SpanTaskReview {
	r := model.SpanTaskReview{Source: "user", Input: "unresolved", Output: "unobserved", Stop: "single_pass"}
	if req < len(s.RequestRoles) && s.RequestRoles[req] == "scheduled" {
		r.Source = "scheduled"
	}
	if SpanSyntheticRequest(s, req) {
		r.Source = "synthetic_context"
		r.Reasons = append(r.Reasons, "synthetic_request")
	}
	request := s.Requests[req]
	if SpanOpenEndedTask(request) {
		r.Reasons = append(r.Reasons, "judgment_boundary_unresolved")
	}
	if len(set) == 1 {
		// A direct tool invocation with its raw result is already available
		// to the agent. It contains no captured composition or transformation.
		r.Reasons = append(r.Reasons, "single_tool_passthrough")
	}
	if SpanHasReference(SpanWords(request)) {
		for i := req - 1; i >= 0; i-- {
			if !SpanSyntheticRequest(s, i) && !trace.IsHarness(s.Requests[i]) && strings.TrimSpace(s.Requests[i]) != "" {
				request = s.Requests[i] + " " + request
				break
			}
		}
	}
	userIntent := false
	for _, i := range set {
		if SpanDirectIntent(s.Requests, req, nodes[i]) || SpanOperationIntent(request, SpanActionRole(nodes[i])) {
			userIntent = true
		}
	}
	if !userIntent {
		r.Reasons = append(r.Reasons, "no_requested_operation")
	}
	known, unknown := false, false
	for _, in := range inputs {
		if in.Source == "unknown" {
			unknown = true
		} else {
			known = true
		}
	}
	switch {
	case unknown:
		r.Input = "unresolved"
		r.Reasons = append(r.Reasons, "input_provenance_unknown")
	case known:
		r.Input = "caller_or_result"
	default:
		// A zero-argument command can have a fixed, visible scope. The task
		// match above still has to establish why it was run.
		r.Input = "fixed_scope"
	}
	goodOutput, failed := false, false
	for _, i := range set {
		c := nodes[i].Call
		if c.Outcome == trace.OutcomeFailed || SpanOversizeResult(c.Output) {
			failed = true
		}
		if strings.TrimSpace(c.Output) != "" {
			goodOutput = true
		}
	}
	if failed {
		r.Reasons = append(r.Reasons, "failed_or_oversized_call")
	}
	if goodOutput && !failed {
		r.Output = "tool_result_observed"
	} else {
		r.Reasons = append(r.Reasons, "output_not_observed")
	}
	composition := SpanComposition(nodes, set)
	for _, repeat := range composition.Repetition {
		if repeat.Kind != "for_each" {
			r.Stop = "unproven_repeat"
			r.Reasons = append(r.Reasons, "stop_condition_unproven")
			break
		}
		r.Stop = "bounded_input_list"
	}
	r.Ready = len(r.Reasons) == 0
	// A result-derived multi-call component may be worth authoring even when
	// its enclosing user request is an investigation. Keep its missing task
	// contract visible instead of calling it a complete procedure.
	r.Component = len(set) > 1 && len(composition.Edges) > 0 && (r.Source == "user" || r.Source == "scheduled") &&
		!failed && goodOutput && !SpanHasReason(r.Reasons, "stop_condition_unproven")
	return r
}

func SpanHasReason(reasons []string, want string) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}

// Match the requested operation and resource, rather than a provider name or
// concrete identifier alone. This extends direct-intent matching to result
// chains whose first call may obtain the input for later calls.
func SpanOperationIntent(request, action string) bool {
	want, actual := SpanWords(request), SpanWords(action)
	verb := false
	for w := range want {
		if SpanVerb(w) != "" && SpanVerb(w) == SpanOperationVerb(actual, SpanNode{}) {
			verb = true
			break
		}
	}
	if !verb {
		return false
	}
	for w := range actual {
		if want[w] && !SpanGenericWord(w) && SpanVerb(w) == "" {
			return true
		}
	}
	return false
}

func SpanOversizeResult(output string) bool {
	text := strings.ToLower(output)
	return strings.Contains(text, "response exceeded") || strings.Contains(text, "output exceeds") ||
		strings.Contains(text, "result too large")
}

func SpanOpenEndedTask(request string) bool {
	w := SpanWords(request)
	for _, cue := range []string{"why", "how", "investigate", "diagnose", "debug", "fix", "implement", "build", "design"} {
		if w[cue] {
			// A named collection step can still be a bounded subtask of a
			// larger investigation. The label must make that step explicit.
			if (cue == "investigate" || cue == "diagnose") && (w["collect"] || w["list"] || w["fetch"]) {
				continue
			}
			return true
		}
	}
	return false
}

// ReviewSpanProposals keeps the broad causal inventory available for audits
// while giving the human queue only proposals with an observable task contract.
func ReviewSpanProposals(ps []model.SpanProposal) []model.SpanProposal {
	out := make([]model.SpanProposal, 0)
	for _, p := range ps {
		if p.Review.Ready {
			out = append(out, p)
		}
	}
	return out
}

func ReviewSpanComponents(ps []model.SpanProposal) []model.SpanProposal {
	out := make([]model.SpanProposal, 0)
	for _, p := range ps {
		if p.Review.Component && !p.Review.Ready {
			out = append(out, p)
		}
	}
	return out
}
