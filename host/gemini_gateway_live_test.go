package main

import (
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var liveGeminiGateway = flag.Bool("live-gemini-gateway", false, "run a primitive that reads through a Telara gateway's dispatcher from Gemini CLI (uses the person's configured gateway and one model run)")

const gatewayReadManifest = `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.test, name: gateway-dispatcher-read, version: 0.1.0}
interface:
  inputSchema: {type: object, properties: {}}
execution: {entrypoint: main.py, timeoutSeconds: 120}
tools:
  - {alias: gl, capability: gitlab.list_projects, effect: read, pin: {server: telara, tool: telara_execute_action}}
`

const gatewayReadProgram = `import json
r = tap.call('gl', {'integration': 'gitlab', 'action': 'list_projects', 'params': {'per_page': 1}})
print('RESULT ' + json.dumps({'ok': True, 'bytes': len(json.dumps(r))}))
`

// TestLiveGeminiReadsThroughAPinnedGatewayDispatcher runs, from Gemini CLI, a
// primitive that pins a Telara gateway's dispatcher and reads one operation
// through it. Gemini lists no tools, so the runner lends the gateway's
// catalog tools beside the pin and verifies the operation is a read from the
// catalog; before, the read was refused because the catalog search was not
// in the relay's inventory. It uses the gateway the person configured in
// Gemini (copied into a Gemini home of the test's own) and signs Gemini in
// with GEMINI_SERVICE_ACCOUNT_JSON or GEMINI_API_KEY.
func TestLiveGeminiReadsThroughAPinnedGatewayDispatcher(t *testing.T) {
	if !*liveGeminiGateway {
		t.Skip("pass -live-gemini-gateway to read through the person's Telara gateway from Gemini")
	}
	gemini, err := exec.LookPath("gemini")
	if err != nil {
		t.Skip("not run: gemini is not installed")
	}
	home, _ := os.UserHomeDir()
	var person struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	b, err := os.ReadFile(filepath.Join(home, ".gemini", "settings.json"))
	if err != nil || json.Unmarshal(b, &person) != nil || person.MCPServers["telara"] == nil {
		t.Skip("not run: the person's Gemini settings configure no telara server")
	}
	store := interpreterStore(t)
	bin := buildRunner(t)
	work, _ := filepath.EvalSymlinks(t.TempDir())
	catalogRoot := filepath.Join(work, "catalog")
	dest := filepath.Join(catalogRoot, "gateway-read")
	os.MkdirAll(dest, 0o700)
	os.WriteFile(filepath.Join(dest, "primitive.yaml"), []byte(gatewayReadManifest), 0o600)
	os.WriteFile(filepath.Join(dest, "main.py"), []byte(gatewayReadProgram), 0o600)
	digest, manifest, err := packageDigest(dest)
	if err != nil {
		t.Fatal(err)
	}
	runArgs, _ := json.Marshal(map[string]any{
		"ref":    manifest.Metadata.Publisher + "/" + manifest.Metadata.Name + "@" + manifest.Metadata.Version,
		"digest": digest, "args": []string{"{}"},
	})
	journal := filepath.Join(work, "journal.jsonl")
	settings := map[string]any{
		"mcpServers": map[string]any{
			"tap": map[string]any{"command": bin, "args": []string{"serve", "--name", "tap", "--interpreters", store,
				"--runs", filepath.Join(work, "runs"), "--journal", journal, "--catalog-root", catalogRoot}},
			"telara": person.MCPServers["telara"],
		},
		"hooks": map[string]any{"AfterTool": []any{map[string]any{"matcher": ".*", "hooks": []any{map[string]any{
			"name": "tap", "type": "command", "command": geminiHookCommand(bin), "timeout": 600000}}}}},
	}
	geminiHome := filepath.Join(t.TempDir(), "gemini-home")
	env := append(os.Environ(), "GEMINI_CLI_HOME="+geminiHome)
	switch sa, key := os.Getenv("GEMINI_SERVICE_ACCOUNT_JSON"), os.Getenv("GEMINI_API_KEY"); {
	case sa != "":
		var acct struct {
			ProjectID string `json:"project_id"`
		}
		if json.Unmarshal([]byte(sa), &acct) != nil || acct.ProjectID == "" {
			t.Fatal("GEMINI_SERVICE_ACCOUNT_JSON is not a service account key")
		}
		keyFile := filepath.Join(t.TempDir(), "sa.json")
		os.WriteFile(keyFile, []byte(sa), 0o600)
		settings["security"] = map[string]any{"auth": map[string]any{"selectedType": "vertex-ai"}}
		env = append(env, "GOOGLE_APPLICATION_CREDENTIALS="+keyFile, "GOOGLE_GENAI_USE_VERTEXAI=true",
			"GOOGLE_CLOUD_PROJECT="+acct.ProjectID, "GOOGLE_CLOUD_LOCATION=us-central1")
	case key != "":
		settings["security"] = map[string]any{"auth": map[string]any{"selectedType": "gemini-api-key"}}
	default:
		t.Skip("not run: set GEMINI_SERVICE_ACCOUNT_JSON or GEMINI_API_KEY for Gemini to sign in")
	}
	sb, _ := json.MarshalIndent(settings, "", "  ")
	os.MkdirAll(filepath.Join(geminiHome, ".gemini"), 0o700)
	os.WriteFile(filepath.Join(geminiHome, ".gemini", "settings.json"), sb, 0o600)

	prompt := "Call the tool tap_run exactly once with these arguments: " + string(runArgs) +
		". If it hands back a run handle, call tap_result with it until the run finishes. Use no other tool. Then reply with exactly the text the run returned."
	cmd := exec.Command(gemini, "-p", prompt, "-m", "gemini-2.5-flash", "--approval-mode", "yolo", "--skip-trust", "-o", "json")
	cmd.Dir = work
	cmd.Env = env
	done := make(chan struct{})
	var out []byte
	go func() { out, err = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
	case <-time.After(8 * time.Minute):
		cmd.Process.Kill()
		<-done
		t.Fatal("gemini did not finish in 8 minutes")
	}
	text := string(out)
	j, _ := os.ReadFile(journal)
	t.Logf("gemini (%v): %.1500s\njournal:\n%.2000s", err, text, j)
	if strings.Contains(text, "cannot verify read effect") {
		t.Fatal("the read through the pinned dispatcher was refused: the gateway catalog was not lent")
	}
	if !strings.Contains(strings.ReplaceAll(text, `\"`, `"`), `RESULT {"ok": true`) {
		t.Fatal("the primitive did not finish its read")
	}
	if !strings.Contains(string(j), `"tool":"telara_execute_action"`) {
		t.Fatal("the runner recorded no call to the gateway's dispatcher")
	}
}
