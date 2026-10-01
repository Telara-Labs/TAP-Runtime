package discover

import (
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestCodexReaderDoesNotAttributeSharedExecOutputToEveryNestedCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared-output.jsonl")
	rows := []map[string]any{
		{"timestamp": "2026-09-15T20:00:00Z", "type": "session_meta", "payload": map[string]any{"id": "shared-output"}},
		{"timestamp": "2026-09-15T20:00:01Z", "type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Inspect two issues."}}}},
		{"timestamp": "2026-09-15T20:00:02Z", "type": "response_item", "payload": map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "shared", "input": `const results = await Promise.allSettled([tools.mcp__telara__telara_jira_get_issue({issue_key:"TENG-1"}), tools.mcp__telara__telara_jira_get_issue({issue_key:"TENG-2"})]); text(results);`}},
		{"timestamp": "2026-09-15T20:00:03Z", "type": "response_item", "payload": map[string]any{"type": "custom_tool_call_output", "call_id": "shared", "output": []any{map[string]any{"type": "text", "text": "Script completed\nOutput:\n{\"id\":\"TENG-999\"}"}}}},
		{"timestamp": "2026-09-15T20:00:04Z", "type": "response_item", "payload": map[string]any{"type": "function_call", "name": "mcp__telara__telara_jira_get_issue", "call_id": "single", "arguments": `{"issue_key":"TENG-3"}`}},
		{"timestamp": "2026-09-15T20:00:05Z", "type": "response_item", "payload": map[string]any{"type": "function_call_output", "call_id": "single", "output": `{"id":"TENG-1000"}`}},
		{"timestamp": "2026-09-15T20:00:06Z", "type": "response_item", "payload": map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "passthrough", "input": `const result = await tools.mcp__telara__telara_jira_search_issues({query:"project = TENG"}); text(JSON.stringify(result));`}},
		{"timestamp": "2026-09-15T20:00:07Z", "type": "response_item", "payload": map[string]any{"type": "custom_tool_call_output", "call_id": "passthrough", "output": []any{map[string]any{"type": "input_text", "text": "Script completed\nWall time: 0.1 seconds"}, map[string]any{"type": "input_text", "text": `{"structuredContent":{"items":[{"id":"TENG-11"},{"id":"TENG-12"}]},"content":[{"type":"text","text":"ignore this preview"}]}`}}}},
		{"timestamp": "2026-09-15T20:00:08Z", "type": "response_item", "payload": map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "transformed", "input": `const result = await tools.mcp__telara__telara_jira_get_issue({issue_key:"TENG-4"}); text(JSON.stringify(result).slice(0,100));`}},
		{"timestamp": "2026-09-15T20:00:09Z", "type": "response_item", "payload": map[string]any{"type": "custom_tool_call_output", "call_id": "transformed", "output": []any{map[string]any{"type": "text", "text": "Script completed\nOutput:\n" + `{"id":"TENG-2000"}`}}}},
	}
	var body []byte
	for _, row := range rows {
		line, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		body = append(append(body, line...), '\n')
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := readCodexFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Calls) != 5 {
		t.Fatalf("calls = %d, want five", len(s.Calls))
	}
	for i := 0; i < 2; i++ {
		if s.Calls[i].Outcome != OutcomeUnknown || len(s.Calls[i].OutIDs) != 0 || len(s.Calls[i].OutPaths) != 0 || s.Calls[i].Output != "" {
			t.Fatalf("nested call %d received an unattributed shared result: %+v", i, s.Calls[i])
		}
	}
	if s.Calls[2].Outcome != OutcomeOK || len(s.Calls[2].OutIDs) != 1 || s.Calls[2].OutIDs[0] != "TENG-1000" || s.Calls[2].OutPaths[0] != ".id" {
		t.Fatalf("direct call lost its own result: %+v", s.Calls[2])
	}
	if s.Calls[3].Outcome != OutcomeOK || len(s.Calls[3].OutCollections) != 1 || len(s.Calls[3].OutIDs) != 2 || s.Calls[3].OutPaths[0] != ".items[0].id" {
		t.Fatalf("single passed-through result was not decoded: %+v", s.Calls[3])
	}
	if s.Calls[4].Outcome != OutcomeUnknown || len(s.Calls[4].OutIDs) != 0 || s.Calls[4].Output != "" {
		t.Fatalf("transformed result was treated as raw: %+v", s.Calls[4])
	}
}

func TestCodexExecResultTextMatchesBridgeContentFallback(t *testing.T) {
	blocks := []map[string]string{
		{"type": "input_text", "text": "Script completed\nWall time: 0.1 seconds"},
		{"type": "input_text", "text": `{"isError":true,"content":[{"type":"text","text":"{\"id\":\"TENG-42\"}"}]}`},
	}
	raw, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	text, failed, ok := codexExecResultText(raw)
	if !ok || !failed || text != `{"id":"TENG-42"}` {
		t.Fatalf("content result = %q, failed=%v, ok=%v", text, failed, ok)
	}
	blocks = append(blocks, map[string]string{"type": "input_text", "text": "extra output"})
	raw, _ = json.Marshal(blocks)
	if _, _, ok := codexExecResultText(raw); ok {
		t.Fatal("multiple printed values do not prove one raw tool result")
	}
}

func TestCodexIndexedExecResultsRequireExactSourceAndCompleteIndexes(t *testing.T) {
	source := `const results = await Promise.allSettled([
  tools.mcp__test__get({id:"A"}),
  tools.mcp__test__get({id:"B"})
]); results.forEach((r,i) => text(JSON.stringify({check:i,result:r})));`
	if count, key, ok := codexIndexedSource(source); !ok || count != 2 || key != "check" {
		t.Fatalf("direct indexed source was not recognized: count=%d key=%q ok=%v", count, key, ok)
	}
	blocks := []map[string]string{
		{"type": "text", "text": "Script completed\nWall time: 0.1 seconds"},
		{"type": "text", "text": `{"check":1,"result":{"status":"fulfilled","value":{"structuredContent":{"id":"B"},"content":[{"type":"text","text":"wrong preview"}]}}}`},
		{"type": "text", "text": `{"check":0,"result":{"status":"fulfilled","value":{"structuredContent":{"id":"A"}}}}`},
	}
	raw, _ := json.Marshal(blocks)
	results, ok := codexExecIndexedResults(source, raw, 2)
	if !ok || len(results) != 2 || results[0].text != `{"id":"A"}` || results[1].text != `{"id":"B"}` {
		t.Fatalf("reordered display lost array-index provenance: %+v %v", results, ok)
	}
	blocks[2]["text"] = blocks[1]["text"] // duplicate index and missing index zero
	raw, _ = json.Marshal(blocks)
	if _, ok := codexExecIndexedResults(source, raw, 2); ok {
		t.Fatal("duplicate result index was attributed")
	}
	blocks = blocks[:2]
	raw, _ = json.Marshal(blocks)
	if _, ok := codexExecIndexedResults(source, raw, 2); ok {
		t.Fatal("missing result block was attributed")
	}
	for _, changed := range []string{
		strings.Replace(source, "result:r", "result:r.value", 1),
		strings.Replace(source, "tools.mcp__test__get({id:\"B\"})", "tools.mcp__test__get({id:\"B\"}).content", 1),
		strings.Replace(source, "Promise.allSettled", "Promise.all", 1),
	} {
		if _, _, ok := codexIndexedSource(changed); ok {
			t.Fatalf("transformed or incompatible source was attributed: %q", changed)
		}
	}
}

func TestCodexReaderAttributesIndexedMultiCallResultsAndFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "indexed-output.jsonl")
	source := `const results = await Promise.allSettled([tools.mcp__test__get({id:"A"}), tools.mcp__test__get({id:"B"})]); results.forEach((r,i)=>text(JSON.stringify({i,result:r})));`
	rows := []map[string]any{
		{"timestamp": "2026-09-15T20:00:00Z", "type": "session_meta", "payload": map[string]any{"id": "indexed-output"}},
		{"timestamp": "2026-09-15T20:00:01Z", "type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Inspect two records."}}}},
		{"timestamp": "2026-09-15T20:00:02Z", "type": "response_item", "payload": map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "indexed", "input": source}},
		{"timestamp": "2026-09-15T20:00:03Z", "type": "response_item", "payload": map[string]any{"type": "custom_tool_call_output", "call_id": "indexed", "output": []any{
			map[string]any{"type": "text", "text": "Script completed\nWall time: 0.1 seconds"},
			map[string]any{"type": "text", "text": `{"i":1,"result":{"status":"rejected","reason":"failed"}}`},
			map[string]any{"type": "text", "text": `{"i":0,"result":{"status":"fulfilled","value":{"structuredContent":{"id":"TENG-1001"}}}}`},
		}}},
	}
	var body []byte
	for _, row := range rows {
		line, _ := json.Marshal(row)
		body = append(append(body, line...), '\n')
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := readCodexFile(path)
	if err != nil || len(s.Calls) != 2 {
		t.Fatalf("indexed fixture: %+v %v", s, err)
	}
	if s.Calls[0].Outcome != OutcomeOK || len(s.Calls[0].OutIDs) != 1 || s.Calls[0].OutIDs[0] != "TENG-1001" || s.Calls[1].Outcome != OutcomeFailed || len(s.Calls[1].OutIDs) != 0 {
		t.Fatalf("indexed calls were not attributed safely: %+v", s.Calls)
	}
}

func TestCodexIndexedFulfilledValueRequiresToolEnvelope(t *testing.T) {
	source := `const rs = await Promise.allSettled([tools.mcp__test__get({id:"A"}),tools.mcp__test__get({id:"B"})]); rs.forEach((x,i)=>text(JSON.stringify({i,result:x.status==="fulfilled"?x.value:x.reason})));`
	if count, key, ok := codexIndexedValueSource(source); !ok || count != 2 || key != "i" {
		t.Fatalf("unmodified fulfilled-value source was not recognized: %d %q %v", count, key, ok)
	}
	blocks := []map[string]string{
		{"type": "text", "text": "Script completed\nWall time: 0.1 seconds"},
		{"type": "text", "text": `{"i":1,"result":{"output":"TENG-1002","exit_code":1}}`},
		{"type": "text", "text": `{"i":0,"result":{"structuredContent":{"id":"TENG-1001"}}}`},
	}
	raw, _ := json.Marshal(blocks)
	results, ok := codexExecIndexedResults(source, raw, 2)
	if !ok || results[0].text != `{"id":"TENG-1001"}` || results[1].text != "TENG-1002" || !results[1].failed {
		t.Fatalf("fulfilled values lost index or tool envelope: %+v %v", results, ok)
	}
	blocks[1]["text"] = `{"i":1,"result":"rejected"}`
	raw, _ = json.Marshal(blocks)
	if _, ok := codexExecIndexedResults(source, raw, 2); ok {
		t.Fatal("rejection text was treated as a tool result")
	}
	if _, _, ok := codexIndexedValueSource(strings.Replace(source, "x.value", "x.value.output", 1)); ok {
		t.Fatal("transformed value source was attributed")
	}
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
	if s.Calls[1].MCPServer != "telara" || s.Calls[1].MCPTool != "telara_task_list" {
		t.Fatalf("Claude MCP inventory identity lost: %+v", s.Calls[1])
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
	if ss[0].Calls[3].MCPServer != "telara" || ss[0].Calls[3].MCPTool != "telara_task_checkpoint" {
		t.Fatalf("Codex MCP inventory identity lost: %+v", ss[0].Calls[3])
	}
}

func TestCodexReaderUsesScheduledPromptBeforeInjectedInstructions(t *testing.T) {
	dir := t.TempDir()
	body := `{"timestamp":"2026-09-15T20:00:00Z","type":"session_meta","payload":{"id":"scheduled"}}
{"timestamp":"2026-09-15T20:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions for /repo\\nFollow the project rules."}]}}
{"timestamp":"2026-09-15T20:00:02Z","type":"response_item","payload":{"type":"function_call_output","name":"automation_update","namespace":"codex_app","output":"Automation: pipeline monitor\nAutomation ID: pipeline-monitor\nAutomation memory: local\n\nList recent pipelines, take the latest failed one, and report its failed jobs."}}
{"timestamp":"2026-09-15T20:00:03Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"git status --short\"}"}}
`
	path := filepath.Join(dir, "scheduled.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := readCodexFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Requests) != 1 || s.Requests[0] != "List recent pipelines, take the latest failed one, and report its failed jobs." {
		t.Fatalf("requests = %q", s.Requests)
	}
	if len(s.Calls) != 1 || s.Calls[0].Request != 0 {
		t.Fatalf("calls = %+v", s.Calls)
	}
}

func TestCodexReaderRecoversScheduledPromptAfterPluginEnvelope(t *testing.T) {
	dir := t.TempDir()
	body := `{"timestamp":"2026-09-15T20:00:00Z","type":"session_meta","payload":{"id":"scheduled-envelope","thread_source":"automation"}}
{"timestamp":"2026-09-15T20:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<recommended_plugins>\\nAvailable plugins\\n</recommended_plugins>\\n# AGENTS.md instructions for /repo\\nFollow the project rules.\\n<environment_context>\\n  <cwd>/repo</cwd>\\n</environment_context>"}]}}
{"timestamp":"2026-09-15T20:00:02Z","type":"response_item","payload":{"type":"function_call_output","name":"automation_update","namespace":"codex_app","output":"Automation: pipeline monitor\nAutomation ID: pipeline-monitor\nAutomation memory: local\n\nList recent pipelines, take the latest failed one, and report its failed jobs."}}
{"timestamp":"2026-09-15T20:00:03Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"git status --short\"}"}}
`
	path := filepath.Join(dir, "scheduled-envelope.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := readCodexFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Requests) != 1 || s.Requests[0] != "List recent pipelines, take the latest failed one, and report its failed jobs." ||
		len(s.RequestRoles) != 1 || s.RequestRoles[0] != "scheduled" || len(s.Calls) != 1 || s.Calls[0].Request != 0 {
		t.Fatalf("scheduled prompt or role was lost: requests=%q roles=%q calls=%+v", s.Requests, s.RequestRoles, s.Calls)
	}
	if codexInjectedAutomationContext("<recommended_plugins>\n</recommended_plugins>\n# AGENTS.md instructions for /repo\n<environment_context>\n</environment_context>\nPlease inspect X") {
		t.Fatal("a request following the context must not be classified as a wrapper")
	}
}

func TestCodexReaderDoesNotReplaceUserRequestWithAutomationRecord(t *testing.T) {
	dir := t.TempDir()
	body := `{"timestamp":"2026-09-15T20:00:00Z","type":"session_meta","payload":{"id":"manual"}}
{"timestamp":"2026-09-15T20:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"List failed jobs for project A."}]}}
{"timestamp":"2026-09-15T20:00:02Z","type":"response_item","payload":{"type":"function_call_output","name":"automation_update","namespace":"codex_app","output":"Automation: unrelated\nAutomation ID: unrelated\n\nOther task."}}
{"timestamp":"2026-09-15T20:00:03Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"git status --short\"}"}}
`
	path := filepath.Join(dir, "manual.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := readCodexFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Requests) != 1 || s.Requests[0] != "List failed jobs for project A." {
		t.Fatalf("requests = %q", s.Requests)
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

func TestCursorReaderKeepsCompleteCollectionEvidencePastPreview(t *testing.T) {
	full := `{"padding":"` + strings.Repeat("x", 700) + `","items":[{"id":"TENG-1"},{"id":"TENG-2"}]}`
	call := cursorCall(cursorRow{Name: "mcp-records-list", Status: "completed", Result: full})
	if len(call.Output) != 600 || len(call.OutCollections) != 1 {
		t.Fatalf("short preview must retain one complete collection summary: output=%d collections=%+v", len(call.Output), call.OutCollections)
	}
	collection := call.OutCollections[0]
	field := collection.Fields[".id"]
	if collection.Path != ".items" || collection.Count != 2 || field.Type != "string" || len(field.Digests) != 2 || field.Digests[0] != resultValueDigest("TENG-1") {
		t.Fatalf("wrong complete-list evidence: %+v", collection)
	}
	if strings.Contains(field.Digests[0], "TENG") || len(resultCollections(full[:600])) != 0 {
		t.Fatal("raw values or incomplete JSON must not become collection proof")
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
