package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// recordingServer is an MCP server with one write tool, create_issue, that
// appends each call it receives to the file named by its first argument, so
// a test can see whether a write was dispatched at all.
const recordingServer = `import sys, json
log = sys.argv[1]
for line in sys.stdin:
    try: m = json.loads(line)
    except Exception: continue
    i = m.get("id")
    meth = m.get("method")
    if meth == "initialize":
        r = {"protocolVersion": m["params"]["protocolVersion"], "capabilities": {"tools": {}}, "serverInfo": {"name": "rec", "version": "1"}}
    elif meth == "tools/list":
        r = {"tools": [{"name": "create_issue", "description": "create an issue", "inputSchema": {"type": "object", "properties": {"title": {"type": "string"}}, "required": ["title"]}, "annotations": {"readOnlyHint": False, "destructiveHint": False}}]}
    elif meth == "tools/call":
        open(log, "a").write(json.dumps(m["params"].get("arguments")) + "\n")
        r = {"content": [{"type": "text", "text": "created ABC-1"}]}
    elif i is not None:
        r = {}
    else:
        continue
    print(json.dumps({"jsonrpc": "2.0", "id": i, "result": r}), flush=True)
`

const copilotWriteManifest = `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.test, name: copilot-write, version: 0.1.0}
execution: {entrypoint: main.py}
tools:
  - {alias: create, capability: tracker.issues.create, effect: write, pin: {server: rec, tool: create_issue}}
`

const copilotWriteProgram = `try:
    print('RESULT', tap.call('create', {'title': 'from a primitive'}))
except PermissionError as e:
    print('REFUSED', e)
`

// TestLiveCopilotWriteNeedsThePersonsYes runs a primitive that makes one
// write through Copilot CLI. Copilot CLI cannot show an MCP prompt, and the
// bridge answers Copilot's own permission request for the call the runner
// is making; that answer must never stand in for the person. Without the
// person's yes (what tap serve has when the client cannot ask) the write is
// refused before anything reaches Copilot; with it, the write is made once.
func TestLiveCopilotWriteNeedsThePersonsYes(t *testing.T) {
	if testing.Short() {
		t.Skip("runs copilot")
	}
	if _, err := exec.LookPath("copilot"); err != nil {
		t.Skip("not run: copilot is not installed")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("not run: python3 runs the recording MCP server")
	}
	store := interpreterStore(t)
	if _, _, _, err := obtain(store, "main.py"); err != nil {
		t.Skipf("the Python interpreter could not be obtained: %v", err)
	}
	for _, c := range []struct {
		name    string
		approve Approver
		written bool
	}{
		{"no answer from the person", func(Ask) Grant { return Grant{} }, false},
		{"the person says yes", func(Ask) Grant { return Grant{OK: true, Limit: 1} }, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			srv := filepath.Join(home, "rec.py")
			calls := filepath.Join(home, "calls.jsonl")
			os.WriteFile(srv, []byte(recordingServer), 0o600)
			cfg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"rec": map[string]any{
				"type": "local", "command": "python3", "args": []string{srv, calls}, "tools": []string{"*"}}}})
			os.WriteFile(filepath.Join(home, "mcp-config.json"), cfg, 0o600)
			t.Setenv("COPILOT_HOME", home)
			inDir(t)
			pkg := t.TempDir()
			os.WriteFile(filepath.Join(pkg, "primitive.yaml"), []byte(copilotWriteManifest), 0o600)
			os.WriteFile(filepath.Join(pkg, "main.py"), []byte(copilotWriteProgram), 0o600)
			res, err := Run(context.Background(), Options{
				Package: pkg, Client: "copilot", Journal: io.Discard, InterpDir: store, RunsDir: t.TempDir(), Approve: c.approve,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("run %s\n%s\n%s", res.RunID, res.Stdout, res.Stderr)
			got, _ := os.ReadFile(calls)
			n := strings.Count(string(got), "\n")
			if c.written {
				if n != 1 || !strings.Contains(res.Stdout, "created ABC-1") {
					t.Fatalf("approved, and the server received %d write(s); stdout %q", n, res.Stdout)
				}
				return
			}
			if n != 0 {
				t.Fatalf("not approved, and the server received %d write(s): %s", n, got)
			}
			if !strings.Contains(res.Stdout, "REFUSED") || res.Calls != 0 {
				t.Fatalf("not approved, and the write was not refused before dispatch (calls %d): %s", res.Calls, res.Stdout)
			}
		})
	}
}
