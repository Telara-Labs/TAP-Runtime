package history

import (
	"encoding/json"
	"testing"
)

// Gemini CLI 0.62 records results inside one
// <untrusted_context> layer; the reader keeps the tool's own text.
func TestUnwrapUntrusted(t *testing.T) {
	for in, want := range map[string]string{
		"<untrusted_context>\n{\"issues\":[]}\n</untrusted_context>":                              `{"issues":[]}`,
		"<untrusted_context>\n<untrusted_context>\nx\n</untrusted_context>\n</untrusted_context>": "<untrusted_context>\nx\n</untrusted_context>",
		"plain":                         "plain",
		"<untrusted_context>\nunclosed": "<untrusted_context>\nunclosed",
	} {
		if got := UnwrapUntrusted(in); got != want {
			t.Errorf("UnwrapUntrusted(%q) = %q", in, got)
		}
	}
}

// Qwen Code 0.24 runs an MCP tool loaded on demand through
// tool_call {name, arguments-as-JSON-string}; the call is that tool's.
func TestQwenToolCallDispatcherIsTheNamedTool(t *testing.T) {
	var rec QwenRecord
	line := `{"uuid":"u1","type":"assistant","timestamp":"2026-10-04T18:15:00Z","message":{"role":"model","parts":[{"functionCall":{"id":"c1","name":"tool_call","args":{"name":"mcp__tracker__get_issue","arguments":"{\"issue_key\":\"ABC-12\"}"}}}]}}`
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatal(err)
	}
	evs := QwenEvents(rec, "s")
	if len(evs) != 1 {
		t.Fatalf("%d events", len(evs))
	}
	c := evs[0].(ToolCall).Call
	if c.Tool != "mcp:get_issue" || c.MCPServer != "tracker" || c.MCPTool != "get_issue" || c.Args["issue_key"] != "ABC-12" {
		t.Fatalf("%+v", c)
	}
}
