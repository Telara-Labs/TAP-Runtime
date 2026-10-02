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
)

// TestLiveElicitationThroughClaudeCode runs the real runner as an MCP server
// inside the real Claude Code, asks Claude Code to call it, and answers the
// approval the way a person at that client would. Nothing here is a double:
// the prompt travels runner -> Claude Code -> its control channel and back.
//
// Claude Code is started headless, so its control channel stands where its
// window would be. That a window shows the same prompt is not tested here.
func TestLiveElicitationThroughClaudeCode(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude is not installed")
	}
	store := interpreterStore(t)
	bin := filepath.Join(t.TempDir(), "host")
	build := exec.Command("go", "build", "-o", bin, "./host")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the runner: %v\n%s", err, out)
	}
	pkg := writePackage(t, writeManifest, "echo approved-content > out/live.txt && echo written || echo refused\n")

	for _, c := range []struct {
		name    string
		answer  map[string]any
		written bool
	}{
		{"the person says yes", map[string]any{"action": "accept", "content": map[string]any{"approve": true, "limit": 1}}, true},
		{"the person says no", map[string]any{"action": "decline"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			work, _ := filepath.EvalSymlinks(t.TempDir())
			catalogRoot := filepath.Join(work, "catalog")
			identity := stageLivePackage(t, catalogRoot, pkg)
			cfg := filepath.Join(work, "mcp.json")
			j, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"tap": map[string]any{
				"command": bin, "args": []string{"serve", "--interpreters", store, "--runs", filepath.Join(work, "runs"), "--journal", filepath.Join(work, "journal.jsonl"), "--config-dir", filepath.Join(work, "config"), "--catalog-root", catalogRoot}}}})
			os.WriteFile(cfg, j, 0o600)

			cmd := exec.Command("claude", "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
				"--mcp-config", cfg, "--strict-mcp-config")
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
			var prompts []string
			// request sends a control request and returns its response,
			// answering any elicitation Claude Code forwards meanwhile.
			request := func(subtype string, fields map[string]any) (map[string]any, error) {
				n++
				rid := fmt.Sprintf("t%d", n)
				req := map[string]any{"subtype": subtype}
				for k, v := range fields {
					req[k] = v
				}
				send(map[string]any{"type": "control_request", "request_id": rid, "request": req})
				deadline := time.NewTimer(30 * time.Second)
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
						return nil, fmt.Errorf("%s: no answer within 30 seconds", subtype)
					}
					if m["type"] == "control_request" {
						r, _ := m["request"].(map[string]any)
						if r["subtype"] == "elicitation" {
							msg, _ := r["message"].(string)
							answer := c.answer
							if strings.Contains(msg, "for the first time on this machine") {
								// Whether to run the package is not the question under test.
								answer = map[string]any{"action": "accept", "content": map[string]any{"approve": true}}
							} else {
								prompts = append(prompts, fmt.Sprintf("from %v: %s", r["mcp_server_name"], msg))
							}
							send(map[string]any{"type": "control_response", "response": map[string]any{
								"subtype": "success", "request_id": m["request_id"], "response": answer}})
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

			if _, err := request("initialize", map[string]any{"hooks": map[string]any{}}); err != nil {
				t.Fatal(err)
			}
			connected := false
			for i := 0; i < 20 && !connected; i++ {
				r, err := request("mcp_status", nil)
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
			probe, err := request("mcp_call", map[string]any{"tool": "mcp__tap__tap_search", "arguments": map[string]any{"query": "writer"}})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("Claude Code TAP search: %v", probe)
			r, err := request("mcp_call", map[string]any{"tool": "mcp__tap__tap_run", "arguments": identity})
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(r)
			t.Logf("prompts Claude Code forwarded: %d", len(prompts))
			for _, p := range prompts {
				t.Logf("  %s", strings.ReplaceAll(p, "\n", " | "))
			}
			t.Logf("tool result: %.300s", body)

			if len(prompts) != 1 {
				t.Fatalf("the person was asked %d times, want 1", len(prompts))
			}
			if !strings.Contains(prompts[0], "out/live.txt") {
				t.Errorf("the prompt does not name the file: %s", prompts[0])
			}
			b, err := os.ReadFile(filepath.Join(work, "out", "live.txt"))
			if c.written && strings.TrimSpace(string(b)) != "approved-content" {
				t.Fatalf("approved, and the file holds %q (%v)", b, err)
			}
			if !c.written && err == nil {
				t.Fatalf("declined, and the file was written: %q", b)
			}
		})
	}
}

// TestLiveElicitationThroughCodex is the same proof through Codex. Codex
// forwards the runner's question on its app-server protocol as a request of
// its own, mcpServer/elicitation/request, which this test answers the way
// Codex's own window would.
func TestLiveElicitationThroughCodex(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex is not installed")
	}
	store := interpreterStore(t)
	bin := filepath.Join(t.TempDir(), "host")
	build := exec.Command("go", "build", "-o", bin, "./host")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the runner: %v\n%s", err, out)
	}
	pkg := writePackage(t, writeManifest, "echo approved-content > out/live.txt && echo written || echo refused\n")

	for _, c := range []struct {
		name    string
		answer  map[string]any
		written bool
	}{
		{"the person says yes", map[string]any{"action": "accept", "content": map[string]any{"approve": true, "limit": 1}}, true},
		{"the person says no", map[string]any{"action": "decline"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			work, _ := filepath.EvalSymlinks(t.TempDir())
			catalogRoot := filepath.Join(work, "catalog")
			identity := stageLivePackage(t, catalogRoot, pkg)
			args, _ := json.Marshal([]string{"serve", "--interpreters", store, "--runs", filepath.Join(work, "runs"), "--config-dir", filepath.Join(work, "config"), "--catalog-root", catalogRoot})
			// The runner is registered for this one process. No
			// configuration file is changed.
			cmd := exec.Command("codex", "-c", fmt.Sprintf("mcp_servers.tap.command=%q", bin),
				"-c", "mcp_servers.tap.args="+string(args), "app-server")
			cmd.Dir = work
			in, _ := cmd.StdinPipe()
			out, _ := cmd.StdoutPipe()
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { in.Close(); cmd.Process.Kill(); cmd.Wait() }()
			sc := bufio.NewScanner(out)
			sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
			send := func(v any) { b, _ := json.Marshal(v); in.Write(append(b, '\n')) }

			n := 0
			var prompts []string
			call := func(method string, params any) (map[string]any, error) {
				n++
				id := n + 1000
				send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
				deadline := time.Now().Add(90 * time.Second)
				for time.Now().Before(deadline) && sc.Scan() {
					var m map[string]any
					if json.Unmarshal(sc.Bytes(), &m) != nil {
						continue
					}
					if m["method"] == "mcpServer/elicitation/request" {
						p, _ := m["params"].(map[string]any)
						msg, _ := p["message"].(string)
						answer := c.answer
						if strings.Contains(msg, "for the first time on this machine") {
							answer = map[string]any{"action": "accept", "content": map[string]any{"approve": true}}
						} else {
							prompts = append(prompts, fmt.Sprintf("from %v: %s", p["serverName"], msg))
						}
						send(map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": answer})
						continue
					}
					if m["id"] == float64(id) && m["method"] == nil {
						if e, ok := m["error"]; ok && e != nil {
							return nil, fmt.Errorf("%s: %v", method, e)
						}
						r, _ := m["result"].(map[string]any)
						return r, nil
					}
				}
				return nil, fmt.Errorf("%s: no answer", method)
			}

			if _, err := call("initialize", map[string]any{"clientInfo": map[string]string{"name": "tap-test", "version": "0"}}); err != nil {
				t.Fatal(err)
			}
			r, err := call("thread/start", map[string]any{"ephemeral": true, "cwd": work})
			if err != nil {
				t.Fatal(err)
			}
			th, _ := r["thread"].(map[string]any)
			thread, _ := th["id"].(string)
			r, err = call("mcpServer/tool/call", map[string]any{"server": "tap", "tool": "tap_run",
				"arguments": identity, "threadId": thread})
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(r)
			t.Logf("prompts Codex forwarded: %d", len(prompts))
			for _, p := range prompts {
				t.Logf("  %s", strings.ReplaceAll(p, "\n", " | "))
			}
			t.Logf("tool result: %.300s", body)

			if len(prompts) != 1 {
				t.Fatalf("the person was asked %d times, want 1", len(prompts))
			}
			if !strings.Contains(prompts[0], "out/live.txt") {
				t.Errorf("the prompt does not name the file: %s", prompts[0])
			}
			b, err := os.ReadFile(filepath.Join(work, "out", "live.txt"))
			if c.written && strings.TrimSpace(string(b)) != "approved-content" {
				t.Fatalf("approved, and the file holds %q (%v)", b, err)
			}
			if !c.written && err == nil {
				t.Fatalf("declined, and the file was written: %q", b)
			}
		})
	}
}

func stageLivePackage(t *testing.T, catalogRoot, pkg string) map[string]any {
	t.Helper()
	sum := sha256.Sum256([]byte(pkg))
	dest := filepath.Join(catalogRoot, hex.EncodeToString(sum[:8]))
	if err := os.MkdirAll(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"primitive.yaml", "main.sh"} {
		body, err := os.ReadFile(filepath.Join(pkg, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dest, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	digest, manifest, err := packageDigest(dest)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"ref": manifest.Metadata.Publisher + "/" + manifest.Metadata.Name + "@" + manifest.Metadata.Version, "digest": digest}
}
