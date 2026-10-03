package bridge

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitlab.com/telara-labs/tap-runtime/bind"
	"gitlab.com/telara-labs/tap-runtime/internal/mcpfixture"
)

// TestGooseFixtureServer is not a test: Goose starts the test binary with
// mcpfixture.Args and it answers as the tracker MCP server.
func TestGooseFixtureServer(t *testing.T) {
	if !mcpfixture.IsServer() {
		t.Skip("run by Goose as an MCP server")
	}
	mcpfixture.Serve(os.Stdin, os.Stdout)
	os.Exit(0)
}

// gooseHome is a fresh HOME whose Goose has the fixture as tracker, its
// get_issue set to never_allow when deny is set.
func gooseHome(t *testing.T, deny bool) []string {
	t.Helper()
	home := t.TempDir()
	if err := mcpfixture.GooseHome(home, "TestGooseFixtureServer", ""); err != nil {
		t.Fatal(err)
	}
	if deny {
		perm := "user:\n  always_allow: []\n  ask_before: []\n  never_allow:\n    - tracker__get_issue\n"
		if err := os.WriteFile(filepath.Join(home, ".config", "goose", "permission.yaml"), []byte(perm), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
}

func gooseBin(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("goose")
	if err != nil {
		if home, _ := os.UserHomeDir(); home != "" {
			if p := filepath.Join(home, ".local", "bin", "goose"); fileExists(p) {
				return p
			}
		}
		t.Skip("goose is not installed")
	}
	return bin
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// TENG-3116: through real Goose, the bridge lists the user's extension
// tools (not Goose's own), calls one with no model turn even though the
// user's mode is approve, and returns a failed call as an error.
func TestLiveGooseBridge(t *testing.T) {
	env := gooseHome(t, false)
	g, err := newGoose(gooseBin(t), env)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	name, version := g.Client()
	t.Logf("client %s %s, tested=%v", name, version, Tested(name, version))
	inv, err := g.Inventory()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range inv {
		names = append(names, tool.Server+"/"+tool.Name)
		if tool.Annotated != bind.Unknown || tool.Schema == nil {
			t.Errorf("%s: effect %v schema %v", tool.Name, tool.Annotated, tool.Schema)
		}
	}
	if strings.Join(names, ",") != "tracker/get_issue,tracker/search_issues" && strings.Join(names, ",") != "tracker/search_issues,tracker/get_issue" {
		t.Fatalf("inventory %v", names)
	}
	out, err := g.Call(bind.Tool{Server: "tracker", Name: "search_issues"}, map[string]any{"jql": "status = open"})
	if err != nil || !strings.Contains(out, "ABC-12") {
		t.Fatalf("search: %q %v", out, err)
	}
	if _, err := g.Call(bind.Tool{Server: "tracker", Name: "get_issue"}, map[string]any{"issue_key": "ABC-99"}); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("a failed call: %v", err)
	}
	if d, err := g.Denied(bind.Tool{Server: "tracker", Name: "get_issue"}); err != nil || d {
		t.Fatalf("denied %v %v", d, err)
	}
	// The bridge's session is deleted: it never shows in Goose's history,
	// where discover would read it.
	g.Close()
	db := filepath.Join(strings.TrimPrefix(env[0], "HOME="), ".local", "share", "goose", "sessions", "sessions.db")
	if sqlite, err := exec.LookPath("sqlite3"); err == nil && fileExists(db) {
		out, err := exec.Command(sqlite, db, "SELECT count(*) FROM sessions").CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != "0" {
			t.Fatalf("sessions left in Goose's history: %s %v", out, err)
		}
	}
}

func TestLiveGooseNeverAllowIsDenied(t *testing.T) {
	g, err := newGoose(gooseBin(t), gooseHome(t, true))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	for tool, want := range map[string]bool{"get_issue": true, "search_issues": false} {
		if d, err := g.Denied(bind.Tool{Server: "tracker", Name: tool}); err != nil || d != want {
			t.Errorf("%s: denied %v %v", tool, d, err)
		}
	}
}

func TestGooseMissing(t *testing.T) {
	if _, err := newGoose(filepath.Join(t.TempDir(), "goose"), nil); err == nil || !strings.Contains(err.Error(), "not on this machine") {
		t.Fatalf("%v", err)
	}
}
