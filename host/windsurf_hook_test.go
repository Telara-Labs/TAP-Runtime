package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/history"
)

func hookInput(path string) string {
	b, _ := json.Marshal(map[string]any{"agent_action_name": "post_cascade_response_with_transcript", "tool_info": map[string]any{"transcript_path": path}})
	return string(b)
}

// The hook copies only a regular transcript from Windsurf's own folder, and
// never fails Windsurf.
func TestWindsurfHookArchivesOnlyItsTranscripts(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(home, ".windsurf", "transcripts")
	os.MkdirAll(src, 0o700)
	os.WriteFile(filepath.Join(src, "traj-1.jsonl"), []byte(`{"type":"user_input","user_input":{"user_response":"hi"}}`+"\n"), 0o600)
	outside := filepath.Join(home, "secret.jsonl")
	os.WriteFile(outside, []byte("x"), 0o600)
	os.Symlink(outside, filepath.Join(src, "link.jsonl"))
	var errOut bytes.Buffer
	for _, p := range []string{filepath.Join(src, "traj-1.jsonl"), outside, filepath.Join(src, "link.jsonl"), filepath.Join(src, "..", "secret.jsonl")} {
		if code := windsurfHook(strings.NewReader(hookInput(p)), &errOut, home); code != 0 {
			t.Fatalf("the hook must never fail Windsurf: %d", code)
		}
	}
	archived, _ := filepath.Glob(filepath.Join(home, ".tap", "windsurf", "transcripts", "*"))
	if len(archived) != 1 || filepath.Base(archived[0]) != "traj-1.jsonl" {
		t.Fatalf("archived %v (%s)", archived, errOut.String())
	}
	if windsurfHook(strings.NewReader(`{"agent_action_name":"pre_read_code"}`), &errOut, home) != 0 || windsurfHook(strings.NewReader("not json"), &errOut, home) != 0 {
		t.Fatal("other events and bad input are ignored")
	}
}

func TestAddWindsurfHookKeepsOtherHooks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	os.WriteFile(path, []byte(`{"hooks":{"pre_read_code":[{"command":"audit"}],"post_cascade_response_with_transcript":[{"command":"mine"}]}}`), 0o600)
	if ch, err := addWindsurfHook(path, "/opt/tap", false); err != nil || !ch {
		t.Fatalf("add: %v %v", ch, err)
	}
	if ch, err := addWindsurfHook(path, "/opt/tap", false); err != nil || ch {
		t.Fatalf("second add changed=%v %v", ch, err)
	}
	b, _ := os.ReadFile(path)
	var doc struct {
		Hooks map[string][]map[string]any `json:"hooks"`
	}
	json.Unmarshal(b, &doc)
	if len(doc.Hooks["pre_read_code"]) != 1 || len(doc.Hooks["post_cascade_response_with_transcript"]) != 2 {
		t.Fatalf("hooks %s", b)
	}
	if ch, err := addWindsurfHook(path, "/opt/tap", true); err != nil || !ch {
		t.Fatalf("remove: %v %v", ch, err)
	}
	b, _ = os.ReadFile(path)
	if !strings.Contains(string(b), `"mine"`) || strings.Contains(string(b), "hook windsurf") {
		t.Fatalf("remove took the wrong entry: %s", b)
	}
	os.WriteFile(path, []byte("{bad"), 0o600)
	if _, err := addWindsurfHook(path, "/opt/tap", false); err == nil {
		t.Fatal("a malformed hooks.json was accepted")
	}
}

// End to end (TENG-3121): tap install --client windsurf sets up the MCP
// server and the transcript hook; Windsurf runs the hook; discover reads the
// archived transcript after Windsurf has pruned its own copy.
func TestWindsurfInstallHookAndDiscover(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".codeium", "windsurf"), 0o755)
	var out, errOut bytes.Buffer
	if code := installCommand([]string{"--client", "windsurf"}, &out, &errOut); code != 0 {
		t.Fatalf("install: %d %s", code, errOut.String())
	}
	hooks, _ := os.ReadFile(filepath.Join(home, ".codeium", "windsurf", "hooks.json"))
	if !strings.Contains(string(hooks), "hook windsurf") {
		t.Fatalf("no hook installed: %s", hooks)
	}
	// Windsurf writes a transcript and runs the hook.
	src := filepath.Join(home, ".windsurf", "transcripts", "traj-9.jsonl")
	os.MkdirAll(filepath.Dir(src), 0o700)
	steps := `{"status":"done","type":"user_input","user_input":{"user_response":"find the open issues"}}
{"mcp_tool_use":{"mcp_server_name":"tracker","mcp_tool_name":"search_issues","mcp_tool_arguments":{"jql":"status = open"},"status":"done"},"type":"mcp_tool_use"}
`
	os.WriteFile(src, []byte(steps), 0o600)
	if code := hookCommand([]string{"windsurf"}, strings.NewReader(hookInput(src)), &out, &errOut); code != 0 {
		t.Fatalf("hook: %d", code)
	}
	os.Remove(src) // Windsurf pruned it
	r, err := history.ReaderFor("windsurf", home)
	if err != nil {
		t.Fatal(err)
	}
	ss, err := r.Read(time.Time{})
	if err != nil || len(ss) != 1 || len(ss[0].Calls) != 1 || ss[0].Calls[0].MCPTool != "search_issues" {
		t.Fatalf("discover read %+v %v", ss, err)
	}
}
