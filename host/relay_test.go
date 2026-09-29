package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGeminiToolName(t *testing.T) {
	for _, c := range [][3]string{
		{"tap", "tap_run", "mcp_tap_tap_run"},
		{"google-workspace", "gmail.search", "mcp_google-workspace_gmail.search"},
		{"claude.ai Gmail", "search threads", "mcp_claude.ai_Gmail_search_threads"},
		{"mcp_x", "y", "mcp_x_y"},
	} {
		if got := geminiToolName(c[0], c[1]); got != c[2] {
			t.Errorf("%s / %s: got %q, want %q", c[0], c[1], got, c[2])
		}
	}
	long := geminiToolName(strings.Repeat("s", 40), strings.Repeat("t", 40))
	if len(long) != 63 || !strings.Contains(long, "...") {
		t.Errorf("a long name is not shortened the way Gemini shortens it: %q", long)
	}
}

func TestPendingTextRoundTrips(t *testing.T) {
	call := relayCall{Name: "mcp_mail_search", Args: map[string]any{"q": "in:inbox"}}
	run, got, ok := relayPending(pendingText("relay-1", call))
	if !ok || run != "relay-1" || got.Name != call.Name || got.Args["q"] != "in:inbox" {
		t.Fatalf("%v %v %v", run, got, ok)
	}
	if _, _, ok := relayPending("an ordinary answer"); ok {
		t.Error("an ordinary answer was read as a pending call")
	}
}

// Install writes Gemini's MCP entry and hook, keeps everything else, and
// leaves one of each when run twice.
func TestAddGeminiKeepsSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(path, []byte(`{"mcpServers":{"telara":{"httpUrl":"https://example.com/mcp"}},"hooks":{"AfterTool":[{"matcher":"write_file","hooks":[{"name":"lint","type":"command","command":"lint.sh"}]}]},"theme":"dark"}`), 0o600)
	for i := 0; i < 2; i++ {
		if err := addGemini(path, "tap", "/opt/tap dir/tap"); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := os.ReadFile(path)
	var s struct {
		MCP   map[string]map[string]any `json:"mcpServers"`
		Hooks map[string][]struct {
			Matcher string           `json:"matcher"`
			Hooks   []map[string]any `json:"hooks"`
		} `json:"hooks"`
		Theme string `json:"theme"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if s.Theme != "dark" || s.MCP["telara"] == nil {
		t.Errorf("settings that were there are gone:\n%s", raw)
	}
	if s.MCP["tap"]["command"] != "/opt/tap dir/tap" {
		t.Errorf("the MCP entry is wrong: %v", s.MCP["tap"])
	}
	var taps, lint int
	for _, g := range s.Hooks["AfterTool"] {
		for _, h := range g.Hooks {
			switch h["name"] {
			case "tap":
				taps++
				if h["command"] != `'/opt/tap dir/tap' hook gemini` {
					t.Errorf("the hook command is not quoted: %v", h["command"])
				}
			case "lint":
				lint++
			}
		}
	}
	if taps != 1 || lint != 1 {
		t.Errorf("want one tap hook and the user's own hook kept, got %d and %d:\n%s", taps, lint, raw)
	}
	if _, err := os.Stat(path + ".tap-backup"); err != nil {
		t.Error("the file was rewritten without a copy of what it held")
	}
	os.WriteFile(path, []byte("{ // a comment\n}"), 0o600)
	if err := addGemini(path, "tap", "/x"); err == nil || !strings.Contains(err.Error(), "by hand") {
		t.Errorf("a file with comments was rewritten: %v", err)
	}
}

// The whole chain, with the test playing Gemini: the model calls tap_run
// once; the hook turns each call the program makes into a tail call; Gemini
// "makes" it; the hook carries the result back; the chain ends with
// tap_result, and only the program's output comes out of it.
func TestRelayChainThroughTheHook(t *testing.T) {
	dir := t.TempDir()
	old := relayDir
	relayDir = func() (string, error) { return dir, nil }
	defer func() { relayDir = old }()

	store := interpreterStore(t)
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	c := &client{t: t, in: cw, sc: bufio.NewScanner(cr), done: make(chan error, 1)}
	c.sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	go func() {
		c.done <- serve(sr, sw, []string{"--interpreters", store, "--runs", t.TempDir(), "--name", "tap"})
		sw.Close()
	}()
	t.Cleanup(func() { cw.Close(); <-c.done })
	c.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "gemini-cli-mcp-client", "version": "0.99.0"}})

	list := c.call("tools/list", map[string]any{})
	names := ""
	for _, tl := range list["tools"].([]any) {
		names += tl.(map[string]any)["name"].(string) + " "
	}
	if !strings.Contains(names, "tap_result") {
		t.Fatalf("Gemini is not offered tap_result: %s", names)
	}

	pkg := writePackage(t, `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.test, name: two-reads, version: 0.1.0}
execution: {entrypoint: main.sh}
tools:
  - {alias: search, capability: mail.messages.search, effect: read, pin: {server: mail, tool: search}}
`, `a=$(tap call search '{"q":"one"}' | jq -r '.n')
b=$(tap call search '{"q":"two"}' | jq -r '.n')
echo "total $a $b"
`)
	first := c.run(pkg)
	if !strings.HasPrefix(first, relayPrefix) {
		t.Fatalf("tap_run did not stop at the first call:\n%s", first)
	}

	hook := func(tool string, input map[string]any, response string) map[string]any {
		t.Helper()
		in, _ := json.Marshal(map[string]any{"hook_event_name": "AfterTool", "tool_name": tool, "tool_input": input,
			"tool_response": map[string]any{"llmContent": []any{map[string]any{"text": response}}}})
		out, err := geminiAfterTool(strings.NewReader(string(in)), dir)
		if err != nil {
			t.Fatalf("hook on %s: %v", tool, err)
		}
		return out
	}
	tail := func(out map[string]any) (string, map[string]any) {
		t.Helper()
		h, _ := out["hookSpecificOutput"].(map[string]any)
		r, _ := h["tailToolCallRequest"].(map[string]any)
		if r == nil {
			t.Fatalf("the hook asked for no tail call: %v", out)
		}
		args, _ := r["args"].(map[string]any)
		return r["name"].(string), args
	}

	// Gemini runs the hook on tap_run's answer: the first call.
	name, args := tail(hook("mcp_tap_tap_run", map[string]any{"package": pkg}, first))
	if name != "mcp_mail_search" || args["q"] != "one" {
		t.Fatalf("first tail call: %s %v", name, args)
	}
	// A call the model makes on its own, with other arguments, is left alone.
	if out := hook("mcp_mail_search", map[string]any{"q": "unrelated"}, `{"n":99}`); len(out) != 0 {
		t.Fatalf("an unrelated call was taken over: %v", out)
	}
	// Gemini makes the call; the hook carries the result and gets the next.
	name, args = tail(hook(name, args, `{"n":3}`))
	if name != "mcp_mail_search" || args["q"] != "two" {
		t.Fatalf("second tail call: %s %v", name, args)
	}
	// The last result: the hook ends the chain with tap_result.
	name, args = tail(hook(name, args, `{"n":4}`))
	if name != "mcp_tap_tap_result" || !strings.HasPrefix(args["run"].(string), "relay-") {
		t.Fatalf("final tail call: %s %v", name, args)
	}
	r := c.call("tools/call", map[string]any{"name": "tap_result", "arguments": args})
	text := r["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "total 3 4") || !strings.Contains(text, "[2 action(s) run, 0 refused]") {
		t.Fatalf("tap_result:\n%s", text)
	}
	time.Sleep(50 * time.Millisecond)
	if left, _ := filepath.Glob(filepath.Join(dir, "*.json")); len(left) != 0 {
		t.Errorf("a finished run left a pending call behind: %v", left)
	}
}

// On Gemini, a tool the primitive does not pin cannot bind: Gemini does not
// say which tools it has.
func TestRelayRefusesAnUnpinnedTool(t *testing.T) {
	dir := t.TempDir()
	old := relayDir
	relayDir = func() (string, error) { return dir, nil }
	defer func() { relayDir = old }()
	h := newRelayHub(dir, "tap")
	defer h.close()
	r, err := h.start("gemini")
	if err != nil {
		t.Fatal(err)
	}
	pkg := writePackage(t, `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.test, name: unpinned, version: 0.1.0}
execution: {entrypoint: main.sh}
tools:
  - {alias: search, capability: mail.messages.search, effect: read}
`, "tap call search '{}'\n")
	_, err = Run(t.Context(), Options{Package: pkg, Journal: io.Discard, InterpDir: interpreterStore(t), RunsDir: t.TempDir(),
		relay: r, relayClient: "gemini-cli-mcp-client"})
	if err == nil || !strings.Contains(err.Error(), "must be pinned") {
		t.Fatalf("an unpinned tool was not refused: %v", err)
	}
}

// Only tool calls are left to the client's approval. A change the client
// never sees, here a file write, is still refused without a person's yes.
func TestRelayStillGatesWhatTheClientDoesNotSee(t *testing.T) {
	dir := t.TempDir()
	old := relayDir
	relayDir = func() (string, error) { return dir, nil }
	defer func() { relayDir = old }()

	store := interpreterStore(t)
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	c := &client{t: t, in: cw, sc: bufio.NewScanner(cr), done: make(chan error, 1)}
	c.sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	work := t.TempDir()
	wd, _ := os.Getwd()
	os.Chdir(work)
	defer os.Chdir(wd)
	go func() {
		c.done <- serve(sr, sw, []string{"--interpreters", store, "--runs", t.TempDir()})
		sw.Close()
	}()
	t.Cleanup(func() { cw.Close(); <-c.done })
	c.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "gemini-cli-mcp-client", "version": "0.99.0"}})
	pkg := writePackage(t, `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.test, name: writes-a-file, version: 0.1.0}
execution: {entrypoint: main.sh}
files:
  - {path: out, access: write}
`, "mkdir -p out 2>/dev/null; echo x > out/y.txt || echo refused\n")
	text := c.run(pkg)
	if !strings.Contains(text, "refused") {
		t.Fatalf("a file write in a relay run was not refused:\n%s", text)
	}
	if _, err := os.Stat(filepath.Join(work, "out", "y.txt")); err == nil {
		t.Fatal("the file was written")
	}
}
