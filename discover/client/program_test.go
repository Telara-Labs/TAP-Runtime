package client

import (
	"errors"
	"os"
	"os/exec"
	"testing"
)

// Tests say what is on PATH; the machine running them does not.
func TestMain(m *testing.M) {
	LookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	os.Exit(m.Run())
}

// Goose, Gemini CLI and Crush installed and never run have no config folder
// yet; setup skipped them on a clean machine. Their program on PATH counts.
func TestAnAgentOnPathIsInstalledBeforeItsFirstRun(t *testing.T) {
	home := t.TempDir()
	old := LookPath
	t.Cleanup(func() { LookPath = old })
	LookPath = func(name string) (string, error) {
		if name == "goose" || name == "crush" {
			return "/usr/local/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
	got := map[string]bool{}
	for _, c := range Detected(home) {
		got[c.ID] = true
	}
	if !got["goose"] || !got["crush"] || got["gemini-cli"] || got["claude-code"] {
		t.Fatalf("detected %v", got)
	}
}

func TestCrushCanBeConnected(t *testing.T) {
	c, ok := Lookup("crush")
	if !ok || c.MCP.Kind != MCPJSONFile || c.MCP.Path != ".config/crush/crush.json" || c.MCP.Key != "mcp" {
		t.Fatalf("crush MCP = %+v", c.MCP)
	}
}
