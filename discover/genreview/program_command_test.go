package genreview_test

import (
	"strings"
	"testing"

	"gitlab.com/telara-labs/tap-runtime/discover/internal/testkit"

	"gitlab.com/telara-labs/tap-runtime/discover/genreview"

	"gitlab.com/telara-labs/tap-runtime/discover/codegen"

	"gitlab.com/telara-labs/tap-runtime/discover/model"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

func TestGeneratedReviewOrderUsesGraphEffectsAndBindings(t *testing.T) {
	cases := []struct {
		name  string
		step  codegen.ProgramStep
		tier  int
		shape string
	}{
		{"dependent write", codegen.ProgramStep{Effect: "write", Args: []codegen.ProgramArg{{Value: codegen.ProgramValue{Kind: "result", Step: 1}}}}, 3, "result-dependent write"},
		{"write sequence", codegen.ProgramStep{Effect: "write", Args: []codegen.ProgramArg{{Value: codegen.ProgramValue{Kind: "input", Input: "id"}}}}, 2, "write sequence"},
		{"dependent read", codegen.ProgramStep{Effect: "read", Args: []codegen.ProgramArg{{Value: codegen.ProgramValue{Kind: "result", Step: 1}}}}, 1, "result-dependent read"},
		{"read sequence", codegen.ProgramStep{Effect: "read", Args: []codegen.ProgramArg{{Value: codegen.ProgramValue{Kind: "input", Input: "id"}}}}, 0, "read sequence"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tier, shape := genreview.ProgramReviewShape(&codegen.ProgramGraph{Steps: []codegen.ProgramStep{{Effect: "read"}, tc.step}})
			if tier != tc.tier || shape != tc.shape {
				t.Fatalf("tier=%d shape=%s, want %d %s", tier, shape, tc.tier, tc.shape)
			}
		})
	}
}

func TestGeneratedProgramQueueShowsOneVariantPerBroadFamily(t *testing.T) {
	create := func(server, key string) trace.Call {
		return testkit.SpanRefs(trace.Call{Tool: "mcp:telara_execute_action", MCPServer: server, MCPTool: "telara_execute_action",
			Args:   map[string]string{"integration": "jira", "action": "create_issue", "params": `{"summary":"follow up"}`},
			Output: `{"key":"` + key + `"}`, Outcome: trace.OutcomeOK})
	}
	link := func(server, created, target string) trace.Call {
		return trace.Call{Tool: "mcp:telara_execute_action", MCPServer: server, MCPTool: "telara_execute_action",
			Args: map[string]string{"integration": "jira", "action": "create_issue_link",
				"params": `{"inward_issue_key":"` + created + `","outward_issue_key":"` + target + `"}`}, Outcome: trace.OutcomeOK}
	}
	sessions := []trace.Session{
		testkit.NewSession("first", "Create follow up and link TENG-2", create("one", "TENG-1"), link("one", "TENG-1", "TENG-2")),
		testkit.NewSession("second", "Create follow up and link TENG-4", create("two", "TENG-3"), link("two", "TENG-3", "TENG-4")),
	}
	candidate, spans := testkit.GraphCandidateFor(t, sessions, "jira.create_issue", "jira.create_issue_link")
	variants, err := codegen.GroupProgramVariants(candidate, spans, sessions)
	if err != nil || len(variants) != 2 {
		t.Fatalf("two exact tool bindings should remain separate variants: %+v %v", variants, err)
	}
	rows, err := genreview.GeneratedProgramQueue([]model.LogicCandidate{candidate}, spans, sessions)
	if err != nil || len(rows) != 1 || rows[0].Candidate.ID != candidate.ID {
		t.Fatalf("queue should show one reviewable variant for the broad family: %+v %v", rows, err)
	}
	if rows[0].Variant.ID != variants[0].ID {
		t.Fatalf("queue chose a different variant from the best-supported deterministic order: %+v", rows[0])
	}
	firstGraph, err := codegen.SynthesizeProgramGraph(variants[0], spans, sessions)
	if err != nil {
		t.Fatal(err)
	}
	firstPackage, err := codegen.GenerateProgramPackage(firstGraph)
	if err != nil {
		t.Fatal(err)
	}
	firstDecision := genreview.GeneratedDecision{Candidate: firstGraph.CandidateID, Digest: firstPackage.Digest, Choice: "deny"}
	remaining, err := genreview.GeneratedProgramQueue([]model.LogicCandidate{candidate}, spans, sessions, firstDecision)
	if err != nil || len(remaining) != 1 || remaining[0].Variant.ID != variants[1].ID {
		t.Fatalf("denied exact draft should not resurface; another variant may be reviewed: %+v %v", remaining, err)
	}
	secondGraph, err := codegen.SynthesizeProgramGraph(variants[1], spans, sessions)
	if err != nil {
		t.Fatal(err)
	}
	secondPackage, err := codegen.GenerateProgramPackage(secondGraph)
	if err != nil {
		t.Fatal(err)
	}
	secondDecision := genreview.GeneratedDecision{Candidate: secondGraph.CandidateID, Digest: secondPackage.Digest, Choice: "refine"}
	empty, err := genreview.GeneratedProgramQueue([]model.LogicCandidate{candidate}, spans, sessions, firstDecision, secondDecision)
	if err != nil || len(empty) != 0 {
		t.Fatalf("reviewed exact drafts should leave the queue: %+v %v", empty, err)
	}
}

func TestGeneratedQueueKeepsUnprovenRepeatedOrderForManualReview(t *testing.T) {
	v := model.LogicCandidate{Members: []string{"a", "b"}}
	spans := map[string]model.SpanProposal{
		"a": {ID: "a", Client: "claude-code", Session: "one", Kind: "repeated_order"},
		"b": {ID: "b", Client: "claude-code", Session: "two", Kind: "repeated_order"},
	}
	if genreview.GeneratedVariantHasTaskEvidence(v, spans) {
		t.Fatal("repeated adjacency with no task contract entered the generated queue")
	}
	spans["a"] = model.SpanProposal{ID: "a", Client: "claude-code", Session: "one", Kind: "repeated_order", Review: model.SpanTaskReview{Ready: true}}
	if genreview.GeneratedVariantHasTaskEvidence(v, spans) {
		t.Fatal("one task-shaped execution is insufficient for a repeated-order queue entry")
	}
	spans["b"] = model.SpanProposal{ID: "b", Client: "claude-code", Session: "two", Kind: "repeated_order", Review: model.SpanTaskReview{Ready: true}}
	if !genreview.GeneratedVariantHasTaskEvidence(v, spans) {
		t.Fatal("two independent task-shaped executions should remain reviewable")
	}
	spans["b"] = model.SpanProposal{ID: "b", Client: "claude-code", Session: "one", Kind: "repeated_order", Review: model.SpanTaskReview{Ready: true}}
	if genreview.GeneratedVariantHasTaskEvidence(v, spans) {
		t.Fatal("duplicate executions in one session counted as independent task evidence")
	}
	single := model.LogicCandidate{Members: []string{"c"}}
	spans["c"] = model.SpanProposal{ID: "c", Client: "claude-code", Session: "one", Kind: "single_call"}
	if genreview.GeneratedVariantHasTaskEvidence(single, spans) {
		t.Fatal("one unassessed source call entered the generated queue")
	}
	spans["c"] = model.SpanProposal{ID: "c", Client: "claude-code", Session: "one", Kind: "single_call", Review: model.SpanTaskReview{Ready: true}}
	if !genreview.GeneratedVariantHasTaskEvidence(single, spans) {
		t.Fatal("a task-shaped one-off execution should remain eligible")
	}
}

func TestGeneratedCandidateTaskEvidenceDoesNotPromoteIncidentalRecurrence(t *testing.T) {
	c := model.LogicCandidate{ID: "lc_example", Members: []string{"incidental_one", "incidental_two", "component_one", "component_two"}}
	spans := map[string]model.SpanProposal{
		"incidental_one": {ID: "incidental_one", Client: "claude-code", Session: "one", Review: model.SpanTaskReview{Source: "user", Reasons: []string{"incidental_bookkeeping"}}},
		"incidental_two": {ID: "incidental_two", Client: "claude-code", Session: "two", Review: model.SpanTaskReview{Source: "user", Reasons: []string{"incidental_bookkeeping"}}},
		"component_one":  {ID: "component_one", Client: "claude-code", Session: "three", Review: model.SpanTaskReview{Source: "user", Component: true}},
		"component_two":  {ID: "component_two", Client: "claude-code", Session: "four", Review: model.SpanTaskReview{Source: "user", Component: true}},
	}
	if _, ok := genreview.GeneratedCandidateTaskEvidence(model.LogicCandidate{Members: c.Members[:2]}, spans); ok {
		t.Fatal("repeated incidental actions entered the generated queue")
	}
	if _, ok := genreview.GeneratedCandidateTaskEvidence(model.LogicCandidate{Members: c.Members[:3]}, spans); ok {
		t.Fatal("one component among incidental actions entered the generated queue")
	}
	qualified, ok := genreview.GeneratedCandidateTaskEvidence(c, spans)
	if !ok || qualified.Proposals != 2 || qualified.Executions != 2 || qualified.Sessions != 2 ||
		len(qualified.Members) != 2 || qualified.Members[0] != "component_one" || qualified.Members[1] != "component_two" {
		t.Fatalf("qualified candidate included incidental evidence or lost independent components: %+v %v", qualified, ok)
	}
	spans["component_two"] = model.SpanProposal{ID: "component_two", Client: "claude-code", Session: "three", Review: model.SpanTaskReview{Source: "user", Component: true}}
	if _, ok := genreview.GeneratedCandidateTaskEvidence(c, spans); ok {
		t.Fatal("overlapping components in one session counted as independent support")
	}
	spans["component_one"] = model.SpanProposal{ID: "component_one", Client: "claude-code", Session: "three", Review: model.SpanTaskReview{Source: "user", Ready: true}}
	qualified, ok = genreview.GeneratedCandidateTaskEvidence(c, spans)
	if !ok || len(qualified.Members) != 1 || qualified.Members[0] != "component_one" {
		t.Fatalf("explicit task-shaped execution was not eligible on its own: %+v %v", qualified, ok)
	}
}

func TestGeneratedCausalComponentsRemainReviewableInsideLargerTasks(t *testing.T) {
	create := func(id string) trace.Call {
		return testkit.SpanRefs(trace.Call{Tool: "mcp:telara_jira_create_issue", MCPServer: "telara", MCPTool: "telara_jira_create_issue",
			Args:   map[string]string{"project_key": "TENG", "summary": "follow up " + id, "issue_type": "Task"},
			Output: `{"key":"` + id + `"}`, Outcome: trace.OutcomeOK})
	}
	transition := func(id string) trace.Call {
		return trace.Call{Tool: "mcp:telara_jira_transition_issue", MCPServer: "telara", MCPTool: "telara_jira_transition_issue",
			Args: map[string]string{"issue_key": id, "transition_id": "in-progress"}, Output: `{"ok":true}`, Outcome: trace.OutcomeOK}
	}
	sessions := []trace.Session{
		testkit.NewSession("one", "Implement feature A", create("TENG-101"), transition("TENG-101")),
		testkit.NewSession("two", "Implement feature B", create("TENG-202"), transition("TENG-202")),
	}
	c, spans := testkit.GraphCandidateFor(t, sessions, "mcp:telara_jira_create_issue", "mcp:telara_jira_transition_issue")
	bySpan := map[string]model.SpanProposal{}
	for _, span := range spans {
		bySpan[span.ID] = span
	}
	qualified, ok := genreview.GeneratedCandidateTaskEvidence(c, bySpan)
	if !ok || qualified.Sessions != 2 || qualified.Executions != 2 || len(qualified.Members) != 2 {
		t.Fatalf("result-linked agent component was lost: %+v %v", qualified, ok)
	}
	for _, id := range qualified.Members {
		if bySpan[id].Review.Ready || !genreview.GeneratedCausalComponent(bySpan[id]) {
			t.Fatalf("test did not exercise the internal-component route: %+v", bySpan[id])
		}
	}
	rows, err := genreview.GeneratedProgramQueue([]model.LogicCandidate{c}, spans, sessions)
	if err != nil || len(rows) != 1 || !genreview.GeneratedVariantIsInternalComponent(rows[0].Variant, bySpan) || !strings.HasPrefix(rows[0].Shape, "agent component") {
		t.Fatalf("result-linked component did not reach labeled review: %+v %v", rows, err)
	}
	one := c
	one.Members = one.Members[:1]
	if _, ok := genreview.GeneratedCandidateTaskEvidence(one, bySpan); ok {
		t.Fatal("single incidental component entered review without independent support")
	}
}
