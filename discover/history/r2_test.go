package history

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

// R2 readers. Fixtures are synthetic, following each agent's
// own writer (testdata/SYNTHETIC.md).

type wantCall struct {
	tool, server, mcpTool string
	request               int
	outcome               trace.Outcome
}

func checkCalls(t *testing.T, s trace.Session, wants []wantCall) {
	t.Helper()
	if len(s.Calls) != len(wants) {
		var got []string
		for _, c := range s.Calls {
			got = append(got, c.Tool)
		}
		t.Fatalf("%s: calls %v, want %d", s.Client, got, len(wants))
	}
	for i, w := range wants {
		c := s.Calls[i]
		if c.Tool != w.tool || c.MCPServer != w.server || c.MCPTool != w.mcpTool || c.Request != w.request || c.Outcome != w.outcome {
			t.Errorf("%s call %d = %s %s/%s r%d outcome %d; want %+v", s.Client, i, c.Tool, c.MCPServer, c.MCPTool, c.Request, c.Outcome, w)
		}
		if c.Client != s.Client || c.Session != s.ID {
			t.Errorf("call %d identity %s %s", i, c.Client, c.Session)
		}
	}
}

func TestReplayGeminiSemantics(t *testing.T) {
	log, skipped, err := ReplayGemini(strings.NewReader(`{"sessionId":"s","projectHash":"p","startTime":"2026-10-01T10:00:00Z"}
{"id":"a","type":"user","content":"one"}
{"id":"b","type":"gemini","content":"first"}
{"id":"c","type":"user","content":"two"}
{"id":"b","type":"gemini","content":"first, updated"}
{"id":"d","type":"user","content":"three"}
{"$rewindTo":"c"}
{"$set":{"lastUpdated":"x"}}
not json`))
	if err != nil || skipped != 1 {
		t.Fatalf("skipped %d, %v", skipped, err)
	}
	var texts []string
	for _, m := range log.Messages {
		var x struct{ Content string }
		json.Unmarshal(m, &x)
		texts = append(texts, x.Content)
	}
	// b replaced in place, c and later dropped by the rewind.
	if strings.Join(texts, "|") != "one|first, updated" || jsonString(log.Meta["lastUpdated"]) != "x" {
		t.Fatalf("messages %q meta %v", texts, log.Meta)
	}
	log, _, _ = ReplayGemini(strings.NewReader(`{"id":"a","content":"x"}
{"$rewindTo":"zzz"}
{"$set":{"messages":[{"id":"m1","content":"kept"}]}}`))
	if len(log.Messages) != 1 {
		t.Fatalf("an unknown rewind clears, $set messages replaces: %d", len(log.Messages))
	}
}

func TestReplayVSCodeSemantics(t *testing.T) {
	state, skipped, err := ReplayVSCode(strings.NewReader(`{"kind":0,"v":{"a":{"list":[1,2,3]},"b":1}}
{"kind":1,"k":["a","x"],"v":"set"}
{"kind":2,"k":["a","list"],"v":[9],"i":1}
{"kind":2,"k":["a","list"],"v":[10]}
{"kind":3,"k":["b"]}
{"kind":1,"k":["missing","deep"],"v":1}
{"kind":1,"k":["a","li`))
	if err != nil || skipped != 2 {
		t.Fatalf("skipped %d (a path that does not exist and a torn line), %v", skipped, err)
	}
	b, _ := json.Marshal(state)
	if string(b) != `{"a":{"list":[1,9,10],"x":"set"}}` {
		t.Fatalf("state %s", b)
	}
	if _, skipped, _ := ReplayVSCode(strings.NewReader(`{"kind":1,"k":["a"],"v":1}`)); skipped != 1 {
		t.Fatal("a change before any state must be skipped")
	}
}

func TestGeminiCLIReader(t *testing.T) {
	ss, st, err := GeminiCLI{Dir: filepath.Join("testdata", "gemini-cli", "tmp")}.ReadWithStats(time.Time{})
	if err != nil || len(ss) != 1 || st.UnreadableFiles != 0 {
		t.Fatalf("%d sessions %+v %v", len(ss), st, err)
	}
	s := ss[0]
	if s.ID != "5f2c0000-0000-4000-8000-000000000001" || s.Start.IsZero() || s.Skipped != 1 {
		t.Fatalf("session %s start %v skipped %d", s.ID, s.Start, s.Skipped)
	}
	// The injected <session_context> is no request; the rewound turn and its
	// call are gone.
	if strings.Join(s.Requests, "|") != "find the open issues|now open ABC-99" {
		t.Fatalf("requests %q", s.Requests)
	}
	checkCalls(t, s, []wantCall{
		{"mcp:search_issues", "jira", "search_issues", 0, trace.OutcomeOK},
		{"shell", "", "", 0, trace.OutcomeOK},
		{"mcp:get_issue", "jira", "get_issue", 1, trace.OutcomeFailed},
	})
	if s.Calls[1].Command != "git status --short" || len(s.Calls[0].OutIDs) == 0 {
		t.Errorf("shell %q, ids %v", s.Calls[1].Command, s.Calls[0].OutIDs)
	}
	// One response's tokens over its two calls: fresh = input - cached.
	if !s.Calls[0].Measured || s.Calls[0].Tokens.Fresh != 100 || s.Calls[0].Tokens.Cached != 500 || s.Calls[0].Tokens.Output != 50 {
		t.Errorf("tokens %+v", s.Calls[0].Tokens)
	}
}

func TestQwenCodeReader(t *testing.T) {
	ss, err := QwenCode{Dir: filepath.Join("testdata", "qwen-code", "projects")}.Read(time.Time{})
	if err != nil || len(ss) != 1 {
		t.Fatalf("%d sessions (the ledger file is not one), %v", len(ss), err)
	}
	s := ss[0]
	if strings.Join(s.Requests, "|") != "find the open issues|open ABC-12" {
		t.Fatalf("requests %q: the abandoned branch must not appear", s.Requests)
	}
	checkCalls(t, s, []wantCall{
		{"mcp:search_issues", "jira", "search_issues", 0, trace.OutcomeOK},
		{"shell", "", "", 0, trace.OutcomeFailed},
		{"mcp:get_issue", "jira", "get_issue", 1, trace.OutcomeOK},
	})
	if !s.Calls[0].Measured || s.Calls[0].Tokens.Fresh != 300 || s.Calls[0].Tokens.Output != 50 {
		t.Errorf("tokens %+v", s.Calls[0].Tokens)
	}
}

func TestVSCodeCopilotReader(t *testing.T) {
	ss, st, err := VSCodeCopilot{User: filepath.Join("testdata", "vscode-copilot", "User")}.ReadWithStats(time.Time{})
	if err != nil || len(ss) != 2 || st.UnreadableFiles != 0 {
		t.Fatalf("%d sessions %+v %v", len(ss), st, err)
	}
	old, s := ss[0], ss[1] // the older whole-document session starts first
	if old.ID != "1b2c0000-0000-4000-8000-000000000004" || len(old.Calls) != 1 || old.Calls[0].MCPTool != "get_issue" {
		t.Fatalf("whole-document session %+v", old)
	}
	if s.Skipped != 1 || strings.Join(s.Requests, "|") != "find the open issues|open ABC-99" {
		t.Fatalf("skipped %d requests %q", s.Skipped, s.Requests)
	}
	checkCalls(t, s, []wantCall{
		{"mcp:search_issues", "jira", "search_issues", 0, trace.OutcomeOK},
		{"shell", "", "", 0, trace.OutcomeOK},
		{"mcp:get_issue", "jira", "get_issue", 1, trace.OutcomeFailed},
	})
	if s.Calls[0].Args["jql"] != "status = open" || s.Calls[1].Command != "git status --short" {
		t.Errorf("args %v command %q", s.Calls[0].Args, s.Calls[1].Command)
	}
}

func TestVSCodeMCPToolName(t *testing.T) {
	for _, c := range []struct{ id, label, want string }{
		{"mcp_jira_search_issues", "jira", "search_issues"},
		{"mcp_jira_cloud_search_issues", "Jira Cloud", "search_issues"},
		{"mcp_a_very_long_s_get", "A very long server name", "get"},
		{"mcp_jira2_search", "jira", "search"},
		{"copilot_readFile", "jira", "copilot_readFile"},
	} {
		if got := VSCodeMCPToolName(c.id, c.label); got != c.want {
			t.Errorf("%s (%s) = %s, want %s", c.id, c.label, got, c.want)
		}
	}
}

// One MCP call reads the same from every R2 dialect as from Claude Code.
func TestR2DialectsAgreeWithClaude(t *testing.T) {
	var claude, gem, vs trace.Call
	args := map[string]json.RawMessage{"issue_key": json.RawMessage(`"ABC-1"`)}
	claude.Tool, _, claude.Args, claude.RawArgs, claude.MCPServer, claude.MCPTool = DoubleUnderscore("mcp__jira__get_issue", args)
	GeminiTool(&gem, "mcp_jira_get_issue", args)
	MCPCall(&vs, "jira", VSCodeMCPToolName("mcp_jira_get_issue", "jira"), args)
	for name, c := range map[string]trace.Call{"gemini-cli": gem, "vscode-copilot": vs} {
		if c.Tool != claude.Tool || c.MCPServer != claude.MCPServer || c.MCPTool != claude.MCPTool || c.Args["issue_key"] != "ABC-1" {
			t.Errorf("%s: %s %s/%s %v", name, c.Tool, c.MCPServer, c.MCPTool, c.Args)
		}
	}
}
