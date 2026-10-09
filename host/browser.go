package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/bind"
	"github.com/Telara-Labs/TAP-Runtime/bridge"
	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

// The browser is a capability the runner provides itself. A primitive
// declares one tool with this capability and no pin:
//
//	tools:
//	  - {alias: browser, capability: tap.browser.use, effect: write}
//
// and calls it with one operation at a time:
//
//	{"op": "navigate", "url": "...", "ready": "<page function>", "backend": "..."}
//	{"op": "read", "function": "<page function>", "args": <any JSON>}
//	{"op": "act", "action": "click" | "scroll_into_view", "selector": "...", "index": 0}
//	{"op": "act", "action": "insert_text", "selector": "...", "index": 0, "text": "..."}
//	{"op": "wait", "ms": 500}
//	{"op": "close"}
//
// The runner maps these onto whichever browser the client already lends:
// Claude in Chrome, Codex's browser, or a Playwright MCP server the person
// configured. It never starts a browser or a browser server of its own. Each
// backend's quirks (how a tab is opened, how page JavaScript is run, how long
// an answer may be, what page JavaScript may do) are handled here once, so a
// primitive carries no per-client code.
const browserCapability = "tap.browser.use"

// isBrowserCapability says whether a declared capability is the runner's
// browser, in the short form or the full publisher/name@version form.
func isBrowserCapability(label string) bool {
	return mf.CapabilityName(label) == browserCapability
}

// Backend kinds, in the order the runner prefers them when the primitive
// gives no way to tell them apart: the person's own signed-in Chrome through
// Claude in Chrome, then Codex's browser, then a Playwright MCP server.
const (
	backendChrome     = "claude-in-chrome"
	backendCodex      = "codex"
	backendPlaywright = "playwright"
)

var backendRank = map[string]int{backendChrome: 0, backendCodex: 1, backendPlaywright: 2}

// browserBackend is one browser the client lends: its kind, the server that
// provides it and the tools the runner drives it with.
type browserBackend struct {
	Kind   string `json:"kind"`
	Server string `json:"server"`
	// Asked is set when the person's client asks before one of its tools is
	// used; every operation on it then needs approval, reads included.
	Asked bool `json:"client_asks,omitempty"`
	tools map[string]bind.Tool
}

func (b browserBackend) name() string { return b.Kind + " (" + b.Server + ")" }

// chromeTools are the Claude in Chrome tools the runner uses.
var chromeToolNames = []string{"tabs_context_mcp", "tabs_create_mcp", "tabs_close_mcp", "navigate", "javascript_tool"}

// findBrowserBackends lists the browsers in a client's inventory, best first.
// A backend is recognised by the tools it offers, not by what its server is
// called, so a server the person renamed is still found.
func findBrowserBackends(inv []bind.Tool) []browserBackend {
	byServer := map[string]map[string]bind.Tool{}
	var order []string
	for _, t := range inv {
		if byServer[t.Server] == nil {
			byServer[t.Server] = map[string]bind.Tool{}
			order = append(order, t.Server)
		}
		byServer[t.Server][t.Name] = t
	}
	var out []browserBackend
	for _, server := range order {
		tools := byServer[server]
		has := func(names ...string) bool {
			for _, n := range names {
				if _, ok := tools[n]; !ok {
					return false
				}
			}
			return true
		}
		switch {
		case has(chromeToolNames...):
			out = append(out, browserBackend{Kind: backendChrome, Server: server, tools: tools})
		case has("js") && strings.Contains(strings.ToLower(server), "cua"):
			// Codex's browser REPL.
			out = append(out, browserBackend{Kind: backendCodex, Server: server, tools: tools})
		case has("browser_navigate", "browser_evaluate"):
			out = append(out, browserBackend{Kind: backendPlaywright, Server: server, tools: tools})
		default:
			out = append(out, prefixedPlaywright(server, tools)...)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return backendRank[out[i].Kind] < backendRank[out[j].Kind] })
	return out
}

// prefixedPlaywright finds Playwright servers that a client lists under its
// own server name with a prefix on each tool: VS Code lists Playwright MCP's
// browser_navigate as mcp_playwright_browser_navigate under "vscode". Tools
// sharing a prefix are one server; the backend maps the plain names onto the
// client's names, which are what a call uses.
func prefixedPlaywright(server string, tools map[string]bind.Tool) []browserBackend {
	byPrefix := map[string]map[string]bind.Tool{}
	var prefixes []string
	for name, t := range tools {
		i := strings.Index(name, "browser_")
		if i <= 0 || name[i-1] != '_' {
			continue
		}
		prefix := name[:i]
		if byPrefix[prefix] == nil {
			byPrefix[prefix] = map[string]bind.Tool{}
			prefixes = append(prefixes, prefix)
		}
		byPrefix[prefix][name[i:]] = t
	}
	sort.Strings(prefixes)
	var out []browserBackend
	for _, p := range prefixes {
		plain := byPrefix[p]
		_, nav := plain["browser_navigate"]
		_, eval := plain["browser_evaluate"]
		if nav && eval {
			out = append(out, browserBackend{Kind: backendPlaywright, Server: server, tools: plain})
		}
	}
	return out
}

// usedTools are the tools of a backend the runner calls.
func (b browserBackend) usedTools() []bind.Tool {
	var names []string
	switch b.Kind {
	case backendChrome:
		names = chromeToolNames
	case backendCodex:
		names = []string{"js"}
	case backendPlaywright:
		names = []string{"browser_navigate", "browser_evaluate"}
	}
	out := make([]bind.Tool, 0, len(names))
	for _, n := range names {
		out = append(out, b.tools[n])
	}
	return out
}

// admitBrowser resolves a declaration of the runner's browser against the
// client's inventory. It refuses a pin (the runner, not a server, provides
// the capability) and a read declaration (navigating and acting are writes).
func admitBrowser(d toolDecl, b bridge.Bridge, inv []bind.Tool) (bd binding, refusal string, absent bool, err error) {
	bd = binding{Alias: d.Alias, Capability: d.Capability, Declared: d.Effect, Adapter: "browser", Score: 1}
	if d.Pin != nil {
		return bd, "the browser is provided by the runner; remove the pin from " + browserCapability, false, nil
	}
	if bind.Rank(bind.Effect(d.Effect)) < bind.Rank(bind.Write) {
		return bd, "a browser tool navigates and acts on pages, which are writes; declare effect: write", false, nil
	}
	var usable []browserBackend
	var dropped []string
	for _, be := range findBrowserBackends(inv) {
		denied := ""
		for _, t := range be.usedTools() {
			no, err := b.Denied(t)
			if err != nil {
				return bd, "", false, fmt.Errorf("reading the user's permission rules: %w", err)
			}
			if no {
				denied = t.Name
				break
			}
			if ak, ok := b.(bridge.Asker); ok {
				asks, err := ak.Asks(t)
				if err != nil {
					return bd, "", false, fmt.Errorf("reading the user's permission rules: %w", err)
				}
				be.Asked = be.Asked || asks
			}
		}
		if denied != "" {
			dropped = append(dropped, fmt.Sprintf("%s (the user has denied %s)", be.name(), denied))
			continue
		}
		usable = append(usable, be)
	}
	if len(usable) == 0 {
		why := "this client lends no browser: connect Claude in Chrome (Claude Code started with --chrome), use Codex's browser from a Codex session, or configure a Playwright MCP server in the client"
		if len(dropped) > 0 {
			why += "; passed over: " + strings.Join(dropped, "; ")
		}
		return bd, why, true, nil
	}
	kinds := make([]string, 0, len(usable))
	for _, be := range usable {
		kinds = append(kinds, be.Kind)
	}
	bd.Server, bd.Tool = "browser", strings.Join(kinds, " or ")
	bd.Annotated = string(bind.Write)
	bd.tool = bind.Tool{Server: bd.Server, Name: bd.Tool, Annotated: bind.Write}
	bd.Candidates = dropped
	bd.browser = &browserDriver{br: b, backends: usable}
	return bd, "", false, nil
}

// browserOpEffect is what one browser operation does. Opening a page and
// acting on it are writes; reading a page, waiting and closing the runner's
// own tab are reads. Page JavaScript runs only in the runner's own tab, which
// an approved navigation opened.
func browserOpEffect(args map[string]any) (string, error) {
	op, _ := args["op"].(string)
	switch op {
	case "navigate", "act":
		return string(bind.Write), nil
	case "read", "wait", "close":
		return string(bind.Read), nil
	}
	return "", fmt.Errorf("unknown browser op %q; use navigate, read, act, wait or close", op)
}

// callBrowser is callTool for the runner's browser.
func callBrowser(bd *binding, rq request, approve bool, journal io.Writer) reply {
	entry := map[string]any{"ts": time.Now().UTC().Format(time.RFC3339Nano), "alias": rq.Alias, "server": bd.Server, "tool": bd.Tool}
	record := func(outcome string, extra map[string]any) {
		entry["outcome"] = outcome
		for k, v := range extra {
			entry[k] = v
		}
		j, _ := json.Marshal(entry)
		journal.Write(append(j, '\n'))
	}
	op, _ := rq.Arguments["op"].(string)
	entry["op"] = op
	effect, err := browserOpEffect(rq.Arguments)
	if err != nil {
		record("refused_arguments", map[string]any{"error": err.Error()})
		return reply{Refused: err.Error()}
	}
	d := bd.browser
	if effect == string(bind.Read) && d.asked() {
		effect = string(bind.Write)
	}
	entry["effect"] = effect
	if effect != string(bind.Read) && !approve {
		logf("  GATED    call %s browser %s  (%s, no approval)", rq.Alias, op, effect)
		record("gated", nil)
		return reply{Refused: effect + " browser operation needs approval", Gated: true}
	}
	t0 := time.Now()
	res, err := d.do(rq.Arguments)
	if cur := d.current(); cur != "" {
		entry["backend"] = cur
	}
	if err != nil {
		logf("  FAILED   call %s browser %s: %v", rq.Alias, op, err)
		record("failed", map[string]any{"error": err.Error()})
		return reply{Exit: 1, Stderr: err.Error()}
	}
	logf("  call     %s browser %s on %s [%s] %dB, %s", rq.Alias, op, d.current(), effect, len(res), time.Since(t0).Round(time.Millisecond))
	record("ran", map[string]any{"result_bytes": len(res), "ms": time.Since(t0).Milliseconds()})
	return reply{Result: res}
}

// browserDriver holds the run's browser: the backends the client lends and
// the session the run has open on one of them.
type browserDriver struct {
	mu       sync.Mutex
	br       bridge.Bridge
	backends []browserBackend
	cur      *browserSession
	// sleep is time.Sleep; tests replace it.
	sleep func(time.Duration)
}

// browserSession is a tab the runner opened on one backend.
type browserSession struct {
	be     browserBackend
	tab    int    // Claude in Chrome
	handle string // Codex: the REPL variable holding the tab
	own    bool
}

func (d *browserDriver) asked() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cur != nil {
		return d.cur.be.Asked
	}
	for _, be := range d.backends {
		if be.Asked {
			return true
		}
	}
	return false
}

func (d *browserDriver) current() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cur == nil {
		return ""
	}
	return d.cur.be.Kind
}

func (d *browserDriver) wait(t time.Duration) {
	if d.sleep != nil {
		d.sleep(t)
		return
	}
	time.Sleep(t)
}

// Bounds on what a primitive may ask the runner to wait for.
const (
	maxBrowserWait  = 30 * time.Second
	defaultReadyFor = 20 * time.Second
	maxReadyFor     = 120 * time.Second
	readyPoll       = 500 * time.Millisecond
)

// do runs one operation. Operations are run one at a time.
func (d *browserDriver) do(args map[string]any) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	op, _ := args["op"].(string)
	switch op {
	case "wait":
		ms, _ := args["ms"].(float64)
		w := time.Duration(ms) * time.Millisecond
		if w <= 0 || w > maxBrowserWait {
			return "", fmt.Errorf("wait takes ms between 1 and %d", maxBrowserWait.Milliseconds())
		}
		d.wait(w)
		return `{"ok":true}`, nil
	case "close":
		if d.cur != nil {
			d.closeSession(d.cur)
			d.cur = nil
		}
		return `{"ok":true}`, nil
	case "navigate":
		return d.navigate(args)
	case "read":
		if d.cur == nil {
			return "", fmt.Errorf("read before navigate: open a page first")
		}
		fn, _ := args["function"].(string)
		if strings.TrimSpace(fn) == "" {
			return "", fmt.Errorf("read needs function: the source of a page function, such as (args) => document.title")
		}
		v, err := d.evaluate(d.cur, fn, args["args"])
		if err != nil {
			return "", err
		}
		return string(v), nil
	case "act":
		if d.cur == nil {
			return "", fmt.Errorf("act before navigate: open a page first")
		}
		return d.act(d.cur, args)
	}
	return "", fmt.Errorf("unknown browser op %q", op)
}

// navigate opens url. The first navigation chooses the backend: the one the
// primitive names, else the first, in the order the backends are preferred,
// whose page passes the primitive's ready check (for example, that the person
// is signed in to the site). When none passes, the first is kept, so the
// program can read the page to say why.
func (d *browserDriver) navigate(args map[string]any) (string, error) {
	url, _ := args["url"].(string)
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return "", fmt.Errorf("navigate needs an http or https url")
	}
	ready, _ := args["ready"].(string)
	readyFor := defaultReadyFor
	if ms, ok := args["ready_timeout_ms"].(float64); ok && ms > 0 {
		readyFor = time.Duration(ms) * time.Millisecond
		if readyFor > maxReadyFor {
			readyFor = maxReadyFor
		}
	}
	if d.cur != nil {
		if err := d.goTo(d.cur, url); err != nil {
			return "", err
		}
		r := map[string]any{"ok": true, "backend": d.cur.be.Kind}
		if ready != "" {
			ok, why := d.awaitReady(d.cur, ready, readyFor)
			r["ready"] = ok
			if why != "" {
				r["note"] = why
			}
		}
		j, _ := json.Marshal(r)
		return string(j), nil
	}
	cands := d.backends
	if want, _ := args["backend"].(string); want != "" && want != "auto" {
		cands = nil
		for _, be := range d.backends {
			if be.Kind == want || be.Server == want {
				cands = append(cands, be)
			}
		}
		if len(cands) == 0 {
			return "", fmt.Errorf("the browser %q is not lent by this client (it lends: %s)", want, d.kinds())
		}
	}
	type try struct {
		Backend string `json:"backend"`
		Ready   any    `json:"ready,omitempty"`
		Error   string `json:"error,omitempty"`
	}
	var tried []try
	var keep *browserSession
	for _, be := range cands {
		s, err := d.open(be)
		if err == nil {
			err = d.goTo(s, url)
			if err != nil {
				d.closeSession(s)
			}
		}
		if err != nil {
			logf("  browser  %s not usable: %v", be.name(), err)
			tried = append(tried, try{Backend: be.Kind, Error: shorten(err.Error(), 300)})
			continue
		}
		if ready == "" {
			keep = s
			break
		}
		ok, why := d.awaitReady(s, ready, readyFor)
		tried = append(tried, try{Backend: be.Kind, Ready: ok, Error: why})
		if ok {
			if keep != nil {
				d.closeSession(keep)
			}
			keep = s
			break
		}
		if keep == nil {
			keep = s
		} else {
			d.closeSession(s)
		}
	}
	if keep == nil {
		j, _ := json.Marshal(tried)
		return "", fmt.Errorf("no browser this client lends could open the page: %s", j)
	}
	d.cur = keep
	r := map[string]any{"ok": true, "backend": keep.be.Kind}
	if ready != "" {
		r["ready"] = len(tried) > 0 && tried[len(tried)-1].Backend == keep.be.Kind && tried[len(tried)-1].Ready == true
	}
	if len(tried) > 1 || (len(tried) == 1 && tried[0].Error != "") {
		r["tried"] = tried
	}
	j, _ := json.Marshal(r)
	return string(j), nil
}

func (d *browserDriver) kinds() string {
	var k []string
	for _, be := range d.backends {
		k = append(k, be.Kind)
	}
	return strings.Join(k, ", ")
}

// awaitReady evaluates the ready function until it answers true or false. A
// null or undefined answer means the page has not decided yet. The runner
// waits between reads; the page function itself never waits.
func (d *browserDriver) awaitReady(s *browserSession, fn string, within time.Duration) (bool, string) {
	deadline := time.Now().Add(within)
	last := ""
	for {
		v, err := d.evaluate(s, fn, nil)
		if err != nil {
			last = shorten(err.Error(), 200)
		} else {
			switch strings.TrimSpace(string(v)) {
			case "true":
				return true, ""
			case "false":
				return false, ""
			}
		}
		if !time.Now().Before(deadline) {
			if last == "" {
				last = "the page did not decide within " + within.String()
			}
			return false, last
		}
		d.wait(readyPoll)
	}
}

func (d *browserDriver) call(be browserBackend, tool string, args map[string]any) (string, error) {
	t, ok := be.tools[tool]
	if !ok {
		return "", fmt.Errorf("%s has no tool %s", be.name(), tool)
	}
	return d.br.Call(t, args)
}

var (
	chromeContextTab = regexp.MustCompile(`tabId\\?"?\s*:\s*(\d+)`)
	chromeCreatedTab = regexp.MustCompile(`(?i)tab\s*id\\?"?\s*[:=]?\s*(\d+)`)
)

// open starts a session: a tab of the runner's own, closed when the run
// ends, never a tab the person or another task is using.
func (d *browserDriver) open(be browserBackend) (*browserSession, error) {
	s := &browserSession{be: be}
	switch be.Kind {
	case backendChrome:
		// When the session's tab group is empty, this makes a new window
		// whose one tab is visible. Chrome throttles timers and skips
		// scroll-triggered loading in background tabs, so that tab is the
		// one to use; otherwise a new tab is made in the group.
		ctx, err := d.call(be, "tabs_context_mcp", map[string]any{"createIfEmpty": true})
		if err != nil {
			return nil, err
		}
		ids := chromeContextTab.FindAllStringSubmatch(ctx, -1)
		if len(uniqueIDs(ids)) == 1 && strings.Contains(ctx, "chrome://newtab") {
			s.tab, _ = strconv.Atoi(ids[0][1])
		} else {
			made, err := d.call(be, "tabs_create_mcp", map[string]any{})
			if err != nil {
				return nil, err
			}
			m := chromeCreatedTab.FindStringSubmatch(made)
			if m == nil {
				return nil, fmt.Errorf("Claude in Chrome made no tab: %s", shorten(made, 200))
			}
			s.tab, _ = strconv.Atoi(m[1])
		}
		s.own = true
	case backendCodex:
		var b [6]byte
		rand.Read(b[:])
		s.handle = "tapBrowser" + hex.EncodeToString(b[:])
		code := fmt.Sprintf("globalThis.%s = await cua.createBrowserTab((await cua.getBrowser()).browserId);", s.handle)
		if _, err := d.call(be, "js", map[string]any{"code": code, "title": "Open a browser tab"}); err != nil {
			return nil, err
		}
		s.own = true
	case backendPlaywright:
		// Playwright MCP drives one page of the browser it was configured
		// with; there is no tab to open.
	}
	return s, nil
}

func uniqueIDs(m [][]string) map[string]bool {
	out := map[string]bool{}
	for _, x := range m {
		out[x[1]] = true
	}
	return out
}

func (d *browserDriver) goTo(s *browserSession, url string) error {
	var err error
	switch s.be.Kind {
	case backendChrome:
		_, err = d.call(s.be, "navigate", map[string]any{"url": url, "tabId": s.tab})
	case backendCodex:
		_, err = d.call(s.be, "js", map[string]any{"code": fmt.Sprintf("await globalThis.%s.goto(%s);", s.handle, jsString(url)), "title": "Open " + url, "timeout_ms": 60000})
	case backendPlaywright:
		_, err = d.call(s.be, "browser_navigate", map[string]any{"url": url})
	}
	return err
}

// closeSession closes a tab the runner opened. Failing to close is logged,
// not raised: the run's answer does not depend on it.
func (d *browserDriver) closeSession(s *browserSession) {
	if !s.own {
		return
	}
	var err error
	switch s.be.Kind {
	case backendChrome:
		_, err = d.call(s.be, "tabs_close_mcp", map[string]any{"tabId": s.tab})
	case backendCodex:
		_, err = d.call(s.be, "js", map[string]any{"code": fmt.Sprintf("await globalThis.%s.close(); delete globalThis.%s;", s.handle, s.handle), "title": "Close the tab"})
	}
	if err != nil {
		logf("  browser  closing the tab on %s: %v", s.be.name(), err)
	}
	s.own = false
}

// closeAll closes the session's tab when the run ends.
func (d *browserDriver) closeAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cur != nil {
		d.closeSession(d.cur)
		d.cur = nil
	}
}

// pageEncoder turns a string into its UTF-8 bytes in base64 between markers,
// in plain JavaScript: Codex runs page functions in a scope without
// encodeURIComponent, btoa or globalThis, and the markers let the answer be
// found in whatever text a backend wraps around it.
const pageEncoder = `s=>{const b=[];for(let i=0;i<s.length;i++){let c=s.charCodeAt(i);if(c>=0xd800&&c<0xdc00&&i+1<s.length){c=0x10000+((c-0xd800)<<10)+(s.charCodeAt(++i)-0xdc00);}if(c<0x80)b.push(c);else if(c<0x800)b.push(0xc0|c>>6,0x80|c&63);else if(c<0x10000)b.push(0xe0|c>>12,0x80|c>>6&63,0x80|c&63);else b.push(0xf0|c>>18,0x80|c>>12&63,0x80|c>>6&63,0x80|c&63);}const t='ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/';let o='';for(let i=0;i<b.length;i+=3){const n=(b[i]<<16)|((b[i+1]||0)<<8)|(b[i+2]||0);o+=t[n>>18&63]+t[n>>12&63]+(i+1<b.length?t[n>>6&63]:'=')+(i+2<b.length?t[n&63]:'=');}return '@@TAP@@'+o+'@@TAP@@';}`

// wrapPageFunction makes the page function the runner sends: it calls the
// primitive's function with its arguments, catches what it throws, and
// returns the JSON of either, encoded.
func wrapPageFunction(fn string, args any) string {
	a, _ := json.Marshal(args)
	return "async () => {const __enc=" + pageEncoder + ";let __r;try{__r={v:await (" + fn + ")(" + string(a) + ")};}catch(e){__r={e:String(e&&e.message||e)};}" +
		"let __s;try{__s=JSON.stringify(__r);}catch(e){__s=JSON.stringify({e:'the page function returned something that is not JSON: '+e});}return __enc(__s);}"
}

var (
	encodedAnswer = regexp.MustCompile(`@@TAP@@([A-Za-z0-9+/=]+)@@TAP@@`)
	storedAnswer  = regexp.MustCompile(`@@TAPLEN@@(\d+)@@`)
	answerPart    = regexp.MustCompile(`@@TAPPART@@([A-Za-z0-9+/=@]*)@@TAPPART@@`)
)

// decodeAnswer finds the encoded answer in a backend's reply and returns the
// page function's value as JSON.
func decodeAnswer(text string) (json.RawMessage, error) {
	all := encodedAnswer.FindAllStringSubmatch(text, -1)
	if len(all) == 0 {
		return nil, fmt.Errorf("the browser returned no answer: %s", shorten(text, 300))
	}
	raw, err := base64.StdEncoding.DecodeString(all[len(all)-1][1])
	if err != nil {
		return nil, fmt.Errorf("the browser's answer is not readable: %w", err)
	}
	var r struct {
		V json.RawMessage `json:"v"`
		E *string         `json:"e"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("the browser's answer is not JSON: %w", err)
	}
	if r.E != nil {
		return nil, fmt.Errorf("the page function failed: %s", *r.E)
	}
	if len(r.V) == 0 {
		return json.RawMessage("null"), nil
	}
	return r.V, nil
}

// chromeAnswerLimit is the longest answer read from Claude in Chrome in one
// call. Claude in Chrome cuts a tool answer off at about 1000 characters, so
// a longer answer is kept in the page and read in parts.
const chromeAnswerLimit = 800

// evaluate runs a page function on the session's page and returns its value.
func (d *browserDriver) evaluate(s *browserSession, fn string, args any) (json.RawMessage, error) {
	wrapped := wrapPageFunction(fn, args)
	var text string
	var err error
	switch s.be.Kind {
	case backendChrome:
		key := "__tapAnswer"
		program := fmt.Sprintf("await (async () => {const s = await (%s)(); if (s.length <= %d) return s; window.%s = s; return '@@TAPLEN@@' + s.length + '@@';})()", wrapped, chromeAnswerLimit, key)
		text, err = d.call(s.be, "javascript_tool", map[string]any{"action": "javascript_exec", "tabId": s.tab, "text": program})
		if err == nil {
			if m := storedAnswer.FindStringSubmatch(text); m != nil && !encodedAnswer.MatchString(text) {
				text, err = d.chromeParts(s, key, m[1])
			}
		}
	case backendCodex:
		text, err = d.call(s.be, "js", map[string]any{"code": fmt.Sprintf("nodeRepl.write(await globalThis.%s.playwright.evaluate(%s));", s.handle, wrapped), "title": "Read the page", "timeout_ms": 60000})
	case backendPlaywright:
		text, err = d.call(s.be, "browser_evaluate", map[string]any{"function": wrapped})
	}
	if err != nil {
		return nil, err
	}
	return decodeAnswer(text)
}

// chromeParts reads an answer Claude in Chrome would cut off, in parts.
func (d *browserDriver) chromeParts(s *browserSession, key, length string) (string, error) {
	n, _ := strconv.Atoi(length)
	var sb strings.Builder
	for at := 0; at < n; at += chromeAnswerLimit {
		program := fmt.Sprintf("'@@TAPPART@@' + window.%s.slice(%d, %d) + '@@TAPPART@@'", key, at, at+chromeAnswerLimit)
		text, err := d.call(s.be, "javascript_tool", map[string]any{"action": "javascript_exec", "tabId": s.tab, "text": program})
		if err != nil {
			return "", err
		}
		m := answerPart.FindStringSubmatch(text)
		if m == nil {
			return "", fmt.Errorf("Claude in Chrome lost part of the answer: %s", shorten(text, 200))
		}
		sb.WriteString(m[1])
	}
	d.call(s.be, "javascript_tool", map[string]any{"action": "javascript_exec", "tabId": s.tab, "text": "delete window." + key + "; 'ok'"})
	return sb.String(), nil
}

// pageAct clicks or scrolls in page JavaScript (Claude in Chrome and
// Playwright). A click lands where a pointer would: on the element at the
// centre of the target once it is scrolled into view, as Playwright's own
// click does.
// insert_text replaces the element's content with the text the way typing
// or pasting would, so the page's own input handlers see the change: an
// editable region gets an insertText edit over its whole content, a form
// field gets its value set through the native setter and an input event.
const pageAct = `(a) => {const e=document.querySelectorAll(a.selector)[a.index];if(!e)throw new Error('no element matches '+a.selector+' at index '+a.index);if(a.action==='scroll_into_view'){e.scrollIntoView({block:'start'});return true;}e.scrollIntoView({block:'center'});if(a.action==='insert_text'){e.focus();if(e.isContentEditable){const r=document.createRange();r.selectNodeContents(e);const sel=getSelection();sel.removeAllRanges();sel.addRange(r);if(!document.execCommand('insertText',false,a.text))throw new Error('the page refused the text');return true;}const d=Object.getOwnPropertyDescriptor(Object.getPrototypeOf(e),'value');if(!d||!d.set)throw new Error(a.selector+' does not take text');d.set.call(e,a.text);e.dispatchEvent(new Event('input',{bubbles:true}));e.dispatchEvent(new Event('change',{bubbles:true}));return true;}const r=e.getBoundingClientRect();let t=document.elementFromPoint(r.left+r.width/2,r.top+r.height/2);if(!t||!(t===e||e.contains(t)))t=e;t.click();return true;}`

func (d *browserDriver) act(s *browserSession, args map[string]any) (string, error) {
	action, _ := args["action"].(string)
	selector, _ := args["selector"].(string)
	index := 0
	if f, ok := args["index"].(float64); ok {
		index = int(f)
	}
	if action != "click" && action != "scroll_into_view" && action != "insert_text" {
		return "", fmt.Errorf("act takes action click, scroll_into_view or insert_text")
	}
	text, hasText := args["text"].(string)
	if action == "insert_text" && !hasText {
		return "", fmt.Errorf("insert_text needs text")
	}
	if selector == "" || index < 0 {
		return "", fmt.Errorf("act needs a CSS selector and an index of 0 or more")
	}
	if s.be.Kind == backendCodex {
		// Codex runs page functions read-only: an element has no click
		// method and scroll positions cannot be set. Actions go through its
		// Playwright locator; it has no scroll method, but a function the
		// locator evaluates on its element may scroll it into view.
		call := "click()"
		switch action {
		case "scroll_into_view":
			call = "evaluate(e => e.scrollIntoView({block: 'start'}))"
		case "insert_text":
			call = "fill(" + jsString(text) + ")"
		}
		code := fmt.Sprintf("await globalThis.%s.playwright.locator(%s).nth(%d).%s;", s.handle, jsString(selector), index, call)
		if _, err := d.call(s.be, "js", map[string]any{"code": code, "title": action + " " + selector, "timeout_ms": 30000}); err != nil {
			return "", err
		}
		return `{"ok":true}`, nil
	}
	if _, err := d.evaluate(s, pageAct, map[string]any{"action": action, "selector": selector, "index": index, "text": text}); err != nil {
		return "", err
	}
	return `{"ok":true}`, nil
}

func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// closeBrowsers closes every tab the run's browser tools opened.
func (a *admission) closeBrowsers() {
	if a == nil {
		return
	}
	for i := range a.Bindings {
		if d := a.Bindings[i].browser; d != nil {
			d.closeAll()
		}
	}
}
