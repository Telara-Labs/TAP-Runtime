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

// TestKiloFixtureServer is not a test: Kilo starts the test binary with
// mcpfixture.Args and it answers as the tracker MCP server.
func TestKiloFixtureServer(t *testing.T) {
	if !mcpfixture.IsServer() {
		t.Skip("run by Kilo as an MCP server")
	}
	mcpfixture.Serve(os.Stdin, os.Stdout)
	os.Exit(0)
}

// kiloHome is a fresh HOME whose Kilo CLI has the fixture as tracker;
// extra keys (tools, permission) go into kilo.json beside it.
func kiloHome(t *testing.T, extra map[string]any) []string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "kilo")
	os.MkdirAll(dir, 0o755)
	cmd := append([]string{os.Args[0]}, mcpfixture.Args("TestKiloFixtureServer")...)
	cfg := map[string]any{"mcp": map[string]any{"tracker": map[string]any{"type": "local", "command": cmd, "enabled": true}}}
	for k, v := range extra {
		cfg[k] = v
	}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "kilo.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
}

func kiloBin(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("kilo")
	if err != nil {
		t.Skip("kilo is not installed")
	}
	return bin
}

var trackerPins = []bind.Tool{{Server: "tracker", Name: "search_issues"}, {Server: "tracker", Name: "get_issue"}}

// TENG-3131: through real Kilo, the bridge offers the pinned tools of a
// server Kilo has connected, calls one with no model turn, and returns a
// failed call as an error.
func TestLiveKiloBridge(t *testing.T) {
	k, err := newKilo(kiloBin(t), kiloHome(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	name, version := k.Client()
	t.Logf("client %s %s, tested=%v", name, version, Tested(name, version))
	k.UsePins(append(trackerPins, bind.Tool{Server: "absent", Name: "x"}))
	inv, err := k.Inventory()
	if err != nil {
		t.Fatal(err)
	}
	if len(inv) != 2 || inv[0].Server != "tracker" || inv[0].Annotated != bind.Unknown {
		t.Fatalf("inventory %+v: a pin on a server Kilo has not connected must not be offered", inv)
	}
	out, err := k.Call(trackerPins[0], map[string]any{"jql": "status = open"})
	if err != nil || !strings.Contains(out, "ABC-12") {
		t.Fatalf("search: %q %v", out, err)
	}
	if _, err := k.Call(trackerPins[1], map[string]any{"issue_key": "ABC-99"}); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("a failed call: %v", err)
	}
	if d, err := k.Denied(trackerPins[1]); err != nil || d {
		t.Fatalf("denied %v %v", d, err)
	}
}

// A tool the person's Kilo configuration switches off or denies is denied;
// the most specific key decides.
func TestLiveKiloDeniesWhatKiloConfigurationDenies(t *testing.T) {
	k, err := newKilo(kiloBin(t), kiloHome(t, map[string]any{
		"tools":      map[string]any{"tracker_get_issue": false},
		"permission": map[string]any{"tracker_*": "deny", "tracker_search_issues": "allow"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	for tool, want := range map[string]bool{"get_issue": true, "search_issues": false, "update_issue": true} {
		if d, err := k.Denied(bind.Tool{Server: "tracker", Name: tool}); err != nil || d != want {
			t.Errorf("%s: denied %v %v, want %v", tool, d, err, want)
		}
	}
}

func TestKiloMissing(t *testing.T) {
	if _, err := newKilo(filepath.Join(t.TempDir(), "kilo"), nil); err == nil || !strings.Contains(err.Error(), "not on this machine") {
		t.Fatalf("%v", err)
	}
}

func TestMostSpecific(t *testing.T) {
	m := map[string]string{"*": "ask", "tracker_*": "deny", "tracker_get_issue": "allow"}
	for name, want := range map[string]string{"tracker_get_issue": "allow", "tracker_x": "deny", "other": "ask"} {
		if got, ok := mostSpecific(m, name); !ok || got != want {
			t.Errorf("%s = %q %v, want %q", name, got, ok, want)
		}
	}
}
