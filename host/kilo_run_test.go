//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/internal/mcpfixture"
)

// TestKiloTrackerServer is not a test: Kilo starts the test binary with
// mcpfixture.Args and it answers as the tracker MCP server.
func TestKiloTrackerServer(t *testing.T) {
	if !mcpfixture.IsServer() {
		t.Skip("run by Kilo as an MCP server")
	}
	mcpfixture.Serve(os.Stdin, os.Stdout)
	os.Exit(0)
}

// kiloOnPath puts a fresh Kilo home with the fixture tracker in HOME, or
// skips.
func kiloOnPath(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("kilo"); err != nil {
		t.Skip("not run: kilo is not on this machine")
	}
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "kilo")
	os.MkdirAll(dir, 0o755)
	cmd := append([]string{os.Args[0]}, mcpfixture.Args("TestKiloTrackerServer")...)
	b, _ := json.Marshal(map[string]any{"mcp": map[string]any{"tracker": map[string]any{"type": "local", "command": cmd, "enabled": true}}})
	os.WriteFile(filepath.Join(dir, "kilo.json"), b, 0o600)
	t.Setenv("HOME", home)
	return home
}

// TENG-3131: a primitive run with --client kilo makes its pinned tool calls
// through real Kilo. Kilo's own approval is not shown on this path, so the
// runner's gate decides; an unpinned tool is refused with the reason.
func TestARunCallsToolsThroughKilo(t *testing.T) {
	if testing.Short() {
		t.Skip("runs kilo")
	}
	store := interpreterStore(t)
	inDir(t)
	kiloOnPath(t)
	write := func(tools string) string {
		pkg := t.TempDir()
		os.WriteFile(filepath.Join(pkg, "primitive.yaml"), []byte(`apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.test, name: through-kilo, version: 0.1.0}
execution: {entrypoint: main.sh}
tools:
`+tools), 0o644)
		os.WriteFile(filepath.Join(pkg, "main.sh"), []byte(`key=$(tap call search '{"jql":"status = open"}' | jq -r '.issues[0].key')
tap call issue "{\"issue_key\":\"$key\"}" | jq -r '.key'
`), 0o644)
		return pkg
	}
	pinned := write(`  - {alias: search, capability: tracker.issues.search, effect: read, pin: {server: tracker, tool: search_issues}}
  - {alias: issue, capability: tracker.issues.get, effect: read, pin: {server: tracker, tool: get_issue}}
`)
	res, err := Run(context.Background(), Options{Package: pinned, Journal: io.Discard, InterpDir: store, RunsDir: t.TempDir(), Client: "kilo"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(res.Stderr, "REFUSED by host: write tool needs approval") != 2 || strings.TrimSpace(res.Stdout) != "" {
		t.Fatalf("unapproved: stdout %q stderr %q", res.Stdout, res.Stderr)
	}
	approve := func(a Ask) Grant { return Grant{OK: true, Limit: Unlimited} }
	res, err = Run(context.Background(), Options{Package: pinned, Journal: io.Discard, InterpDir: store, RunsDir: t.TempDir(), Client: "kilo", Approve: approve})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Stdout) != "ABC-12" {
		t.Fatalf("stdout %q stderr %q", res.Stdout, res.Stderr)
	}
	unpinned := write(`  - {alias: search, capability: tracker.issues.search, effect: read}
  - {alias: issue, capability: tracker.issues.get, effect: read, pin: {server: tracker, tool: get_issue}}
`)
	if _, err := Run(context.Background(), Options{Package: unpinned, Journal: io.Discard, InterpDir: store, RunsDir: t.TempDir(), Client: "kilo", Approve: approve}); err == nil || !strings.Contains(err.Error(), "must be pinned") {
		t.Fatalf("an unpinned tool through Kilo: %v", err)
	}
}

// tap install --client kilo writes Kilo's entry shape, and real Kilo starts
// the installed runner as a connected MCP server.
func TestInstallIsSeenByKilo(t *testing.T) {
	if testing.Short() {
		t.Skip("runs kilo")
	}
	home := kiloOnPath(t)
	tap := filepath.Join(t.TempDir(), "tap")
	build := exec.Command("go", "build", "-o", tap, "./host")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	inst := exec.Command(tap, "install", "--client", "kilo")
	inst.Env = append(os.Environ(), "HOME="+home)
	if out, err := inst.CombinedOutput(); err != nil {
		t.Fatalf("install: %v %s", err, out)
	}
	b, _ := os.ReadFile(filepath.Join(home, ".config", "kilo", "kilo.json"))
	var cfg struct {
		MCP map[string]struct {
			Type    string   `json:"type"`
			Command []string `json:"command"`
		} `json:"mcp"`
	}
	json.Unmarshal(b, &cfg)
	if e := cfg.MCP["tap"]; e.Type != "local" || len(e.Command) != 4 || e.Command[1] != "serve" || cfg.MCP["tracker"].Type != "local" {
		t.Fatalf("kilo.json after install: %s", b)
	}
	work := t.TempDir()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	l.Close()
	serve := exec.Command("kilo", "serve", "--port", port, "--hostname", "127.0.0.1")
	serve.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	serve.Dir = work
	serve.Env = append(os.Environ(), "HOME="+home, "KILO_SERVER_PASSWORD=t")
	var serveLog strings.Builder
	serve.Stdout, serve.Stderr = &serveLog, &serveLog
	if err := serve.Start(); err != nil {
		t.Fatal(err)
	}
	// kilo is a launcher: stop the server it started too.
	defer func() { syscall.Kill(-serve.Process.Pid, syscall.SIGKILL); serve.Wait() }()
	var status map[string]struct {
		Status string `json:"status"`
	}
	last := ""
	for i := 0; i < 60; i++ {
		req, _ := http.NewRequest("GET", "http://127.0.0.1:"+port+"/mcp?directory="+work, nil)
		req.SetBasicAuth("kilo", "t")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			last = string(body)
			status = nil
			json.Unmarshal(body, &status)
			if status["tap"].Status == "connected" {
				return
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("Kilo did not connect the installed runner: %s\nkilo serve said:\n%s", last, serveLog.String())
}
