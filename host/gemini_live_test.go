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

// TestGeminiTrackerServer is not a test: Gemini CLI starts the test binary
// with mcpfixture.Args and it answers as the tracker MCP server.
func TestGeminiTrackerServer(t *testing.T) {
	if !mcpfixture.IsServer() {
		t.Skip("run by Gemini CLI as an MCP server")
	}
	mcpfixture.Serve(os.Stdin, os.Stdout)
	os.Exit(0)
}

// Live: in the real Gemini CLI, one model call to tap_run runs a
// primitive whose two tool calls (search, then a lookup of the key the
// search returned) Gemini makes itself through the AfterTool hook, with its
// own connection to the tracker server. The primitive's output is what
// tap_run returns. Needs gemini on PATH and GEMINI_API_KEY; the person's
// Gemini settings are never touched (HOME is a temporary directory).
func TestLiveGeminiRunsAPrimitiveThroughItsOwnTools(t *testing.T) {
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
metadata: {publisher: dev.test, name: first-open-issue, version: 0.1.0}
execution: {entrypoint: main.sh}
tools:
  - {alias: search, capability: tracker.issues.search, effect: read, pin: {server: tracker, tool: search_issues}}
  - {alias: issue, capability: tracker.issues.get, effect: read, pin: {server: tracker, tool: get_issue}}
`), 0o644)
	os.WriteFile(filepath.Join(pkg, "main.sh"), []byte(`key=$(tap call search '{"jql":"status = open"}' | jq -r '.issues[0].key')
echo "first open issue: $(tap call issue "{\"issue_key\":\"$key\"}" | jq -r '.key')"
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

	prompt := "Call the tap_run tool exactly once with ref " + identity["ref"].(string) + " and digest " + identity["digest"].(string) +
		". Do not call any other tool. Then reply with exactly the text tap_run returned."
	cmd := exec.Command(gemini, "-p", prompt, "--approval-mode", "yolo", "--skip-trust", "-m", "gemini-2.5-flash", "-o", "json")
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "HOME="+home, "GEMINI_API_KEY="+key)
	done := make(chan struct{})
	var out []byte
	go func() { out, err = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Minute):
		cmd.Process.Kill()
		<-done
		t.Fatalf("gemini did not finish in 5 minutes:\n%s", out)
	}
	text := string(out)
	if strings.Contains(text, key) {
		t.Fatal("the key appears in gemini's output")
	}
	if err != nil || !strings.Contains(text, "first open issue: ABC-12") {
		t.Fatalf("gemini: %v\n%s", err, text)
	}
	// The runner made both calls through Gemini (the relay), in order.
	j, _ := os.ReadFile(journal)
	var calls []string
	for _, line := range strings.Split(string(j), "\n") {
		var e map[string]any
		if json.Unmarshal([]byte(line), &e) == nil && e["outcome"] == "ran" && e["server"] == "tracker" {
			calls = append(calls, e["tool"].(string))
		}
	}
	if strings.Join(calls, ",") != "search_issues,get_issue" {
		t.Fatalf("calls the runner recorded: %v\njournal:\n%s", calls, j)
	}
	t.Logf("gemini output:\n%s", text)
	// The model saw only the primitive's output: Gemini's record of the
	// conversation holds neither the chained calls nor what they returned
	// (ABC-13 is only in the search's result).
	recs, _ := filepath.Glob(filepath.Join(home, ".gemini", "tmp", "*", "chats", "*"))
	if len(recs) == 0 {
		t.Fatal("gemini kept no session record")
	}
	for _, r := range recs {
		b, _ := os.ReadFile(r)
		if strings.Contains(string(b), "ABC-13") || strings.Contains(string(b), "search_issues") {
			t.Errorf("%s: the model's context holds a chained call or its result", filepath.Base(r))
		}
	}
}
