package discover

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The selection pass (select.go). Each case builds sessions whose evidence
// either supports a route or is the look-alike that must not.

var selT0 = time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)

func selSession(id, req string, calls ...Call) Session {
	for i := range calls {
		calls[i].Time = selT0.Add(time.Duration(i) * time.Second)
		if calls[i].Outcome == OutcomeUnknown {
			calls[i].Outcome = OutcomeOK
		}
	}
	return Session{Client: "codex", ID: id, Start: selT0, Requests: []string{req}, Calls: calls}
}

func judged(t *testing.T, ss []Session, id string) Opportunity {
	t.Helper()
	for _, o := range SelectOpportunities(ss) {
		if o.Session == id {
			return o
		}
	}
	t.Fatalf("no judgment for %s", id)
	return Opportunity{}
}

const statedPrompt = `Automation: queue monitor. Run %d.
- Read /work/tracker/queue.json and count rows by status.
- Append the pass to /work/tracker/log.jsonl.
- Check that log.jsonl parses.`

// A recurring prompt that states its procedure, and whose calls touch what
// it names, is recommended even though no two runs call alike.
func TestStatedTemplateIsRecommendedAcrossDifferentRuns(t *testing.T) {
	var ss []Session
	for i := 0; i < 3; i++ {
		ss = append(ss, selSession(fmt.Sprintf("run%d", i), fmt.Sprintf(statedPrompt, i),
			Call{Tool: "shell", Command: fmt.Sprintf("jq -r '.[] | .status' /work/tracker/queue.json | sort | uniq -c # %d", i)},
			Call{Tool: "shell", Command: "tail -n 1 /work/tracker/log.jsonl"}))
	}
	o := judged(t, ss, "run0")
	if !o.Recommended || o.Route != RouteStatedTemplate {
		t.Fatalf("want stated_template, got %+v", o)
	}
	if o.Task != "codex/run0/0" || o.ID != EpisodeID("codex", "run0", 0) {
		t.Errorf("task ref %q id %q", o.Task, o.ID)
	}
}

// A recurring prompt that states no procedure (an open goal) is not.
func TestRecurringOpenGoalIsNotAStatedTemplate(t *testing.T) {
	var ss []Session
	for i := 0; i < 3; i++ {
		ss = append(ss, selSession(fmt.Sprintf("goal%d", i), fmt.Sprintf("Run %d: figure out why the indexer keeps failing and fix it", i),
			Call{Tool: "shell", Command: "go test ./indexer/..."}, Call{Tool: "shell", Command: "git diff"}))
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
	check := selSession("check", "how many are in each state now",
		Call{Tool: "shell", Command: fmt.Sprintf(script, "/data/a.json")},
		Call{Tool: "shell", Command: "date -u"},
		Call{Tool: "shell", Command: fmt.Sprintf(script, "/data/b.json")})
	if o := judged(t, []Session{check}, "check"); !o.Recommended || o.Route != RouteRerunCheck {
		t.Errorf("want rerun_check, got %+v", o)
	}
	view := selSession("view", "look at the handler",
		Call{Tool: "shell", Command: "sed -n '1,200p' /repo/services/gateway/internal/handlers/very/long/path/to/the/handler_file_name.go"},
		Call{Tool: "shell", Command: "sed -n '200,400p' /repo/services/gateway/internal/handlers/very/long/path/to/the/handler_file_name.go"})
	if o := judged(t, []Session{view}, "view"); o.Recommended {
		t.Errorf("viewing a file twice is navigation, got %+v", o)
	}
}

// The same step over several ids, none taken from what the loop just read,
// is a list a caller could give. A loop over search phrasings is not.
func TestParametricLoopNeedsIdentifiersNotSearchPhrasings(t *testing.T) {
	ids := selSession("ids", "tail the failed jobs",
		Call{Tool: "mcp:gitlab_get_job_log", Args: map[string]string{"job_id": "81234567"}, Output: "log one"},
		Call{Tool: "mcp:gitlab_get_job_log", Args: map[string]string{"job_id": "81234599"}, Output: "log two"},
		Call{Tool: "mcp:gitlab_get_job_log", Args: map[string]string{"job_id": "81234612"}, Output: "log three"})
	if o := judged(t, []Session{ids}, "ids"); !o.Recommended || o.Route != RouteParamLoop {
		t.Errorf("want parametric_loop, got %+v", o)
	}
	search := selSession("search", "why is recall low",
		Call{Tool: "mcp:telara_knowledge_search", Args: map[string]string{"query": "recall evaluation holdout"}, Output: "a"},
		Call{Tool: "mcp:telara_knowledge_search", Args: map[string]string{"query": "episode label disagreement"}, Output: "b"},
		Call{Tool: "mcp:telara_knowledge_search", Args: map[string]string{"query": "routine consistency check"}, Output: "c"})
	if o := judged(t, []Session{search}, "search"); o.Recommended {
		t.Errorf("a loop over search phrasings is exploration, got %+v", o)
	}
	chained := selSession("chained", "follow the links",
		Call{Tool: "mcp:web_get", Args: map[string]string{"url": "https://example.com/start"}, Output: "see https://example.com/next"},
		Call{Tool: "mcp:web_get", Args: map[string]string{"url": "https://example.com/next"}, Output: "see https://example.com/last"},
		Call{Tool: "mcp:web_get", Args: map[string]string{"url": "https://example.com/last"}, Output: "end"})
	if o := judged(t, []Session{chained}, "chained"); o.Recommended {
		t.Errorf("each item came from the previous result: exploration, got %+v", o)
	}
}

// Harness text and single calls are never recommended.
func TestSelectionRefusesHarnessAndSingleCalls(t *testing.T) {
	h := selSession("h", "# AGENTS.md instructions for /repo", Call{Tool: "shell", Command: "ls"}, Call{Tool: "shell", Command: "pwd"})
	one := selSession("one", "what time is it", Call{Tool: "shell", Command: "date"})
	for _, o := range SelectOpportunities([]Session{h, one}) {
		if o.Recommended {
			t.Errorf("%s must not be recommended: %+v", o.Session, o)
		}
	}
}

// A short request naming an object, whose calls act on it with a later step
// using an earlier result, is a single-pass procedure. The same request with
// no dependency between its steps, or naming nothing the calls touch, is not.
func TestNamedObjectNeedsATouchedObjectAndADependency(t *testing.T) {
	close := selSession("close", "close TENG-4321 with a note that it shipped",
		Call{Tool: "mcp:jira_list_transitions", Args: map[string]string{"issue_key": "TENG-4321"}, Output: `{"transitions":[{"id":"31","name":"Done"}],"done_id":"trn-31-done"}`},
		Call{Tool: "mcp:jira_add_comment", Args: map[string]string{"issue_key": "TENG-4321", "body": "shipped"}},
		Call{Tool: "mcp:jira_transition_issue", Args: map[string]string{"issue_key": "TENG-4321", "transition_id": "trn-31-done"}})
	if o := judged(t, []Session{close}, "close"); !o.Recommended || o.Route != RouteNamedObject {
		t.Errorf("want named_object, got %+v", o)
	}
	unrelated := selSession("unrelated", "close TENG-4321 with a note that it shipped",
		Call{Tool: "mcp:jira_add_comment", Args: map[string]string{"issue_key": "TENG-4321", "body": "shipped"}},
		Call{Tool: "mcp:jira_get_issue", Args: map[string]string{"issue_key": "TENG-4321"}})
	if o := judged(t, []Session{unrelated}, "unrelated"); o.Recommended {
		t.Errorf("no step depends on another: not a procedure, got %+v", o)
	}
	nothing := selSession("nothing", "why is it slow today",
		Call{Tool: "mcp:metrics_query", Args: map[string]string{"query": "p95 latency"}, Output: "series-abc123"},
		Call{Tool: "mcp:metrics_series", Args: map[string]string{"id": "series-abc123"}})
	if o := judged(t, []Session{nothing}, "nothing"); o.Recommended {
		t.Errorf("the request names nothing the calls act on, got %+v", o)
	}
}
