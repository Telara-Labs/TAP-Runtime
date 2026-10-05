package history

import (
	"testing"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

// TENG-3160: a default Codex 0.147 records a direct MCP call's result
// cleanly in an mcp_tool_call_end event, and hands the model the same result
// inside a "Wall time / Output:" text envelope. The reader takes the clean
// one: the result is the tool's own text, and a call the server marked
// isError is a failure. testdata/codex-0147 is three real codex exec runs of
// the scripted task (TENG-3124), trimmed to the records the reader uses.
func TestCodex0147ReadsMCPResultsFromTheirEndEvents(t *testing.T) {
	ss, err := Codex{Dir: "testdata/codex-0147/sessions"}.Read(time.Time{})
	if err != nil || len(ss) != 3 {
		t.Fatalf("%d sessions, %v", len(ss), err)
	}
	for _, s := range ss {
		checkCalls(t, s, []wantCall{
			{"shell", "", "", 0, trace.OutcomeOK},
			{"mcp:search_issues", "tracker", "search_issues", 0, trace.OutcomeOK},
			{"mcp:get_issue", "tracker", "get_issue", 0, trace.OutcomeFailed},
			{"mcp:get_issue", "tracker", "get_issue", 0, trace.OutcomeOK},
		})
		if out := s.Calls[1].Output; out != `{"issues":[{"key":"ABC-12"},{"key":"ABC-13"}]}` {
			t.Errorf("search result %q", out)
		}
		if out := s.Calls[2].Output; out != "Issue ABC-99 does not exist" {
			t.Errorf("failed result %q", out)
		}
	}
}

// Without the end event (older Codex), the envelope is unwrapped to the
// content's text; a shell's output, not a content array, is left alone.
func TestCodexMCPEnvelope(t *testing.T) {
	got, ok := CodexMCPEnvelope("Wall time: 0.0036 seconds\nOutput:\n[{\"type\":\"text\",\"text\":\"{\\\"issues\\\":[]}\"}]")
	if !ok || got != `{"issues":[]}` {
		t.Fatalf("%v %q", ok, got)
	}
	if _, ok := CodexMCPEnvelope("Chunk ID: 1\nWall time: 0.0 seconds\nProcess exited with code 0\nOutput:\n       1 notes.txt\n"); ok {
		t.Fatal("a shell's output was read as an MCP envelope")
	}
	if _, ok := CodexMCPEnvelope("Wall time: 0.1 seconds\nOutput:\nplain text"); ok {
		t.Fatal("plain text was read as a content array")
	}
}
