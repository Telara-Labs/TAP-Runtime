package history

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TENG-3163: Cursor's current toolFormerData, as read from a real
// state.vscdb on 2026-10-04. A terminal call keeps rawArgs as an empty
// string and its arguments in params; an MCP call names its server in
// params.tools[0].serverName; an MCP result is {"result": "<the MCP
// {content: [...]} object as a JSON string>"}. The reader keeps the
// command, the server and the tool's own result text, so a value the
// lookup took from the search's JSON result is an explicit binding.
func TestCursorReaderReadsTheCurrentToolFormerShapes(t *testing.T) {
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 is not installed")
	}
	const comp = "33333333-3333-3333-3333-333333333333"
	js := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	mcpResult := func(text string) string {
		return js(map[string]string{"result": js(map[string]any{"content": []any{map[string]string{"type": "text", "text": text}}})})
	}
	tf := func(name, rawArgs, params, result string) string {
		return js(map[string]any{"createdAt": "2026-09-21T10:00:00Z", "toolFormerData": map[string]any{
			"name": name, "rawArgs": rawArgs, "params": params, "status": "completed", "result": result}})
	}
	bubbles := []string{
		tf("run_terminal_command_v2", "", js(map[string]any{"command": "wc -l notes.txt"}), js(map[string]string{"output": "1 notes.txt"})),
		tf("mcp-tracker-search_issues",
			js(map[string]any{"name": "user-tracker-search_issues", "args": map[string]string{"jql": "status = open"}, "toolCallId": "c1"}),
			js(map[string]any{"tools": []any{map[string]string{"name": "search_issues", "parameters": `{"jql":"status = open"}`, "serverName": "tracker"}}}),
			mcpResult(`{"issues":[{"key":"ABC-12"},{"key":"ABC-13"}]}`)),
		tf("mcp-tracker-get_issue",
			js(map[string]any{"name": "user-tracker-get_issue", "args": map[string]string{"issue_key": "ABC-12"}, "toolCallId": "c2"}),
			js(map[string]any{"tools": []any{map[string]string{"name": "get_issue", "parameters": `{"issue_key":"ABC-12"}`, "serverName": "tracker"}}}),
			mcpResult(`{"key":"ABC-12","status":"Open"}`)),
	}
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	sql := "CREATE TABLE cursorDiskKV (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB);\n" +
		"INSERT INTO cursorDiskKV VALUES ('composerData:" + comp + "', " + q(js(map[string]any{"createdAt": 1790000000000,
		"fullConversationHeadersOnly": []any{map[string]any{"bubbleId": "b1", "type": 2}, map[string]any{"bubbleId": "b2", "type": 2}, map[string]any{"bubbleId": "b3", "type": 2}}})) + ");\n"
	for i, b := range bubbles {
		sql += "INSERT INTO cursorDiskKV VALUES ('bubbleId:" + comp + ":b" + string(rune('1'+i)) + "', " + q(b) + ");\n"
	}
	db := filepath.Join(t.TempDir(), "state.vscdb")
	if out, err := exec.Command(bin, db, sql).CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v %s", err, out)
	}
	ss, err := Cursor{DB: db}.Read(time.Time{})
	if err != nil || len(ss) != 1 || len(ss[0].Calls) != 3 {
		t.Fatalf("%v %v", ss, err)
	}
	sh, search, get := ss[0].Calls[0], ss[0].Calls[1], ss[0].Calls[2]
	if sh.Tool != "shell" || sh.Command != "wc -l notes.txt" {
		t.Errorf("terminal call %q %q: the command is in params when rawArgs is empty", sh.Tool, sh.Command)
	}
	if search.Tool != "mcp:search_issues" || search.MCPServer != "tracker" || search.MCPTool != "search_issues" || search.Args["jql"] != "status = open" {
		t.Errorf("search %+v", search)
	}
	if search.Output != `{"issues":[{"key":"ABC-12"},{"key":"ABC-13"}]}` {
		t.Errorf("search result %q", search.Output)
	}
	found := false
	for i, id := range search.OutIDs {
		if id == "ABC-12" && i < len(search.OutPaths) && search.OutPaths[i] == ".issues[0].key" {
			found = true
		}
	}
	if !found || get.MCPServer != "tracker" || get.Args["issue_key"] != "ABC-12" {
		t.Errorf("ABC-12 has no JSON path in the search result (%v %v), or the lookup lost its server/argument: %+v", search.OutIDs, search.OutPaths, get)
	}
}
