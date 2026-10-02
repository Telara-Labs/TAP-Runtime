package history

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// R3 readers (TENG-3118).

func TestClineCLIReader(t *testing.T) {
	ss, err := ClineCLI{Dir: "testdata/cline-cli/sessions", Configs: []string{"testdata/cline-cli/cline_mcp_settings.json"}}.Read(time.Time{})
	if err != nil || len(ss) != 1 {
		t.Fatalf("%d sessions, %v", len(ss), err)
	}
	s := ss[0]
	// The prompt is unwrapped from <user_input>; one turn (the CLI's JSON
	// mode cannot resume a session).
	if len(s.Requests) != 1 || !strings.HasPrefix(s.Requests[0], "Do these steps") {
		t.Fatalf("requests %q", s.Requests)
	}
	checkCalls(t, s, scripted(true)[:4])
	if s.Calls[0].Command != "wc -l notes.txt" || s.Start.IsZero() {
		t.Errorf("command %q start %v", s.Calls[0].Command, s.Start)
	}
}

func extTasks(id, ext string) ExtensionTasks {
	return ExtensionTasks{ID: id, Dirs: []string{filepath.Join("testdata", "vscode-ext", "Code", "User", "globalStorage", ext, "tasks")}}
}

func TestExtensionTaskReaders(t *testing.T) {
	native := []wantCall{
		{"mcp:search_issues", "tracker", "search_issues", 0, trace.OutcomeOK},
		{"shell", "", "", 0, trace.OutcomeOK},
		{"mcp:get_issue", "tracker", "get_issue", 0, trace.OutcomeFailed},
	}
	for _, c := range []struct{ id, ext string }{{"roo", "rooveterinaryinc.roo-cline"}, {"kilo", "kilocode.kilo-code"}} {
		ss, err := extTasks(c.id, c.ext).Read(time.Time{})
		if err != nil || len(ss) != 1 || ss[0].Client != c.id {
			t.Fatalf("%s: %d sessions, %v", c.id, len(ss), err)
		}
		if ss[0].Requests[0] != "find the open issues" {
			t.Errorf("%s: <task> not unwrapped: %q", c.id, ss[0].Requests)
		}
		checkCalls(t, ss[0], native)
	}
	ss, err := extTasks("cline", "saoudrizwan.claude-dev").Read(time.Time{})
	if err != nil || len(ss) != 2 {
		t.Fatalf("cline: %d sessions, %v", len(ss), err)
	}
	// The older task wrote its calls as XML in the text, paired by order.
	if len(ss[0].Calls) != 2 {
		ss[0], ss[1] = ss[1], ss[0]
	}
	checkCalls(t, ss[0], []wantCall{
		{"mcp:get_issue", "tracker", "get_issue", 0, trace.OutcomeOK},
		{"shell", "", "", 0, trace.OutcomeFailed},
	})
	if ss[0].Calls[1].Command != "ls missing-dir" || ss[0].Calls[0].Args["issue_key"] != "ABC-12" {
		t.Errorf("xml calls %+v", ss[0].Calls)
	}
	checkCalls(t, ss[1], native)
}

// The Cline CLI and the Cline extension are one agent.
func TestReaderSetReadsBoth(t *testing.T) {
	r := ReaderSet{ID: "cline", List: []trace.Reader{
		ClineCLI{Dir: "testdata/cline-cli/sessions"},
		extTasks("cline", "saoudrizwan.claude-dev"),
	}}
	ss, st, err := r.ReadWithStats(time.Time{})
	if err != nil || len(ss) != 3 || st.UnreadableFiles != 0 {
		t.Fatalf("%d sessions %+v %v", len(ss), st, err)
	}
}
