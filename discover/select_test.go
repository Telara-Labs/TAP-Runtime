package discover

import (
	"fmt"
	"strings"
	"testing"

	"gitlab.com/telara-labs/tap-runtime/discover/internal/testkit"

	"gitlab.com/telara-labs/tap-runtime/discover/retrieval"

	"gitlab.com/telara-labs/tap-runtime/discover/model"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// The selection pass (select.go). Each case builds sessions whose evidence
// either supports a route or is the look-alike that must not.

func judged(t *testing.T, ss []trace.Session, id string) model.Opportunity {
	t.Helper()
	for _, o := range retrieval.SelectOpportunities(ss) {
		if o.Session == id {
			return o
		}
	}
	t.Fatalf("no judgment for %s", id)
	return model.Opportunity{}
}

// A recurring prompt that states its procedure, and whose calls touch what
// it names, is recommended even though no two runs call alike.
func TestStatedTemplateIsRecommendedAcrossDifferentRuns(t *testing.T) {
	var ss []trace.Session
	for i := 0; i < 3; i++ {
		ss = append(ss, testkit.NewSession(fmt.Sprintf("run%d", i), fmt.Sprintf(testkit.StatedPrompt, i),
			trace.Call{Tool: "shell", Command: fmt.Sprintf("jq -r '.[] | .status' /work/tracker/queue.json | sort | uniq -c # %d", i)},
			trace.Call{Tool: "shell", Command: "tail -n 1 /work/tracker/log.jsonl"}))
	}
	o := judged(t, ss, "run0")
	if !o.Recommended || o.Route != model.RouteStatedTemplate {
		t.Fatalf("want stated_template, got %+v", o)
	}
	if o.Task != "codex/run0/0" || o.ID != trace.EpisodeID("codex", "run0", 0) {
		t.Errorf("task ref %q id %q", o.Task, o.ID)
	}
}

// A recurring prompt that states no procedure (an open goal) is not.
func TestRecurringOpenGoalIsNotAStatedTemplate(t *testing.T) {
	var ss []trace.Session
	for i := 0; i < 3; i++ {
		ss = append(ss, testkit.NewSession(fmt.Sprintf("goal%d", i), fmt.Sprintf("Run %d: figure out why the indexer keeps failing and fix it", i),
			trace.Call{Tool: "shell", Command: "go test ./indexer/..."}, trace.Call{Tool: "shell", Command: "git diff"}))
	}
	o := judged(t, ss, "goal0")
	if o.Recommended {
		t.Fatalf("an open goal must not be recommended: %+v", o)
	}
	if !strings.Contains(strings.Join(o.Reasons, " "), "template_states_no_procedure") {
		t.Errorf("reason %v should say the template states no procedure", o.Reasons)
	}
}

// A program the agent wrote and ran twice is a check; viewing a file twice
// with sed -n is navigation, however long the command.
func TestRerunProgramIsACheckButRepeatedViewingIsNot(t *testing.T) {
	script := `python3 - <<'EOF'
import json, sys
rows = json.load(open("%s"))
print({s: sum(1 for r in rows if r["status"] == s) for s in {r["status"] for r in rows}})
EOF`
	check := testkit.NewSession("check", "how many are in each state now",
		trace.Call{Tool: "shell", Command: fmt.Sprintf(script, "/data/a.json")},
		trace.Call{Tool: "shell", Command: "date -u"},
		trace.Call{Tool: "shell", Command: fmt.Sprintf(script, "/data/b.json")})
	if o := judged(t, []trace.Session{check}, "check"); !o.Recommended || o.Route != model.RouteRerunCheck {
		t.Errorf("want rerun_check, got %+v", o)
	}
	view := testkit.NewSession("view", "look at the handler",
		trace.Call{Tool: "shell", Command: "sed -n '1,200p' /repo/services/gateway/internal/handlers/very/long/path/to/the/handler_file_name.go"},
		trace.Call{Tool: "shell", Command: "sed -n '200,400p' /repo/services/gateway/internal/handlers/very/long/path/to/the/handler_file_name.go"})
	if o := judged(t, []trace.Session{view}, "view"); o.Recommended {
		t.Errorf("viewing a file twice is navigation, got %+v", o)
	}
}

// The same step over several ids, none taken from what the loop just read,
// is a list a caller could give. A loop over search phrasings is not.
func TestParametricLoopNeedsIdentifiersNotSearchPhrasings(t *testing.T) {
	ids := testkit.NewSession("ids", "tail the failed jobs 81234567, 81234599, and 81234612",
		trace.Call{Tool: "mcp:gitlab_get_job_log", Args: map[string]string{"job_id": "81234567"}, Output: "log one"},
		trace.Call{Tool: "mcp:gitlab_get_job_log", Args: map[string]string{"job_id": "81234599"}, Output: "log two"},
		trace.Call{Tool: "mcp:gitlab_get_job_log", Args: map[string]string{"job_id": "81234612"}, Output: "log three"})
	if o := judged(t, []trace.Session{ids}, "ids"); !o.Recommended || o.Route != model.RouteParamLoop {
		t.Errorf("want parametric_loop, got %+v", o)
	}
	fromResult := testkit.NewSession("from_result", "list the failed jobs, then fetch each log",
		trace.Call{Tool: "mcp:gitlab_list_failed_jobs", Output: `{"jobs":[{"id":"81234567"},{"id":"81234599"}]}`},
		trace.Call{Tool: "mcp:gitlab_get_job_log", Args: map[string]string{"job_id": "81234567"}, Output: "log one"},
		trace.Call{Tool: "mcp:gitlab_get_job_log", Args: map[string]string{"job_id": "81234599"}, Output: "log two"})
	if o := judged(t, []trace.Session{fromResult}, "from_result"); !o.Recommended || o.Route != model.RouteParamLoop || !strings.Contains(o.Contract, "prior_output") {
		t.Errorf("want loop over the earlier result, got %+v", o)
	}
	unknown := testkit.NewSession("unknown", "tail the failed jobs",
		trace.Call{Tool: "mcp:gitlab_get_job_log", Args: map[string]string{"job_id": "81234567"}, Output: "log one"},
		trace.Call{Tool: "mcp:gitlab_get_job_log", Args: map[string]string{"job_id": "81234599"}, Output: "log two"})
	if o := judged(t, []trace.Session{unknown}, "unknown"); o.Recommended {
		t.Errorf("the list has no visible source, got %+v", o)
	} else if !strings.Contains(strings.Join(o.Reasons, " "), "loop_source_unknown") {
		t.Errorf("missing list provenance reason: %+v", o)
	}
	search := testkit.NewSession("search", "why is recall low",
		trace.Call{Tool: "mcp:telara_knowledge_search", Args: map[string]string{"query": "recall evaluation holdout"}, Output: "a"},
		trace.Call{Tool: "mcp:telara_knowledge_search", Args: map[string]string{"query": "episode label disagreement"}, Output: "b"},
		trace.Call{Tool: "mcp:telara_knowledge_search", Args: map[string]string{"query": "routine consistency check"}, Output: "c"})
	if o := judged(t, []trace.Session{search}, "search"); o.Recommended {
		t.Errorf("a loop over search phrasings is exploration, got %+v", o)
	}
	chained := testkit.NewSession("chained", "follow the links",
		trace.Call{Tool: "mcp:web_get", Args: map[string]string{"url": "https://example.com/start"}, Output: "see https://example.com/next"},
		trace.Call{Tool: "mcp:web_get", Args: map[string]string{"url": "https://example.com/next"}, Output: "see https://example.com/last"},
		trace.Call{Tool: "mcp:web_get", Args: map[string]string{"url": "https://example.com/last"}, Output: "end"})
	if o := judged(t, []trace.Session{chained}, "chained"); o.Recommended {
		t.Errorf("each item came from the previous result: exploration, got %+v", o)
	}
}

func TestLoopListSourceRequiresWholeItems(t *testing.T) {
	if retrieval.ContainsItem("1812345679", "81234567") || !retrieval.ContainsItem("jobs 81234567, 81234599", "81234567") {
		t.Fatal("item boundary mismatch")
	}
	steps := []trace.Step{{Output: "job 81234567 only", Outcome: trace.OutcomeOK}}
	if source := retrieval.LoopListSource("", steps, 1, []string{"81234567", "81234599"}); source != "" {
		t.Fatalf("partial list should not establish provenance: %q", source)
	}
}

// Harness text and single calls are never recommended.
func TestSelectionRefusesHarnessAndSingleCalls(t *testing.T) {
	h := testkit.NewSession("h", "# AGENTS.md instructions for /repo", trace.Call{Tool: "shell", Command: "ls"}, trace.Call{Tool: "shell", Command: "pwd"})
	one := testkit.NewSession("one", "what time is it", trace.Call{Tool: "shell", Command: "date"})
	for _, o := range retrieval.SelectOpportunities([]trace.Session{h, one}) {
		if o.Recommended {
			t.Errorf("%s must not be recommended: %+v", o.Session, o)
		}
	}
}

// A short request naming an object, whose calls act on it with a later step
// using an earlier result, is a single-pass procedure. The same request with
// no dependency between its steps, or naming nothing the calls touch, is not.
func TestNamedObjectNeedsATouchedObjectAndADependency(t *testing.T) {
	close := testkit.NewSession("close", "close TENG-4321 with a note that it shipped",
		trace.Call{Tool: "mcp:jira_list_transitions", Args: map[string]string{"issue_key": "TENG-4321"}, Output: `{"transitions":[{"id":"31","name":"Done"}],"done_id":"trn-31-done"}`},
		trace.Call{Tool: "mcp:jira_add_comment", Args: map[string]string{"issue_key": "TENG-4321", "body": "shipped"}},
		trace.Call{Tool: "mcp:jira_transition_issue", Args: map[string]string{"issue_key": "TENG-4321", "transition_id": "trn-31-done"}})
	if o := judged(t, []trace.Session{close}, "close"); !o.Recommended || o.Route != model.RouteNamedObject {
		t.Errorf("want named_object, got %+v", o)
	}
	unrelated := testkit.NewSession("unrelated", "close TENG-4321 with a note that it shipped",
		trace.Call{Tool: "mcp:jira_add_comment", Args: map[string]string{"issue_key": "TENG-4321", "body": "shipped"}},
		trace.Call{Tool: "mcp:jira_get_issue", Args: map[string]string{"issue_key": "TENG-4321"}})
	if o := judged(t, []trace.Session{unrelated}, "unrelated"); o.Recommended {
		t.Errorf("no step depends on another: not a procedure, got %+v", o)
	}
	nothing := testkit.NewSession("nothing", "why is it slow today",
		trace.Call{Tool: "mcp:metrics_query", Args: map[string]string{"query": "p95 latency"}, Output: "series-abc123"},
		trace.Call{Tool: "mcp:metrics_series", Args: map[string]string{"id": "series-abc123"}})
	if o := judged(t, []trace.Session{nothing}, "nothing"); o.Recommended {
		t.Errorf("the request names nothing the calls act on, got %+v", o)
	}
}

// Grouping puts requests with the same contract together and ranks the
// group seen in more sessions first; it never changes what is recommended.
func TestGroupOpportunitiesByContractRanksBySessions(t *testing.T) {
	var ss []trace.Session
	for i := 0; i < 3; i++ {
		ss = append(ss, testkit.NewSession(fmt.Sprintf("run%d", i), fmt.Sprintf(testkit.StatedPrompt, i),
			trace.Call{Tool: "shell", Command: fmt.Sprintf("jq -r '.[] | .status' /work/tracker/queue.json # %d", i)},
			trace.Call{Tool: "shell", Command: "tail -n 1 /work/tracker/log.jsonl"}))
	}
	ss = append(ss, testkit.NewSession("ids", "tail the failed jobs 81234567 and 81234599",
		trace.Call{Tool: "mcp:gitlab_get_job_log", Args: map[string]string{"job_id": "81234567"}, Output: "a"},
		trace.Call{Tool: "mcp:gitlab_get_job_log", Args: map[string]string{"job_id": "81234599"}, Output: "b"}))
	ops := retrieval.SelectOpportunities(ss)
	n := 0
	for _, o := range ops {
		if o.Recommended {
			n++
			if o.Contract == "" {
				t.Errorf("a recommended opportunity needs a contract: %+v", o)
			}
		}
	}
	gs := retrieval.GroupOpportunities(ops)
	if len(gs) != 2 || gs[0].Route != model.RouteStatedTemplate || gs[0].Sessions != 3 || gs[0].Requests != 3 || gs[1].Sessions != 1 {
		t.Fatalf("want the 3-session template group first, then the loop: %+v", gs)
	}
	members := 0
	for _, g := range gs {
		members += len(g.Members)
	}
	if members != n {
		t.Errorf("groups hold %d members, %d were recommended", members, n)
	}
}
