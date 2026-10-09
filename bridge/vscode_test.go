package bridge

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/bind"
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
					case "rules":
						res["ask"] = []any{"mcp_github_search_issues"}
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

func TestVSCodeAsksComesFromTheAutoApprovalSetting(t *testing.T) {
	var calls []map[string]any
	v, err := NewVSCode(fakeExtension(t, &calls))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	inv, _ := v.Inventory()
	if ask, err := v.Asks(inv[0]); err != nil || !ask {
		t.Errorf("a tool set to never auto-approve was not an ask rule: %v %v", ask, err)
	}
	if ask, _ := v.Asks(inv[1]); ask {
		t.Error("a tool not listed was an ask rule")
	}
}

func TestVSCodeAskKeysAreMatchedByReferenceName(t *testing.T) {
	tool := bind.Tool{Server: "github", Name: "mcp_github_search_issues"}
	for key, want := range map[string]bool{
		"mcp_github_search_issues": true,  // the editor's own name
		"github/search_issues":     true,  // server/tool reference name
		"github/*":                 true,  // a whole server
		"search_issues":            true,  // a bare reference name
		"gitlab/search_issues":     false, // another server's tool
		"github/create_issue":      false,
		"fetch":                    false,
		"search":                   false, // a part of a name is not the name
	} {
		if got := askKeyCovers(key, tool); got != want {
			t.Errorf("%q: %v, want %v", key, got, want)
		}
	}
}

func TestVSCodeAskKeyForABuiltInToolMatchesItsListedName(t *testing.T) {
	// Seen in a live VS Code 1.140: the editor lists run_task, and the setting's
	// own example key is runTask.
	if !askKeyCovers("runTask", bind.Tool{Server: "vscode", Name: "run_task"}) {
		t.Error("runTask did not match run_task")
	}
	if askKeyCovers("runTask", bind.Tool{Server: "vscode", Name: "get_task_output"}) {
		t.Error("runTask matched another tool")
	}
}

func TestVSCodeAskKeyMatchesAnMCPToolListedOnlyByItsPrefixedName(t *testing.T) {
	// A live VS Code 1.140 lists mcp_probe_search_issues with the tag "mcp" and no
	// server, so the bridge reports its server as "vscode".
	tool := bind.Tool{Server: "vscode", Name: "mcp_probe_search_issues"}
	for key, want := range map[string]bool{
		"probe/search_issues": true,
		"probe/*":             true,
		"other/search_issues": false,
		"probe/create_issue":  false,
	} {
		if got := askKeyCovers(key, tool); got != want {
			t.Errorf("%q: %v, want %v", key, got, want)
		}
	}
}

// VS Code lists an MCP tool as mcp_<server>_<tool> and tags it only "mcp".
// The runner names it by server and tool, as on every other client, and
// still calls it by VS Code's name.
func TestVSCodeNamesMCPToolsByServerAndTool(t *testing.T) {
	cases := []struct{ name, server, tool string }{
		{"mcp_telara-mcp_telara_execute_action", "telara-mcp", "telara_execute_action"},
		{"mcp_playwright_browser_navigate", "playwright", "browser_navigate"},
		{"run_task", "vscode", "run_task"},
		{"mcp_", "vscode", "mcp_"},
		{"mcp_onlyserver_", "vscode", "mcp_onlyserver_"},
	}
	for _, c := range cases {
		server, tool := splitEditorName(c.name, map[string]any{"name": c.name, "tags": []any{"mcp"}})
		if server != c.server || tool != c.tool {
			t.Errorf("%s: got %s / %s, want %s / %s", c.name, server, tool, c.server, c.tool)
		}
	}
	// A tag that names the server is used as given, also when VS Code shortened it.
	if s, n := splitEditorName("mcp_a_very_long_s_fetch", map[string]any{"tags": []any{"mcp:A Very Long Server"}}); s != "A Very Long Server" || n != "fetch" {
		t.Errorf("tagged long server: %s / %s", s, n)
	}
}

func TestVSCodeCallsAToolByTheEditorsName(t *testing.T) {
	var calls []map[string]any
	v, err := NewVSCode(fakeExtension(t, &calls))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	inv, err := v.Inventory()
	if err != nil {
		t.Fatal(err)
	}
	if inv[0].Server != "github" || inv[0].Name != "search_issues" {
		t.Fatalf("the MCP tool is %s / %s", inv[0].Server, inv[0].Name)
	}
	if _, err := v.Call(inv[0], nil); err != nil {
		t.Fatal(err)
	}
	if calls[0]["name"] != "mcp_github_search_issues" {
		t.Errorf("called %v, want VS Code's name", calls[0]["name"])
	}
}
