package discover

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlab.com/telara-labs/tap-runtime/discover/history"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

func TestClaudeReaderAttributesCompactionTextAsSynthetic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	lines := `{"type":"user","timestamp":"2026-09-10T00:00:00Z","message":{"content":"This session is being continued from a previous conversation that ran out of context. The summary below covers earlier work."}}` + "\n" +
		`{"type":"assistant","timestamp":"2026-09-10T00:00:01Z","message":{"id":"m1","content":[{"type":"tool_use","id":"t1","name":"mcp__telara__telara_jira_search_issues","input":{"query":"project = TENG"}}]}}` + "\n" +
		`{"type":"user","timestamp":"2026-09-10T00:00:02Z","message":{"content":"List failed pipelines"}}` + "\n" +
		`{"type":"assistant","timestamp":"2026-09-10T00:00:03Z","message":{"id":"m2","content":[{"type":"tool_use","id":"t2","name":"mcp__telara__telara_gitlab_list_pipelines","input":{}}]}}` + "\n"
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := history.ReadClaudeFile(path)
	if err != nil || len(s.Requests) != 2 || len(s.Calls) != 2 || s.Calls[0].Request != 0 || s.Calls[1].Request != 1 ||
		s.RequestRoles[0] != "synthetic_context" || s.RequestRoles[1] != "user" {
		t.Fatalf("compaction attribution lost: session=%+v error=%v", s, err)
	}
}

// The review never depends on which provider a call reaches: the same
// result-linked chain is judged the same for any integration. Inside
// unrelated work it is a visible component, not a ready task; when the
// request asks for exactly that operation it is a ready task.
func TestTaskReviewIsProviderNeutral(t *testing.T) {
	chain := func(integration, request, id string) SpanProposal {
		s := selSession(id, request,
			spanRefs(trace.Call{Tool: "mcp:telara_execute_action", Args: map[string]string{"integration": integration, "action": "create_issue", "params": `{"summary":"broken"}`}, Output: `{"key":"TENG-4321"}`, Outcome: trace.OutcomeOK}),
			trace.Call{Tool: "mcp:telara_execute_action", Args: map[string]string{"integration": integration, "action": "transition_issue", "params": `{"issue_key":"TENG-4321","transition_id":"in_progress"}`}, Output: `{"status":"In Progress"}`, Outcome: trace.OutcomeOK})
		ps := SelectSpanProposals([]trace.Session{s})
		p := spanWithCalls(ps, 1, 2)
		if p == nil {
			t.Fatalf("missing causal diagnostic trace: %+v", ps)
		}
		return *p
	}
	for _, integration := range []string{"jira", "linear"} {
		inside := chain(integration, "Fix the indexing tab reload defect", integration+"-inside")
		if inside.Review.Ready || !inside.Review.Component {
			t.Errorf("%s: a result-linked chain inside unrelated work is a component, not a ready task: %+v", integration, inside.Review)
		}
		requested := chain(integration, "Create an issue and transition it to In Progress", integration+"-requested")
		if !requested.Review.Ready || requested.Review.Input != "caller_or_result" {
			t.Errorf("%s: a requested chain should be ready: %+v", integration, requested.Review)
		}
	}
}

func TestTaskReviewHandlesResultDerivedPipelineAndFailedCalls(t *testing.T) {
	s := selSession("pipeline-review", "List pipelines, take the latest failed one, and fetch its failed job logs",
		spanRefs(trace.Call{Tool: "mcp:gitlab_list_pipelines", Output: `{"pipelines":[{"id":"81234567","status":"failed"}]}`, Outcome: trace.OutcomeOK}),
		spanRefs(trace.Call{Tool: "mcp:gitlab_list_jobs", Args: map[string]string{"pipeline_id": "81234567"}, Output: `{"jobs":[{"id":"91234567"}]}`, Outcome: trace.OutcomeOK}),
		trace.Call{Tool: "mcp:gitlab_get_job_log", Args: map[string]string{"job_id": "91234567"}, Output: "assertion failed", Outcome: trace.OutcomeOK})
	p := spanWithCalls(SelectSpanProposals([]trace.Session{s}), 1, 2, 3)
	if p == nil || !p.Review.Ready {
		t.Fatalf("result-derived pipeline task should enter queue: %+v", p)
	}
	s.Calls[2].Outcome = trace.OutcomeFailed
	p = spanWithCalls(SelectSpanProposals([]trace.Session{s}), 1, 2, 3)
	if p == nil || p.Review.Ready {
		t.Fatalf("failed terminal call must not enter queue: %+v", p)
	}
}

func TestTaskReviewKeepsInvestigativeResultChainAsComponent(t *testing.T) {
	s := selSession("failed-build", "Why did the production build fail?",
		spanRefs(trace.Call{Tool: "mcp:gitlab_list_pipelines", Output: `{"id":"81234567"}`, Outcome: trace.OutcomeOK}),
		trace.Call{Tool: "mcp:gitlab_list_jobs", Args: map[string]string{"pipeline_id": "81234567"}, Output: `{"status":"failed"}`, Outcome: trace.OutcomeOK})
	p := spanWithCalls(SelectSpanProposals([]trace.Session{s}), 1, 2)
	if p == nil || p.Review.Ready || !p.Review.Component {
		t.Fatalf("evidence-gathering chain is a component with an unresolved task contract: %+v", p)
	}
}

func TestTaskReviewMarksContinuationSummarySynthetic(t *testing.T) {
	s := selSession("summary", "This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier work. List pipelines and fetch jobs.",
		spanRefs(trace.Call{Tool: "mcp:gitlab_list_pipelines", Output: `{"id":"81234567"}`, Outcome: trace.OutcomeOK}),
		trace.Call{Tool: "mcp:gitlab_list_jobs", Args: map[string]string{"pipeline_id": "81234567"}, Output: "jobs", Outcome: trace.OutcomeOK})
	s.RequestRoles = []string{"synthetic_context"}
	p := spanWithCalls(SelectSpanProposals([]trace.Session{s}), 1, 2)
	if p == nil || p.Review.Ready || p.Review.Source != "synthetic_context" {
		t.Fatalf("continuation text is diagnostic context, not user demand: %+v", p)
	}
}

func TestTaskReviewRequiresStopEvidenceForRepeatedPolling(t *testing.T) {
	s := selSession("poll", "Check pipeline 81234567 status",
		trace.Call{Tool: "mcp:gitlab_get_pipeline", Args: map[string]string{"pipeline_id": "81234567"}, Output: `{"status":"running"}`, Outcome: trace.OutcomeOK},
		trace.Call{Tool: "mcp:gitlab_get_pipeline", Args: map[string]string{"pipeline_id": "81234567"}, Output: `{"status":"running"}`, Outcome: trace.OutcomeOK},
		trace.Call{Tool: "mcp:gitlab_get_pipeline", Args: map[string]string{"pipeline_id": "81234567"}, Output: `{"status":"success"}`, Outcome: trace.OutcomeOK})
	ps := SelectSpanProposals([]trace.Session{s})
	for _, p := range ps {
		if len(p.Calls) > 1 && p.Review.Ready {
			t.Fatalf("repeated same-target polling lacks reusable stop rule: %+v", p)
		}
	}
}

func TestTaskReviewKeepsExploratoryReadsOutOfQueue(t *testing.T) {
	s := selSession("investigate", "Why are half the minikube indexing jobs failing?",
		trace.Call{Tool: "shell", Command: "kubectl --context minikube get jobs -n telara-knowledge | head", Output: "failed jobs", Outcome: trace.OutcomeOK})
	ps := SelectSpanProposals([]trace.Session{s})
	for _, p := range ps {
		if p.Review.Ready {
			t.Fatalf("exploratory read does not resolve the requested diagnosis: %+v", p)
		}
	}
}

func TestTaskReviewDoesNotTreatToolPassthroughAsComposition(t *testing.T) {
	s := selSession("one", "Report dirty worktrees",
		trace.Call{Tool: "shell", Command: "git worktree list --porcelain", Output: "worktree /repo", Outcome: trace.OutcomeOK})
	ps := SelectSpanProposals([]trace.Session{s})
	p := spanWithCalls(ps, 1)
	if p == nil || p.Review.Ready {
		t.Fatalf("raw one-call result is diagnostic until a transformation is defined: %+v", p)
	}
}

func TestSpanReportShowsComponentQueueWhenTaskQueueEmpty(t *testing.T) {
	p := SpanProposal{ID: "sp_component", Review: SpanTaskReview{Source: "user", Component: true}, Composition: SpanComposition{Actions: []string{"gitlab.list_jobs", "gitlab.get_job"}}}
	r := Report{SpanProposals: []SpanProposal{p}, ComponentSpans: []SpanProposal{p}, ComponentGroups: []SpanCompositionGroup{{Proposals: 1, Sessions: 1, Example: p}}}
	var out bytes.Buffer
	WriteSpanProposals(&out, &r, 10)
	if !strings.Contains(out.String(), "Task-first queue: 0") || !strings.Contains(out.String(), "component queue: 1") || !strings.Contains(out.String(), "gitlab.list_jobs") {
		t.Fatalf("component queue hidden behind empty strict queue: %s", out.String())
	}
}
