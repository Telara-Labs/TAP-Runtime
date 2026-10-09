package bridge

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/bind"
	"github.com/Telara-Labs/TAP-Runtime/internal/mcpfixture"
)

// TestCopilotFixtureServer is not a test: Copilot starts the test binary
// with mcpfixture.Args and it answers as the tracker MCP server.
func TestCopilotFixtureServer(t *testing.T) {
	if !mcpfixture.IsServer() {
		t.Skip("run by Copilot as an MCP server")
	}
	mcpfixture.Serve(os.Stdin, os.Stdout)
	os.Exit(0)
}

func TestCopilotVersion(t *testing.T) {
	if got := copilotVersion("GitHub Copilot CLI 1.0.94.\nRun 'copilot update' to check for updates.\n"); got != "1.0.94" {
		t.Fatalf("version = %q", got)
	}
}

func TestReadFramed(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("Content-Length: 14\r\n\r\n{\"id\":1,\"a\":2}Content-Length: 2\r\n\r\n{}"))
	m, err := readFramed(r)
	if err != nil || m["a"] != float64(2) {
		t.Fatalf("first message %v %v", m, err)
	}
	if m, err := readFramed(r); err != nil || len(m) != 0 {
		t.Fatalf("second message %v %v", m, err)
	}
}

func TestSameArgs(t *testing.T) {
	if !sameArgs(map[string]any{"jql": "x", "n": float64(2)}, map[string]any{"n": 2, "jql": "x"}) {
		t.Fatal("equal arguments compared unequal")
	}
	if sameArgs(map[string]any{"jql": "y"}, map[string]any{"jql": "x"}) {
		t.Fatal("different arguments compared equal")
	}
}

// copilotHome is a fresh Copilot configuration directory whose only MCP
// server is the fixture tracker. The person's own ~/.copilot is not read.
func copilotHome(t *testing.T) []string {
	t.Helper()
	home := t.TempDir()
	cfg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"tracker": map[string]any{
		"type": "local", "command": os.Args[0], "args": mcpfixture.Args("TestCopilotFixtureServer"), "tools": []string{"*"},
	}}})
	if err := os.WriteFile(filepath.Join(home, "mcp-config.json"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	return append(os.Environ(), "COPILOT_HOME="+home)
}

// Through real Copilot CLI, the bridge lists the tools the person connected
// (not Copilot's own), calls one with no model turn, answering Copilot's own
// permission request for that call, and returns a failed call as an error.
func TestLiveCopilotBridge(t *testing.T) {
	if _, err := exec.LookPath("copilot"); err != nil {
		t.Skip("copilot is not installed")
	}
	c, err := NewCopilotIn(Proc{Env: copilotHome(t), Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	name, version := c.Client()
	t.Logf("client %s %s", name, version)
	inv, err := c.Inventory()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range inv {
		names = append(names, tool.Server+"/"+tool.Name)
		if tool.Schema == nil {
			t.Errorf("%s has no input schema", tool.Name)
		}
	}
	if strings.Join(names, ",") != "tracker/get_issue,tracker/search_issues" && strings.Join(names, ",") != "tracker/search_issues,tracker/get_issue" {
		t.Fatalf("inventory %v", names)
	}
	out, err := c.Call(bind.Tool{Server: "tracker", Name: "search_issues"}, map[string]any{"jql": "status = open"})
	if err != nil || !strings.Contains(out, "ABC-12") {
		t.Fatalf("search: %q %v", out, err)
	}
	if _, err := c.Call(bind.Tool{Server: "tracker", Name: "get_issue"}, map[string]any{"issue_key": "ABC-99"}); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("a failed call: %v", err)
	}
}
