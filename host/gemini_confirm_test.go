package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// geminiTree makes this process look started by Gemini CLI (pid 1000)
// through a wrapper, with the given command line and terminal.
func geminiTree(t *testing.T, gemini []string, terminal bool) {
	t.Helper()
	oldArgs, oldTTY := processArgs, processHasTerminal
	t.Cleanup(func() { processArgs, processHasTerminal = oldArgs, oldTTY })
	processArgs = func(pid int) ([]string, int, bool) {
		if pid == 1000 {
			return gemini, 1, true
		}
		return []string{"/bin/sh", "-c", "/opt/tap hook gemini"}, 1000, true
	}
	processHasTerminal = func(pid int) bool { return pid == 1000 && terminal }
}

func geminiRelayDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := relayDir
	relayDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { relayDir = old })
	return dir
}

func asGemini(t *testing.T) {
	t.Helper()
	old := testClientName
	testClientName = "gemini-cli-mcp-client"
	t.Cleanup(func() { testClientName = old })
}

// beforeTool runs the hook as Gemini would before a call to tool.
func beforeTool(t *testing.T, dir, tool string, input map[string]any, serveArgs ...string) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{
		"hook_event_name": "BeforeTool", "cwd": t.TempDir(), "tool_name": "mcp_tap_" + tool, "tool_input": input,
		"mcp_context": map[string]any{"server_name": "tap", "tool_name": tool, "args": append([]string{"serve", "--name", "tap"}, serveArgs...)},
	})
	out, err := geminiBeforeTool(raw, dir)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Gemini CLI 0.63 adds wait_for_previous to every tool and sends it with the
// call. The runner read arguments strictly and refused tap_save and tap_run.
func TestGeminiSchedulingArgumentIsIgnored(t *testing.T) {
	var a struct {
		Package string `json:"package"`
	}
	if err := readToolArgs(json.RawMessage(`{"package":"/p","wait_for_previous":true}`), &a); err != nil || a.Package != "/p" {
		t.Fatalf("wait_for_previous refused: %v", err)
	}
	if err := readToolArgs(json.RawMessage(`{"package":"/p","other":true}`), &a); err == nil {
		t.Fatal("an unknown argument was accepted")
	}
	if err := readToolArgs(json.RawMessage(`{"package":"/p","wait_for_previous":"x"}`), &a); err == nil {
		t.Fatal("a wait_for_previous that is not Gemini's was accepted")
	}
}

// In an interactive Gemini CLI the hook makes Gemini ask before tap_save,
// with TAP's question, and the save goes ahead once the call arrives. In
// gemini -p nobody can answer: the hook asks nothing (a forced ask waits
// forever there) and the save is refused as before.
func TestGeminiAsksBeforeTapSave(t *testing.T) {
	saveHome(t)
	asGemini(t)
	dir := geminiRelayDir(t)
	c := startServer(t, false, nil)
	draft := authoredDraft(t)
	input := map[string]any{"package": draft, "wait_for_previous": true}

	geminiTree(t, []string{"node", "/usr/lib/node_modules/@google/gemini-cli/bundle/gemini.js", "-p", "save it"}, false)
	if out := beforeTool(t, dir, "tap_save", input); out["decision"] != nil {
		t.Fatalf("headless Gemini was made to ask: %v", out)
	}
	res := c.call("tools/call", map[string]any{"name": "tap_save", "arguments": input})
	if res["isError"] != true || !strings.Contains(toolText(t, res), "non-interactive") {
		t.Fatalf("headless save = %#v", res)
	}

	geminiTree(t, []string{"node", "/usr/bin/gemini"}, true)
	out := beforeTool(t, dir, "tap_save", input)
	msg, _ := out["systemMessage"].(string)
	if out["decision"] != "ask" || !strings.Contains(msg, "local.me/release-check@0.1.0") {
		t.Fatalf("interactive Gemini was not made to ask: %v", out)
	}
	res = c.call("tools/call", map[string]any{"name": "tap_save", "arguments": input})
	if res["isError"] == true || !strings.Contains(toolText(t, res), "saved") {
		t.Fatalf("confirmed save = %#v", res)
	}
	// The record is used once.
	res = c.call("tools/call", map[string]any{"name": "tap_save", "arguments": input})
	if res["isError"] != true {
		t.Fatalf("a second save without a confirmation = %#v", res)
	}
}

// A record only speaks for the Gemini process that wrote it, recently, and
// once; a record saying nobody was asked replaces one planted earlier.
func TestGeminiConfirmRecordIsBoundToItsCall(t *testing.T) {
	dir := t.TempDir()
	args := map[string]any{"ref": "a/b@1", "digest": "d"}
	writeGeminiConfirm(dir, "tap_run", args, 7, true)
	if ok, _ := takeGeminiConfirm(dir, "tap_run", args, 8); ok {
		t.Fatal("another Gemini's confirmation was taken")
	}
	writeGeminiConfirm(dir, "tap_run", args, 7, true)
	if ok, _ := takeGeminiConfirm(dir, "tap_run", map[string]any{"ref": "a/b@1", "digest": "e"}, 7); ok {
		t.Fatal("a confirmation for other arguments was taken")
	}
	if ok, why := takeGeminiConfirm(dir, "tap_run", args, 7); !ok {
		t.Fatalf("the confirmation was not taken: %s", why)
	}
	if ok, _ := takeGeminiConfirm(dir, "tap_run", args, 7); ok {
		t.Fatal("a confirmation was taken twice")
	}
	writeGeminiConfirm(dir, "tap_run", args, 7, true)
	writeGeminiConfirm(dir, "tap_run", args, 7, false)
	if ok, _ := takeGeminiConfirm(dir, "tap_run", args, 7); ok {
		t.Fatal("a planted record outlived a call nobody was asked about")
	}
	old, _ := json.Marshal(geminiConfirmRecord{Gemini: 7, Asked: true, At: time.Now().Add(-2 * geminiConfirmTTL)})
	os.WriteFile(filepath.Join(dir, geminiConfirmKey("tap_run", args)), old, 0o600)
	if ok, _ := takeGeminiConfirm(dir, "tap_run", args, 7); ok {
		t.Fatal("a stale confirmation was taken")
	}
}

// The first run of a primitive that reads the web: Gemini asks, with its
// declarations, and once allowed it runs, its reads included, and a later
// run of the same version is not asked again.
func TestGeminiAsksBeforeAFirstRun(t *testing.T) {
	inDir(t)
	saveHome(t)
	asGemini(t)
	dir := geminiRelayDir(t)
	c := startServer(t, false, nil)
	var seen atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen.Add(1); fmt.Fprint(w, "read-ok") }))
	defer srv.Close()
	pkg := writePackage(t, "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.test, name: reads, version: 1.0.0}\nexecution: {entrypoint: main.sh}\nfetch:\n  - {origin: "+srv.URL+"}\n", "tap fetch "+srv.URL+"/probe\n")
	identity := c.stagePackage(pkg)
	geminiTree(t, []string{"node", "/usr/bin/gemini", "--approval-mode", "default"}, true)

	res := c.call("tools/call", map[string]any{"name": "tap_run", "arguments": identity})
	if res["isError"] != true || seen.Load() != 0 {
		t.Fatalf("ran without anyone asked: %#v", res)
	}

	out := beforeTool(t, dir, "tap_run", identity, "--catalog-root", c.catalogRoot)
	msg, _ := out["systemMessage"].(string)
	if out["decision"] != "ask" || !strings.Contains(msg, "web: "+srv.URL) {
		t.Fatalf("Gemini was not made to ask with the declarations: %v", out)
	}
	res = c.call("tools/call", map[string]any{"name": "tap_run", "arguments": identity})
	if text := toolText(t, res); res["isError"] == true || !strings.Contains(text, "read-ok") || seen.Load() != 1 {
		t.Fatalf("allowed run = %s", text)
	}

	// Trusted now: the hook does not ask again, and the run reads.
	if out := beforeTool(t, dir, "tap_run", identity, "--catalog-root", c.catalogRoot); out["decision"] != nil {
		t.Fatalf("asked again for a trusted version: %v", out)
	}
	res = c.call("tools/call", map[string]any{"name": "tap_run", "arguments": identity})
	if text := toolText(t, res); res["isError"] == true || !strings.Contains(text, "read-ok") || seen.Load() != 2 {
		t.Fatalf("second run = %s", text)
	}
}

// Other tools, and other servers' tools of the same name, pass untouched.
func TestGeminiBeforeToolLeavesOtherCallsAlone(t *testing.T) {
	dir := t.TempDir()
	geminiTree(t, []string{"node", "/usr/bin/gemini"}, true)
	if out := beforeTool(t, dir, "tap_search", map[string]any{"query": "x"}); len(out) != 0 {
		t.Fatalf("tap_search = %v", out)
	}
	raw, _ := json.Marshal(map[string]any{"hook_event_name": "BeforeTool", "tool_name": "mcp_other_tap_save",
		"tool_input": map[string]any{}, "mcp_context": map[string]any{"server_name": "other", "tool_name": "tap_save", "args": []string{"run"}}})
	if out, _ := geminiBeforeTool(raw, dir); len(out) != 0 {
		t.Fatalf("another server's tool = %v", out)
	}
}

// Gemini CLI is found by its program, not by the word in an argument: this
// hook runs as "tap hook gemini".
func TestGeminiProgramIsFoundByItsProgram(t *testing.T) {
	for _, c := range []struct {
		args []string
		want bool
	}{
		{[]string{"node", "/usr/bin/gemini"}, true},
		{[]string{"/usr/bin/node", "--max-old-space-size=8192", "/usr/lib/node_modules/@google/gemini-cli/bundle/gemini.js"}, true},
		{[]string{"/opt/tap", "hook", "gemini"}, false},
		{[]string{"/bin/sh", "-c", "/opt/tap hook gemini"}, false},
		{[]string{"gemini", "-p", "x"}, true},
	} {
		if got := isGeminiProgram(c.args); got != c.want {
			t.Errorf("%v: %v, want %v", c.args, got, c.want)
		}
	}
	old := processHasTerminal
	t.Cleanup(func() { processHasTerminal = old })
	processHasTerminal = func(int) bool { return true }
	for _, c := range []struct {
		args []string
		want bool
	}{
		{[]string{"node", "/usr/bin/gemini"}, true},
		{[]string{"node", "/usr/bin/gemini", "-p", "x"}, false},
		{[]string{"node", "/usr/bin/gemini", "--prompt=x"}, false},
		{[]string{"node", "/usr/bin/gemini", "-i", "x"}, true},
	} {
		if got := geminiInteractive(1, c.args); got != c.want {
			t.Errorf("interactive %v: %v, want %v", c.args, got, c.want)
		}
	}
}

// tap install writes both hooks, once each, and remove takes both out.
func TestAddGeminiWritesBothHooks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	for i := 0; i < 2; i++ {
		if err := addGemini(path, "tap", "/opt/tap", nil); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := os.ReadFile(path)
	var s struct {
		Hooks map[string][]struct {
			Matcher string           `json:"matcher"`
			Hooks   []map[string]any `json:"hooks"`
		} `json:"hooks"`
	}
	json.Unmarshal(raw, &s)
	if len(s.Hooks["AfterTool"]) != 1 || len(s.Hooks["BeforeTool"]) != 1 || s.Hooks["BeforeTool"][0].Matcher != "tap_(save|run)$" {
		t.Fatalf("hooks:\n%s", raw)
	}
	if changed, err := removeGemini(path, "tap"); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if raw, _ := os.ReadFile(path); strings.Contains(string(raw), "hook gemini") {
		t.Fatalf("a hook was left:\n%s", raw)
	}
}
