package discover

import (
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func toolsOf(s Session) []string {
	var out []string
	for _, c := range s.Calls {
		if c.Tool == "shell" {
			out = append(out, "shell:"+c.Command)
		} else {
			out = append(out, c.Tool)
		}
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestClaudeCodeReader(t *testing.T) {
	ss, err := ClaudeCode{Dir: "testdata/claude"}.Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 {
		t.Fatalf("sessions = %d, want 1", len(ss))
	}
	s := ss[0]
	want := []string{"shell:cd /x && git status --short", "mcp:telara_task_list", "Skill", "Read"}
	if got := toolsOf(s); !equal(got, want) {
		t.Fatalf("calls = %q, want %q", got, want)
	}
	if s.ID != "s1" || s.Start.IsZero() {
		t.Fatalf("id %q start %v", s.ID, s.Start)
	}
	if s.Calls[2].Args["skill"] != "minikube-ops" {
		t.Fatalf("skill arg = %q", s.Calls[2].Args["skill"])
	}
}

func TestClaudeCodeReaderSince(t *testing.T) {
	ss, err := ClaudeCode{Dir: "testdata/claude"}.Read(time.Now().Add(24 * time.Hour))
	if err != nil || len(ss) != 0 {
		t.Fatalf("sessions after a future cutoff = %d, err %v", len(ss), err)
	}
}

func TestCodexReaderAllThreeShapes(t *testing.T) {
	ss, err := Codex{Dir: "testdata/codex"}.Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 {
		t.Fatalf("sessions = %d, want 1", len(ss))
	}
	want := []string{
		"shell:git status --short",   // function_call exec_command
		"shell:go test ./...",        // local_shell_call
		"shell:git diff --stat HEAD", // exec body, tools.exec_command
		"mcp:telara_task_checkpoint", // exec body, MCP tool
		"mcp:gmail_search_emails",    // namespaced function_call: namespace + "_name"
		"mcp:js",                     // namespace without trailing __: joined with __
	}
	if got := toolsOf(ss[0]); !equal(got, want) {
		t.Fatalf("calls = %q\nwant %q", got, want)
	}
	if ss[0].ID != "c1" {
		t.Fatalf("id = %q, want session_meta id c1", ss[0].ID)
	}
	if got := ss[0].Calls[3].Args["task_id"]; got != "abc" {
		t.Fatalf("JS single-quoted arg = %q", got)
	}
}

func TestCodexMissingDirIsNotAnError(t *testing.T) {
	ss, err := Codex{Dir: filepath.Join(t.TempDir(), "absent")}.Read(time.Time{})
	if err != nil || ss != nil {
		t.Fatalf("got %v, %v", ss, err)
	}
}

func TestJSObjectFieldsTruncatedInput(t *testing.T) {
	// Real exec bodies are sometimes cut mid-literal; none of these may panic.
	// Stray closers at the top level once looped forever on real Codex input.
	for _, in := range []string{`{`, `{"cmd"`, `{"cmd":`, `{cmd: "abc`, `{a: {b: [1, `, `{"k"}`, `{...rest, cmd: "x"}`, `{)}`, `{a: )}`, `{a: 1, ]}`, `{a: ], b: 2}`} {
		done := make(chan struct{})
		go func() { _ = jsObjectFields(in); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("jsObjectFields(%q) did not return", in)
		}
	}
	if got := jsObjectFields(`{...rest, cmd: "x"}`)["cmd"]; got != "x" {
		t.Fatalf("after a spread, cmd = %q", got)
	}
}

func TestJSObjectFields(t *testing.T) {
	got := jsObjectFields(`{cmd:"a \"b\"", "workdir": '/x', n: 5, nested: {a: 1}, t:` + "`x`" + `}`)
	want := map[string]string{"cmd": `a "b"`, "workdir": "/x", "n": "5", "nested": "{a: 1}", "t": "x"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// TestCursorReader builds a real state.vscdb-shaped database with the system
// sqlite3 and reads it back. Both storage forms are covered: bubbles listed
// in fullConversationHeadersOnly, and an early conversation held inline.
func TestCursorReader(t *testing.T) {
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 is not installed; the reader reports itself unavailable in that case (TestCursorWithoutSQLite)")
	}
	db := filepath.Join(t.TempDir(), "state.vscdb")
	const comp = "11111111-1111-1111-1111-111111111111"
	const inline = "22222222-2222-2222-2222-222222222222"
	sql := `CREATE TABLE cursorDiskKV (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB);
INSERT INTO cursorDiskKV VALUES ('composerData:` + comp + `', '{"createdAt":1790000000000,"fullConversationHeadersOnly":[{"bubbleId":"b2","type":2},{"bubbleId":"b1","type":2},{"bubbleId":"b3","type":2}]}');
INSERT INTO cursorDiskKV VALUES ('bubbleId:` + comp + `:b1', '{"createdAt":"2026-09-21T10:00:02Z","toolFormerData":{"name":"mcp-telara-telara_task_list","rawArgs":"{\"query\":\"x\"}"}}');
INSERT INTO cursorDiskKV VALUES ('bubbleId:` + comp + `:b2', '{"createdAt":"2026-09-21T10:00:01Z","toolFormerData":{"name":"run_terminal_cmd","rawArgs":"{\"command\":\"git status\",\"is_background\":false}"}}');
INSERT INTO cursorDiskKV VALUES ('bubbleId:` + comp + `:b3', '{"createdAt":"2026-09-21T10:00:03Z","text":"no tool here"}');
INSERT INTO cursorDiskKV VALUES ('composerData:` + inline + `', '{"createdAt":1745000000000,"conversation":[{"bubbleId":"i1","toolFormerData":{"name":"read_file","rawArgs":"{\"target_file\":\"a.go\"}"}},{"bubbleId":"i2","text":"hi"},{"bubbleId":"i3","toolFormerData":{"name":"run_terminal_command_v2","params":"{\"command\":\"make\"}"}}]}');`
	if out, err := exec.Command(bin, db, sql).CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v %s", err, out)
	}
	ss, err := Cursor{DB: db}.Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, s := range ss {
		got[s.ID] = toolsOf(s)
	}
	if want := []string{"shell:git status", "mcp:telara_task_list"}; !equal(got[comp], want) {
		t.Errorf("header-ordered conversation = %q, want %q", got[comp], want)
	}
	if want := []string{"read_file", "shell:make"}; !equal(got[inline], want) {
		t.Errorf("inline conversation = %q, want %q", got[inline], want)
	}
	if fi, _ := os.Stat(db + "-journal"); fi != nil {
		t.Error("reader left a journal: the store must be opened read-only")
	}
}

func TestCursorWithoutSQLite(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.vscdb")
	if err := os.WriteFile(db, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Cursor{DB: db, SQLite3: filepath.Join(t.TempDir(), "no-such-sqlite3")}.Read(time.Time{})
	if err == nil {
		t.Fatal("want an error when sqlite3 cannot run")
	}
}

func TestCursorAbsentStore(t *testing.T) {
	ss, err := Cursor{DB: filepath.Join(t.TempDir(), "absent.vscdb")}.Read(time.Time{})
	if err != nil || ss != nil {
		t.Fatalf("got %v, %v", ss, err)
	}
}

func TestTokenUsageIsAttributed(t *testing.T) {
	// Claude: one response written as two lines with the same id and usage
	// must count once, split over its two tool calls.
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "p"), 0o755)
	body := `{"type":"assistant","timestamp":"2026-09-20T10:00:00Z","message":{"id":"m1","usage":{"input_tokens":10,"cache_creation_input_tokens":90,"cache_read_input_tokens":1000,"output_tokens":40},"content":[{"type":"tool_use","name":"Bash","input":{"command":"git status"}}]}}
{"type":"assistant","timestamp":"2026-09-20T10:00:00Z","message":{"id":"m1","usage":{"input_tokens":10,"cache_creation_input_tokens":90,"cache_read_input_tokens":1000,"output_tokens":40},"content":[{"type":"tool_use","name":"Bash","input":{"command":"git diff"}}]}}
`
	os.WriteFile(filepath.Join(dir, "p", "s.jsonl"), []byte(body), 0o600)
	ss, err := ClaudeCode{Dir: dir}.Read(time.Time{})
	if err != nil || len(ss) != 1 || len(ss[0].Calls) != 2 {
		t.Fatalf("read: %v %+v", err, ss)
	}
	for _, c := range ss[0].Calls {
		if !c.Measured || c.Tokens != (Usage{Fresh: 50, Cached: 500, Output: 20}) {
			t.Fatalf("call tokens = %+v measured %v", c.Tokens, c.Measured)
		}
	}
}

func TestCodexTokenCountAttributesToPrecedingCalls(t *testing.T) {
	dir := t.TempDir()
	body := `{"timestamp":"2026-09-27T10:00:00Z","type":"session_meta","payload":{"id":"c"}}
{"timestamp":"2026-09-27T10:00:01Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"git status\"}"}}
{"timestamp":"2026-09-27T10:00:02Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":1000,"cached_input_tokens":800,"output_tokens":30}}}}
{"timestamp":"2026-09-27T10:00:03Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"git diff\"}"}}
`
	os.WriteFile(filepath.Join(dir, "r.jsonl"), []byte(body), 0o600)
	ss, err := Codex{Dir: dir}.Read(time.Time{})
	if err != nil || len(ss) != 1 {
		t.Fatalf("read: %v", err)
	}
	c := ss[0].Calls
	if !c[0].Measured || c[0].Tokens != (Usage{Fresh: 200, Cached: 800, Output: 30}) {
		t.Fatalf("first call = %+v", c[0])
	}
	if c[1].Measured {
		t.Fatal("a call with no following token_count must stay unmeasured")
	}
}

func TestRunCostSavesAllButOneTurn(t *testing.T) {
	st := func(turn int, total float64) Step {
		return Step{Label: "sh:git status", Tokens: Usage{Cached: total}, Turn: turn, Measured: true, Turns: 1}
	}
	// An edit decided per run is not replayed, so it saves nothing.
	edit := Step{Label: "patch:update", Tokens: Usage{Cached: 500}, Turn: 9, Measured: true, Turns: 1}
	if run, saved, ok := runCost([]Step{st(1, 100), edit, st(2, 100)}); !ok || run.Total() != 200 || math.Abs(saved.Total()-100) > 1e-9 {
		t.Fatalf("with an edit: run %v saved %v ok %v", run, saved, ok)
	}
	if _, _, ok := runCost([]Step{edit}); ok {
		t.Fatal("a run with nothing replayable has no saving")
	}
	run, saved, ok := runCost([]Step{st(1, 100), st(2, 100), st(3, 100)})
	if !ok || run.Total() != 300 || math.Abs(saved.Total()-200) > 1e-9 {
		t.Fatalf("run %v saved %v", run, saved)
	}
	if _, _, ok := runCost([]Step{st(1, 1), {}}); ok {
		t.Fatal("an unmeasured step must make the run unmeasured")
	}
}

func TestOutcomesAreRead(t *testing.T) {
	ss, err := ClaudeCode{Dir: "testdata/claude"}.Read(time.Time{})
	if err != nil || ss[0].Calls[0].Outcome != OutcomeOK {
		t.Fatalf("claude: the tool_result for the git call must mark it OK: %+v %v", ss[0].Calls[0], err)
	}
	cs, err := Codex{Dir: "testdata/codex"}.Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	first := cs[0].Calls[0]
	if first.Outcome != OutcomeFailed || len(first.OutIDs) != 1 || first.OutIDs[0] != "TENG-1234" {
		t.Fatalf("codex: exit code 1 must mark the call failed and keep its ids: %+v", first)
	}
	if cs[0].Calls[1].Outcome != OutcomeUnknown {
		t.Fatalf("codex: a call with no output is unknown: %+v", cs[0].Calls[1])
	}
	if c := cursorCall(cursorRow{Name: "run_terminal_cmd", Args: `{"command":"make"}`, Status: "error", Result: `{"output":"see https://ci.example.com/j/42"}`}); c.Outcome != OutcomeFailed || c.OutIDs[0] != "https://ci.example.com/j/42" {
		t.Fatalf("cursor: %+v", c)
	}
}

func TestCodexToolNameShapes(t *testing.T) {
	for _, c := range []struct{ ns, name, want string }{
		{"mcp__node_repl__", "js", "mcp:js"},
		{"mcp__node_repl", "js", "mcp:js"},
		{"mcp__telara", "_telara_task_list", "mcp:telara_task_list"},
		{"mcp__codex_apps__gmail", "_search_emails", "mcp:gmail_search_emails"},
		{"", "mcp__telara__telara_task_create", "mcp:telara_task_create"},
		{"", "mcp__codex_apps__telara_telara_task_list", "mcp:telara_task_list"},
		{"", "mcp__codex_apps__gmail_search_emails", "mcp:gmail_search_emails"},
	} {
		if got := codexCall(Session{}, time.Time{}, c.ns, c.name, nil).Tool; got != c.want {
			t.Errorf("%q + %q = %q, want %q", c.ns, c.name, got, c.want)
		}
	}
}
