package compile

import (
	"fmt"
	"sort"
	"strings"

	"telara.dev/tap/internal/diag"
)

// maxLivenessCombos caps the branch-combination search. Real primitives carry a
// handful of gates at most; the cap guards a pathological fan-in without ever
// silently skipping the check for the corpus.
const maxLivenessCombos = 200000

// simulateLiveness runs the §6 liveness simulation: for every combination of
// branch outcomes it computes which nodes stay active and asserts the terminal
// END node is reached. The engine's static validator only proves reachability
// on the full graph; at run time a join whose incoming edges all belong to
// non-selected branch arms is skipped, and if that join is END the workflow
// completes without ever assembling named outputs (or dead-ends as
// WORKFLOW_DEADLOCK). Catching it here, before emitting, is the whole point.
func simulateLiveness(def *Definition) diag.Findings {
	var out diag.Findings

	nodesByID := map[string]*Node{}
	for i := range def.Nodes {
		nodesByID[def.Nodes[i].ID] = &def.Nodes[i]
	}
	outgoing := map[string][]Edge{}
	for _, e := range def.Edges {
		outgoing[e.FromNodeID] = append(outgoing[e.FromNodeID], e)
	}

	topo, ok := topoOrder(def)
	if !ok {
		out = append(out, diag.Error(diag.ClassSchema, "compile-cycle", "workflow.yaml#steps",
			"compiled graph is cyclic; the executor cannot schedule it", "break the step dependency cycle"))
		return out
	}

	// Each branch node contributes one "selected edge" per combination.
	var branches []string
	for _, n := range def.Nodes {
		if n.Type == NodeBranch {
			branches = append(branches, n.ID)
		}
	}
	sort.Strings(branches)

	combos := 1
	for _, br := range branches {
		deg := len(outgoing[br])
		if deg == 0 {
			deg = 1
		}
		combos *= deg
		if combos > maxLivenessCombos {
			break
		}
	}
	if combos > maxLivenessCombos {
		out = append(out, diag.Warn(diag.ClassSchema, "liveness-search-capped", "workflow.yaml#steps",
			fmt.Sprintf("branch-combination space (%d+) exceeds the liveness cap; exhaustive unreachable-join proof skipped", combos),
			"reduce branch fan-in or accept a representative-combination check"))
		// Fall back to the two extreme combos: all-default and all-first.
		for _, sel := range []string{"default", "first"} {
			if bad, combo := deadEndCombo(def, nodesByID, outgoing, branches, sel); bad != "" {
				out = append(out, deadEndFinding(bad, combo))
			}
		}
		return out
	}

	// Exhaustive: iterate the mixed-radix branch-choice space.
	radix := make([]int, len(branches))
	for i, br := range branches {
		radix[i] = len(outgoing[br])
		if radix[i] == 0 {
			radix[i] = 1
		}
	}
	idx := make([]int, len(branches))
	for {
		selected := map[string]string{} // branch id -> selected to-node id
		for i, br := range branches {
			edges := outgoing[br]
			if len(edges) > 0 {
				selected[br] = edges[idx[i]].ToNodeID
			}
		}
		if !endActive(def, outgoing, topo, selected) {
			out = append(out, deadEndFinding(endNodeID, describeCombo(outgoing, branches, idx)))
			// One representative failure is enough; keep output actionable.
			return out
		}
		// increment mixed-radix counter
		k := 0
		for k < len(idx) {
			idx[k]++
			if idx[k] < radix[k] {
				break
			}
			idx[k] = 0
			k++
		}
		if k == len(idx) {
			break
		}
	}
	return out
}

// endActive computes the active set for one branch-selection combo and reports
// whether END is active. A node is active iff it is the entry, or it has at
// least one live incoming edge from an active source: for a branch source the
// live edge is the selected one; for any other source every outgoing edge is
// live. Processed in topological order so each node's sources are settled first.
func endActive(def *Definition, outgoing map[string][]Edge, topo []string, selected map[string]string) bool {
	active := map[string]bool{}
	if def.EntryNodeID != "" {
		active[def.EntryNodeID] = true
	}
	typeByID := map[string]NodeType{}
	for _, n := range def.Nodes {
		typeByID[n.ID] = n.Type
	}
	for _, id := range topo {
		if active[id] {
			// propagate from an already-active source
			for _, e := range outgoing[id] {
				if typeByID[id] == NodeBranch {
					if selected[id] == e.ToNodeID {
						active[e.ToNodeID] = true
					}
					continue
				}
				active[e.ToNodeID] = true
			}
		}
	}
	return active[endNodeID]
}

// deadEndCombo checks a single named selection strategy ("default" or "first")
// for the capped fallback path.
func deadEndCombo(def *Definition, nodesByID map[string]*Node, outgoing map[string][]Edge, branches []string, strategy string) (string, string) {
	selected := map[string]string{}
	var parts []string
	for _, br := range branches {
		edges := outgoing[br]
		if len(edges) == 0 {
			continue
		}
		pick := edges[0]
		if strategy == "default" {
			for _, e := range edges {
				if e.DefaultEdge {
					pick = e
					break
				}
			}
		}
		selected[br] = pick.ToNodeID
		parts = append(parts, br+"->"+pick.ToNodeID)
	}
	topo, _ := topoOrder(def)
	if !endActive(def, outgoing, topo, selected) {
		return endNodeID, strings.Join(parts, ", ")
	}
	return "", ""
}

func deadEndFinding(join, combo string) diag.Finding {
	return diag.Error(diag.ClassSchema, "unreachable-join", "workflow.yaml#steps",
		fmt.Sprintf("liveness simulation: node %q is not reached under branch combination [%s]; the executor would skip it and never assemble outputs (WORKFLOW_DEADLOCK risk, 09 §6)", join, combo),
		"ensure every branch combination has an active path to the terminal node (e.g. route the skip/default arm to the same join)")
}

func describeCombo(outgoing map[string][]Edge, branches []string, idx []int) string {
	var parts []string
	for i, br := range branches {
		edges := outgoing[br]
		if len(edges) > 0 {
			parts = append(parts, br+"->"+edges[idx[i]].ToNodeID)
		}
	}
	return strings.Join(parts, ", ")
}

// topoOrder returns a topological ordering of node ids over all edges (Kahn),
// and false if the graph is cyclic.
func topoOrder(def *Definition) ([]string, bool) {
	indeg := map[string]int{}
	outgoing := map[string][]string{}
	present := map[string]bool{}
	for _, n := range def.Nodes {
		present[n.ID] = true
		if _, ok := indeg[n.ID]; !ok {
			indeg[n.ID] = 0
		}
	}
	for _, e := range def.Edges {
		if !present[e.FromNodeID] || !present[e.ToNodeID] {
			continue
		}
		outgoing[e.FromNodeID] = append(outgoing[e.FromNodeID], e.ToNodeID)
		indeg[e.ToNodeID]++
	}
	var queue []string
	for id, d := range indeg {
		if d == 0 {
			queue = append(queue, id)
		}
	}
	sort.Strings(queue)
	var order []string
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		order = append(order, id)
		var next []string
		for _, to := range outgoing[id] {
			indeg[to]--
			if indeg[to] == 0 {
				next = append(next, to)
			}
		}
		sort.Strings(next)
		queue = append(queue, next...)
	}
	return order, len(order) == len(present)
}
