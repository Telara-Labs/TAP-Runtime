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

var liveSessionRelay = flag.Bool("live-session-relay", false, "run a primitive from real Kilo and OpenCode sessions through the TAP relay plugin (one model run per client)")

const relayReadManifest = `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.test, name: relay-read, version: 0.1.0}
interface:
  inputSchema: {type: object, properties: {}}
execution: {entrypoint: main.sh}
tools:
  - {alias: search, capability: tracker.issues.search, effect: read, pin: {server: tracker, tool: search_issues}}
`

// readOnlyTracker is an MCP server whose search_issues says it only reads.
const readOnlyTracker = `import sys, json
for line in sys.stdin:
    try: m = json.loads(line)
    except Exception: continue
    i, meth = m.get("id"), m.get("method")
    if meth == "initialize":
        r = {"protocolVersion": m["params"]["protocolVersion"], "capabilities": {"tools": {}}, "serverInfo": {"name": "tracker", "version": "1"}}
    elif meth == "tools/list":
        r = {"tools": [{"name": "search_issues", "description": "Search issues", "inputSchema": {"type": "object", "properties": {"jql": {"type": "string"}}, "required": ["jql"]}, "annotations": {"readOnlyHint": True}}]}
    elif meth == "tools/call":
        r = {"content": [{"type": "text", "text": json.dumps({"issues": [{"key": "ABC-12"}, {"key": "ABC-13"}]})}]}
    elif i is not None:
        r = {}
    else:
        continue
    print(json.dumps({"jsonrpc": "2.0", "id": i, "result": r}), flush=True)
`

const relayReadProgram = `echo "first open issue: $(tap call search '{"jql":"status = open"}' | jq -r '.issues[0].key')"
`

// TestLiveSessionRelay runs, from a real Kilo or OpenCode session, a
// primitive that reads from an MCP server the session has connected. The
// session loads the TAP relay plugin from the project, and the runner the
// session starts reaches the session's own connection through it: no
// server of the runner's own, no port, no password, and no change to the
// client: the relay opens its own connection to the session's configured
// server. For Kilo the session's PATH has no kilo, so the runner cannot fall
// back to starting a Kilo server; the read can only come through the relay.
func TestLiveSessionRelay(t *testing.T) {
	if !*liveSessionRelay {
		t.Skip("pass -live-session-relay to run real Kilo and OpenCode sessions")
	}
	// The model is Gemini on Vertex AI, signed in with a service account:
	// it needs no account in either client.
	sa := os.Getenv("GEMINI_SERVICE_ACCOUNT_JSON")
	var acct struct {
		ProjectID string `json:"project_id"`
	}
	if sa == "" || json.Unmarshal([]byte(sa), &acct) != nil || acct.ProjectID == "" {
		t.Skip("not run: set GEMINI_SERVICE_ACCOUNT_JSON for the sessions' model")
	}
	keyFile := filepath.Join(t.TempDir(), "sa.json")
	os.WriteFile(keyFile, []byte(sa), 0o600)
	vertex := []string{"GOOGLE_APPLICATION_CREDENTIALS=" + keyFile, "GOOGLE_VERTEX_PROJECT=" + acct.ProjectID,
		"GOOGLE_CLOUD_PROJECT=" + acct.ProjectID, "GOOGLE_VERTEX_LOCATION=global", "GOOGLE_CLOUD_LOCATION=global"}
	store := interpreterStore(t)
	bin := buildRunner(t)
	for _, c := range []struct {
		client, dotDir, config, model string
		env                           []string
	}{
		{"kilo", ".kilo", "kilo.json", "google-vertex/gemini-3-flash-preview", nil},
		{"opencode", ".opencode", "opencode.json", "google-vertex/gemini-3-flash-preview", nil},
	} {
		t.Run(c.client, func(t *testing.T) {
			agent, err := exec.LookPath(c.client)
			if err != nil {
				t.Skipf("not run: %s is not installed", c.client)
			}
			work, _ := filepath.EvalSymlinks(t.TempDir())
			plugin := filepath.Join(work, c.dotDir, "plugin", relayPluginName)
			os.MkdirAll(filepath.Dir(plugin), 0o755)
			os.WriteFile(plugin, relayPlugin, 0o644)
			catalogRoot := filepath.Join(work, "catalog")
			dest := filepath.Join(catalogRoot, "relay-read")
			os.MkdirAll(dest, 0o700)
			os.WriteFile(filepath.Join(dest, "primitive.yaml"), []byte(relayReadManifest), 0o600)
			os.WriteFile(filepath.Join(dest, "main.sh"), []byte(relayReadProgram), 0o600)
			digest, manifest, err := packageDigest(dest)
			if err != nil {
				t.Fatal(err)
			}
			journal := filepath.Join(work, "journal.jsonl")
			trackerPy := filepath.Join(work, "tracker.py")
			os.WriteFile(trackerPy, []byte(readOnlyTracker), 0o600)
			// The session may only call tools: no shell, no edits, no web.
			cfg, _ := json.Marshal(map[string]any{"permission": map[string]any{"bash": "deny", "edit": "deny", "webfetch": "deny"}, "mcp": map[string]any{
				// Not "tap": the person's own configuration may already have
				// a server by that name, which the client would merge with it.
				"taprelay": map[string]any{"type": "local", "command": []string{bin, "serve", "--name", "taprelay", "--interpreters", store,
					"--runs", filepath.Join(work, "runs"), "--journal", journal, "--catalog-root", catalogRoot}},
				"tracker": map[string]any{"type": "local", "command": []string{"python3", trackerPy}},
			}})
			os.WriteFile(filepath.Join(work, c.config), cfg, 0o600)
			runArgs, _ := json.Marshal(map[string]any{
				"ref":    manifest.Metadata.Publisher + "/" + manifest.Metadata.Name + "@" + manifest.Metadata.Version,
				"digest": digest, "args": []string{"{}"},
			})
			prompt := "Call the tool named taprelay_tap_run exactly once with these arguments: " + string(runArgs) +
				". If it hands back a run handle, call taprelay_tap_result with it until the run finishes. Use no other tool. Then reply with exactly the text the run returned."
			cmd := exec.Command(agent, "run", "-m", c.model, prompt)
			cmd.Dir = work
			// An empty configuration home for this process, so the person's own
			// TAP server does not sit beside the test's (the model may pick it).
			env := append(append(os.Environ(), vertex...), c.env...)
			// OpenCode and Kilo take the project from PWD, not the process's
			// working directory.
			cfgHome := t.TempDir()
			env = append(env, "XDG_CONFIG_HOME="+cfgHome, "PWD="+work)
			if c.client == "kilo" {
				env = withoutDirOnPath(env, filepath.Dir(agent))
			}
			cmd.Env = env
			done := make(chan struct{})
			var out []byte
			go func() { out, err = cmd.CombinedOutput(); close(done) }()
			select {
			case <-done:
			case <-time.After(6 * time.Minute):
				cmd.Process.Kill()
				<-done
				t.Fatalf("%s did not finish in 6 minutes:\n%.2000s", c.client, out)
			}
			text := string(out)
			j, _ := os.ReadFile(journal)
			t.Logf("%s (%v):\n%.2500s\njournal:\n%.1500s", c.client, err, text, j)
			if strings.Contains(text, "cannot run a tool for another program") {
				t.Skipf("not run: this %s has no call-tool route", c.client)
			}
			if !strings.Contains(text, "first open issue: ABC-12") {
				t.Fatalf("the read through the session relay did not complete")
			}
			if !strings.Contains(string(j), `"tool":"search_issues"`) {
				t.Fatal("the runner recorded no call to the session's tracker server")
			}
		})
	}
}

// withoutDirOnPath drops dir from PATH in env.
func withoutDirOnPath(env []string, dir string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			var keep []string
			for _, d := range filepath.SplitList(v) {
				if filepath.Clean(d) != filepath.Clean(dir) {
					keep = append(keep, d)
				}
			}
			kv = "PATH=" + strings.Join(keep, string(os.PathListSeparator))
		}
		out = append(out, kv)
	}
	return out
}
