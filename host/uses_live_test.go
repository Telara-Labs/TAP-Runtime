package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/bridge"
)

// noteTracker is an MCP server with one read and one write. It records every
// call it receives in the file named by its first argument.
const noteTracker = `import sys, json
log = sys.argv[1]
for line in sys.stdin:
    try: m = json.loads(line)
    except Exception: continue
    i, meth = m.get("id"), m.get("method")
    if meth == "initialize":
        r = {"protocolVersion": m["params"]["protocolVersion"], "capabilities": {"tools": {}}, "serverInfo": {"name": "tracker", "version": "1"}}
    elif meth == "tools/list":
        obj = {"type": "object", "properties": {}}
        r = {"tools": [
            {"name": "search_issues", "description": "Search issues", "inputSchema": obj, "annotations": {"readOnlyHint": True}},
            {"name": "create_note", "description": "Add a note", "inputSchema": obj, "annotations": {"readOnlyHint": False, "destructiveHint": False}},
        ]}
    elif meth == "tools/call":
        with open(log, "a") as f: f.write(m["params"]["name"] + "\n")
        r = {"content": [{"type": "text", "text": json.dumps({"issues": [{"key": "ABC-12"}], "ok": True})}]}
    elif i is not None:
        r = {}
    else:
        continue
    print(json.dumps({"jsonrpc": "2.0", "id": i, "result": r}), flush=True)
`

// TestLiveAPrimitiveUsesAPrimitiveThroughClaudeCode runs, inside the real
// Claude Code, a primitive that runs another one. The child reads through a
// connected tool and makes one change the person approves. Claude Code is
// started headless, so its control channel stands where its window would be.
func TestLiveAPrimitiveUsesAPrimitiveThroughClaudeCode(t *testing.T) {
	claude, err := bridge.ClaudeExecutable()
	if err != nil {
		t.Skip("claude is not installed")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed, and the connected tool is a python program")
	}
	bin := filepath.Join(t.TempDir(), "host")
	build := exec.Command("go", "build", "-ldflags", testRunnerLinkFlags(t), "-o", bin, "./host")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the runner: %v\n%s", err, out)
	}
	work, _ := filepath.EvalSymlinks(t.TempDir())
	catalogRoot := filepath.Join(work, "catalog")
	toolLog := filepath.Join(work, "tracker.log")
	trackerPy := filepath.Join(work, "tracker.py")
	os.WriteFile(trackerPy, []byte(noteTracker), 0o600)
	// The copy of Claude Code the runner starts reads the project's servers.
	project, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"tracker": map[string]any{"command": "python3", "args": []string{trackerPy, toolLog}}}})
	os.WriteFile(filepath.Join(work, ".mcp.json"), project, 0o600)

	stage := func(pkg pkgSpec) (dir string, identity map[string]any) {
		src := pkg.write(t)
		sum := sha256.Sum256([]byte(src))
		dir = filepath.Join(catalogRoot, hex.EncodeToString(sum[:8]))
		os.MkdirAll(dir, 0o700)
		for _, name := range []string{"primitive.yaml", "main.wasm"} {
			b, err := os.ReadFile(filepath.Join(src, name))
			if err != nil {
				t.Fatal(err)
			}
			os.WriteFile(filepath.Join(dir, name), b, 0o600)
		}
		digest, m, err := packageDigest(dir)
		if err != nil {
			t.Fatal(err)
		}
		return dir, map[string]any{"ref": m.Metadata.Publisher + "/" + m.Metadata.Name + "@" + m.Metadata.Version, "digest": digest}
	}
	kidDir, _ := stage(pkgSpec{name: "kid", rest: `tools:
  - {alias: search, capability: tracker.issues.search, effect: read, pin: {server: tracker, tool: search_issues}}
  - {alias: note, capability: tracker.notes.create, effect: write, pin: {server: tracker, tool: create_note}}
`})
	_, identity := stage(pkgSpec{name: "boss", rest: "uses:\n" + use(t, "kid", kidDir, "write")})
	args := script(t, map[string]any{"steps": steps(runStep("kid", map[string]any{"steps": steps(
		map[string]any{"method": "call", "alias": "search", "arguments": map[string]any{}},
		map[string]any{"method": "call", "alias": "note", "arguments": map[string]any{}},
	), "seq": true}))})
	identity["args"] = []string{args}

	cfg := filepath.Join(work, "mcp.json")
	j, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"tap": map[string]any{
		"command": bin, "args": []string{"serve", "--runs", filepath.Join(work, "runs"), "--config-dir", filepath.Join(work, "config"), "--catalog-root", catalogRoot}}}})
	os.WriteFile(cfg, j, 0o600)

	cmd := exec.Command(claude, "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--mcp-config", cfg, "--strict-mcp-config")
	cmd.Dir = work
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { in.Close(); cmd.Process.Kill(); cmd.Wait() }()
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	messages := make(chan map[string]any, 64)
	go func() {
		defer close(messages)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				messages <- m
			}
		}
	}()
	send := func(v any) { b, _ := json.Marshal(v); in.Write(append(b, '\n')) }

	n := 0
	var trust, writes []string
	request := func(subtype string, fields map[string]any, wait time.Duration) (map[string]any, error) {
		n++
		rid := fmt.Sprintf("t%d", n)
		req := map[string]any{"subtype": subtype}
		for k, v := range fields {
			req[k] = v
		}
		send(map[string]any{"type": "control_request", "request_id": rid, "request": req})
		deadline := time.NewTimer(wait)
		defer deadline.Stop()
		for {
			var m map[string]any
			select {
			case next, ok := <-messages:
				if !ok {
					return nil, fmt.Errorf("%s: client closed its output", subtype)
				}
				m = next
			case <-deadline.C:
				return nil, fmt.Errorf("%s: no answer within %s", subtype, wait)
			}
			if m["type"] == "control_request" {
				r, _ := m["request"].(map[string]any)
				if r["subtype"] == "elicitation" {
					msg, _ := r["message"].(string)
					if strings.Contains(msg, "for the first time on this machine") {
						trust = append(trust, msg)
					} else {
						writes = append(writes, msg)
					}
					send(map[string]any{"type": "control_response", "response": map[string]any{
						"subtype": "success", "request_id": m["request_id"], "response": map[string]any{"action": "accept", "content": map[string]any{"approve": true, "limit": 1}}}})
				}
				continue
			}
			if m["type"] != "control_response" {
				continue
			}
			r, _ := m["response"].(map[string]any)
			if r["request_id"] != rid {
				continue
			}
			if r["subtype"] == "error" {
				return nil, fmt.Errorf("%s: %v", subtype, r["error"])
			}
			res, _ := r["response"].(map[string]any)
			return res, nil
		}
	}

	if _, err := request("initialize", map[string]any{"hooks": map[string]any{}}, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	connected := false
	for i := 0; i < 20 && !connected; i++ {
		r, err := request("mcp_status", nil, 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		servers, _ := r["mcpServers"].([]any)
		for _, s := range servers {
			if sm, _ := s.(map[string]any); sm["name"] == "tap" && sm["status"] == "connected" {
				connected = true
			}
		}
		if !connected {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if !connected {
		t.Fatal("Claude Code did not connect to the runner")
	}
	r, err := request("mcp_call", map[string]any{"tool": "mcp__tap__tap_run", "arguments": identity}, 120*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(r)
	t.Logf("tool result: %.600s", body)

	if len(trust) != 1 || !strings.Contains(trust[0], "uses kid") || !strings.Contains(trust[0], "tracker") {
		t.Fatalf("the first-run question did not show the child: %q", trust)
	}
	if len(writes) != 1 || !strings.Contains(writes[0], `"boss › kid"`) {
		t.Fatalf("the person's question for the change did not name the call path: %q", writes)
	}
	logged, _ := os.ReadFile(toolLog)
	if got := strings.Fields(string(logged)); strings.Join(got, ",") != "search_issues,create_note" {
		t.Fatalf("the connected tool received %v, want one read then one approved write", got)
	}
}
