package history

import (
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// R5 readers (TENG-3120), on synthetic fixtures (testdata/SYNTHETIC.md).

func TestCopilotCLIReader(t *testing.T) {
	ss, st, err := CopilotCLI{Dir: "testdata/copilot-cli/session-state", Configs: []string{"testdata/copilot-cli/mcp-config.json"}}.ReadWithStats(time.Time{})
	if err != nil || len(ss) != 1 || st.UnreadableFiles != 0 {
		t.Fatalf("%d sessions %+v %v", len(ss), st, err)
	}
	s := ss[0]
	if s.ID != "4c1e0000-0000-4000-8000-000000000005" || s.Skipped != 2 || len(s.Requests) != 2 {
		t.Fatalf("id %s skipped %d (the torn line's two pieces) requests %q", s.ID, s.Skipped, s.Requests)
	}
	checkCalls(t, s, []wantCall{
		{"shell", "", "", 0, trace.OutcomeOK},
		{"mcp:search_issues", "tracker", "search_issues", 0, trace.OutcomeOK},
		{"mcp:get_issue", "tracker", "get_issue", 0, trace.OutcomeFailed},
		{"mcp:get_issue", "tracker", "get_issue", 1, trace.OutcomeOK},
		{"view", "", "", 1, trace.OutcomeUnknown}, // the orphan: never completed
	})
}

func TestZedReader(t *testing.T) {
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("zstd is not installed; the reader reports itself unavailable then")
	}
	dir := t.TempDir()
	buildDB(t, "testdata/zed/threads/threads.sql", filepath.Join(dir, "threads.db"))
	ss, st, err := Zed{Dir: dir}.ReadWithStats(time.Time{})
	if err != nil || len(ss) != 1 || st.UnreadableFiles != 0 {
		t.Fatalf("%d sessions (the legacy thread has no calls) %+v %v", len(ss), st, err)
	}
	s := ss[0]
	if s.ID != "thread-1" || len(s.Requests) != 2 {
		t.Fatalf("id %s requests %q", s.ID, s.Requests)
	}
	// Zed records MCP tools by their bare name: no server.
	checkCalls(t, s, []wantCall{
		{"search_issues", "", "", 0, trace.OutcomeOK},
		{"shell", "", "", 0, trace.OutcomeOK},
		{"get_issue", "", "", 1, trace.OutcomeFailed},
	})
}

func TestZedLegacyThread(t *testing.T) {
	s, err := ZedThread("t", time.Time{}, []byte(`{"version":"0.2.0","messages":[{"id":1,"role":"user","segments":[{"type":"text","text":"hello there"}]}]}`))
	if err != nil || len(s.Requests) != 1 || s.Requests[0] != "hello there" {
		t.Fatalf("%q %v", s.Requests, err)
	}
}
