package discover

import (
	"fmt"
	"gitlab.com/telara-labs/tap-runtime/discover/pack"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/primitive"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

func TestMultiContinuationFamilyDoesNotInstallOneChain(t *testing.T) {
	install := primitiveInstaller(nil, pack.Destination{Collection: t.TempDir()})
	result, err := install(primitive.Family{FollowUps: []primitive.FollowUp{
		{Steps: []string{"mcp:issue_transition"}, Runs: 3},
		{Steps: []string{"mcp:issue_link"}, Runs: 2},
	}}, []primitive.Primitive{{Steps: []string{"mcp:issue_create", "mcp:issue_transition"}}})
	if err != nil || result.Installed || !strings.Contains(result.Reason, "causal bundle") {
		t.Fatalf("multi-continuation family installed one chain: %+v, %v", result, err)
	}
}

func mcpCall(id, tool string, args map[string]string, out string, at int) trace.Call {
	c := trace.Call{ID: id, Tool: "mcp:" + tool, MCPServer: "jira", MCPTool: tool, Args: args, Output: out,
		Outcome: trace.OutcomeOK, Time: time.Date(2026, 9, 1, 9, 0, at, 0, time.UTC)}
	c.OutIDs, c.OutCtx, c.OutPaths = trace.OutputRefsPaths(out)
	return c
}

// Accepting a tool-call chain generates its program from the recorded
// bindings and installs it, named after what it does; the value one step
// returns is taken by the next, not asked of the caller.
func TestAcceptGeneratesAndInstalls(t *testing.T) {
	var ss []trace.Session
	for i := 0; i < 3; i++ {
		key := fmt.Sprintf("KEY-%d4", i)
		ss = append(ss, trace.Session{Client: "claude-code", ID: fmt.Sprint("s", i), Requests: []string{"note it on the ticket"}, Calls: []trace.Call{
			mcpCall(fmt.Sprintf("a%d", i), "issue_create", map[string]string{"summary": fmt.Sprint("follow up ", i)}, `{"key":"`+key+`"}`, 0),
			mcpCall(fmt.Sprintf("b%d", i), "issue_comment", map[string]string{"issue_key": key, "body": fmt.Sprint("note ", i)}, `{"ok":true}`, 1),
		}})
	}
	res := primitive.Discover(ss, nil)
	if len(res.Families) != 1 {
		t.Fatalf("fixture: %d families", len(res.Families))
	}
	byID := map[string]primitive.Primitive{}
	for _, p := range res.Primitives {
		byID[p.ID] = p
	}
	var members []primitive.Primitive
	for _, id := range res.Families[0].Members {
		members = append(members, byID[id])
	}
	home := t.TempDir()
	r, err := primitiveInstaller(ss, pack.Destination{Collection: home})(res.Families[0], members)
	if err != nil || !r.Installed || !strings.HasPrefix(r.Name, "discovered-issue-create-comment-") {
		t.Fatalf("install: %+v %v", r, err)
	}
	main, err := os.ReadFile(filepath.Join(r.Where, "main.py"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(main), `"step_2_issue_key"`) || strings.Contains(string(main), "null") {
		t.Fatalf("the created key must flow from step 1, not be asked for:\n%s", main)
	}
}
