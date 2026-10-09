package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/internal/mcpfixture"
)

// Live: in the real Gemini CLI, a primitive with no pin runs through
// Gemini's own connection. Gemini says nothing about its tools, so the first
// run is blocked with one question naming the capability; once the answer
// is kept (tap bind), the run resolves it against the server in Gemini's
// settings and Gemini makes the call. Needs gemini on PATH and
// GEMINI_API_KEY; the person's Gemini settings are never touched (HOME is a
// temporary directory).
func TestLiveGeminiResolvesAnUnpinnedCapability(t *testing.T) {
	if testing.Short() {
		t.Skip("runs gemini")
	}
	gemini, err := exec.LookPath("gemini")
	if err != nil {
		t.Skip("not run: gemini is not on this machine")
	}
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		t.Skip("not run: GEMINI_API_KEY is not set")
	}
	store := interpreterStore(t)
	bin := filepath.Join(t.TempDir(), "tap")
	build := exec.Command("go", "build", "-o", bin, "./host")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the runner: %v\n%s", err, out)
	}
	home, _ := filepath.EvalSymlinks(t.TempDir())
	work := filepath.Join(home, "work")
	os.MkdirAll(work, 0o755)
	pkg := t.TempDir()
	os.WriteFile(filepath.Join(pkg, "primitive.yaml"), []byte(`apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.test, name: first-open-issue-unpinned, version: 0.1.0}
execution: {entrypoint: main.sh}
tools:
  - {alias: search, capability: tracker.issues.search, effect: read}
`), 0o644)
	os.WriteFile(filepath.Join(pkg, "main.sh"), []byte(`echo "first open issue: $(tap call search '{"jql":"status = open"}' | jq -r '.issues[0].key')"
`), 0o644)
	catalogRoot := filepath.Join(home, "catalog")
	identity := stageLivePackage(t, catalogRoot, pkg)
	journal := filepath.Join(home, "journal.jsonl")
	settings := map[string]any{
		"security": map[string]any{"auth": map[string]any{"selectedType": "gemini-api-key"}},
		"mcpServers": map[string]any{
			"tap": map[string]any{"command": bin, "args": []string{"serve", "--name", "tap", "--interpreters", store,
				"--runs", filepath.Join(home, "runs"), "--journal", journal, "--catalog-root", catalogRoot}},
			"tracker": map[string]any{"command": os.Args[0], "args": mcpfixture.Args("TestGeminiTrackerServer")},
		},
		"hooks": map[string]any{"AfterTool": []any{map[string]any{"matcher": ".*", "hooks": []any{map[string]any{
			"name": "tap", "type": "command", "command": geminiHookCommand(bin), "timeout": 600000}}}}},
	}
	b, _ := json.MarshalIndent(settings, "", "  ")
	os.MkdirAll(filepath.Join(home, ".gemini"), 0o755)
	os.WriteFile(filepath.Join(home, ".gemini", "settings.json"), b, 0o600)
	env := append(os.Environ(), "HOME="+home, "GEMINI_API_KEY="+key)
	run := func() string {
		prompt := "Call the tap_run tool exactly once with ref " + identity["ref"].(string) + " and digest " + identity["digest"].(string) +
			". Do not call any other tool. Then reply with exactly the text tap_run returned."
		cmd := exec.Command(gemini, "-p", prompt, "--approval-mode", "yolo", "--skip-trust", "-o", "json")
		cmd.Dir, cmd.Env = work, env
		done := make(chan struct{})
		var out []byte
		go func() { out, _ = cmd.CombinedOutput(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Minute):
			cmd.Process.Kill()
			<-done
			t.Fatalf("gemini did not finish in 5 minutes:\n%s", out)
		}
		if strings.Contains(string(out), key) {
			t.Fatal("the key appears in gemini's output")
		}
		return string(out)
	}
	if out := run(); !strings.Contains(out, "blocked") || !strings.Contains(out, "tracker.issues.search") || !strings.Contains(out, "tap bind --client gemini") {
		t.Fatalf("the first run was not blocked with the question:\n%s", out)
	}
	bind := exec.Command(bin, "bind", "--client", "gemini", "tracker.issues.search", "tracker/search_issues")
	bind.Env = env
	if out, err := bind.CombinedOutput(); err != nil {
		t.Fatalf("tap bind: %v %s", err, out)
	}
	if out := run(); !strings.Contains(out, "first open issue: ABC-12") {
		t.Fatalf("the run with the kept answer:\n%s", out)
	}
}
