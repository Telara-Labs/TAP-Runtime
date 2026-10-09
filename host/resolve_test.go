package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/bind"
	"github.com/Telara-Labs/TAP-Runtime/bridge"
	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

// unlistedBridge is a client that, like Kilo, reports which servers it is
// configured with and whether each is connected, and lists no tools: its
// inventory is the tools the runner gives it, on connected servers.
type unlistedBridge struct {
	*operationBridge
	servers []bridge.ConfiguredServer
	given   []bind.Tool
}

func (u *unlistedBridge) Client() (string, string) { return "kilo", "7.8.3" }
func (u *unlistedBridge) HasSchemas() bool         { return false }
func (u *unlistedBridge) UsePins(t []bind.Tool)    { u.given = append([]bind.Tool(nil), t...) }
func (u *unlistedBridge) ConfiguredServers() ([]bridge.ConfiguredServer, error) {
	return u.servers, nil
}
func (u *unlistedBridge) Inventory() ([]bind.Tool, error) {
	var out []bind.Tool
	for _, t := range u.given {
		if s, ok := findServer(u.servers, t.Server); ok && usableServer(s) {
			out = append(out, t)
		}
	}
	return out, nil
}

func newUnlisted(servers ...bridge.ConfiguredServer) *unlistedBridge {
	ob := calendarOperationBridge()
	ob.inv = nil
	return &unlistedBridge{operationBridge: ob, servers: servers}
}

func connected(name, command, url string) bridge.ConfiguredServer {
	return bridge.ConfiguredServer{Name: name, Command: command, URL: url, Connected: true, StatusKnown: true}
}

func resolveManifest(tools ...toolDecl) *mf.Manifest {
	return &mf.Manifest{Metadata: mf.Metadata{Name: "resolve-test"}, Tools: tools}
}

func tempStore(t *testing.T) *fileBindings {
	return newFileBindings(filepath.Join(t.TempDir(), "bindings.json"))
}

// A Playwright server in the client's own configuration gives the runner's
// browser its known tool names, with no pin and nothing asked.
func TestResolveBrowserFromAConfiguredPlaywrightServer(t *testing.T) {
	b := newUnlisted(connected("pw", "npx @playwright/mcp@latest --extension", ""), connected("notes", "notes-mcp", ""))
	m := resolveManifest(toolDecl{Alias: "browser", Capability: browserCapability, Effect: "write"})
	ask := func(ToolQuestion) (string, bool) {
		t.Fatal("asked although the configuration answers")
		return "", false
	}
	decls, notes, err := resolveUnlisted(m, b, tempStore(t), ask)
	if err != nil {
		t.Fatal(err)
	}
	a, err := admitWith(nil, nil, decls, b)
	if err != nil {
		t.Fatal(err)
	}
	noteResolutions(a, notes)
	bd := a.byAlias["browser"]
	if bd.browser == nil || len(bd.browser.backends) != 1 || bd.browser.backends[0].Kind != backendPlaywright || bd.browser.backends[0].Server != "pw" {
		t.Fatalf("browser binding: %+v", bd)
	}
	if !strings.Contains(bd.ResolvedBy, "known tool names") || !strings.Contains(bd.Verified, "connected") {
		t.Fatalf("receipt: %q / %q", bd.ResolvedBy, bd.Verified)
	}
	for _, tl := range b.given {
		if tl.Server == "notes" {
			t.Fatalf("a server that is no browser was given browser tools: %+v", b.given)
		}
	}
}

// A configured browser that is not connected does not bind.
func TestResolveBrowserSkipsADisconnectedServer(t *testing.T) {
	s := connected("playwright", "npx @playwright/mcp", "")
	s.Connected = false
	b := newUnlisted(s)
	m := resolveManifest(toolDecl{Alias: "browser", Capability: browserCapability, Effect: "write"})
	_, _, err := resolveUnlisted(m, b, tempStore(t), nil)
	if err == nil || !strings.HasPrefix(err.Error(), "blocked:") || !strings.Contains(err.Error(), browserCapability) {
		t.Fatalf("a disconnected browser: %v", err)
	}
}

// A Telara gateway in the configuration is given its dispatcher's known
// names; its catalog says which operation fills the capability and what it
// does.
func TestResolveConnectorThroughAConfiguredGateway(t *testing.T) {
	b := newUnlisted(connected("work", "", "https://gateway.example/telara/mcp"))
	m := resolveManifest(calendarDecl())
	m.Capabilities = []mf.Capability{calendarContract()}
	decls, notes, err := resolveUnlisted(m, b, tempStore(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	a, err := admitWith(nil, nil, decls, b, m.Capabilities...)
	if err != nil {
		t.Fatal(err)
	}
	noteResolutions(a, notes)
	bd := a.byAlias["calendar"]
	if bd.Server != "work" || bd.Operation != "google_workspace/calendar_list_events" || bd.effective() != "read" || !strings.Contains(bd.ResolvedBy, "gateway") {
		t.Fatalf("gateway binding: %+v", bd)
	}
}

// Nothing in the configuration fits: one question is asked, naming the
// capability and offering no list; the answer is checked against the
// connected servers, bound, kept for the client, and not asked again.
func TestResolveAsksOnceChecksTheAnswerAndKeepsIt(t *testing.T) {
	b := newUnlisted(connected("tracker", "tracker-mcp", ""), connected("notes", "notes-mcp", ""))
	m := resolveManifest(toolDecl{Alias: "search", Capability: "tracker.issues.search", Effect: "read"})
	store := tempStore(t)
	var asked []ToolQuestion
	ask := func(q ToolQuestion) (string, bool) {
		asked = append(asked, q)
		return "mcp__tracker__search_issues", true
	}
	decls, notes, err := resolveUnlisted(m, b, store, ask)
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0].Capability != "tracker.issues.search" || asked[0].Client != "kilo" {
		t.Fatalf("asked: %+v", asked)
	}
	text := asked[0].Text()
	if strings.Contains(text, "notes") || strings.Contains(text, "tracker-mcp") || !strings.Contains(text, "one tool name, or none") {
		t.Fatalf("the question lists servers or asks for more than one name: %q", text)
	}
	a, err := admitWith(nil, nil, decls, b)
	if err != nil {
		t.Fatal(err)
	}
	noteResolutions(a, notes)
	bd := a.byAlias["search"]
	if bd.Server != "tracker" || bd.Tool != "search_issues" || bd.Pinned || !strings.Contains(bd.ResolvedBy, "asked") {
		t.Fatalf("binding: %+v", bd)
	}
	// What it does comes from the server, which says nothing here: the
	// answer cannot make it a read.
	if bd.effective() != "write" {
		t.Fatalf("an unannotated answered tool is treated as %s", bd.effective())
	}
	if got := store.get("kilo", "tracker.issues.search"); got != "tracker/search_issues" {
		t.Fatalf("kept %q", got)
	}
	again := func(ToolQuestion) (string, bool) { t.Fatal("asked again after the answer was kept"); return "", false }
	decls, notes, err = resolveUnlisted(m, newUnlisted(b.servers...), store, again)
	if err != nil || decls[0].Pin == nil || decls[0].Pin.Tool != "search_issues" || !strings.Contains(notes["search"].by, "kept") {
		t.Fatalf("the kept mapping: %v %+v %+v", err, decls, notes)
	}
}

// An answer is never taken on trust: a tool on a server that is not
// connected, a list, or a tool the person denies binds nothing, is not kept,
// and the run is blocked naming the capability.
func TestResolveRefusesAnAnswerThatDoesNotCheck(t *testing.T) {
	down := connected("other", "other-mcp", "")
	down.Connected = false
	for name, answer := range map[string]string{
		"server not connected": "other/search_issues",
		"unknown server":       "nowhere/search_issues",
		"a list":               "tracker/search_issues, tracker/get_issue",
		"none":                 "none",
		"denied":               "tracker/search_issues",
	} {
		t.Run(name, func(t *testing.T) {
			b := newUnlisted(connected("tracker", "tracker-mcp", ""), down)
			if name == "denied" {
				b.deny["tracker/search_issues"] = true
			}
			store := tempStore(t)
			m := resolveManifest(toolDecl{Alias: "search", Capability: "tracker.issues.search", Effect: "read"})
			_, _, err := resolveUnlisted(m, b, store, func(ToolQuestion) (string, bool) { return answer, true })
			if err == nil || !strings.HasPrefix(err.Error(), "blocked:") || !strings.Contains(err.Error(), "tracker.issues.search") {
				t.Fatalf("%q was accepted: %v", answer, err)
			}
			if got := store.get("kilo", "tracker.issues.search"); got != "" {
				t.Fatalf("an unchecked answer was kept: %q", got)
			}
		})
	}
}

// Nobody to ask: the run is blocked with the question and the one way to
// answer it; an optional tool is left out instead.
func TestResolveBlocksWithTheQuestionWhenNobodyCanBeAsked(t *testing.T) {
	b := newUnlisted(connected("tracker", "tracker-mcp", ""))
	m := resolveManifest(toolDecl{Alias: "search", Capability: "tracker.issues.search", Effect: "read"})
	_, _, err := resolveUnlisted(m, b, tempStore(t), nil)
	if err == nil || !strings.Contains(err.Error(), "Which of your tools does this?") || !strings.Contains(err.Error(), "tap bind --client kilo tracker.issues.search <server>/<tool>") {
		t.Fatalf("blocked: %v", err)
	}
	m.Tools[0].Optional = true
	decls, _, err := resolveUnlisted(m, b, tempStore(t), nil)
	if err != nil || decls[0].Pin != nil {
		t.Fatalf("an optional tool: %v %+v", err, decls)
	}
}

// A pin is an override and binds as it always has, with nothing asked.
func TestResolveKeepsPins(t *testing.T) {
	b := newUnlisted(connected("tracker", "tracker-mcp", ""))
	m := resolveManifest(toolDecl{Alias: "search", Capability: "tracker.issues.search", Effect: "read", Pin: &mf.Pin{Server: "tracker", Tool: "search_issues"}})
	decls, notes, err := resolveUnlisted(m, b, tempStore(t), func(ToolQuestion) (string, bool) { t.Fatal("asked about a pinned tool"); return "", false })
	if err != nil {
		t.Fatal(err)
	}
	a, err := admitWith(nil, nil, decls, b)
	if err != nil {
		t.Fatal(err)
	}
	noteResolutions(a, notes)
	if bd := a.byAlias["search"]; !bd.Pinned || bd.Tool != "search_issues" || bd.ResolvedBy != "pin" {
		t.Fatalf("pinned: %+v", bd)
	}
}

func TestParseToolAnswer(t *testing.T) {
	down := connected("down", "", "")
	down.Connected = false
	servers := []bridge.ConfiguredServer{connected("tracker", "", ""), connected("my notes", "", ""), down}
	for answer, want := range map[string]string{
		"tracker/search_issues":       "tracker/search_issues",
		"tracker / search_issues":     "tracker/search_issues",
		"`tracker_search_issues`":     "tracker/search_issues",
		"mcp__tracker__search_issues": "tracker/search_issues",
		"mcp_tracker_search_issues":   "tracker/search_issues",
		"my_notes_find":               "my notes/find",
		"my notes/find":               "my notes/find",
		"search_issues":               "", // two servers could hold it
		"down/x":                      "",
		"none":                        "",
		"a, b":                        "",
		"use the tracker tool please": "",
	} {
		s, tl, ok := parseToolAnswer(answer, servers)
		got := ""
		if ok {
			got = s + "/" + tl
		}
		if got != want {
			t.Errorf("%q -> %q, want %q", answer, got, want)
		}
	}
	if s, tl, ok := parseToolAnswer("search_issues", servers[:1]); !ok || s != "tracker" || tl != "search_issues" {
		t.Errorf("a bare name with one server: %q %q %v", s, tl, ok)
	}
}

// Gemini CLI's servers come from its settings files, the project's
// overriding the person's; no status is claimed.
func TestGeminiServersFromSettings(t *testing.T) {
	dir := t.TempDir()
	user, project := filepath.Join(dir, "user.json"), filepath.Join(dir, "project.json")
	writeResolveFile(t, user, `{"mcpServers":{"pw":{"command":"npx","args":["@playwright/mcp@latest"]},"work":{"httpUrl":"https://gateway.example/telara/mcp","headers":{"Authorization":"Bearer secret"}}}}`)
	writeResolveFile(t, project, `{"mcpServers":{"pw":{"command":"npx","args":["other-mcp"]}}}`)
	got := bridge.GeminiServers(user, project)
	if len(got) != 2 || got[0].Name != "pw" || got[0].Command != "npx other-mcp" || got[1].URL != "https://gateway.example/telara/mcp" || got[0].StatusKnown {
		t.Fatalf("servers: %+v", got)
	}
	if browserFamily(got[0]) != "" || !isGatewayServer(got[1]) {
		t.Fatalf("kinds: %q %v", browserFamily(got[0]), isGatewayServer(got[1]))
	}
}

func writeResolveFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The runner's own server, installed from a package whose path names the
// publisher, is not taken for a gateway.
func TestALocalCommandPathDoesNotMakeAGateway(t *testing.T) {
	if isGatewayServer(connected("tap", "/home/me/node_modules/@telaralabs/tap/assets/tap serve", "")) {
		t.Fatal("a command path made a gateway")
	}
	if !isGatewayServer(connected("work", "", "https://gateway.example/telara/mcp")) || !isGatewayServer(connected("telara", "", "")) {
		t.Fatal("a gateway by address or name was missed")
	}
}

// A browser server the runner cannot tell by its configuration is asked
// about once: an answer naming one of a known browser's tools brings that
// browser's tools, is kept, and is not asked again.
func TestResolveBrowserByAnsweredToolName(t *testing.T) {
	b := newUnlisted(connected("web", "my-browser-server", ""))
	m := resolveManifest(toolDecl{Alias: "browser", Capability: browserCapability, Effect: "write"})
	store := tempStore(t)
	n := 0
	ask := func(q ToolQuestion) (string, bool) { n++; return "web/browser_navigate", true }
	for i := 0; i < 2; i++ {
		decls, _, err := resolveUnlisted(m, b, store, ask)
		if err != nil {
			t.Fatal(err)
		}
		a, err := admitWith(nil, nil, decls, b)
		if err != nil {
			t.Fatal(err)
		}
		if bd := a.byAlias["browser"]; bd.browser == nil || bd.browser.backends[0].Server != "web" {
			t.Fatalf("browser: %+v", bd)
		}
	}
	if n != 1 {
		t.Fatalf("asked %d times", n)
	}
	_, _, err := resolveUnlisted(m, newUnlisted(connected("web", "my-browser-server", "")), tempStore(t), func(ToolQuestion) (string, bool) { return "web/click_thing", true })
	if err == nil || !strings.Contains(err.Error(), "configure a Playwright MCP server") {
		t.Fatalf("an unknown browser tool: %v", err)
	}
}
