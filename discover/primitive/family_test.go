package primitive

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

func familyWith(t *testing.T, res Result, head string) Family {
	t.Helper()
	for _, f := range res.Families {
		if f.Head == head {
			return f
		}
	}
	var got []string
	for _, f := range res.Families {
		got = append(got, f.Head)
	}
	t.Fatalf("no family headed %s among %v", head, got)
	return Family{}
}

// Rule 1: a call whose argument the agent built from the previous output
// (a line range from grep's line number) starts a new chain.
func TestConstructedArgumentStartsANewChain(t *testing.T) {
	var ss []trace.Session
	for i := 0; i < 3; i++ {
		file := fmt.Sprintf("src/f%d.go", i)
		ss = append(ss, session(fmt.Sprint("s", i), []string{"look at the handler"},
			shell("grep -n handler "+file, file+":12"+fmt.Sprint(i)+":func handler()", 0, 0),
			shell(fmt.Sprintf("sed -n 12%d,16%dp %s", i, i, file), "func handler() {", 0, time.Second)))
	}
	res := Discover(ss, nil)
	if res.Summary.DecisionCalls != 3 {
		t.Fatalf("decisions = %d", res.Summary.DecisionCalls)
	}
	for _, p := range res.Primitives {
		if len(p.Steps) > 1 {
			t.Fatalf("a decision was chained: %v", p.Steps)
		}
	}
}

// Rules 2 and 7: chains acting on what one head produced are one procedure
// with optional follow-ups; nothing is left as a lone call.
func TestSharedHeadIsOneProcedureWithOptionalFollowUps(t *testing.T) {
	var ss []trace.Session
	for i := 0; i < 6; i++ {
		key := fmt.Sprintf("KEY-%d5", i)
		calls := []trace.Call{call("mcp:issue_create", map[string]string{"summary": "a b"}, `{"key":"`+key+`"}`, 0, 0)}
		switch i % 3 {
		case 0:
			calls = append(calls, call("mcp:issue_transition", map[string]string{"issue_key": key, "to": "doing"}, `{"ok":true}`, 0, time.Second))
		case 1:
			calls = append(calls, call("mcp:issue_comment", map[string]string{"issue_key": key, "body": "note here"}, `{"ok":true}`, 0, time.Second))
		case 2:
			calls = append(calls, call("mcp:issue_link", map[string]string{"inward": key, "type": "relates"}, `{"ok":true}`, 0, time.Second))
		}
		calls[1].Tokens = trace.Usage{Fresh: float64(100 + i)}
		ss = append(ss, session(fmt.Sprint("s", i), []string{"file it"}, calls...))
	}
	res := Discover(ss, nil)
	if len(res.Families) != 1 {
		t.Fatalf("want one procedure, got %d: %+v", len(res.Families), res.Families)
	}
	f := res.Families[0]
	if f.Head != "mcp:issue_create" || len(f.FollowUps) != 3 || !f.FollowUps[0].Optional || f.ExecutionCount != 6 {
		t.Fatalf("family %+v", f)
	}
	var estimated float64
	var turns int
	for _, fu := range f.FollowUps {
		if len(fu.Members) != 1 || fu.PotentialTokens <= 0 || fu.PotentialTurns != 2 {
			t.Fatalf("continuation lost its own opportunity: %+v", fu)
		}
		estimated += fu.PotentialTokens
		turns += fu.PotentialTurns
	}
	if estimated != inputEquivalent(f.Saved) || turns != f.TurnsSaved {
		t.Fatalf("option estimates %g/%d differ from family %g/%d", estimated, turns, inputEquivalent(f.Saved), f.TurnsSaved)
	}
}

// Rule 3: heads that only read (by declared evidence) and feed the same
// follow-up are alternative sources of one procedure. Heads whose effect is
// unknown (an MCP tool's never is declared) stay apart: a creating call must
// not merge with a finding one.
func TestReadHeadsFeedingTheSameActAreAlternatives(t *testing.T) {
	var ss []trace.Session
	for i := 0; i < 4; i++ {
		key := fmt.Sprintf("KEY-%d6", i)
		head := shell("cat tickets.txt", key, 0, 0)
		if i%2 == 1 {
			head = shell("tail -1 tickets.txt", key, 0, 0)
		}
		ss = append(ss, session(fmt.Sprint("s", i), []string{"note it on the ticket"}, head,
			call("mcp:issue_comment", map[string]string{"issue_key": key, "body": "note here"}, `{"ok":true}`, 0, time.Second)))
	}
	res := Discover(ss, nil)
	if len(res.Families) != 1 || len(res.Families[0].Sources) != 1 {
		t.Fatalf("want one procedure with an alternative source: %+v", res.Families)
	}
	var tools []trace.Session
	for i := 0; i < 4; i++ {
		key := fmt.Sprintf("KEY-%d7", i)
		head := call("mcp:issue_search", map[string]string{"jql": "project = X"}, `{"issues":[{"key":"`+key+`"}]}`, 0, 0)
		if i%2 == 1 {
			head = call("mcp:issue_create", map[string]string{"summary": "a b"}, `{"key":"`+key+`"}`, 0, 0)
		}
		tools = append(tools, session(fmt.Sprint("t", i), []string{"note it on the ticket"}, head,
			call("mcp:issue_comment", map[string]string{"issue_key": key, "body": "note here"}, `{"ok":true}`, 0, time.Second)))
	}
	if res := Discover(tools, nil); len(res.Families) != 2 {
		t.Fatalf("heads of unknown effect were merged: %+v", res.Families)
	}
}

// Rule 4: a chain whose every run is part of a longer chain's run is a
// fragment of it.
func TestFragmentFoldsIntoTheLongerChain(t *testing.T) {
	var ss []trace.Session
	for i := 0; i < 3; i++ {
		pid, jid := fmt.Sprintf("8100000%d", i), fmt.Sprintf("9100000%d", i)
		ss = append(ss, session(fmt.Sprint("s", i), []string{"why did it fail"},
			call("mcp:pipelines_list", map[string]string{"project": "web"}, `{"id":"`+pid+`"}`, 0, 0),
			call("mcp:jobs_list", map[string]string{"pipeline_id": pid}, `{"id":"`+jid+`"}`, 0, time.Second),
			call("mcp:job_get", map[string]string{"job_id": jid}, `{"log":"x"}`, 0, 2*time.Second)))
	}
	res := Discover(ss, nil)
	if res.Summary.Fragments == 0 {
		t.Fatalf("no fragment dropped: %+v", res.Primitives)
	}
	for _, p := range res.Primitives {
		if strings.Join(p.Steps, ">") == "mcp:pipelines_list>mcp:jobs_list" {
			t.Fatal("the fragment is still listed")
		}
	}
}

// Rule 5: argument names that differ only in case or separators are one
// input.
func TestAliasesJoinOneValue(t *testing.T) {
	got := aliases([]string{"issueIdOrKey", "issue_id_or_key", "body"})
	if strings.Join(got, ";") != "body;issueIdOrKey|issue_id_or_key" {
		t.Fatalf("aliases = %v", got)
	}
}

// Rule 6: a gateway dispatch whose words name no tool matches the one direct
// tool that takes all its arguments.
func TestGatewayMatchesByArgumentShape(t *testing.T) {
	direct := map[string]map[string]bool{
		"mcp:telara_jira_search_issues": {"jql": true, "max_results": true, "fields": true},
		"mcp:telara_jira_get_issue":     {"issue_key": true},
		"mcp:telara_gitlab_search":      {"query": true},
	}
	got, ok := resolve(map[string]string{"integration": "jira", "action": "list_issues"}, map[string]bool{"jql": true, "maxResults": true}, direct)
	if !ok || got != "mcp:telara_jira_search_issues" {
		t.Fatalf("resolved %s %v", got, ok)
	}
}
