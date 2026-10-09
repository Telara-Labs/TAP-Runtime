package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Telara-Labs/TAP-Runtime/bind"
	"github.com/Telara-Labs/TAP-Runtime/bridge"
	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

// Some clients do not tell the runner which tools they have: Kilo reports
// only whether each MCP server is connected, and Gemini CLI reports nothing.
// On those clients the runner resolves only the capabilities the primitive
// declares, one at a time, and never asks for or sends a whole catalog:
//
//  1. A tool pinned in the manifest binds as it always has.
//  2. A mapping kept on this machine for the client (tap bind) is used.
//  3. Servers from the client's own configuration whose name, command or
//     address fits a well-known capability are given that capability's known
//     tool names: Playwright and Claude in Chrome for the runner's browser,
//     and a Telara gateway's dispatcher, whose catalog then says which
//     operation fills the capability and what it does.
//  4. Anything still unresolved is one narrow question to the client: which
//     of your tools does this? The answer is one tool name, or none. It is
//     never given a list to pick from.
//
// An answer is checked before it binds: the tool must be on a server the
// client has configured and connected, and the person's own rules must not
// deny it. What it does comes from its server's annotation, never from the
// answer; a client that reports no annotation leaves it treated as a write.
// A checked answer is kept per client, so later runs ask nothing. A
// capability nothing fills blocks the run and names the capability.

// ToolQuestion is the one question asked for a capability nothing else
// resolved.
type ToolQuestion struct {
	Primitive  string
	Alias      string
	Capability string
	Effect     string
	Purpose    string // what the tool must do, in a few words
	Client     string
}

// Text is the question as it is put to the client.
func (q ToolQuestion) Text() string {
	return fmt.Sprintf("The primitive %q needs one of your tools to %s (capability %s, %s). Which of your tools does this? Answer with one tool name, or none.",
		q.Primitive, q.Purpose, q.Capability, q.Effect)
}

// ToolAsker puts a ToolQuestion to the client. ok is false when there is
// nobody to ask, or they did not answer.
type ToolAsker func(ToolQuestion) (answer string, ok bool)

// resolution is how one alias was resolved on a client that lists no tools,
// for the receipt.
type resolution struct {
	by, verified string
	// asPin is set when the resolution became a pin the manifest does not
	// carry; the receipt then does not call the binding pinned.
	asPin bool
}

// Known tool names of well-known providers. A server of that kind offers
// these under these names whatever the server is called.
var (
	playwrightToolNames = []string{"browser_navigate", "browser_evaluate"}
	gatewayToolNames    = []string{"telara_execute_action", "telara_tool_search", "telara_tool_describe"}
)

// serverText is what the runner matches a configured server's kind against.
func serverText(s bridge.ConfiguredServer) string {
	return strings.ToLower(s.Name + " " + s.Command + " " + s.URL)
}

// browserFamily names the known browser a configured server is, or "".
func browserFamily(s bridge.ConfiguredServer) string {
	t := serverText(s)
	switch {
	case strings.Contains(t, "claude-in-chrome"):
		return backendChrome
	case strings.Contains(t, "playwright"):
		return backendPlaywright
	}
	return ""
}

func familyTools(server, family string) []bind.Tool {
	names := playwrightToolNames
	if family == backendChrome {
		names = chromeToolNames
	}
	out := make([]bind.Tool, 0, len(names))
	for _, n := range names {
		out = append(out, bind.Tool{Server: server, Name: n, Annotated: bind.Unknown})
	}
	return out
}

// familyOfTool says which known browser a tool name belongs to, so an answer
// naming one of its tools brings the rest.
func familyOfTool(tool string) string {
	for _, n := range chromeToolNames {
		if n == tool {
			return backendChrome
		}
	}
	for _, n := range playwrightToolNames {
		if n == tool {
			return backendPlaywright
		}
	}
	return ""
}

// isGatewayServer says a configured server is a Telara gateway, by its name
// or its address. A local command is not read for it: a package path such
// as the runner's own can contain the word.
func isGatewayServer(s bridge.ConfiguredServer) bool {
	return strings.Contains(strings.ToLower(s.Name+" "+s.URL), "telara")
}

func usableServer(s bridge.ConfiguredServer) bool { return !s.StatusKnown || s.Connected }

func verifiedText(s bridge.ConfiguredServer) string {
	if s.StatusKnown {
		return fmt.Sprintf("the client reports server %q connected; it does not list tools, so the tool's presence is confirmed by its first call", s.Name)
	}
	return fmt.Sprintf("server %q is in the client's settings; the client reports no connection status and lists no tools", s.Name)
}

// configuredServers reads the servers the client is configured with, when it
// can say.
func configuredServers(b bridge.Bridge) []bridge.ConfiguredServer {
	c, ok := b.(bridge.Configured)
	if !ok {
		return nil
	}
	servers, err := c.ConfiguredServers()
	if err != nil {
		logf("resolve    could not read the client's configured servers: %v", err)
		return nil
	}
	return servers
}

var nonNameChars = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// parseToolAnswer reads an answer into a server and a tool among the
// configured servers. It accepts server/tool and the names clients give MCP
// tools (mcp__server__tool, mcp_server_tool, server_tool), and a bare tool
// name when only one server could hold it. Anything that reads as more than
// one name, or as none, is no answer.
func parseToolAnswer(answer string, servers []bridge.ConfiguredServer) (server, tool string, ok bool) {
	a := strings.Trim(strings.TrimSpace(answer), "`'\".")
	if a == "" || strings.EqualFold(a, "none") || strings.ContainsAny(a, ",;\n\t") {
		return "", "", false
	}
	if l, r, cut := strings.Cut(a, " / "); cut {
		a = l + "/" + r
	}
	if strings.Contains(a, " ") && !strings.Contains(a, "/") {
		return "", "", false
	}
	sorted := append([]bridge.ConfiguredServer(nil), servers...)
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i].Name) > len(sorted[j].Name) })
	for _, s := range sorted {
		if !usableServer(s) {
			continue
		}
		safe := nonNameChars.ReplaceAllString(s.Name, "_")
		for _, prefix := range []string{s.Name + "/", "mcp__" + safe + "__", "mcp_" + safe + "_", safe + "_"} {
			if rest := strings.TrimPrefix(a, prefix); rest != a && rest != "" && !strings.ContainsAny(rest, " /") {
				return s.Name, rest, true
			}
		}
	}
	if strings.ContainsAny(a, " /") {
		return "", "", false
	}
	var only []string
	for _, s := range servers {
		if usableServer(s) {
			only = append(only, s.Name)
		}
	}
	if len(only) == 1 {
		return only[0], a, true
	}
	return "", "", false
}

func findServer(servers []bridge.ConfiguredServer, name string) (bridge.ConfiguredServer, bool) {
	for _, s := range servers {
		if s.Name == name {
			return s, true
		}
	}
	return bridge.ConfiguredServer{}, false
}

// keptTool is a mapping kept for a capability as server/tool. A kept plain
// server (a tie settled on a client that lists tools) names no tool.
func keptTool(v string) (server, tool string, ok bool) {
	i := strings.LastIndex(v, "/")
	if i <= 0 || i == len(v)-1 {
		return "", "", false
	}
	return v[:i], v[i+1:], true
}

// resolveUnlisted resolves every unpinned declaration of m on a client that
// does not list its tools, and gives the bridge the tools that may fill them.
// It returns the declarations to admit, with a resolved tool written in as a
// pin, and how each alias was resolved.
func resolveUnlisted(m *mf.Manifest, pb bridge.PinnedOnly, store bindingStore, ask ToolAsker) ([]toolDecl, map[string]resolution, error) {
	name, _ := pb.Client()
	// The name the person types to tap bind: gemini, not the name Gemini
	// CLI's MCP client gives itself.
	client := clientFor(name)
	servers := configuredServers(pb)
	notes := map[string]resolution{}
	var pool []bind.Tool
	use := func(extra ...bind.Tool) {
		pb.UsePins(append(append([]bind.Tool(nil), pool...), extra...))
	}
	trial := func(d toolDecl) error {
		_, _, err := admitOnce(store, nil, []toolDecl{d}, pb, m.Capabilities...)
		return err
	}
	purposeOf := func(d toolDecl) string {
		for _, c := range m.Capabilities {
			if c.Label == d.Capability && strings.TrimSpace(c.Question) != "" {
				return strings.TrimSpace(c.Question)
			}
		}
		if isBrowserCapability(d.Capability) {
			return "open a web page and run JavaScript in it"
		}
		return strings.Join(bind.Tokens(mf.CapabilityName(d.Capability)), " ")
	}
	question := func(d toolDecl) ToolQuestion {
		return ToolQuestion{Primitive: m.Metadata.Name, Alias: d.Alias, Capability: mf.CapabilityName(d.Capability), Effect: d.Effect, Purpose: purposeOf(d), Client: client}
	}
	out := append([]toolDecl(nil), m.Tools...)
	for _, d := range out {
		if d.Pin != nil {
			pool = append(pool, bind.Tool{Server: d.Pin.Server, Name: d.Pin.Tool, Annotated: bind.Unknown})
			notes[d.Alias] = resolution{by: "pin"}
			// A pin on a gateway's dispatcher learns each operation's effect
			// from that gateway's catalog, so its catalog tools are lent too.
			if s, ok := findServer(servers, d.Pin.Server); ok && isGatewayServer(s) && usableServer(s) && d.Pin.Tool == "telara_execute_action" {
				pool = append(pool, gatewayCatalogTools(s.Name)...)
			}
		}
	}
	for i := range out {
		d := out[i]
		if d.Pin != nil {
			continue
		}
		capName := mf.CapabilityName(d.Capability)
		var tried []string
		if isBrowserCapability(d.Capability) {
			// The browser binds by its tools' names (browser.go), so known
			// names on a configured browser server are enough.
			var found []string
			for _, s := range servers {
				if f := browserFamily(s); f != "" && usableServer(s) {
					pool = append(pool, familyTools(s.Name, f)...)
					found = append(found, fmt.Sprintf("%s as %s (%s)", s.Name, f, verifiedText(s)))
				}
			}
			if len(found) > 0 {
				notes[d.Alias] = resolution{by: "known tool names of a configured browser server", verified: strings.Join(found, "; ")}
				continue
			}
			tried = append(tried, "no server in the client's configuration is a browser the runner knows (Playwright, Claude in Chrome)")
			if store != nil {
				if server, tool, ok := keptTool(store.get(client, capName)); ok {
					s, known := findServer(servers, server)
					if f := familyOfTool(tool); known && usableServer(s) && f != "" {
						pool = append(pool, familyTools(server, f)...)
						notes[d.Alias] = resolution{by: "mapping kept on this machine (tap bind)", verified: fmt.Sprintf("%s is a known %s tool; %s", tool, f, verifiedText(s))}
						continue
					}
					tried = append(tried, "the kept mapping "+server+"/"+tool+" is not a known browser tool on a connected server")
				}
			}
			if ask != nil {
				if ans, ok := ask(question(d)); ok {
					server, tool, ok := parseToolAnswer(ans, servers)
					s, known := findServer(servers, server)
					if f := familyOfTool(tool); ok && known && f != "" {
						pool = append(pool, familyTools(server, f)...)
						notes[d.Alias] = resolution{by: fmt.Sprintf("asked the client (answer %q)", ans), verified: fmt.Sprintf("%s is a known %s tool; %s", tool, f, verifiedText(s))}
						if store != nil {
							if err := store.set(client, capName, server+"/"+tool); err != nil {
								logf("resolve    could not keep %s/%s for %s: %v", server, tool, capName, err)
							}
						}
						continue
					}
					tried = append(tried, fmt.Sprintf("the answer %q is not a tool of a known browser on a connected server", ans))
				}
			}
			if d.Optional {
				continue
			}
			return nil, nil, blockedError(d, client, tried)
		}
		// A mapping kept for this client.
		if store != nil {
			if server, tool, ok := keptTool(store.get(client, capName)); ok {
				if s, known := findServer(servers, server); known && usableServer(s) {
					pinned := d
					pinned.Pin = &mf.Pin{Server: server, Tool: tool}
					t := bind.Tool{Server: server, Name: tool, Annotated: bind.Unknown}
					use(t)
					if err := trial(pinned); err == nil {
						out[i], pool = pinned, append(pool, t)
						notes[d.Alias] = resolution{by: "mapping kept on this machine (tap bind)", verified: verifiedText(s), asPin: true}
						continue
					} else {
						tried = append(tried, "the kept mapping "+server+"/"+tool+" no longer binds: "+err.Error())
					}
				} else {
					tried = append(tried, "the kept mapping "+server+"/"+tool+" names a server that is not connected")
				}
			}
		}
		// A gateway whose catalog may hold the operation.
		var gw []bind.Tool
		var gwServers []string
		for _, s := range servers {
			if isGatewayServer(s) && usableServer(s) {
				for _, n := range gatewayToolNames {
					// The gateway annotates its catalog tools read-only on
					// clients that pass annotations on; a client that passes
					// none leaves the runner's knowledge of them. The
					// dispatcher stays unannotated: each operation's effect
					// comes from the catalog.
					effect := bind.Read
					if n == "telara_execute_action" {
						effect = bind.Unknown
					}
					gw = append(gw, bind.Tool{Server: s.Name, Name: n, Annotated: effect})
				}
				gwServers = append(gwServers, s.Name)
			}
		}
		if len(gw) > 0 {
			use(gw...)
			if err := trial(d); err == nil {
				pool = append(pool, gw...)
				notes[d.Alias] = resolution{by: "gateway catalog on " + strings.Join(gwServers, ", "), verified: "the operation and its effect come from the gateway's catalog"}
				continue
			} else {
				tried = append(tried, "the gateway catalog offers no operation for it ("+err.Error()+")")
			}
		} else {
			tried = append(tried, "no configured server is a gateway the runner knows")
		}
		// One question, one name back.
		if ask != nil {
			if ans, ok := ask(question(d)); ok {
				server, tool, parsed := parseToolAnswer(ans, servers)
				s, known := findServer(servers, server)
				if parsed && known {
					pinned := d
					pinned.Pin = &mf.Pin{Server: server, Tool: tool}
					t := bind.Tool{Server: server, Name: tool, Annotated: bind.Unknown}
					use(t)
					if err := trial(pinned); err == nil {
						out[i], pool = pinned, append(pool, t)
						notes[d.Alias] = resolution{by: fmt.Sprintf("asked the client (answer %q)", ans), verified: verifiedText(s), asPin: true}
						if store != nil {
							if err := store.set(client, capName, server+"/"+tool); err != nil {
								logf("resolve    could not keep %s/%s for %s: %v", server, tool, capName, err)
							}
						}
						continue
					} else {
						tried = append(tried, fmt.Sprintf("the answer %q does not bind: %v", ans, err))
					}
				} else {
					tried = append(tried, fmt.Sprintf("the answer %q is not a tool on a connected server of this client", ans))
				}
			} else {
				tried = append(tried, "the client gave no tool name")
			}
		}
		if d.Optional {
			continue
		}
		return nil, nil, blockedError(d, client, tried)
	}
	pb.UsePins(pool)
	return out, notes, nil
}

// gatewayCatalogTools are a gateway's catalog tools, as reads: the gateway
// annotates them read-only, and a client that passes no annotations on
// leaves the runner's knowledge of them.
func gatewayCatalogTools(server string) []bind.Tool {
	return []bind.Tool{
		{Server: server, Name: "telara_tool_search", Annotated: bind.Read},
		{Server: server, Name: "telara_tool_describe", Annotated: bind.Read},
	}
}

// blockedError names the capability nothing filled, says what was tried, and
// asks the one question, with the way to answer it.
func blockedError(d toolDecl, client string, tried []string) error {
	capName := mf.CapabilityName(d.Capability)
	if strings.HasSuffix(capName, ".execute_action") {
		// The capability names the gateway's dispatcher, not an operation,
		// so no catalog entry can match it.
		tried = append(tried, fmt.Sprintf("%s names the gateway's dispatcher, not an operation: declare the operation itself (for example %s.list_projects), or pin the dispatcher", capName, strings.TrimSuffix(capName, ".execute_action")))
	}
	answer := fmt.Sprintf("tap bind --client %s %s <server>/<tool>", client, capName)
	if isBrowserCapability(d.Capability) {
		answer += "; if none does, configure a Playwright MCP server (or Claude in Chrome) in " + client
	}
	return fmt.Errorf("blocked: tool %q needs the capability %s and no tool on %s was found for it (%s does not list its tools; %s). Which of your tools does this? If one does: %s, then run again",
		d.Alias, capName, client, client, strings.Join(tried, "; "), answer)
}

// noteResolutions writes how each alias was resolved onto the receipt.
func noteResolutions(a *admission, notes map[string]resolution) {
	if a == nil {
		return
	}
	for i := range a.Bindings {
		n, ok := notes[a.Bindings[i].Alias]
		if !ok {
			continue
		}
		a.Bindings[i].ResolvedBy, a.Bindings[i].Verified = n.by, n.verified
		if n.asPin {
			a.Bindings[i].Pinned = false
		}
	}
}

// ConfiguredServers is Gemini CLI's configured servers, from the person's
// settings and this project's. Nothing is changed.
func (b *relayBridge) ConfiguredServers() ([]bridge.ConfiguredServer, error) {
	return bridge.GeminiServers(b.settingsFiles...), nil
}

// UsePins gives the relay the tools that may fill the primitive's
// capabilities, as for Kilo.
func (b *relayBridge) UsePins(tools []bind.Tool) { b.tools = append([]bind.Tool(nil), tools...) }

// geminiSettingsFiles are Gemini CLI's user and project settings.
func geminiSettingsFiles(wd string) []string {
	var files []string
	// Gemini CLI reads its user settings from GEMINI_CLI_HOME when that is
	// set, in place of the home directory, and a system settings file that
	// overrides the others from GEMINI_CLI_SYSTEM_SETTINGS_PATH. The runner
	// is started by Gemini, so it sees the same values.
	home := os.Getenv("GEMINI_CLI_HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home != "" {
		files = append(files, filepath.Join(home, ".gemini", "settings.json"))
	}
	if sys := os.Getenv("GEMINI_CLI_SYSTEM_SETTINGS_PATH"); sys != "" {
		files = append(files, sys)
	}
	if wd != "" {
		files = append(files, filepath.Join(wd, ".gemini", "settings.json"))
	}
	return files
}
