package bridge

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"

	"gitlab.com/telara-labs/tap-runtime/bind"
)

// fakeExtension answers on a socket the way the TAP extension does.
func fakeExtension(t *testing.T, calls *[]map[string]any) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tapvs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "x.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					var req map[string]any
					json.Unmarshal(sc.Bytes(), &req)
					res := map[string]any{"id": req["id"]}
					switch req["op"] {
					case "hello":
						res["version"] = "1.138.0"
					case "tools":
						res["tools"] = []any{
							map[string]any{"name": "mcp_github_search_issues", "tags": []any{"mcp:github"}, "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}},
							map[string]any{"name": "mcp_tap_tap_run", "tags": []any{}},
							map[string]any{"name": "copilot_readFile", "tags": []any{"vscode_editing"}},
						}
					case "call":
						*calls = append(*calls, req)
						if req["name"] == "broken" {
							res["error"] = "the tool failed"
						} else {
							res["text"] = `{"total_count":7}`
						}
					}
					b, _ := json.Marshal(res)
					c.Write(append(b, '\n'))
				}
			}(c)
		}
	}()
	return sock
}

func TestVSCodeBridge(t *testing.T) {
	var calls []map[string]any
	v, err := NewVSCode(fakeExtension(t, &calls))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if name, ver := v.Client(); name != "vscode" || ver != "1.138.0" {
		t.Errorf("client %s %s", name, ver)
	}
	inv, err := v.Inventory()
	if err != nil {
		t.Fatal(err)
	}
	if len(inv) != 2 {
		t.Fatalf("the runner's own tool must be left out, got %v", inv)
	}
	if inv[0].Server != "github" || inv[0].Schema == nil || inv[1].Server != "vscode" {
		t.Errorf("servers and schemas: %+v", inv)
	}
	out, err := v.Call(inv[0], map[string]any{"q": "is:open"})
	if err != nil || out != `{"total_count":7}` {
		t.Fatalf("%q %v", out, err)
	}
	if calls[0]["name"] != "mcp_github_search_issues" || calls[0]["input"].(map[string]any)["q"] != "is:open" {
		t.Errorf("the call sent: %v", calls[0])
	}
	if _, err := v.Call(bindTool("broken"), nil); err == nil || err.Error() != "the tool failed" {
		t.Errorf("a failed tool: %v", err)
	}
}

func TestVSCodeUnreachable(t *testing.T) {
	if _, err := NewVSCode(filepath.Join(t.TempDir(), "none.sock")); err == nil {
		t.Fatal("a missing extension was not reported")
	}
}

func bindTool(name string) bind.Tool { return bind.Tool{Server: "x", Name: name} }
