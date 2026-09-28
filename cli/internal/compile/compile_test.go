package compile

import (
	"testing"

	"telara.dev/tap/internal/model"
)

func TestRewriteExpression(t *testing.T) {
	cases := map[string]string{
		"inputs.x":                    "trigger.x",
		"inputs.now":                  "trigger.now",
		"steps.fetch":                 "nodes.fetch.result",
		"steps.fetch.items.0.id":      "nodes.fetch.result.items.0.id",
		"steps.classify.unclassified": "nodes.classify.result.unclassified",
	}
	for in, want := range cases {
		if got := RewriteExpression(in); got != want {
			t.Errorf("RewriteExpression(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRewriteCondition(t *testing.T) {
	cases := map[string]string{
		"steps.classify.has_unclassified == true": "nodes.classify.result.has_unclassified == true",
		"steps.pick_thread.thread_ts":             "nodes.pick_thread.result.thread_ts",
		"steps.rank.has_mrs == true":              "nodes.rank.result.has_mrs == true",
		"steps.page.drift == true":                "nodes.page.result.drift == true",
	}
	for in, want := range cases {
		if got := RewriteCondition(in); got != want {
			t.Errorf("RewriteCondition(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitParams(t *testing.T) {
	params := map[string]interface{}{
		"project_id":  map[string]interface{}{"from": "inputs.project_id"},
		"per_page":    1,
		"order_by":    "updated_at",
		"pipeline_id": map[string]interface{}{"from": "steps.pipeline.items.0.id"},
	}
	statics, bindings := splitParams(params)
	if statics["per_page"] != 1 || statics["order_by"] != "updated_at" {
		t.Errorf("statics wrong: %v", statics)
	}
	if _, ok := statics["project_id"]; ok {
		t.Errorf("from-ref leaked into statics")
	}
	if len(bindings) != 2 {
		t.Fatalf("expected 2 bindings, got %d: %v", len(bindings), bindings)
	}
	byPath := map[string]Binding{}
	for _, b := range bindings {
		byPath[b.TargetPath] = b
	}
	if byPath["project_id"].SourceExpression != "trigger.project_id" {
		t.Errorf("project_id binding = %q", byPath["project_id"].SourceExpression)
	}
	if byPath["pipeline_id"].SourceExpression != "nodes.pipeline.result.items.0.id" {
		t.Errorf("pipeline_id binding = %q", byPath["pipeline_id"].SourceExpression)
	}
}

func TestSplitParams_NestedComposite(t *testing.T) {
	// {updated_after: {days_ago: {from: inputs.lookback}}} flattens onto a dot
	// target-path (09 §5), with any literal siblings kept in statics.
	params := map[string]interface{}{
		"updated_after": map[string]interface{}{
			"days_ago": map[string]interface{}{"from": "inputs.lookback"},
			"unit":     "days",
		},
	}
	statics, bindings := splitParams(params)
	if len(bindings) != 1 || bindings[0].TargetPath != "updated_after.days_ago" {
		t.Fatalf("expected nested binding updated_after.days_ago, got %v", bindings)
	}
	if bindings[0].SourceExpression != "trigger.lookback" {
		t.Errorf("nested source_expression = %q", bindings[0].SourceExpression)
	}
	sub, ok := statics["updated_after"].(map[string]interface{})
	if !ok || sub["unit"] != "days" {
		t.Errorf("literal sibling lost: %v", statics)
	}
}

// TestLiveness_UnreachableJoin builds a graph where a branch's two arms both
// bypass the terminal node under one combination, and asserts the simulation
// flags it (the WORKFLOW_DEADLOCK class the §6 sim exists to catch).
func TestLiveness_UnreachableJoin(t *testing.T) {
	def := &Definition{
		EntryNodeID: "b",
		Nodes: []Node{
			{ID: "b", Type: NodeBranch},
			{ID: "left", Type: NodeNoop},
			{ID: "right", Type: NodeNoop},
			{ID: endNodeID, Type: NodeEnd},
		},
		Edges: []Edge{
			{FromNodeID: "b", ToNodeID: "left", ConditionExpression: "trigger.x == \"a\""},
			{FromNodeID: "b", ToNodeID: "right", DefaultEdge: true},
			// left reaches END, right does NOT -> the default arm strands END.
			{FromNodeID: "left", ToNodeID: endNodeID},
		},
	}
	findings := simulateLiveness(def)
	if !findings.HasErrors() {
		t.Fatalf("expected an unreachable-join error, got: %v", findings)
	}
}

// TestLiveness_DiamondReconverges confirms the when-gate diamond the compiler
// emits is deadlock-free: both branch arms reach END.
func TestLiveness_DiamondReconverges(t *testing.T) {
	def := &Definition{
		EntryNodeID: "src",
		Nodes: []Node{
			{ID: "src", Type: NodeNoop},
			{ID: "g", Type: NodeBranch},
			{ID: "work", Type: NodeModel, Model: &ModelConfig{Prompt: "x"}},
			{ID: "skip", Type: NodeNoop},
			{ID: "join", Type: NodeTransform},
			{ID: endNodeID, Type: NodeEnd},
		},
		Edges: []Edge{
			{FromNodeID: "src", ToNodeID: "g"},
			{FromNodeID: "g", ToNodeID: "work", ConditionExpression: "trigger.on == true"},
			{FromNodeID: "g", ToNodeID: "skip", DefaultEdge: true},
			{FromNodeID: "work", ToNodeID: "join"},
			{FromNodeID: "skip", ToNodeID: "join"},
			{FromNodeID: "join", ToNodeID: endNodeID},
		},
	}
	if findings := simulateLiveness(def); findings.HasErrors() {
		t.Fatalf("diamond should be live, got: %v", findings.Errors())
	}
}

// sanity: model.ParseFrom is the binding-detection primitive the splitter uses.
func TestParseFromContract(t *testing.T) {
	if _, ok := model.ParseFrom(map[string]interface{}{"from": "inputs.x"}); !ok {
		t.Fatal("ParseFrom failed on a valid from-map")
	}
	if _, ok := model.ParseFrom("literal"); ok {
		t.Fatal("ParseFrom matched a literal")
	}
}
