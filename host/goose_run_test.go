package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/internal/mcpfixture"
)

// TestGooseTrackerServer is not a test: Goose starts the test binary with
// mcpfixture.Args and it answers as the tracker MCP server.
func TestGooseTrackerServer(t *testing.T) {
	if !mcpfixture.IsServer() {
		t.Skip("run by Goose as an MCP server")
	}
	mcpfixture.Serve(os.Stdin, os.Stdout)
	os.Exit(0)
}

// gooseOnPath puts a fresh Goose home in HOME and goose on PATH, or skips.
func gooseOnPath(t *testing.T) {
	t.Helper()
	bin, err := exec.LookPath("goose")
	if err != nil {
		if h, _ := os.UserHomeDir(); h != "" && fileIsThere(filepath.Join(h, ".local", "bin", "goose")) {
			bin = filepath.Join(h, ".local", "bin", "goose")
		} else {
			t.Skip("not run: goose is not on this machine")
		}
	}
	home := t.TempDir()
	if err := mcpfixture.GooseHome(home, "TestGooseTrackerServer", ""); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TENG-3116: a primitive run with --client goose makes its tool calls
// through real Goose: the search, then a lookup of the key it returned.
// Goose annotates nothing, so each tool is gated as a write by the runner,
// the only approval on this path.
func TestARunCallsToolsThroughGoose(t *testing.T) {
	if testing.Short() {
		t.Skip("runs goose")
	}
	store := interpreterStore(t)
	inDir(t)
	gooseOnPath(t)
	pkg := t.TempDir()
	os.WriteFile(filepath.Join(pkg, "primitive.yaml"), []byte(`apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.test, name: through-goose, version: 0.1.0}
execution: {entrypoint: main.sh}
tools:
  - {alias: search, capability: tracker.issues.search, effect: read, pin: {server: tracker, tool: search_issues}}
  - {alias: issue, capability: tracker.issues.get, effect: read, pin: {server: tracker, tool: get_issue}}
`), 0o644)
	os.WriteFile(filepath.Join(pkg, "main.sh"), []byte(`key=$(tap call search '{"jql":"status = open"}' | jq -r '.issues[0].key')
tap call issue "{\"issue_key\":\"$key\"}" | jq -r '.key'
`), 0o644)
	// Goose shows no approval of its own on this path, so without the
	// runner's approval nothing runs.
	res, err := Run(context.Background(), Options{Package: pkg, Journal: io.Discard, InterpDir: store, RunsDir: t.TempDir(), Client: "goose"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(res.Stderr, "REFUSED by host: write tool needs approval") != 2 || strings.TrimSpace(res.Stdout) != "" {
		t.Fatalf("unapproved: stdout %q stderr %q", res.Stdout, res.Stderr)
	}
	// Approved by the runner's gate, the calls go through Goose.
	var asked []string
	approve := func(a Ask) Grant { asked = append(asked, a.Kind); return Grant{OK: true, Limit: Unlimited} }
	res, err = Run(context.Background(), Options{Package: pkg, Journal: io.Discard, InterpDir: store, RunsDir: t.TempDir(), Client: "goose", Approve: approve})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Stdout) != "ABC-12" {
		t.Fatalf("stdout %q stderr %q", res.Stdout, res.Stderr)
	}
	if len(asked) == 0 {
		t.Fatal("the runner asked no approval")
	}
	t.Logf("the runner asked: %v", asked)
}
