package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/bind"
	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

// browserClient stands in for a client that lends browsers. It is a test
// double for third-party programs (Claude in Chrome, Codex's browser,
// Playwright MCP), not for any part of the runner. answer decides each tool's
// reply from its arguments.
type browserClient struct {
	fakeBridge
	answer func(t bind.Tool, args map[string]any) string
}

func (c *browserClient) Call(t bind.Tool, args map[string]any) (string, error) {
	c.calls = append(c.calls, t.Server+"/"+t.Name)
	c.args = append(c.args, args)
	if c.answer != nil {
		if r := c.answer(t, args); r != "" {
			return r, nil
		}
	}
	return `{"ok":true}`, nil
}

// encoded is what the runner's page function returns for v.
func encoded(v any) string {
	j, _ := json.Marshal(map[string]any{"v": v})
	return "@@TAP@@" + base64.StdEncoding.EncodeToString(j) + "@@TAP@@"
}

func chromeInv(server string) []bind.Tool {
	var out []bind.Tool
	for _, n := range chromeToolNames {
		out = append(out, bind.Tool{Server: server, Name: n, Annotated: bind.Unknown})
	}
	return out
}

func playwrightInv(server string) []bind.Tool {
	return []bind.Tool{
		{Server: server, Name: "browser_navigate", Annotated: bind.Unknown},
		{Server: server, Name: "browser_evaluate", Annotated: bind.Unknown},
		{Server: server, Name: "browser_click", Annotated: bind.Unknown},
	}
}

func codexInv() []bind.Tool {
	return []bind.Tool{{Server: "cua_repl", Name: "js", Annotated: bind.Unknown}}
}

func browserDecl() []toolDecl {
	return []toolDecl{{Alias: "browser", Capability: browserCapability, Effect: "write"}}
}

func TestFindBrowserBackendsByToolsInPreferredOrder(t *testing.T) {
	var inv []bind.Tool
	inv = append(inv, playwrightInv("my-renamed-playwright")...)
	inv = append(inv, bind.Tool{Server: "docs", Name: "navigate"}) // a navigate alone is not a browser
	inv = append(inv, codexInv()...)
	inv = append(inv, chromeInv("claude-in-chrome")...)
	got := findBrowserBackends(inv)
	var kinds []string
	for _, b := range got {
		kinds = append(kinds, b.Kind+"@"+b.Server)
	}
	want := "claude-in-chrome@claude-in-chrome codex@cua_repl playwright@my-renamed-playwright"
	if strings.Join(kinds, " ") != want {
		t.Fatalf("backends = %v, want %s", kinds, want)
	}
}

func TestAdmitBrowser(t *testing.T) {
	t.Run("binds every browser the client lends", func(t *testing.T) {
		c := &browserClient{fakeBridge: fakeBridge{inv: append(chromeInv("claude-in-chrome"), playwrightInv("playwright")...)}}
		a, err := admit(browserDecl(), c)
		if err != nil {
			t.Fatal(err)
		}
		b := a.byAlias["browser"]
		if b == nil || b.browser == nil || len(b.browser.backends) != 2 || b.Tool != "claude-in-chrome or playwright" {
			t.Fatalf("bound %+v", b)
		}
	})
	t.Run("refuses a pin, a read declaration and a client with no browser", func(t *testing.T) {
		c := &browserClient{fakeBridge: fakeBridge{inv: chromeInv("claude-in-chrome")}}
		cases := map[string]toolDecl{
			"remove the pin":        {Alias: "b", Capability: browserCapability, Effect: "write", Pin: &mf.Pin{Server: "claude-in-chrome", Tool: "navigate"}},
			"declare effect: write": {Alias: "b", Capability: "local.me/" + browserCapability + "@1", Effect: "read"},
		}
		for want, d := range cases {
			if _, err := admit([]toolDecl{d}, c); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("%+v: err = %v, want %q", d, err, want)
			}
		}
		if _, err := admit(browserDecl(), gmail()); err == nil || !strings.Contains(err.Error(), "this client lends no browser") {
			t.Fatalf("err = %v", err)
		}
		opt := []toolDecl{{Alias: "browser", Capability: browserCapability, Effect: "write", Optional: true}}
		a, err := admit(opt, gmail())
		if err != nil || len(a.Skipped) != 1 {
			t.Fatalf("an optional browser with none lent: %v %+v", err, a)
		}
	})
	t.Run("passes over a browser whose tool the person denied", func(t *testing.T) {
		c := &browserClient{fakeBridge: fakeBridge{inv: append(chromeInv("claude-in-chrome"), playwrightInv("playwright")...),
			deny: map[string]bool{"claude-in-chrome/javascript_tool": true}}}
		a, err := admit(browserDecl(), c)
		if err != nil {
			t.Fatal(err)
		}
		if b := a.byAlias["browser"]; len(b.browser.backends) != 1 || b.browser.backends[0].Kind != backendPlaywright || len(b.Candidates) != 1 {
			t.Fatalf("bound %+v", b)
		}
	})
}

func TestUsesChromeForTheBrowserCapability(t *testing.T) {
	if !usesChrome(&mf.Manifest{Tools: browserDecl()}) {
		t.Fatal("a primitive declaring the runner's browser does not start Claude Code with --chrome")
	}
	if usesChrome(&mf.Manifest{Tools: []mf.Tool{{Alias: "s", Capability: "gmail.threads.search", Effect: "read"}}}) {
		t.Fatal("a primitive with no browser starts Claude Code with --chrome")
	}
}

// chromeClient answers like Claude in Chrome: one empty group whose window
// tab is visible, and page reads answered with page.
func chromeClient(page func(text string) string) *browserClient {
	c := &browserClient{fakeBridge: fakeBridge{inv: chromeInv("claude-in-chrome")}}
	c.answer = func(t bind.Tool, args map[string]any) string {
		switch t.Name {
		case "tabs_context_mcp":
			return `{"availableTabs":[{"tabId":4242,"title":"New Tab","url":"chrome://newtab/"}],"tabGroupId":1}`
		case "javascript_tool":
			return page(args["text"].(string))
		}
		return ""
	}
	return c
}

func call(a *admission, c *browserClient, approve bool, args map[string]any) reply {
	var j bytes.Buffer
	return callTool(a, c, request{Method: "call", Alias: "browser", Arguments: args}, approve, &j)
}

func TestBrowserWritesNeedApprovalAndReadsDoNot(t *testing.T) {
	c := chromeClient(func(string) string { return encoded("TAP Browser Counter") })
	a, err := admit(browserDecl(), c)
	if err != nil {
		t.Fatal(err)
	}
	nav := map[string]any{"op": "navigate", "url": "http://localhost:4173/counter.html"}
	if r := call(a, c, false, nav); !r.Gated {
		t.Fatalf("navigation ran without approval: %+v", r)
	}
	if len(c.calls) != 0 {
		t.Fatalf("a gated navigation reached the browser: %v", c.calls)
	}
	if r := call(a, c, true, nav); r.Exit != 0 || !strings.Contains(r.Result, `"backend":"claude-in-chrome"`) {
		t.Fatalf("approved navigation: %+v", r)
	}
	r := call(a, c, false, map[string]any{"op": "read", "function": "() => document.title"})
	if r.Gated || r.Result != `"TAP Browser Counter"` {
		t.Fatalf("read: %+v", r)
	}
	if r := call(a, c, false, map[string]any{"op": "act", "action": "click", "selector": "#add"}); !r.Gated {
		t.Fatalf("a click ran without approval: %+v", r)
	}
	if r := call(a, c, false, map[string]any{"op": "type", "text": "x"}); r.Refused == "" || r.Gated {
		t.Fatalf("an unknown op: %+v", r)
	}
	// The tab the runner opened is the one it navigates, and it closes it
	// when the run ends.
	if got := c.args[len(c.args)-2]["tabId"]; got != 4242 {
		t.Fatalf("navigated tab %v", got)
	}
	a.closeBrowsers()
	if last := c.calls[len(c.calls)-1]; last != "claude-in-chrome/tabs_close_mcp" {
		t.Fatalf("the run's tab was not closed: %v", c.calls)
	}
}

func TestBrowserPicksTheBackendSignedInToTheSite(t *testing.T) {
	inv := append(chromeInv("claude-in-chrome"), playwrightInv("playwright")...)
	signedIn := map[string]bool{}
	c := &browserClient{fakeBridge: fakeBridge{inv: inv}}
	c.answer = func(t bind.Tool, args map[string]any) string {
		switch t.Name {
		case "tabs_context_mcp":
			return `{"availableTabs":[{"tabId":7,"url":"chrome://newtab/"}]}`
		case "javascript_tool":
			return encoded(signedIn[backendChrome])
		case "browser_evaluate":
			return "### Result\n\"" + encoded(signedIn[backendPlaywright]) + "\""
		}
		return ""
	}
	nav := map[string]any{"op": "navigate", "url": "https://example.com/", "ready": "() => !!document.querySelector('nav')"}

	signedIn[backendPlaywright] = true
	a, _ := admit(browserDecl(), c)
	a.byAlias["browser"].browser.sleep = func(time.Duration) {}
	r := call(a, c, true, nav)
	var got struct {
		Backend string
		Ready   bool
		Tried   []map[string]any
	}
	json.Unmarshal([]byte(r.Result), &got)
	if got.Backend != backendPlaywright || !got.Ready || len(got.Tried) != 2 {
		t.Fatalf("navigate = %s", r.Result)
	}
	if !strings.Contains(strings.Join(c.calls, " "), "claude-in-chrome/tabs_close_mcp") {
		t.Fatalf("the tab opened on the browser that was not signed in was left open: %v", c.calls)
	}

	// Signed in nowhere: the first is kept so the program can say why.
	signedIn[backendPlaywright] = false
	c.calls = nil
	a, _ = admit(browserDecl(), c)
	r = call(a, c, true, nav)
	json.Unmarshal([]byte(r.Result), &got)
	if got.Backend != backendChrome || got.Ready {
		t.Fatalf("navigate = %s", r.Result)
	}

	// A browser the primitive names is used, and one the client does not
	// lend is refused.
	a, _ = admit(browserDecl(), c)
	r = call(a, c, true, map[string]any{"op": "navigate", "url": "https://example.com/", "backend": "codex"})
	if r.Exit == 0 || !strings.Contains(r.Stderr, "is not lent by this client") {
		t.Fatalf("an unlent backend: %+v", r)
	}
}

func TestChromeLongAnswersAreReadInParts(t *testing.T) {
	long := strings.Repeat("é中 item ", 400)
	whole := encoded(long)
	c := chromeClient(func(text string) string {
		switch {
		case strings.HasPrefix(text, "await "):
			return "@@TAPLEN@@" + strconv.Itoa(len(whole)) + "@@"
		case strings.Contains(text, ".slice("):
			var from, to int
			s := text[strings.Index(text, ".slice(")+7:]
			parts := strings.SplitN(strings.TrimRight(strings.SplitN(s, ")", 2)[0], " "), ",", 2)
			from, to = atoi(parts[0]), atoi(strings.TrimSpace(parts[1]))
			if to > len(whole) {
				to = len(whole)
			}
			return "@@TAPPART@@" + whole[from:to] + "@@TAPPART@@\n\nTab Context: ..."
		}
		return "ok"
	})
	a, _ := admit(browserDecl(), c)
	call(a, c, true, map[string]any{"op": "navigate", "url": "http://localhost:4173/"})
	r := call(a, c, true, map[string]any{"op": "read", "function": "() => 1"})
	var got string
	if err := json.Unmarshal([]byte(r.Result), &got); err != nil || got != long {
		t.Fatalf("long answer = %.80q (%v), %+v", got, err, r)
	}
	parts := 0
	for _, a := range c.args {
		if s, _ := a["text"].(string); strings.Contains(s, ".slice(") {
			parts++
		}
	}
	if want := (len(whole) + chromeAnswerLimit - 1) / chromeAnswerLimit; parts != want {
		t.Fatalf("read in %d parts, want %d", parts, want)
	}
}

func TestCodexActsThroughItsLocator(t *testing.T) {
	c := &browserClient{fakeBridge: fakeBridge{inv: codexInv()}}
	c.answer = func(t bind.Tool, args map[string]any) string {
		if strings.Contains(args["code"].(string), "playwright.evaluate(") {
			return `{"result":"` + encoded(map[string]any{"count": "1"}) + `"}`
		}
		return ""
	}
	a, _ := admit(browserDecl(), c)
	call(a, c, true, map[string]any{"op": "navigate", "url": "http://localhost:4173/counter.html"})
	call(a, c, true, map[string]any{"op": "act", "action": "click", "selector": "#add", "index": float64(2)})
	call(a, c, true, map[string]any{"op": "act", "action": "scroll_into_view", "selector": ".row"})
	r := call(a, c, true, map[string]any{"op": "read", "function": "() => ({count: '1'})"})
	if r.Result != `{"count":"1"}` {
		t.Fatalf("read = %+v", r)
	}
	var codes []string
	for _, a := range c.args {
		codes = append(codes, a["code"].(string))
	}
	all := strings.Join(codes, "\n")
	for _, want := range []string{"cua.createBrowserTab(", `.goto("http://localhost:4173/counter.html")`,
		`.playwright.locator("#add").nth(2).click()`, `.playwright.locator(".row").nth(0).evaluate(e => e.scrollIntoView({block: 'start'}))`} {
		if !strings.Contains(all, want) {
			t.Fatalf("no %q in the Codex calls:\n%s", want, all)
		}
	}
	a.closeBrowsers()
	if !strings.Contains(c.args[len(c.args)-1]["code"].(string), ".close()") {
		t.Fatalf("the Codex tab was not closed: %v", c.args[len(c.args)-1])
	}
}

func TestDecodeAnswer(t *testing.T) {
	if _, err := decodeAnswer("no answer here"); err == nil {
		t.Fatal("a reply without an answer decoded")
	}
	j, _ := json.Marshal(map[string]any{"e": "boom"})
	if _, err := decodeAnswer("@@TAP@@" + base64.StdEncoding.EncodeToString(j) + "@@TAP@@"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("a failed page function: %v", err)
	}
	// Playwright echoes the code it ran; the encoder's own markers in that
	// echo are not an answer.
	v, err := decodeAnswer("### Result\n\"" + encoded(3) + "\"\n### Ran Playwright code\nawait page.evaluate('" + wrapPageFunction("() => 3", nil) + "')")
	if err != nil || string(v) != "3" {
		t.Fatalf("decoded %s, %v", v, err)
	}
}

// TestPageFunctionWrapperInJavaScript runs the wrapper the runner sends in a
// real JavaScript engine, so the encoder is checked on text a browser gives
// it (accents, CJK, emoji) and a thrown error comes back as an error.
func TestPageFunctionWrapperInJavaScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	run := func(fn string, args any) (json.RawMessage, error) {
		out, err := exec.Command(node, "-e", "("+wrapPageFunction(fn, args)+")().then(s => process.stdout.write(s))").Output()
		if err != nil {
			t.Fatal(err)
		}
		return decodeAnswer(string(out))
	}
	v, err := run("async (a) => ({name: a.name + ' é中😀', n: a.n * 2, none: undefined})", map[string]any{"name": "Zoë", "n": 21})
	if err != nil || string(v) != `{"name":"Zoë é中😀","n":42}` {
		t.Fatalf("value = %s, %v", v, err)
	}
	if v, err := run("() => undefined", nil); err != nil || string(v) != "null" {
		t.Fatalf("undefined = %s, %v", v, err)
	}
	if _, err := run("() => { throw new Error('no element') }", nil); err == nil || !strings.Contains(err.Error(), "no element") {
		t.Fatalf("a throwing page function: %v", err)
	}
}

func atoi(s string) int {
	var n int
	json.Unmarshal([]byte(strings.TrimSpace(s)), &n)
	return n
}
