package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tap remove is the reverse of setup: after it, no agent's configuration
// names the runner, Gemini keeps no hook that would run it, and everything
// else in each file is as it was.
func TestRemoveDisconnectsEveryAgent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	os.MkdirAll(filepath.Join(home, ".cursor", "chats"), 0o755)
	os.WriteFile(filepath.Join(home, ".cursor", "mcp.json"), []byte(cursorConfig), 0o600)
	gemini := filepath.Join(home, ".gemini", "settings.json")
	os.MkdirAll(filepath.Dir(gemini), 0o755)
	os.WriteFile(gemini, []byte(`{"theme": "dark", "hooks": {"AfterTool": [{"matcher": "x", "hooks": [{"name": "mine", "type": "command", "command": "true"}]}]}}`), 0o600)
	var out, errOut bytes.Buffer
	if code := installCommand([]string{"--client", "cursor,gemini-cli"}, &out, &errOut); code != 0 {
		t.Fatalf("install exit %d: %s", code, errOut.String())
	}
	if b, _ := os.ReadFile(gemini); !strings.Contains(string(b), "hook gemini") {
		t.Fatalf("install wrote no Gemini hook:\n%s", b)
	}

	out.Reset()
	if code := installCommand([]string{"--client", "all", "--remove"}, &out, &errOut); code != 0 {
		t.Fatalf("remove exit %d: %s\n%s", code, errOut.String(), out.String())
	}
	b, _ := os.ReadFile(filepath.Join(home, ".cursor", "mcp.json"))
	if _, ok := decode(t, b)["mcpServers"].(map[string]any)["tap"]; ok {
		t.Errorf("Cursor still has tap:\n%s", b)
	}
	b, _ = os.ReadFile(gemini)
	g := decode(t, b)
	if _, ok := g["mcpServers"].(map[string]any)["tap"]; ok {
		t.Errorf("Gemini still has tap:\n%s", b)
	}
	if strings.Contains(string(b), "hook gemini") || !strings.Contains(string(b), `"mine"`) || g["theme"] != "dark" {
		t.Errorf("Gemini settings after remove:\n%s", b)
	}
	for _, w := range []string{"Cursor: removed tap", "Gemini CLI: removed tap and its hook", "is not connected"} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("output lacks %q:\n%s", w, out.String())
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); err == nil {
		t.Error("remove created a configuration for an agent that had none")
	}

	// A second remove finds nothing and changes nothing.
	out.Reset()
	if code := installCommand([]string{"--client", "all", "--remove"}, &out, &errOut); code != 0 || strings.Contains(out.String(), "removed") {
		t.Fatalf("second remove exit %d:\n%s", code, out.String())
	}
}
