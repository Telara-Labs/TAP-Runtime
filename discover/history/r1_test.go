package history

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/redact"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// The R1 fixtures (TENG-3112) are slices of real sessions captured with
// cmd/discover-fixture and redacted with discover/redact. A Cursor CLI store
// is checked in as the SQL that rebuilds it.

// buildCursorCLIStores rebuilds testdata/cursor-cli under a temp dir laid
// out as ~/.cursor/chats: <workspace>/<session>/store.db.
func buildCursorCLIStores(t *testing.T) (dir, bin string) {
	t.Helper()
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 is not installed; the reader reports itself unavailable in that case")
	}
	dir = t.TempDir()
	sqls, _ := filepath.Glob(filepath.Join("testdata", "cursor-cli", "*", "*", "store.sql"))
	if len(sqls) == 0 {
		t.Fatal("no cursor-cli fixtures")
	}
	for _, f := range sqls {
		rel, _ := filepath.Rel(filepath.Join("testdata", "cursor-cli"), filepath.Dir(f))
		db := filepath.Join(dir, rel, "store.db")
		if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
			t.Fatal(err)
		}
		sql, _ := os.ReadFile(f)
		cmd := exec.Command(bin, db)
		cmd.Stdin = strings.NewReader(string(sql))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v %s", db, err, out)
		}
	}
	return dir, bin
}

func TestCursorCLIReader(t *testing.T) {
	dir, _ := buildCursorCLIStores(t)
	ss, err := CursorCLI{Dir: dir}.Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 2 || ss[0].ID != "758ccfd6-542b-4fed-b2ac-d9bbb242eab4" || ss[1].ID != "3887962d-107f-4b4c-a3ee-17703931a585" {
		t.Fatalf("sessions %v", ids(ss))
	}
	s := ss[0]
	if s.Client != "cursor-cli" || s.Start.IsZero() || s.Start.Year() != 2026 || len(s.Calls) != 19 {
		t.Fatalf("session %s: client %s start %s, %d calls", s.ID, s.Client, s.Start, len(s.Calls))
	}
	// The person's words come from inside <user_query>; the CLI's own
	// context (<user_info>, <system_reminder>) is not a request.
	if len(s.Requests) != 2 || !strings.HasPrefix(s.Requests[0], "Implement the gateway route") {
		t.Fatalf("requests %.80q", s.Requests)
	}
	for _, r := range append(s.Requests, ss[1].Requests...) {
		if strings.HasPrefix(strings.TrimSpace(r), "<") {
			t.Errorf("an envelope became a request: %.60q", r)
		}
	}
	var shell, jira *trace.Call
	for i := range s.Calls {
		c := &s.Calls[i]
		if c.Outcome == trace.OutcomeUnknown {
			t.Errorf("call %d (%s) has no result", i, c.Tool)
		}
		if c.Client != "cursor-cli" || c.Session != s.ID || c.ID == "" {
			t.Errorf("call %d identity %q %q %q", i, c.Client, c.Session, c.ID)
		}
		switch {
		case c.Tool == "shell" && shell == nil:
			shell = c
		case c.Tool == "mcp:telara_jira_get_issue" && jira == nil:
			jira = c
		}
	}
	if shell == nil || !strings.Contains(shell.Command, "go env GOMODCACHE") {
		t.Fatalf("Shell call not decoded: %+v", shell)
	}
	// CallMcpTool is a dispatcher: the call takes the server and tool it
	// names, and only the tool's own arguments.
	if jira == nil || jira.MCPServer != "telara" || jira.MCPTool != "telara_jira_get_issue" || jira.Args["issue_key"] == "" {
		t.Fatalf("CallMcpTool not unwrapped: %+v", jira)
	}
	for _, k := range []string{"server", "toolName", "description", "arguments"} {
		if _, ok := jira.Args[k]; ok {
			t.Errorf("dispatcher field %q kept as an argument", k)
		}
	}
	// The second session's Grep failed.
	failed := 0
	for _, c := range ss[1].Calls {
		if c.Outcome == trace.OutcomeFailed {
			failed++
			if c.Tool != "Grep" {
				t.Errorf("unexpected failure %s", c.Tool)
			}
		}
	}
	if failed != 1 {
		t.Errorf("%d failed calls, want the one Grep", failed)
	}
	// The digest names the messages read: stable across reads, distinct per session.
	again, _ := CursorCLI{Dir: dir}.Read(time.Time{})
	if s.SourceDigest == "" || again[0].SourceDigest != s.SourceDigest || ss[1].SourceDigest == s.SourceDigest {
		t.Fatal("source digest is not stable and distinct")
	}
	for _, x := range []string{"store.db-journal", "store.db-wal"} {
		if _, err := os.Stat(filepath.Join(dir, "ws1", s.ID, x)); err == nil {
			t.Errorf("reader left %s: the store must be opened read-only", x)
		}
	}
}

func TestCursorCLIAbsentAndUnavailable(t *testing.T) {
	ss, err := CursorCLI{Dir: filepath.Join(t.TempDir(), "absent")}.Read(time.Time{})
	if err != nil || ss != nil {
		t.Fatalf("absent store: %v, %v", ss, err)
	}
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "w", "s"), 0o755)
	os.WriteFile(filepath.Join(dir, "w", "s", "store.db"), []byte("x"), 0o600)
	if _, err := (CursorCLI{Dir: dir, SQLite3: filepath.Join(dir, "no-sqlite3")}).Read(time.Time{}); err == nil {
		// A named binary that does not run fails per store (skipped), so
		// the read itself succeeds with nothing; only a missing sqlite3
		// on PATH is an error. Either way nothing is invented.
		ss, _ := CursorCLI{Dir: dir, SQLite3: filepath.Join(dir, "no-sqlite3")}.Read(time.Time{})
		if len(ss) != 0 {
			t.Fatal("an unreadable store produced sessions")
		}
	}
}

// A corrupt message blob is skipped; the rest of the session survives.
func TestCursorCLICorruptMessageIsSkipped(t *testing.T) {
	dir, bin := buildCursorCLIStores(t)
	db := filepath.Join(dir, "ws2", "3887962d-107f-4b4c-a3ee-17703931a585", "store.db")
	out, err := exec.Command(bin, db, `UPDATE blobs SET data = '{"role":' WHERE rowid = (SELECT min(rowid) FROM blobs WHERE data LIKE '%"tool-result"%')`).CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	ss, err := CursorCLI{Dir: dir}.Read(time.Time{})
	if err != nil || len(ss) != 2 {
		t.Fatalf("%d sessions, %v", len(ss), err)
	}
	unknown := 0
	for _, c := range ss[1].Calls {
		if c.Outcome == trace.OutcomeUnknown {
			unknown++
		}
	}
	if len(ss[1].Calls) != 12 || unknown != 1 {
		t.Fatalf("%d calls, %d without a result; want 12 and the one whose result was corrupted", len(ss[1].Calls), unknown)
	}
}

func TestCursorCLIOrderReadsFieldOneIDs(t *testing.T) {
	idField := func(first byte) []byte {
		b := make([]byte, 32)
		b[0] = first
		return append([]byte{0x0a, 32}, b...)
	}
	var root []byte
	root = append(root, idField(0xaa)...)
	root = append(root, 0x10, 0x96, 0x01)       // field 2, varint 150
	root = append(root, 0x1a, 3, 'x', 'y', 'z') // field 3, bytes
	root = append(root, 0x0a, 2, 1, 2)          // field 1, but not an id
	root = append(root, idField(0xbb)...)
	got := CursorCLIOrder(root)
	if len(got) != 2 || !strings.HasPrefix(got[0], "aa") || !strings.HasPrefix(got[1], "bb") {
		t.Fatalf("ids %v", got)
	}
	if len(CursorCLIOrder([]byte{0x0a, 200})) != 0 {
		t.Fatal("a truncated root yielded ids")
	}
}

func TestAntigravityReader(t *testing.T) {
	ss, err := Antigravity{Dir: filepath.Join("testdata", "antigravity")}.Read(time.Time{})
	if err != nil || len(ss) != 1 {
		t.Fatalf("%d sessions, %v", len(ss), err)
	}
	s := ss[0]
	if s.Client != "antigravity" || s.ID != "1408d4ed-d5d8-4cd2-b3a4-d8f70bd2eed4" || s.SourceDigest == "" {
		t.Fatalf("session %s %s", s.Client, s.ID)
	}
	if len(s.Requests) != 4 || s.Requests[0] != "Do you have access to the file system?" || s.Requests[3] != "status?" {
		t.Fatalf("requests %.60q", s.Requests)
	}
	type want struct {
		tool, server string
		request      int
		outcome      trace.Outcome
	}
	// Results pair by step index: a call at step k is answered by step
	// k+1, and a skipped k+1 leaves the call without a result.
	wants := []want{
		{"find_by_name", "", 2, trace.OutcomeUnknown}, // step 6 never written
		{"list_dir", "", 2, trace.OutcomeOK},
		{"list_dir", "", 2, trace.OutcomeUnknown}, // step 10 not in the slice
		{"shell", "", 2, trace.OutcomeOK},
		{"shell", "", 2, trace.OutcomeOK},
		{"shell", "", 2, trace.OutcomeFailed}, // exited non-zero
		{"mcp:telara_tool_search", "telara", 2, trace.OutcomeOK},
		{"view_file", "", 2, trace.OutcomeOK},
		{"mcp:telara_tool_describe", "telara", 2, trace.OutcomeUnknown}, // step 118 never written
		{"mcp:telara_tool_describe", "telara", 2, trace.OutcomeOK},
		{"mcp:telara_execute_action", "telara", 2, trace.OutcomeUnknown},
		{"find_by_name", "", 3, trace.OutcomeOK},
		{"find_by_name", "", 3, trace.OutcomeUnknown},
		{"shell", "", 3, trace.OutcomeUnknown}, // still RUNNING
	}
	if len(s.Calls) != len(wants) {
		t.Fatalf("%d calls, want %d", len(s.Calls), len(wants))
	}
	for i, w := range wants {
		c := s.Calls[i]
		if c.Tool != w.tool || c.MCPServer != w.server || c.Request != w.request || c.Outcome != w.outcome {
			t.Errorf("call %d = %s %s r%d outcome %d; want %+v", i, c.Tool, c.MCPServer, c.Request, c.Outcome, w)
		}
		for _, k := range antigravityUIArgs {
			if _, ok := c.Args[k]; ok {
				t.Errorf("call %d keeps the UI field %s", i, k)
			}
		}
	}
	if s.Calls[13].Output == "" {
		t.Error("a running call keeps the text it has so far")
	}
	if c := s.Calls[3]; c.Command == "" || c.Time.IsZero() {
		t.Errorf("run_command not decoded: %+v", c)
	}
}

func writeSteps(t *testing.T, conv string, steps ...string) string {
	t.Helper()
	f := filepath.Join(conv, ".system_generated", "logs", "transcript_full.jsonl")
	os.MkdirAll(filepath.Dir(f), 0o755)
	if err := os.WriteFile(f, []byte(strings.Join(steps, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// A large output saved inside the conversation folder is read; a path
// outside it never is. An error step after a call fails that call; one
// after no call answers nothing. A torn last line is ignored.
func TestAntigravitySavedOutputErrorsAndTornLine(t *testing.T) {
	dir := t.TempDir()
	conv := filepath.Join(dir, "c1")
	saved := filepath.Join(conv, ".system_generated", "steps", "2", "output.txt")
	os.MkdirAll(filepath.Dir(saved), 0o755)
	os.WriteFile(saved, []byte(`{"issues":[{"key":"ABC-123"}]}`), 0o600)
	outside := filepath.Join(dir, "secret.txt")
	os.WriteFile(outside, []byte("do not read"), 0o600)
	step := func(i int, typ, content string, call string) string {
		m := map[string]any{"step_index": i, "type": typ, "status": "DONE", "created_at": "2026-09-17T18:00:00Z", "content": content}
		if call != "" {
			var tc any
			json.Unmarshal([]byte(call), &tc)
			m["tool_calls"] = []any{tc}
		}
		b, _ := json.Marshal(m)
		return string(b)
	}
	mcp := `{"name":"call_mcp_tool","args":{"ServerName":"jira","ToolName":"search","Arguments":{"jql":"x"},"toolAction":"a","toolSummary":"b"}}`
	writeSteps(t, conv,
		step(0, "USER_INPUT", "<USER_REQUEST>\nfind the issue\n</USER_REQUEST>", ""),
		step(1, "PLANNER_RESPONSE", "", mcp),
		step(2, "GENERIC", "The output was large and was saved to: file://"+saved, ""),
		step(3, "PLANNER_RESPONSE", "", mcp),
		step(4, "GENERIC", "The output was large and was saved to: file://"+outside, ""),
		step(5, "PLANNER_RESPONSE", "", `{"name":"view_file","args":{"AbsolutePath":"/x"}}`),
		step(6, "ERROR_MESSAGE", "Error: permission denied", ""),
		step(7, "PLANNER_RESPONSE", "thinking", ""),
		step(8, "ERROR_MESSAGE", "Error: The stream was interrupted.", ""),
		`{"step_index":9,"type":"PLANNER_RESP`,
	)
	ss, err := Antigravity{Dir: dir}.Read(time.Time{})
	if err != nil || len(ss) != 1 {
		t.Fatalf("%d sessions, %v", len(ss), err)
	}
	c := ss[0].Calls
	if len(c) != 3 {
		t.Fatalf("%d calls", len(c))
	}
	if c[0].MCPServer != "jira" || c[0].MCPTool != "search" || c[0].Args["jql"] != "x" || len(c[0].OutIDs) == 0 {
		t.Errorf("saved output not read: %+v", c[0])
	}
	if strings.Contains(c[1].Output, "do not read") {
		t.Error("read a file outside the conversation folder")
	}
	if c[2].Outcome != trace.OutcomeFailed {
		t.Errorf("error after a call: outcome %d", c[2].Outcome)
	}
}

func TestAntigravityAbsentDir(t *testing.T) {
	ss, err := Antigravity{Dir: filepath.Join(t.TempDir(), "absent")}.Read(time.Time{})
	if err != nil || ss != nil {
		t.Fatalf("%v %v", ss, err)
	}
}

// One MCP call decodes to the same server, tool and arguments whichever
// dialect recorded it (plan §6.4).
func TestDialectsAgreeOnOneMCPCall(t *testing.T) {
	args := map[string]json.RawMessage{"issue_key": json.RawMessage(`"ABC-1"`), "fields": json.RawMessage(`["summary"]`)}
	claude := ClaudeEvents(ClaudeLine{Type: "assistant", Message: claudeMessage(t, `[{"type":"tool_use","id":"t","name":"mcp__jira__get_issue","input":{"issue_key":"ABC-1","fields":["summary"]}}]`)}, "s")
	var want trace.Call
	for _, e := range claude {
		if tc, ok := e.(ToolCall); ok {
			want = tc.Call
		}
	}
	var cur, ag trace.Call
	CursorCLITool(&cur, "CallMcpTool", map[string]json.RawMessage{"server": json.RawMessage(`"jira"`), "toolName": json.RawMessage(`"get_issue"`), "arguments": mustJSON(args)})
	Dispatcher(&ag, map[string]json.RawMessage{"ServerName": json.RawMessage(`"jira"`), "ToolName": json.RawMessage(`"get_issue"`), "Arguments": mustJSON(args)}, "ServerName", "ToolName", "Arguments")
	for name, got := range map[string]trace.Call{"cursor-cli": cur, "antigravity": ag} {
		if got.Tool != want.Tool || got.MCPServer != want.MCPServer || got.MCPTool != want.MCPTool ||
			mustJSONs(got.Args) != mustJSONs(want.Args) || mustJSONs(got.RawArgs) != mustJSONs(want.RawArgs) {
			t.Errorf("%s: %s %s/%s %v; claude-code: %s %s/%s %v", name, got.Tool, got.MCPServer, got.MCPTool, got.Args, want.Tool, want.MCPServer, want.MCPTool, want.Args)
		}
	}
}

// The checked-in fixtures carry no credential and no home directory. Each
// string is checked as the reader decodes it: scanning the escaped file
// text would read code such as `Token: token,` + an escaped newline as one
// token-shaped value.
func TestR1FixturesAreRedacted(t *testing.T) {
	var docs [][]byte
	sqls, _ := filepath.Glob("testdata/cursor-cli/*/*/store.sql")
	for _, f := range sqls {
		b, _ := os.ReadFile(f)
		for _, line := range strings.Split(string(b), "\n") {
			if _, rest, ok := strings.Cut(line, "', '{"); ok {
				docs = append(docs, []byte("{"+strings.ReplaceAll(strings.TrimSuffix(rest, "');"), "''", "'")))
			}
		}
	}
	jsonls, _ := filepath.Glob("testdata/antigravity/*/.system_generated/logs/*.jsonl")
	for _, f := range jsonls {
		b, _ := os.ReadFile(f)
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			docs = append(docs, []byte(line))
		}
	}
	if len(sqls) != 2 || len(jsonls) != 1 || len(docs) < 40 {
		t.Fatalf("%d sql, %d jsonl, %d documents", len(sqls), len(jsonls), len(docs))
	}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			if sh := redact.SecretShape(x); sh != "" {
				t.Errorf("fixture string holds a %s", sh)
			}
			if strings.Contains(x, "/Users/") {
				t.Errorf("fixture string holds a home path: %.60q", x)
			}
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	for _, d := range docs {
		var v any
		if err := json.Unmarshal(d, &v); err != nil {
			t.Fatalf("fixture document is not JSON: %v: %.80s", err, d)
		}
		walk(v)
	}
}

func claudeMessage(t *testing.T, content string) (m struct {
	ID      string          `json:"id"`
	Content json.RawMessage `json:"content"`
	Usage   *struct {
		Input       float64 `json:"input_tokens"`
		CacheCreate float64 `json:"cache_creation_input_tokens"`
		CacheRead   float64 `json:"cache_read_input_tokens"`
		Output      float64 `json:"output_tokens"`
	} `json:"usage"`
}) {
	m.Content = json.RawMessage(content)
	return m
}

func mustJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
func mustJSONs(v any) string         { return string(mustJSON(v)) }

func ids(ss []trace.Session) []string {
	var out []string
	for _, s := range ss {
		out = append(out, s.ID)
	}
	return out
}
