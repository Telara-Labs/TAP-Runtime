package main

import (
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/bind"
)

// VS Code lists every MCP tool under its own server name, with the MCP
// server's name as a prefix on the tool: Playwright MCP's browser_navigate is
// mcp_playwright_browser_navigate under "vscode". The runner still finds
// that browser, and calls it by the client's names.
func TestFindBrowserBackendsWithPrefixedToolNames(t *testing.T) {
	inv := []bind.Tool{
		{Server: "vscode", Name: "run_task"},
		{Server: "vscode", Name: "mcp_playwright_browser_navigate"},
		{Server: "vscode", Name: "mcp_playwright_browser_navigate_back"},
		{Server: "vscode", Name: "mcp_playwright_browser_evaluate"},
		{Server: "vscode", Name: "mcp_playwright_browser_click"},
		// A navigate without an evaluate is not a browser the runner drives.
		{Server: "vscode", Name: "mcp_other_browser_navigate"},
		// No prefix separator: not a prefixed Playwright tool.
		{Server: "vscode", Name: "xbrowser_evaluate"},
	}
	got := findBrowserBackends(inv)
	if len(got) != 1 || got[0].Kind != backendPlaywright || got[0].Server != "vscode" {
		t.Fatalf("backends = %+v, want one playwright backend on vscode", got)
	}
	var names []string
	for _, tool := range got[0].usedTools() {
		names = append(names, tool.Name)
	}
	if len(names) != 2 || names[0] != "mcp_playwright_browser_navigate" || names[1] != "mcp_playwright_browser_evaluate" {
		t.Fatalf("the runner would call %v, want the client's own tool names", names)
	}
}

// A server that offers both plain and prefixed names is matched by its plain
// names, once.
func TestFindBrowserBackendsPrefersPlainNames(t *testing.T) {
	inv := []bind.Tool{
		{Server: "pw", Name: "browser_navigate"},
		{Server: "pw", Name: "browser_evaluate"},
		{Server: "pw", Name: "x_browser_navigate"},
		{Server: "pw", Name: "x_browser_evaluate"},
	}
	got := findBrowserBackends(inv)
	if len(got) != 1 || got[0].tools["browser_navigate"].Name != "browser_navigate" {
		t.Fatalf("backends = %+v, want the plain-named server once", got)
	}
}
