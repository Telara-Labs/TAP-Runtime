package history

import (
	"encoding/json"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

func TestAssemblerAttributesPairsAndSpreads(t *testing.T) {
	a := NewAssembler("x", "s1")
	a.Add(UserText{Text: "list the open tickets"})
	a.Add(TurnUsage{Turn: "m1", Usage: trace.Usage{Fresh: 90, Output: 10}})
	a.Add(ToolCall{Key: "k1", Turn: "m1", Call: trace.Call{Tool: "mcp:list"}})
	a.Add(ToolCall{Key: "k2", Turn: "m1", Call: trace.Call{Tool: "shell", Command: "make"}})
	// The same response's usage repeated on its next record is not counted again.
	a.Add(TurnUsage{Turn: "m1", Usage: trace.Usage{Fresh: 999}})
	a.Add(ToolResult{Key: "k1", Nth: -1, Text: `{"issues":[{"key":"ABC-12"}]}`})
	a.Add(ToolResult{Key: "k2", Nth: -1, Text: "make: *** [all] Error 2\nexit code 2"})
	a.Add(ToolResult{Key: "nope", Nth: -1, Text: "ignored"})
	a.Add(UserText{Text: "now close ABC-12"})
	a.Add(ToolCall{Key: "k3", Turn: "m2", Call: trace.Call{Tool: "mcp:close"}})
	a.Add(ToolResult{Key: "k3", Nth: 0, Text: "ok", IsError: true})
	s := a.Finish()

	if s.Client != "x" || s.ID != "s1" || len(s.Requests) != 2 || len(s.Calls) != 3 {
		t.Fatalf("session %+v", s)
	}
	c := s.Calls
	if c[0].Request != 0 || c[1].Request != 0 || c[2].Request != 1 {
		t.Errorf("requests %d %d %d", c[0].Request, c[1].Request, c[2].Request)
	}
	for _, x := range c {
		if x.Client != "x" || x.Session != "s1" {
			t.Errorf("call %+v lacks client/session", x)
		}
	}
	if c[0].Outcome != trace.OutcomeOK || c[1].Outcome != trace.OutcomeFailed || c[2].Outcome != trace.OutcomeFailed {
		t.Errorf("outcomes %q %q %q", c[0].Outcome, c[1].Outcome, c[2].Outcome)
	}
	if len(c[0].OutIDs) == 0 || c[0].Output == "" {
		t.Errorf("result fields not filled: %+v", c[0])
	}
	if !c[0].Measured || c[0].Tokens.Total() != 50 || c[1].Tokens.Total() != 50 || c[0].Turn != 0 {
		t.Errorf("turn usage: %+v %+v", c[0].Tokens, c[1].Tokens)
	}
	if c[2].Measured {
		t.Error("a turn with no usage must stay unmeasured")
	}
}

func TestAssemblerUnnamedUsageCoversCallsSinceTheLast(t *testing.T) {
	a := NewAssembler("x", "s")
	a.Add(ToolCall{Call: trace.Call{Tool: "a"}})
	a.Add(TurnUsage{Usage: trace.Usage{Output: 4}})
	a.Add(ToolCall{Call: trace.Call{Tool: "b"}})
	a.Add(ToolCall{Call: trace.Call{Tool: "c"}})
	a.Add(TurnUsage{Usage: trace.Usage{Output: 8}})
	a.Add(TurnUsage{Usage: trace.Usage{Output: 100}}) // no calls since: nothing to attribute
	s := a.Finish()
	if s.Calls[0].Tokens.Output != 4 || s.Calls[1].Tokens.Output != 4 || s.Calls[2].Tokens.Output != 4 {
		t.Fatalf("spread %+v", s.Calls)
	}
	if s.Calls[0].Turn != 0 || s.Calls[1].Turn != 1 {
		t.Fatalf("turns %d %d", s.Calls[0].Turn, s.Calls[1].Turn)
	}
}

func TestAssemblerSharedKeyAndStoredOutcome(t *testing.T) {
	a := NewAssembler("x", "s")
	// One script made three calls under one key; results arrive by position.
	for _, tool := range []string{"a", "b", "c"} {
		a.Add(ToolCall{Key: "exec", Call: trace.Call{Tool: tool}})
	}
	if a.CallsUnder("exec") != 3 {
		t.Fatal("CallsUnder")
	}
	a.Add(ToolResult{Key: "exec", Nth: 1, Text: "fine"})
	a.Add(ToolResult{Key: "exec", Nth: 7, Text: "out of range"})
	// A store that records status keeps it, even against the text.
	a.Add(ToolCall{Key: "row", Call: trace.Call{Tool: "d"}})
	a.Add(ToolResult{Key: "row", Text: "exit code 1", HasOutcome: true, Outcome: trace.OutcomeOK})
	s := a.Finish()
	if s.Calls[0].Output != "" || s.Calls[1].Output != "fine" || s.Calls[2].Output != "" {
		t.Fatalf("positional results: %q %q %q", s.Calls[0].Output, s.Calls[1].Output, s.Calls[2].Output)
	}
	if s.Calls[3].Outcome != trace.OutcomeOK {
		t.Fatalf("stored outcome overridden: %q", s.Calls[3].Outcome)
	}
}

func TestAssemblerIgnoresNonRequestsAndRepeats(t *testing.T) {
	a := NewAssembler("x", "s")
	a.Add(UserText{Text: ""})
	a.Add(UserText{Text: "deploy the gateway"})
	a.Add(UserText{Text: "deploy the gateway"})
	a.Add(UserText{Text: "summary of earlier work", Role: "synthetic_context"})
	s := a.Finish()
	if len(s.Requests) != 2 || s.RequestRoles[1] != "synthetic_context" {
		t.Fatalf("requests %q roles %q", s.Requests, s.RequestRoles)
	}
}

// The tool-name dialects decode the same MCP call to the same server and
// tool, whichever agent recorded it.
func TestDoubleUnderscoreDialect(t *testing.T) {
	tool, cmd, args, _, server, mcpTool := DoubleUnderscore("mcp__jira__get_issue", nil)
	if tool != "mcp:get_issue" || server != "jira" || mcpTool != "get_issue" || cmd != "" || args == nil {
		t.Fatalf("%q %q %q %q", tool, server, mcpTool, cmd)
	}
	tool, cmd, _, _, _, _ = DoubleUnderscore("Bash", map[string]json.RawMessage{"command": json.RawMessage(`"make test"`)})
	if tool != "shell" || cmd != "make test" {
		t.Fatalf("Bash = %q %q", tool, cmd)
	}
}
