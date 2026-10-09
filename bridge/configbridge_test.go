package bridge

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/bind"
	"github.com/Telara-Labs/TAP-Runtime/internal/mcpfixture"
)

// TestConfigFixtureServer is not a test: the configuration bridge starts the
// test binary with mcpfixture.Args and it answers as the tracker MCP server.
func TestConfigFixtureServer(t *testing.T) {
	if !mcpfixture.IsServer() {
		t.Skip("run by the configuration bridge as an MCP server")
	}
	mcpfixture.Serve(os.Stdin, os.Stdout)
	os.Exit(0)
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	b, _ := json.Marshal(v)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// From Crush's own configuration the bridge connects to a server that
// carries no secret, lists its tools, calls one and returns a failed call
// as an error; a server whose entry carries a secret is not used, and says
// why. Crush itself is not run or changed.
func TestConfigBridgeCrush(t *testing.T) {
	home := t.TempDir()
	writeJSON(t, filepath.Join(home, ".config", "crush", "crush.json"), map[string]any{"mcp": map[string]any{
		"tracker": map[string]any{"type": "stdio", "command": os.Args[0], "args": mcpfixture.Args("TestConfigFixtureServer")},
		"secret":  map[string]any{"type": "stdio", "command": "never-run", "env": map[string]string{"TOKEN": "x"}},
		"off":     map[string]any{"type": "stdio", "command": "never-run", "disabled": true},
	}})
	env := []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	b, err := NewConfigBridgeIn("crush", Proc{Env: env, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	inv, err := b.Inventory()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range inv {
		names = append(names, tool.Server+"/"+tool.Name)
		if tool.Schema == nil {
			t.Errorf("%s has no schema", tool.Name)
		}
	}
	if len(names) != 2 || !strings.Contains(strings.Join(names, ","), "tracker/search_issues") {
		t.Fatalf("inventory %v", names)
	}
	blocked := b.Blocked()
	if !strings.Contains(blocked["secret"], "carries a secret") || !strings.Contains(blocked["off"], "disabled") {
		t.Fatalf("blocked %v", blocked)
	}
	out, err := b.Call(bind.Tool{Server: "tracker", Name: "search_issues"}, map[string]any{"jql": "status = open"})
	if err != nil || !strings.Contains(out, "ABC-12") {
		t.Fatalf("search: %q %v", out, err)
	}
	if _, err := b.Call(bind.Tool{Server: "tracker", Name: "get_issue"}, map[string]any{"issue_key": "ABC-99"}); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("a failed call: %v", err)
	}
	if _, err := b.Call(bind.Tool{Server: "secret", Name: "x"}, nil); err == nil || !strings.Contains(err.Error(), "carries a secret") {
		t.Fatalf("a call to the blocked server: %v", err)
	}
}

// Without Cursor's own approvals the bridge uses no server: connecting
// would pass over approvals the person gave or withheld.
func TestConfigBridgeCursorNeedsItsApprovals(t *testing.T) {
	home := t.TempDir()
	writeJSON(t, filepath.Join(home, ".cursor", "mcp.json"), map[string]any{"mcpServers": map[string]any{
		"tracker": map[string]any{"command": os.Args[0], "args": mcpfixture.Args("TestConfigFixtureServer")},
	}})
	b, err := NewConfigBridgeIn("cursor", Proc{Env: []string{"HOME=" + home, "PATH=" + t.TempDir()}, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	inv, _ := b.Inventory()
	if len(inv) != 0 || !strings.Contains(b.Blocked()["tracker"], "approvals could not be read") {
		t.Fatalf("inventory %v, blocked %v", inv, b.Blocked())
	}
}

// With the Cursor CLI, a server the person has not approved in Cursor is
// not used. Cursor's approvals are only read.
func TestLiveConfigBridgeCursorHonoursApprovals(t *testing.T) {
	agent, err := exec.LookPath("cursor-agent")
	if err != nil {
		t.Skip("not run: cursor-agent is not installed")
	}
	project := t.TempDir()
	writeJSON(t, filepath.Join(project, ".cursor", "mcp.json"), map[string]any{"mcpServers": map[string]any{
		"tapunapproved": map[string]any{"command": os.Args[0], "args": mcpfixture.Args("TestConfigFixtureServer")},
	}})
	b, err := NewConfigBridgeIn("cursor", Proc{Env: append(os.Environ(), "PATH="+filepath.Dir(agent)+string(os.PathListSeparator)+os.Getenv("PATH")), Dir: project})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if why := b.Blocked()["tapunapproved"]; !strings.Contains(why, "not approved in Cursor") {
		t.Fatalf("an unapproved server: blocked %q", why)
	}
}

// OpenCode's format (shared by Kilo): command as one array, environment,
// enabled. Used when no TAP relay is running in a session.
func TestConfigBridgeOpenCodeFormat(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	writeJSON(t, filepath.Join(home, ".config", "opencode", "opencode.json"), map[string]any{"mcp": map[string]any{
		"secret": map[string]any{"type": "local", "command": []string{"never-run"}, "environment": map[string]string{"TOKEN": "{env:X}"}},
	}})
	writeJSON(t, filepath.Join(project, "opencode.json"), map[string]any{"mcp": map[string]any{
		"tracker": map[string]any{"type": "local", "command": append([]string{os.Args[0]}, mcpfixture.Args("TestConfigFixtureServer")...)},
		"off":     map[string]any{"type": "local", "command": []string{"never-run"}, "enabled": false},
	}})
	b, err := NewConfigBridgeIn("opencode", Proc{Env: []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}, Dir: project})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	inv, _ := b.Inventory()
	if len(inv) != 2 || inv[0].Server != "tracker" {
		t.Fatalf("inventory %+v", inv)
	}
	blocked := b.Blocked()
	if !strings.Contains(blocked["secret"], "carries a secret") || !strings.Contains(blocked["off"], "disabled") {
		t.Fatalf("blocked %v", blocked)
	}
	if out, err := b.Call(bind.Tool{Server: "tracker", Name: "search_issues"}, map[string]any{"jql": "x"}); err != nil || !strings.Contains(out, "ABC-12") {
		t.Fatalf("search: %q %v", out, err)
	}
}
