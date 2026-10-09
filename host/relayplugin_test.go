package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// relayHarness loads the relay plugin in Node with a stand-in session
// server, then talks to its socket the way the runner does.
const relayHarness = `
import http from "node:http"
import fs from "node:fs"
const { TapRelay } = await import(process.argv[2])
const sessionConfig = { mcp: { remote1: { type: "remote", url: "https://example.invalid/mcp", headers: { Authorization: "Bearer SECRET-HEADER" } },
  local1: { type: "local", command: ["echo", "hi"], environment: { TOKEN: "SECRET-ENV" } } }, permission: { bash: "deny" } }
const fakeFetch = async (req) => {
  const u = new URL(req.url)
  const body = u.pathname === "/config" ? sessionConfig : u.pathname === "/mcp" ? { remote1: { status: "connected" } } : { healthy: true, version: "test" }
  return new Response(JSON.stringify(body), { headers: { "content-type": "application/json" } })
}
const input = { directory: process.argv[3], worktree: process.argv[3], client: { _client: { getConfig: () => ({ baseUrl: "http://session", headers: {}, fetch: fakeFetch }) } } }
const hooks = await TapRelay(input)
const dir = "/tmp/tap-relay-" + process.getuid()
const meta = fs.readdirSync(dir).filter((f) => f.startsWith(process.pid + "-") && f.endsWith(".json"))[0]
const sock = JSON.parse(fs.readFileSync(dir + "/" + meta)).socket
const req = (method, path, body) => new Promise((resolve) => {
  const r = http.request({ socketPath: sock, method, path, headers: { "content-type": "application/json" } }, (res) => {
    let d = ""; res.on("data", (c) => (d += c)); res.on("end", () => resolve(res.statusCode + " " + d))
  })
  if (body) r.write(JSON.stringify(body))
  r.end()
})
console.log("CONFIG " + await req("GET", "/config"))
console.log("CALL-OUTSIDE-RUN " + await req("POST", "/experimental/mcp/call-tool", { server: "local1", name: "x" }))
console.log("TOOLS-OUTSIDE-RUN " + await req("GET", "/tap/tools"))
console.log("OTHER " + await req("POST", "/session/x/shell", {}))
await hooks["tool.execute.before"]({ tool: "tap_tap_run" })
console.log("CALL-IN-RUN " + await req("POST", "/experimental/mcp/call-tool", { server: "missing", name: "x" }))
await hooks.dispose()
console.log("CLEANED " + !fs.existsSync(sock))
process.exit(0)
`

// The relay gives the runner no secret from the session's configuration,
// refuses a tool call and a tool listing when no TAP run is in progress,
// refuses routes it does not serve, and removes its socket when disposed.
func TestRelayPluginKeepsSecretsAndGatesCalls(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	dir := t.TempDir()
	plugin := filepath.Join(dir, relayPluginName)
	os.WriteFile(plugin, relayPlugin, 0o644)
	harness := filepath.Join(dir, "harness.mjs")
	os.WriteFile(harness, []byte(relayHarness), 0o644)
	out, err := exec.Command(node, harness, "file://"+plugin, dir).CombinedOutput()
	text := string(out)
	if err != nil {
		t.Fatalf("harness: %v\n%s", err, text)
	}
	line := func(prefix string) string {
		for _, l := range strings.Split(text, "\n") {
			if strings.HasPrefix(l, prefix+" ") {
				return strings.TrimPrefix(l, prefix+" ")
			}
		}
		t.Fatalf("no %s line in:\n%s", prefix, text)
		return ""
	}
	cfg := line("CONFIG")
	if !strings.HasPrefix(cfg, "200 ") || strings.Contains(cfg, "SECRET") || !strings.Contains(cfg, "remote1") || !strings.Contains(cfg, "local1") {
		t.Fatalf("the configuration the runner sees: %s", cfg)
	}
	for _, p := range []string{"CALL-OUTSIDE-RUN", "TOOLS-OUTSIDE-RUN"} {
		if got := line(p); !strings.HasPrefix(got, "409 ") {
			t.Fatalf("%s: %s, want 409 outside a TAP run", p, got)
		}
	}
	if got := line("OTHER"); !strings.HasPrefix(got, "404 ") {
		t.Fatalf("a route the relay does not serve: %s", got)
	}
	if got := line("CALL-IN-RUN"); !strings.HasPrefix(got, "502 ") || !strings.Contains(got, "no enabled MCP server named missing") {
		t.Fatalf("a call during a run reached the relay's own connection logic: %s", got)
	}
	if got := line("CLEANED"); got != "true" {
		t.Fatalf("the socket was left behind")
	}
}
