package compile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"telara.dev/tap/internal/model"
	"telara.dev/tap/internal/validate"
)

// corpusPackage locates one of the seven golden packages: the two example
// packages live under telara-documentation, the five seeds under telara-tap.
type corpusPackage struct {
	name string
	dir  string
}

func corpus(t *testing.T) []corpusPackage {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Skip("cannot resolve workspace root")
	}
	examples := filepath.Join(root, "..", "telara-documentation", "architecture", "tap", "examples")
	primitives := filepath.Join(root, "primitives")
	pkgs := []corpusPackage{
		{"gitlab-pipeline-triage", filepath.Join(examples, "gitlab-pipeline-triage")},
		{"web-changelog-watch", filepath.Join(examples, "web-changelog-watch")},
		{"jira-gitlab-sprint-delivery-audit", filepath.Join(primitives, "jira-gitlab-sprint-delivery-audit")},
		{"slack-standup-digest", filepath.Join(primitives, "slack-standup-digest")},
		{"linear-cycle-health-monitor", filepath.Join(primitives, "linear-cycle-health-monitor")},
		{"confluence-doc-engagement-pulse", filepath.Join(primitives, "confluence-doc-engagement-pulse")},
		{"gitlab-mr-review-queue", filepath.Join(primitives, "gitlab-mr-review-queue")},
	}
	var out []corpusPackage
	for _, p := range pkgs {
		if _, err := os.Stat(p.dir); err == nil {
			out = append(out, p)
		} else {
			t.Logf("skipping %s: %v", p.name, err)
		}
	}
	if len(out) == 0 {
		t.Skip("no corpus packages found on disk")
	}
	return out
}

// TestGolden_CompileCorpus compiles all seven packages, asserts each produces a
// valid, engine-conformant WorkflowDefinition, and diffs against committed
// golden fixtures (regenerate with UPDATE_GOLDEN=1).
func TestGolden_CompileCorpus(t *testing.T) {
	for _, p := range corpus(t) {
		t.Run(p.name, func(t *testing.T) {
			pkg, err := model.LoadPackage(p.dir)
			if err != nil {
				t.Fatalf("LoadPackage: %v", err)
			}
			if v := validate.Validate(pkg); v.HasErrors() {
				t.Fatalf("package does not validate: %v", v.Errors())
			}
			def, findings, err := Compile(pkg)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			if findings.HasErrors() {
				t.Fatalf("compile errors: %v", findings.Errors())
			}

			assertEngineInvariants(t, def)
			assertExpressionDialect(t, def)

			got, err := json.MarshalIndent(def, "", "  ")
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			goldenPath := filepath.Join("testdata", "golden", p.name+".compiled.json")
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(goldenPath, append(got, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
				t.Logf("updated golden %s", goldenPath)
				return
			}
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden (run with UPDATE_GOLDEN=1 to create): %v", err)
			}
			if strings.TrimSpace(string(want)) != strings.TrimSpace(string(got)) {
				t.Errorf("compiled output drifted from golden %s (run UPDATE_GOLDEN=1 to refresh if intended)", goldenPath)
			}
		})
	}
}

// assertEngineInvariants mirrors the executor's own static validator
// (telara-orchestrator/.../workflowgraph/validator.go) so the golden output is
// provably executable-shaped without importing the engine module:
//   - exactly one terminal END node, and it is a sink
//   - entry_node_id references an existing node
//   - every node is reachable from entry_node_id (single-entry reachability)
//   - every edge references existing nodes
//   - every branch node has an outgoing conditional-or-default edge
//   - tool nodes carry integration_type + tool_name
func assertEngineInvariants(t *testing.T, def *Definition) {
	t.Helper()
	byID := map[string]*Node{}
	for i := range def.Nodes {
		if _, dup := byID[def.Nodes[i].ID]; dup {
			t.Fatalf("duplicate node id %q", def.Nodes[i].ID)
		}
		byID[def.Nodes[i].ID] = &def.Nodes[i]
	}

	ends := 0
	for _, n := range def.Nodes {
		if n.Type == NodeEnd {
			ends++
		}
	}
	if ends != 1 {
		t.Fatalf("expected exactly 1 END node, got %d", ends)
	}

	if def.EntryNodeID == "" {
		t.Fatal("entry_node_id is empty")
	}
	if _, ok := byID[def.EntryNodeID]; !ok {
		t.Fatalf("entry_node_id %q does not reference a node", def.EntryNodeID)
	}

	outgoing := map[string][]Edge{}
	for _, e := range def.Edges {
		if _, ok := byID[e.FromNodeID]; !ok {
			t.Fatalf("edge from unknown node %q", e.FromNodeID)
		}
		if _, ok := byID[e.ToNodeID]; !ok {
			t.Fatalf("edge to unknown node %q", e.ToNodeID)
		}
		outgoing[e.FromNodeID] = append(outgoing[e.FromNodeID], e)
	}

	// Single-entry reachability (validator.go validateReachability).
	seen := map[string]bool{}
	stack := []string{def.EntryNodeID}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[id] {
			continue
		}
		seen[id] = true
		for _, e := range outgoing[id] {
			stack = append(stack, e.ToNodeID)
		}
	}
	var unreachable []string
	for id := range byID {
		if !seen[id] {
			unreachable = append(unreachable, id)
		}
	}
	if len(unreachable) > 0 {
		sort.Strings(unreachable)
		t.Fatalf("nodes unreachable from entry %q: %v", def.EntryNodeID, unreachable)
	}

	// END is a sink.
	if len(outgoing[endNodeID]) != 0 {
		t.Fatalf("END node has outgoing edges: %v", outgoing[endNodeID])
	}

	for _, n := range def.Nodes {
		switch n.Type {
		case NodeTool:
			if n.Tool == nil || n.Tool.IntegrationType == "" || n.Tool.ToolName == "" {
				t.Errorf("tool node %q missing integration_type/tool_name", n.ID)
			}
		case NodeBranch:
			hasCondOrDefault := false
			for _, e := range outgoing[n.ID] {
				if e.DefaultEdge || strings.TrimSpace(e.ConditionExpression) != "" {
					hasCondOrDefault = true
				}
			}
			if !hasCondOrDefault {
				t.Errorf("branch node %q has no conditional/default outgoing edge", n.ID)
			}
		case NodeModel:
			if n.Model == nil || strings.TrimSpace(n.Model.Prompt) == "" {
				t.Errorf("model node %q missing prompt", n.ID)
			}
		}
	}
}

// assertExpressionDialect confirms deliverable #3's round-trip contract: every
// emitted source_expression / condition parses under the executor's real
// resolveWorkflowExpression cases (trigger. / nodes. / node. / outputs.) and the
// compiler never emits workflow.* , the outputs.* alias, or {{ }} wrappers.
func assertExpressionDialect(t *testing.T, def *Definition) {
	t.Helper()
	check := func(loc, expr string) {
		if expr == "" {
			return
		}
		if strings.Contains(expr, "{{") || strings.Contains(expr, "${") {
			t.Errorf("%s: expression %q uses a brace/template wrapper (forbidden)", loc, expr)
		}
		if strings.HasPrefix(expr, "workflow.") || strings.HasPrefix(expr, "outputs.") {
			t.Errorf("%s: expression %q uses an accidental executor alias (workflow.*/outputs.*)", loc, expr)
		}
	}
	// Bindings and outputs must resolve to a node output or trigger data.
	sourcePrefixOK := func(expr string) bool {
		return strings.HasPrefix(expr, "trigger.") || strings.HasPrefix(expr, "nodes.") ||
			expr == "trigger" || expr == "inputs"
	}
	for _, n := range def.Nodes {
		for _, bnd := range append(append([]Binding{}, n.InputBindings...), n.OutputBindings...) {
			check(n.ID+".binding", bnd.SourceExpression)
			if bnd.SourceExpression != "" && !sourcePrefixOK(bnd.SourceExpression) {
				t.Errorf("node %q binding source_expression %q is not a trigger./nodes. path", n.ID, bnd.SourceExpression)
			}
		}
		if n.Branch != nil {
			check(n.ID+".branch", n.Branch.ConditionExpression)
		}
	}
	for _, e := range def.Edges {
		check("edge", e.ConditionExpression)
	}
	for _, o := range def.Outputs {
		check("output."+o.Name, o.SourceExpression)
		if o.SourceExpression != "" && !sourcePrefixOK(o.SourceExpression) {
			t.Errorf("output %q source_expression %q is not a trigger./nodes. path", o.Name, o.SourceExpression)
		}
	}
}
