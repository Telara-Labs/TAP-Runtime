package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/bind"
	"github.com/Telara-Labs/TAP-Runtime/bridge"
	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

// chromeFixtureProgram opens a tab of its own in Claude in Chrome, reads the
// local fixture page with page JavaScript, presses its button, and closes the
// tab. It is the navigate + evaluate shape a client-agnostic browser
// primitive uses for this backend.
const chromeFixtureProgram = `import json, re, sys
base = json.loads(sys.argv[1])['base_url']
print('BOUND', json.dumps(sorted(tap.tools())))
tap.call('tabs', {'createIfEmpty': True})
made = json.dumps(tap.call('new_tab', {}), default=str)
tab = int(re.search(r'Tab ID:\s*(\d+)', made).group(1))
try:
    tap.call('nav', {'url': base + '/counter.html', 'tabId': tab})
    read = "'[heading ' + document.querySelector('h1').textContent + ' | count ' + document.getElementById('count').textContent + ']'"
    def page():
        text = json.dumps(tap.call('js', {'action': 'javascript_exec', 'tabId': tab, 'text': read}), default=str)
        m = re.search(r'\[heading ([^|\]]*) \| count (\d+)\]', text)
        if not m:
            raise RuntimeError('the page did not answer: ' + text[:400])
        return m.groups()
    print('BEFORE heading=%s count=%s' % page())
    tap.call('js', {'action': 'javascript_exec', 'tabId': tab, 'text': "document.getElementById('add').click()"})
    print('AFTER heading=%s count=%s' % page())
finally:
    tap.call('close_tab', {'tabId': tab})
`

const chromeFixtureManifest = `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.test, name: chrome-fixture, version: 0.1.0}
execution: {entrypoint: main.py}
tools:
  - {alias: tabs, capability: browser.tabs.list, effect: read, pin: {server: claude-in-chrome, tool: tabs_context_mcp}}
  - {alias: new_tab, capability: browser.tabs.create, effect: write, pin: {server: claude-in-chrome, tool: tabs_create_mcp}}
  - {alias: nav, capability: browser.page.navigate, effect: write, pin: {server: claude-in-chrome, tool: navigate}}
  - {alias: js, capability: browser.page.evaluate, effect: write, pin: {server: claude-in-chrome, tool: javascript_tool}}
  - {alias: close_tab, capability: browser.tabs.close, effect: write, pin: {server: claude-in-chrome, tool: tabs_close_mcp}}
`

// browserFixturePort is where examples/browser-support/README.md serves the
// fixture site.
const browserFixturePort = 4173

func chromeTools(inv []bind.Tool) []string {
	var out []string
	for _, t := range inv {
		if t.Server == chromeServer {
			out = append(out, t.Name)
		}
	}
	return out
}

// TestLiveClaudeInChromeBindsOnlyForAPrimitiveThatPinsIt starts the real
// Claude Code the way the runner does for each kind of primitive. A primitive
// that pins Claude in Chrome gets a copy started with --chrome, binds the
// extension's tools and drives a local fixture page through them. A
// primitive that does not pin it gets a copy whose inventory has no Claude in
// Chrome tools, because it was not started with --chrome.
//
// It needs Claude Code, Chrome running with the Claude in Chrome extension,
// and localhost allowed in the extension's site permissions; it skips, saying
// which, when one is missing. It opens one tab and closes it.
func TestLiveClaudeInChromeBindsOnlyForAPrimitiveThatPinsIt(t *testing.T) {
	if _, err := bridge.ClaudeExecutable(); err != nil {
		t.Skip("claude is not installed")
	}

	t.Run("a primitive that does not pin it starts Claude Code without --chrome", func(t *testing.T) {
		m := &mf.Manifest{Tools: []mf.Tool{{Alias: "pw", Optional: true, Pin: &mf.Pin{Server: "plugin:playwright:playwright", Tool: "browser_navigate"}}}}
		br, err := openBridge("claude", bridge.Proc{}, m)
		if err != nil {
			t.Fatal(err)
		}
		defer br.Close()
		inv, err := br.Inventory()
		if err != nil {
			t.Fatal(err)
		}
		if got := chromeTools(inv); len(got) != 0 {
			t.Fatalf("Claude in Chrome tools were loaded for a primitive that does not pin them: %v", got)
		}
	})

	t.Run("a primitive that pins it binds and drives a local page", func(t *testing.T) {
		br, err := openBridge("claude", bridge.Proc{}, &mf.Manifest{Tools: []mf.Tool{{Alias: "js", Pin: &mf.Pin{Server: chromeServer, Tool: "javascript_tool"}}}})
		if err != nil {
			t.Fatal(err)
		}
		inv, err := br.Inventory()
		br.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(chromeTools(inv)) == 0 {
			t.Skip("Claude in Chrome is not connected: start Chrome with the Claude in Chrome extension")
		}

		store := interpreterStore(t)
		if _, _, _, err := obtain(store, "main.py"); err != nil {
			t.Skipf("the Python interpreter could not be obtained: %v", err)
		}
		// Claude in Chrome's site permission is per origin, port included.
		// Serve on the port the browser examples use, so one permission the
		// person gave for local development covers this test.
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(browserFixturePort))
		if err != nil {
			t.Skipf("port %d is in use; the fixture is served there: %v", browserFixturePort, err)
		}
		srv := &http.Server{Handler: http.FileServer(http.Dir(filepath.Join(repoRoot, "examples", "browser-support", "site")))}
		go srv.Serve(ln)
		defer srv.Close()
		base := "http://localhost:" + strconv.Itoa(browserFixturePort)

		inDir(t)
		pkg := t.TempDir()
		os.WriteFile(filepath.Join(pkg, "primitive.yaml"), []byte(chromeFixtureManifest), 0o644)
		os.WriteFile(filepath.Join(pkg, "main.py"), []byte(chromeFixtureProgram), 0o644)
		res, err := Run(context.Background(), Options{
			Package: pkg, Args: []string{`{"base_url":"` + base + `"}`}, Client: "claude",
			Journal: io.Discard, InterpDir: store, RunsDir: t.TempDir(),
			Approve: func(Ask) Grant { return Grant{OK: true, Limit: Unlimited} },
		})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(res.Stderr, "Permission denied") {
			// The extension asks the person in Chrome; nobody answered.
			t.Skipf("Claude in Chrome was not allowed on localhost; allow it in the extension's site permissions (run %s): %s", res.RunID, res.Stderr)
		}
		t.Logf("run %s\n%s", res.RunID, res.Stdout)
		for _, b := range res.Admission.Bindings {
			if b.Server != chromeServer || !b.Pinned {
				t.Errorf("%s bound to %s/%s (pinned %v), want the pinned Claude in Chrome tool", b.Alias, b.Server, b.Tool, b.Pinned)
			}
		}
		if !strings.Contains(res.Stdout, "BEFORE heading=TAP Browser Counter count=0") || !strings.Contains(res.Stdout, "AFTER heading=TAP Browser Counter count=1") {
			t.Fatalf("the fixture was not read, or the button press made by page JavaScript did not take effect:\n%s\n%s", res.Stdout, res.Stderr)
		}
	})
}

// TestLiveCodexBrowserRefusesARunnerOutsideACodexTurn calls Codex's browser
// tool (cua_repl/js) through the real app-server from a runner that no Codex
// turn started. Codex lends the tool in its inventory but refuses to reach a
// browser;
// the runner says where the call can succeed instead of passing on the tool's
// manual as the error.
func TestLiveCodexBrowserRefusesARunnerOutsideACodexTurn(t *testing.T) {
	if os.Getenv("CODEX_THREAD_ID") != "" {
		t.Skip("running inside a Codex session, which has a turn to give")
	}
	br, err := openBridge("codex", bridge.Proc{}, nil)
	if err != nil {
		t.Skipf("codex is not available: %v", err)
	}
	defer br.Close()
	inv, err := br.Inventory()
	if err != nil {
		t.Fatal(err)
	}
	var js *bind.Tool
	for i := range inv {
		if inv[i].Server == "cua_repl" && inv[i].Name == "js" {
			js = &inv[i]
		}
	}
	if js == nil {
		t.Skip("Codex's browser (cua_repl) is not installed")
	}
	// Plain JavaScript runs without a turn; reaching a browser does not.
	out, err := br.Call(*js, map[string]any{"code": "nodeRepl.write(JSON.stringify(await cua.listBrowsers({emit: false})))", "title": "List browsers"})
	if err == nil {
		t.Fatalf("Codex's browser listed browsers for a call made outside any Codex turn: %.300s", out)
	}
	if !strings.Contains(err.Error(), "only inside a Codex session's turn") || len(err.Error()) > 400 {
		t.Fatalf("the refusal does not say where the call can run, or carries the tool's manual (%d bytes): %.400s", len(err.Error()), err)
	}
}
