package discover

import (
	"reflect"
	"strings"
	"testing"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

func spanWithCalls(ps []SpanProposal, want ...int) *SpanProposal {
	for i := range ps {
		if reflect.DeepEqual(ps[i].Calls, want) {
			return &ps[i]
		}
	}
	return nil
}

func spanRefs(c trace.Call) trace.Call {
	c.OutIDs, c.OutCtx, c.OutPaths = trace.OutputRefsPaths(c.Output)
	return c
}

func TestSpanProposalsTraceResultDerivedIDsWithoutNamedObject(t *testing.T) {
	s := selSession("pipeline", "List pipelines, take the latest failed one, and fetch its failed job logs",
		spanRefs(trace.Call{Tool: "mcp:gitlab_list_pipelines", Output: `{"pipelines":[{"id":"81234567","status":"failed"}]}`}),
		spanRefs(trace.Call{Tool: "mcp:gitlab_list_jobs", Args: map[string]string{"pipeline_id": "81234567"}, Output: `{"jobs":[{"id":"91234567","status":"failed"}]}`}),
		trace.Call{Tool: "mcp:gitlab_get_job_log", Args: map[string]string{"job_id": "91234567"}, Output: "assertion failed"})
	ps := SelectSpanProposals([]trace.Session{s})
	p := spanWithCalls(ps, 1, 2, 3)
	if p == nil || p.Kind != "result_chain" || p.Status != BriefStatus {
		t.Fatalf("want unassessed result chain [1 2 3], got %+v", ps)
	}
	from := map[string]bool{}
	for _, in := range p.Inputs {
		from[in.Source] = true
	}
	if !from["prior_result"] {
		t.Fatalf("missing result-derived input: %+v", p.Inputs)
	}
}

func TestSpanProposalsTraceNestedGatewayParameters(t *testing.T) {
	s := selSession("nested", "List failed jobs and fetch their details",
		spanRefs(trace.Call{Tool: "mcp:telara_execute_action", Args: map[string]string{"action": "list_jobs", "integration": "gitlab", "params": `{"project_id":"telara-labs/cloud"}`}, Output: `{"items":[{"id":16438397787,"status":"failed"}]}`}),
		trace.Call{Tool: "mcp:telara_execute_action", Args: map[string]string{"action": "get_job", "integration": "gitlab", "params": `{"job_id":16438397787,"project_id":"telara-labs/cloud"}`}, Output: `{"id":16438397787,"name":"deploy"}`})
	ps := SelectSpanProposals([]trace.Session{s})
	p := spanWithCalls(ps, 1, 2)
	if p == nil || p.Kind != "result_chain" {
		t.Fatalf("nested params should carry the job id edge: %+v", ps)
	}
}

func TestSpanProposalsGroupSiblingCallsOverResultList(t *testing.T) {
	s := selSession("jobs", "Get details for each failed job",
		spanRefs(trace.Call{Tool: "mcp:gitlab_list_jobs", Output: `{"items":[{"id":16438397787},{"id":16438397790}]}`}),
		trace.Call{Tool: "mcp:gitlab_get_job", Args: map[string]string{"job_id": "16438397787"}, Output: "first"},
		trace.Call{Tool: "mcp:gitlab_get_job", Args: map[string]string{"job_id": "16438397790"}, Output: "second"})
	ps := SelectSpanProposals([]trace.Session{s})
	if p := spanWithCalls(ps, 1, 2, 3); p == nil || p.Kind != "result_chain" {
		t.Fatalf("result list should give one bounded fan-out: %+v", ps)
	}
	if spanWithCalls(ps, 1, 2) != nil || spanWithCalls(ps, 1, 3) != nil {
		t.Fatalf("sibling fan-out should not flood queue: %+v", ps)
	}
}

func TestSpanProposalsFindPartInsideLongRequest(t *testing.T) {
	var calls []trace.Call
	for i := 0; i < 24; i++ {
		calls = append(calls, trace.Call{Tool: "shell", Command: "sed -n '1,40p' /repo/file.go", Output: "source"})
	}
	calls = append(calls,
		spanRefs(trace.Call{Tool: "mcp:gitlab_list_pipelines", Output: `{"id":"81234567"}`}),
		spanRefs(trace.Call{Tool: "mcp:gitlab_list_jobs", Args: map[string]string{"pipeline_id": "81234567"}, Output: `{"id":"91234567"}`}),
		trace.Call{Tool: "mcp:gitlab_get_job_log", Args: map[string]string{"job_id": "91234567"}, Output: "failure log"})
	s := selSession("long", "Investigate the release; collect the failed jobs from the latest pipeline", calls...)
	ps := SelectSpanProposals([]trace.Session{s})
	if p := spanWithCalls(ps, 25, 26, 27); p == nil || p.Kind != "result_chain" {
		t.Fatalf("want bounded result chain within 27-call request, got %+v", ps)
	}
}

func TestSpanProposalsFindAuthoredProgramReusedInsideLongRequest(t *testing.T) {
	var calls []trace.Call
	for i := 0; i < 30; i++ {
		calls = append(calls, trace.Call{Tool: "shell", Command: "sed -n '1,20p' /repo/code.go", Output: "source"})
	}
	calls = append(calls,
		trace.Call{Tool: "shell", Command: "cat > /tmp/run_e2e.sh <<'EOF'\n#!/bin/sh\ngo test -run \"$1\"\nEOF", Output: "written"},
		trace.Call{Tool: "shell", Command: "timeout 1500 /tmp/run_e2e.sh TestKnowledge 22m", Output: "PASS"},
		trace.Call{Tool: "shell", Command: "/tmp/run_e2e.sh TestSummary 22m", Output: "PASS"})
	s := selSession("authored", "Investigate why session knowledge is missing", calls...)
	ps := SelectSpanProposals([]trace.Session{s})
	if p := spanWithCalls(ps, 31, 32, 33); p == nil || p.Kind != "authored_program" {
		t.Fatalf("want authored helper reused twice inside long task: %+v", ps)
	}
}

func TestSpanProposalsFindProgramWrittenByToolAndRunTwice(t *testing.T) {
	s := selSession("written-program", "Analyze two release snapshots",
		trace.Call{Tool: "Write", Args: map[string]string{"file_path": "/tmp/release_probe.py", "content": "print('ok')"}, Output: "written", Outcome: trace.OutcomeOK},
		trace.Call{Tool: "shell", Command: "python3 /tmp/release_probe.py snapshot-a", Output: "a", Outcome: trace.OutcomeOK},
		trace.Call{Tool: "shell", Command: "python3 /tmp/release_probe.py snapshot-b", Output: "b", Outcome: trace.OutcomeOK})
	ps := SelectSpanProposals([]trace.Session{s})
	p := spanWithCalls(ps, 1, 2, 3)
	if p == nil || p.Kind != "authored_program" {
		t.Fatalf("agent-written program reused with varying arguments should be one logic span: %+v", ps)
	}
	if got := GroupLogicCandidates(ps); len(got) == 0 {
		t.Fatalf("reused authored program should reach logic candidate queue: %+v", ps)
	}
}

func TestSpanProposalsKeepSingleCall(t *testing.T) {
	s := selSession("one", "Report dirty worktrees",
		trace.Call{Tool: "shell", Command: "git worktree list --porcelain", Output: "worktree /repo\nbranch refs/heads/main"})
	ps := SelectSpanProposals([]trace.Session{s})
	if p := spanWithCalls(ps, 1); p == nil || p.Kind != "single_call" {
		t.Fatalf("one observed read should be an unassessed proposal: %+v", ps)
	}
}

func TestSpanProposalsDoNotPromoteIncidentalRead(t *testing.T) {
	s := selSession("investigate", "Investigate why the server fails",
		trace.Call{Tool: "Read", Args: map[string]string{"file_path": "/repo/server.go"}, Output: "source code"})
	if ps := SelectSpanProposals([]trace.Session{s}); len(ps) != 0 {
		t.Fatalf("an incidental read is not a standalone task proposal: %+v", ps)
	}
}

func TestSpanProposalsDoNotTreatEchoedTextAsResultDependency(t *testing.T) {
	s := selSession("echo", "Investigate the deployment failure",
		spanRefs(trace.Call{Tool: "shell", Command: "grep pipeline /repo/log", Output: "error mentions 81234567"}),
		trace.Call{Tool: "mcp:gitlab_list_jobs", Args: map[string]string{"pipeline_id": "81234567"}, Output: "jobs"})
	if ps := SelectSpanProposals([]trace.Session{s}); spanWithCalls(ps, 1, 2) != nil {
		t.Fatalf("text echoed by grep is not a structured result edge: %+v", ps)
	}
}

func TestSpanProposalsDoNotJoinReadAndEditBySharedFilePath(t *testing.T) {
	s := selSession("edit", "Fix the login error in /repo/auth.go",
		trace.Call{Tool: "Read", Args: map[string]string{"file_path": "/repo/auth.go"}, Output: "source"},
		trace.Call{Tool: "Edit", Args: map[string]string{"file_path": "/repo/auth.go", "new_string": "changed"}, Output: "edited"})
	if ps := SelectSpanProposals([]trace.Session{s}); spanWithCalls(ps, 1, 2) != nil {
		t.Fatalf("same file is insufficient to define a procedure: %+v", ps)
	}
}

func TestSpanProposalsDoNotPromoteProductNameOverlap(t *testing.T) {
	s := selSession("overlap", "Investigate why Telara calls fail",
		trace.Call{Tool: "mcp:telara_execute_action", Args: map[string]string{"action": "update_issue"}, Output: "done"})
	if ps := SelectSpanProposals([]trace.Session{s}); len(ps) != 0 {
		t.Fatalf("product and tool words alone are not direct intent: %+v", ps)
	}
}

func TestSpanProposalsKeepFixedSequenceWithoutResultDependency(t *testing.T) {
	s := selSession("issue", "Comment on and close issue TENG-4321",
		trace.Call{Tool: "mcp:jira_get_issue", Args: map[string]string{"issue_key": "TENG-4321"}, Output: `{"key":"TENG-4321"}`},
		trace.Call{Tool: "mcp:jira_add_comment", Args: map[string]string{"issue_key": "TENG-4321", "body": "done"}, Output: "comment added"},
		trace.Call{Tool: "mcp:jira_transition_issue", Args: map[string]string{"issue_key": "TENG-4321", "transition_id": "done"}, Output: "transitioned"})
	ps := SelectSpanProposals([]trace.Session{s})
	if p := spanWithCalls(ps, 1, 2, 3); p == nil || p.Kind != "shared_input" {
		t.Fatalf("want caller-supplied issue sequence: %+v", ps)
	}
}

func TestSpanProposalsCarryPriorTurnInputWithoutGrantingAuthority(t *testing.T) {
	s := selSession("followup", "Inspect issue TENG-4321",
		trace.Call{Tool: "mcp:jira_get_issue", Args: map[string]string{"issue_key": "TENG-4321"}, Output: "open"})
	s.Requests = append(s.Requests, "Now add the approved comment to that issue")
	s.Calls = append(s.Calls, trace.Call{Tool: "mcp:jira_add_comment", Args: map[string]string{"issue_key": "TENG-4321", "body": "approved"}, Output: "added", Outcome: trace.OutcomeOK, Request: 1})
	ps := SelectSpanProposals([]trace.Session{s})
	var p *SpanProposal
	for i := range ps {
		if ps[i].Request == 1 {
			p = &ps[i]
			break
		}
	}
	if p == nil {
		t.Fatalf("no follow-up proposal: %+v", ps)
	}
	prior := false
	for _, in := range p.Inputs {
		if in.Key == "issue_key" && in.Source == "prior_request" && in.FromRequest == 1 {
			prior = true
		}
	}
	if !prior || p.Status != BriefStatus {
		t.Fatalf("prior context unaccounted or promoted: %+v", p)
	}
}

func TestSpanGroupsSeparateDifferentGoalsWithSameTools(t *testing.T) {
	a := selSession("a", "List failed jobs for project 12345", trace.Call{Tool: "mcp:gitlab_list_jobs", Args: map[string]string{"project_id": "12345"}, Output: "failed jobs"})
	b := selSession("b", "List completed jobs for project 67890", trace.Call{Tool: "mcp:gitlab_list_jobs", Args: map[string]string{"project_id": "67890"}, Output: "completed jobs"})
	c := selSession("c", "List failed jobs for project 67890", trace.Call{Tool: "mcp:gitlab_list_jobs", Args: map[string]string{"project_id": "67890"}, Output: "failed jobs"})
	ps := SelectSpanProposals([]trace.Session{a, b, c})
	gs := GroupSpanProposals(ps)
	if len(gs) != 2 || gs[0].Sessions != 2 {
		t.Fatalf("want two goal-specific groups, one shared by parameterized IDs: %+v", gs)
	}
	for _, p := range ps {
		if p.Status != BriefStatus || strings.Contains(p.ShapeKey, "12345") {
			t.Fatalf("unassessed private shape expected: %+v", p)
		}
	}
}

func TestSpanGroupsKeepGenericActionsAndAuthorityDistinct(t *testing.T) {
	a := selSession("a", "Add a comment to issue TENG-4321", trace.Call{Tool: "mcp:telara_execute_action", Args: map[string]string{"action": "add_comment", "issue_key": "TENG-4321"}, Output: "done"})
	b := selSession("b", "Transition issue TENG-9876", trace.Call{Tool: "mcp:telara_execute_action", Args: map[string]string{"action": "transition_issue", "issue_key": "TENG-9876"}, Output: "done"})
	c := selSession("c", "Check deployment status", trace.Call{Tool: "mcp:kubernetes_get_deployment", Args: map[string]string{"environment": "prod"}, Output: "ready"})
	d := selSession("d", "Check deployment status", trace.Call{Tool: "mcp:kubernetes_get_deployment", Args: map[string]string{"environment": "staging"}, Output: "ready"})
	ps := SelectSpanProposals([]trace.Session{a, b, c, d})
	if len(ps) != 4 || len(GroupSpanProposals(ps)) != 4 {
		t.Fatalf("different generic actions or authority scope merged: %+v", ps)
	}
}

func TestSpanBriefIncludesOnlyVerifiedCallsAndPriorContext(t *testing.T) {
	s := selSession("brief", "Check the latest pipeline for project 12345",
		trace.Call{Tool: "mcp:gitlab_list_pipelines", Args: map[string]string{"project_id": "12345"}, Output: `{"id":"81234567"}`})
	s.Requests = append(s.Requests, "Now fetch its failed jobs")
	s.Calls = append(s.Calls,
		trace.Call{Tool: "mcp:telara_task_checkpoint", Request: 1, Output: "recorded", Outcome: trace.OutcomeOK},
		trace.Call{Tool: "mcp:gitlab_list_jobs", Request: 1, Args: map[string]string{"pipeline_id": "81234567"}, Output: "failed job", Outcome: trace.OutcomeOK})
	ps := SelectSpanProposals([]trace.Session{s})
	var p *SpanProposal
	for i := range ps {
		if ps[i].Request == 1 {
			p = &ps[i]
			break
		}
	}
	if p == nil {
		t.Fatalf("no follow-up span: %+v", ps)
	}
	b, err := NewBriefSpan(s, *p)
	if err != nil {
		t.Fatal(err)
	}
	if b.Selection != DiscoverSpan || b.Status != BriefStatus || len(b.Evidence.Steps) != 1 || b.Evidence.Steps[0].Tool != "mcp:gitlab_list_jobs" || b.Evidence.Steps[0].SourceCall != 2 {
		t.Fatalf("span brief included wrong calls: %+v", b)
	}
	if !strings.Contains(b.Evidence.PreviousRequest, "project 12345") {
		t.Fatalf("prior context missing: %+v", b.Evidence)
	}
	s.Calls[2].Output = "changed after report"
	if _, err := NewBriefSpan(s, *p); err == nil {
		t.Fatal("stale source must fail")
	}
}
