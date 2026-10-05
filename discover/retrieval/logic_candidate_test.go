package retrieval_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/discover/internal/testkit"

	"github.com/Telara-Labs/TAP-Runtime/discover/routine"

	"github.com/Telara-Labs/TAP-Runtime/discover/retrieval"

	"github.com/Telara-Labs/TAP-Runtime/discover/model"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

func TestLogicCandidateAbstractsRuntimeValuesAndTaskWording(t *testing.T) {
	first := testkit.NewSession("one", "Fix the indexing tab reload",
		testkit.SpanRefs(trace.Call{Tool: "mcp:telara_execute_action", Args: map[string]string{"integration": "jira", "action": "create_issue", "params": `{"summary":"indexing tab"}`}, Output: `{"key":"TENG-4321"}`, Outcome: trace.OutcomeOK}),
		trace.Call{Tool: "mcp:telara_execute_action", Args: map[string]string{"integration": "jira", "action": "transition_issue", "params": `{"issue_key":"TENG-4321","transition_id":"11"}`}, Output: `{"status":"In Progress"}`, Outcome: trace.OutcomeOK})
	second := testkit.NewSession("two", "Remove the old integrations page",
		testkit.SpanRefs(trace.Call{Tool: "mcp:telara_execute_action", Args: map[string]string{"integration": "jira", "action": "create_issue", "params": `{"summary":"integrations page"}`}, Output: `{"key":"TENG-9876"}`, Outcome: trace.OutcomeOK}),
		trace.Call{Tool: "mcp:telara_execute_action", Args: map[string]string{"integration": "jira", "action": "transition_issue", "params": `{"issue_key":"TENG-9876","transition_id":"21"}`}, Output: `{"status":"Todo"}`, Outcome: trace.OutcomeOK})
	ps := retrieval.SelectSpanProposals([]trace.Session{first, second})
	if testkit.SpanWithCalls(ps, 1, 2) == nil {
		t.Fatalf("missing observed result-linked workflow: %+v", ps)
	}
	gs := retrieval.GroupLogicCandidates(ps)
	if len(gs) != 1 || gs[0].Sessions != 2 || gs[0].Executions != 2 || gs[0].Proposals != 2 {
		t.Fatalf("same reusable logic should be one candidate despite different tasks/values: %+v", gs)
	}
	if strings.Contains(gs[0].Key, "4321") || strings.Contains(gs[0].Key, "9876") || strings.Contains(gs[0].Key, "transition_id=11") {
		t.Fatalf("concrete runtime values leaked into logic identity: %s", gs[0].Key)
	}
}

func TestLogicCandidateRequiresIndependentRepetition(t *testing.T) {
	c := model.SpanComposition{Actions: []string{"gitlab.list_pipelines", "gitlab.list_jobs"}, Edges: []string{"gitlab.list_pipelines -> gitlab.list_jobs (pipeline_id:number)"}}
	one := model.SpanProposal{ID: "one", Client: "claude-code", Session: "s", Request: 0, Calls: []int{1, 2}, Composition: c}
	two := one
	two.ID = "overlapping-selection"
	if got := retrieval.GroupLogicCandidates([]model.SpanProposal{one, two}); len(got) != 0 {
		t.Fatalf("overlapping selections are one execution, not recurrence: %+v", got)
	}
	two.ID, two.Calls = "second-execution", []int{3, 4}
	got := retrieval.GroupLogicCandidates([]model.SpanProposal{one, two})
	if len(got) != 1 || got[0].Executions != 2 || got[0].Sessions != 1 {
		t.Fatalf("disjoint same-session executions should qualify: %+v", got)
	}
}

func TestLogicCandidateIncludesReusedAuthoredProgram(t *testing.T) {
	p := model.SpanProposal{ID: "script", Client: "claude-code", Session: "s", Kind: "authored_program", Calls: []int{1, 2, 3},
		Composition: model.SpanComposition{Key: "program", Actions: []string{"sh:cat", "sh:python3"}}}
	got := retrieval.GroupLogicCandidates([]model.SpanProposal{p})
	if len(got) != 1 || got[0].Evidence[0] != "authored_program_reused" {
		t.Fatalf("program written then run repeatedly should qualify alone: %+v", got)
	}
}

func TestLogicCandidateOneOrManyResultChildrenShareIdentity(t *testing.T) {
	one := model.SpanProposal{ID: "one", Client: "claude-code", Session: "a", Calls: []int{1, 2},
		Composition: model.SpanComposition{Actions: []string{"jira.create_issue", "jira.create_issue_link"}, Edges: []string{"jira.create_issue -> jira.create_issue_link (inward_issue_key:id)"}}}
	many := model.SpanProposal{ID: "many", Client: "claude-code", Session: "b", Calls: []int{1, 2, 3},
		Composition: model.SpanComposition{Actions: []string{"jira.create_issue", "jira.create_issue_link"}, Edges: []string{"jira.create_issue -> jira.create_issue_link (inward_issue_key:id)"}, Repetition: []model.SpanRepeat{{Action: "jira.create_issue_link", Count: 2, Kind: "for_each"}}}}
	got := retrieval.GroupLogicCandidates([]model.SpanProposal{one, many})
	if len(got) != 1 || got[0].Sessions != 2 || got[0].Executions != 2 {
		t.Fatalf("one link and a link loop are one parameterized composition: %+v", got)
	}
}

func TestLogicFunnelsCollectIndependentBranchesAndLoops(t *testing.T) {
	spans := []model.SpanProposal{
		{ID: "transition", Client: "claude-code", Session: "a", Composition: model.SpanComposition{Edges: []string{"jira.create_issue -> jira.transition_issue (issue_key:id)"}}},
		{ID: "link", Client: "claude-code", Session: "b", Composition: model.SpanComposition{Edges: []string{"jira.create_issue -> jira.create_issue_link (inward_issue_key:id)"}, Repetition: []model.SpanRepeat{{Action: "jira.create_issue_link", Count: 2, Kind: "for_each"}}}},
		{ID: "comment", Client: "claude-code", Session: "c", Composition: model.SpanComposition{Edges: []string{"jira.create_issue -> jira.add_comment (issue_key:id)"}}},
		{ID: "shared-text-only", Client: "claude-code", Session: "d", Composition: model.SpanComposition{Actions: []string{"jira.create_issue", "jira.add_comment"}}},
	}
	candidates := []model.LogicCandidate{
		{ID: "lc_transition", Members: []string{"transition"}},
		{ID: "lc_link", Members: []string{"link"}},
		{ID: "lc_comment", Members: []string{"comment"}},
		{ID: "lc_shared", Members: []string{"shared-text-only"}},
	}
	got := retrieval.GroupLogicFunnels(candidates, spans)
	if len(got) != 1 || got[0].Root != "jira.create_issue" || got[0].Sessions != 3 || len(got[0].Branches) != 3 {
		t.Fatalf("create result should be one observed three-branch funnel: %+v", got)
	}
	for _, branch := range got[0].Branches {
		if branch.Action == "jira.create_issue_link" && !branch.ForEach {
			t.Fatalf("repeated links over distinct IDs need a loop marker: %+v", got)
		}
	}
}

func TestCreatedIssueLinkFanoutReachesLogicQueue(t *testing.T) {
	created := testkit.SpanRefs(trace.Call{Tool: "mcp:telara_execute_action", Args: map[string]string{"integration": "jira", "action": "create_issue", "params": `{"summary":"follow-up"}`}, Output: `{"key":"TENG-1"}`, Outcome: trace.OutcomeOK})
	link := func(related string) trace.Call {
		return trace.Call{Tool: "mcp:telara_execute_action", Args: map[string]string{"integration": "jira", "action": "create_issue_link", "params": `{"inward_issue_key":"TENG-1","outward_issue_key":"` + related + `"}`}, Output: `{"status":"linked"}`, Outcome: trace.OutcomeOK}
	}
	one := testkit.NewSession("one-link", "Create an issue and link TENG-2", created, link("TENG-2"))
	many := testkit.NewSession("many-links", "Create an issue and link TENG-2 and TENG-3", created, link("TENG-2"), link("TENG-3"))
	ps := retrieval.SelectSpanProposals([]trace.Session{one, many})
	p := testkit.SpanWithCalls(ps, 1, 2, 3)
	if p == nil || len(p.Composition.Actions) != 2 || len(p.Composition.Repetition) != 1 || p.Composition.Repetition[0].Kind != "for_each" {
		t.Fatalf("created issue with two related IDs should be one observed loop: %+v", ps)
	}
	candidates := retrieval.GroupLogicCandidates(ps)
	funnels := retrieval.GroupLogicFunnels(candidates, ps)
	if len(candidates) == 0 || len(funnels) != 1 || funnels[0].Root != testkit.GatewayRole("create_issue")+"#params/summary=follow-up" || len(funnels[0].Branches) != 1 || !funnels[0].Branches[0].ForEach {
		t.Fatalf("one-link and multi-link traces should share an authoring lead: candidates=%+v funnels=%+v", candidates, funnels)
	}
}

func TestLogicCandidatesArePrimaryDiscoverQueue(t *testing.T) {
	r := model.Report{SpanProposals: []model.SpanProposal{{}}, LogicCandidates: []model.LogicCandidate{{ID: "lc_example", Sessions: 2, Executions: 3, Actions: []string{"jira.create_issue", "jira.transition_issue"}, Cautions: []string{"source_role_uncertain"}}}}
	var out bytes.Buffer
	routine.WriteSpanProposals(&out, &r, 10)
	if !strings.Contains(out.String(), "lc_example") || !strings.Contains(out.String(), "--logic") || strings.Contains(out.String(), "Task-first queue: 0") {
		t.Fatalf("logic candidates should be primary rather than complete-task gate: %s", out.String())
	}
}
