package discover

import (
	"sort"
	"strings"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// SpanComposition is a structural retrieval bucket. It describes observed
// operations and result dependencies, not a safe or useful primitive contract.
// Repetition is evidence about this trace; the key omits its observed count.
type SpanComposition struct {
	Key        string       `json:"key"`
	Actions    []string     `json:"actions"`
	Edges      []string     `json:"edges,omitempty"`
	Repetition []SpanRepeat `json:"repetition,omitempty"`
}

type SpanRepeat struct {
	Action string `json:"action"`
	Count  int    `json:"count"`
	Kind   string `json:"kind"` // for_each, repeated, or dependent_repeat
}

type SpanCompositionGroup struct {
	Key       string       `json:"key"`
	Proposals int          `json:"proposals"`
	Sessions  int          `json:"sessions"`
	Example   SpanProposal `json:"example"`
	Members   []string     `json:"members"`
}

// spanComposition keeps the operation's provider and explicit state selector,
// then records which selected operation supplied each result-derived input.
// Repeated independent calls and repeated whole motifs share the same action
// skeleton, while dependency edges keep a sequential state machine distinct.
func spanComposition(nodes []spanNode, set []int) SpanComposition {
	roles := make([]string, len(set))
	byOrdinal := make(map[int]string, len(set))
	for j, i := range set {
		roles[j] = spanActionRole(nodes[i])
		byOrdinal[nodes[i].ordinal] = roles[j]
	}
	edgeSet := map[string]bool{}
	for j, i := range set {
		for _, in := range nodes[i].inputs {
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
		if j > 0 && role == roles[j-1] && spanCanFoldRepeat(nodes[set[j-1]], nodes[set[j]]) {
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
			if !spanCanFoldRepeat(nodes[set[actionNodes[j-period]]], nodes[set[actionNodes[j]]]) {
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
	var repeats []SpanRepeat
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
			for _, in := range nodes[i].inputs {
				if in.Source != "prior_result" || in.FromCall == 0 {
					continue
				}
				if byOrdinal[in.FromCall] == role {
					kind = "dependent_repeat"
					break
				}
			}
		}
		if kind != "dependent_repeat" && spanRepeatedItems(nodes, set, roles, role) {
			kind = "for_each"
		}
		repeats = append(repeats, SpanRepeat{Action: role, Count: counts[role], Kind: kind})
	}
	// A readable structural key allows reviewers to see exactly why two
	// traces were bucketed. It is not a hash of prompt text or concrete IDs.
	key := "actions=" + strings.Join(actions, " -> ") + "|edges=" + strings.Join(edges, ";")
	return SpanComposition{Key: key, Actions: actions, Edges: edges, Repetition: repeats}
}

func spanDependsOn(n spanNode, ordinal int) bool {
	for _, in := range n.inputs {
		if in.Source == "prior_result" && in.FromCall == ordinal {
			return true
		}
	}
	return false
}

func spanCanFoldRepeat(a, b spanNode) bool {
	if spanActionRole(a) != spanActionRole(b) || spanDependsOn(b, a.ordinal) {
		return false
	}
	aTargets, bTargets := spanTargetSlots(a), spanTargetSlots(b)
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
	if len(a.call.OutIDs) > 0 && len(b.call.OutIDs) > 0 {
		return strings.Join(a.call.OutIDs, "\x00") != strings.Join(b.call.OutIDs, "\x00")
	}
	return false
}

func spanTargetSlots(n spanNode) map[string]string {
	out := map[string]string{}
	for _, st := range n.steps {
		for _, sl := range spanExpandedSlots(st.Slots) {
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
func spanRepeatedItems(nodes []spanNode, set []int, roles []string, role string) bool {
	values := map[string]map[string]bool{}
	byOrdinal := map[int]string{}
	for j, i := range set {
		byOrdinal[nodes[i].ordinal] = roles[j]
	}
	for j, i := range set {
		if roles[j] != role {
			continue
		}
		for _, st := range nodes[i].steps {
			for _, sl := range spanExpandedSlots(st.Slots) {
				if sl.Type != trace.SlotID && sl.Type != trace.SlotURL && sl.Type != trace.SlotPath && sl.Type != trace.SlotNumber {
					continue
				}
				for _, in := range nodes[i].inputs {
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
	return spanRepeatedArgumentVariation(nodes, set, roles, role)
}

// A call can be a loop item even when the agent authored a text field that
// was not copied verbatim from the request. At invocation that changing field
// becomes a typed caller input. Require independent operations and a stable
// argument slot; changed output IDs alone could be a retry.
func spanRepeatedArgumentVariation(nodes []spanNode, set []int, roles []string, role string) bool {
	var operations []spanNode
	for j, i := range set {
		if roles[j] == role {
			operations = append(operations, nodes[i])
		}
	}
	if len(operations) < 2 {
		return false
	}
	for i := 1; i < len(operations); i++ {
		if !spanCanFoldRepeat(operations[i-1], operations[i]) {
			return false
		}
	}
	fields := map[string]map[string]bool{}
	present := map[string]int{}
	for _, op := range operations {
		for path, field := range trace.ObservedArgs(op.call) {
			if trace.OperationSelector(op.call, path) {
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

func spanActionRole(n spanNode) string {
	tool := strings.ToLower(n.call.Tool)
	role := n.label
	switch {
	case tool == "mcp:telara_execute_action":
		integration, action := strings.ToLower(n.call.Args["integration"]), strings.ToLower(n.call.Args["action"])
		if integration != "" && action != "" {
			role = integration + "." + action
		} else {
			role = "telara.execute_action[unresolved]"
		}
	case tool == "shell":
		role = n.label
	case tool != "":
		role = tool
	}
	// State and relationship choices can change the operation. Preserve
	// these declared selectors, but never use variable resource IDs or text.
	var selectors []string
	for _, st := range n.steps {
		for _, sl := range spanExpandedSlots(st.Slots) {
			if sensitiveSlot(st.Label, sl) {
				continue
			}
			k := strings.ToLower(sl.Key)
			if trace.ScopeFlags[strings.SplitN(k, "#", 2)[0]] && sl.Value != "" && len(sl.Value) <= 40 {
				selectors = append(selectors, k+"="+strings.ToLower(Redact(sl.Value)))
				continue
			}
			switch k {
			case "status", "state", "transition", "transition_id", "resolution":
				if sl.Value != "" && len(sl.Value) <= 40 {
					selectors = append(selectors, k+"="+strings.ToLower(Redact(sl.Value)))
				}
			case "environment", "env", "cluster", "namespace", "context", "profile":
				if sl.Value != "" && len(sl.Value) <= 40 {
					selectors = append(selectors, k+"="+strings.ToLower(Redact(sl.Value)))
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
func GroupSpanCompositions(ps []SpanProposal) []SpanCompositionGroup {
	by := map[string]*SpanCompositionGroup{}
	sessions := map[string]map[string]bool{}
	for _, p := range ps {
		key := p.Composition.Key
		if key == "" {
			continue
		}
		g := by[key]
		if g == nil {
			g = &SpanCompositionGroup{Key: key, Example: p}
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
	out := make([]SpanCompositionGroup, 0, len(by))
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
