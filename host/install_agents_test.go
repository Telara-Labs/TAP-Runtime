package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab.com/telara-labs/tap-runtime/bridge"
	agents "gitlab.com/telara-labs/tap-runtime/discover/client"
)

const cursorConfig = `{
  "mcpServers": {
    "telara": {"url": "https://example.test/mcp", "headers": {"X-Note": "keep"}},
    "zeta": {"command": "z"}
  },
  "unknownSetting": [1, 2, {"deep": true}],
  "another": "value"
}
`

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return m
}

func keysInOrder(t *testing.T, b []byte) []string {
	t.Helper()
	o, err := orderedObject(b)
	if err != nil {
		t.Fatal(err)
	}
	return o.keys
}

// The JSON writer adds exactly one entry and keeps everything else: other
// servers, unknown keys, key order. It backs the file up once, does nothing
// the second time, removes only its own entry, and refuses a file that is
// not JSON (TENG-3114).
func TestSetMCPEntryMergesWithoutDisturbing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	os.WriteFile(path, []byte(cursorConfig), 0o600)
	entry := mcpEntry("/opt/tap", "tap", nil)
	changed, err := setMCPEntry(path, "mcpServers", "tap", entry)
	if err != nil || !changed {
		t.Fatalf("add: %v %v", changed, err)
	}
	after, _ := os.ReadFile(path)
	got, want := decode(t, after), decode(t, []byte(cursorConfig))
	servers := got["mcpServers"].(map[string]any)
	if servers["tap"] == nil || len(servers) != 3 {
		t.Fatalf("servers %v", servers)
	}
	delete(servers, "tap")
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(want)
	if !bytes.Equal(a, b) {
		t.Fatalf("something else changed:\n%s\n%s", a, b)
	}
	if k := keysInOrder(t, after); strings.Join(k, ",") != "mcpServers,unknownSetting,another" {
		t.Errorf("key order %v", k)
	}
	if k := keysInOrder(t, []byte(mustJSONField(t, after, "mcpServers"))); strings.Join(k, ",") != "telara,zeta,tap" {
		t.Errorf("server order %v", k)
	}
	if b, _ := os.ReadFile(path + ".tap-backup"); string(b) != cursorConfig {
		t.Error("no backup of the original")
	}
	// Twice is a no-op: the file is not rewritten.
	st1, _ := os.Stat(path)
	time.Sleep(20 * time.Millisecond)
	if changed, err := setMCPEntry(path, "mcpServers", "tap", entry); err != nil || changed {
		t.Fatalf("second add changed=%v %v", changed, err)
	}
	if st2, _ := os.Stat(path); !st2.ModTime().Equal(st1.ModTime()) {
		t.Error("an unchanged entry rewrote the file")
	}
	// Remove takes out only ours; the backup still holds the original.
	if changed, err := setMCPEntry(path, "mcpServers", "tap", nil); err != nil || !changed {
		t.Fatalf("remove: %v %v", changed, err)
	}
	after, _ = os.ReadFile(path)
	a, _ = json.Marshal(decode(t, after))
	if !bytes.Equal(a, b) {
		t.Fatalf("remove left:\n%s", a)
	}
	if b, _ := os.ReadFile(path + ".tap-backup"); string(b) != cursorConfig {
		t.Error("the backup was overwritten by a later change")
	}
	// A file that is not JSON is refused and left exactly as it was.
	os.WriteFile(path, []byte("// comment\n{"), 0o600)
	if _, err := setMCPEntry(path, "mcpServers", "tap", entry); err == nil || !strings.Contains(err.Error(), "left unchanged") {
		t.Fatalf("malformed accepted: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "// comment\n{" {
		t.Fatal("a malformed file was rewritten")
	}
	// A new file is created.
	fresh := filepath.Join(t.TempDir(), "sub", "mcp_config.json")
	if changed, err := setMCPEntry(fresh, "mcpServers", "tap", entry); err != nil || !changed {
		t.Fatalf("new file: %v %v", changed, err)
	}
}

func mustJSONField(t *testing.T, b []byte, k string) string {
	o, err := orderedObject(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(o.get(k))
}

// End to end: `tap install --client detected` in a home with Cursor and
// Windsurf installed writes both their configurations and touches nothing
// else; --remove takes the entries out again.
func TestInstallDetectedWritesEachAgentsConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".cursor", "chats"), 0o755)                         // Cursor CLI
	os.MkdirAll(filepath.Join(home, "Library", "Application Support", "Cursor"), 0o755) // Cursor (macOS)
	os.MkdirAll(filepath.Join(home, ".config", "Cursor"), 0o755)                        // Cursor (Linux)
	os.MkdirAll(filepath.Join(home, ".codeium", "windsurf"), 0o755)
	os.WriteFile(filepath.Join(home, ".cursor", "mcp.json"), []byte(cursorConfig), 0o600)
	var out, errOut bytes.Buffer
	if code := installCommand([]string{"--client", "detected"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, errOut.String(), out.String())
	}
	for _, p := range []string{".cursor/mcp.json", ".codeium/windsurf/mcp_config.json"} {
		b, err := os.ReadFile(filepath.Join(home, p))
		if err != nil {
			t.Fatal(err)
		}
		s := decode(t, b)["mcpServers"].(map[string]any)["tap"].(map[string]any)
		if args := s["args"].([]any); len(args) != 3 || args[0] != "serve" {
			t.Errorf("%s entry %v", p, s)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); err == nil {
		t.Error("touched an agent that is not installed")
	}
	// Cursor CLI and Cursor share one file: written once, then already as wanted.
	if !strings.Contains(out.String(), "already as wanted") {
		t.Errorf("output:\n%s", out.String())
	}
	out.Reset()
	if code := installCommand([]string{"--client", "detected", "--remove"}, &out, &errOut); code != 0 {
		t.Fatalf("remove exit %d: %s", code, errOut.String())
	}
	b, _ := os.ReadFile(filepath.Join(home, ".cursor", "mcp.json"))
	if _, ok := decode(t, b)["mcpServers"].(map[string]any)["tap"]; ok {
		t.Fatal("remove left the entry")
	}
	// An unknown agent name is refused with the registry's list.
	if code := installCommand([]string{"--client", "nope"}, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "claude-code") {
		t.Fatalf("unknown agent: %d %s", code, errOut.String())
	}
}

// Live, with each agent CLI that is on this machine: after tap install into
// a temporary home, the agent's own `mcp list` shows tap. An absent CLI is
// reported as not run. The person's real configuration is never touched:
// HOME points at the temporary home.
func TestInstallIsSeenByEachAgentCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("runs agent CLIs")
	}
	for _, tc := range []struct{ id, bin string }{{"claude-code", "claude"}, {"codex", "codex"}, {"copilot-cli", "copilot"}} {
		t.Run(tc.id, func(t *testing.T) {
			bin := tc.bin
			if tc.id == "claude-code" {
				var err error
				bin, err = bridge.ClaudeExecutable()
				if err != nil {
					t.Skipf("not run: %s is not on this machine", tc.id)
				}
			} else if _, err := exec.LookPath(bin); err != nil {
				t.Skipf("not run: %s is not on this machine", bin)
			}
			home := t.TempDir()
			// Claude Code may populate its temporary home with read-only module
			// cache directories. Make only this test-owned tree removable before
			// testing.T's TempDir cleanup runs.
			t.Cleanup(func() {
				_ = filepath.WalkDir(home, func(path string, entry os.DirEntry, err error) error {
					if err != nil {
						return nil
					}
					if entry.IsDir() {
						return os.Chmod(path, 0o700)
					}
					if entry.Type()&os.ModeSymlink == 0 {
						return os.Chmod(path, 0o600)
					}
					return nil
				})
			})
			t.Setenv("HOME", home)
			var out, errOut bytes.Buffer
			if code := installCommand([]string{"--client", tc.id}, &out, &errOut); code != 0 {
				t.Fatalf("install exit %d: %s", code, errOut.String())
			}
			cmd := exec.Command(bin, "mcp", "list")
			cmd.Env = append(os.Environ(), "HOME="+home)
			b, _ := cmd.CombinedOutput()
			if !strings.Contains(string(b), "tap") {
				t.Fatalf("%s mcp list does not show tap:\n%s", tc.bin, b)
			}
		})
	}
}

const gooseConfig = `# my goose settings
GOOSE_PROVIDER: openai # the provider
GOOSE_MODE: approve
extensions:
  developer:
    enabled: true
    type: builtin
    name: developer
    timeout: 300
  telara:
    enabled: true
    type: streamable_http
    uri: https://example.test/mcp
`

// Goose keeps its MCP servers as extensions in config.yaml and has no
// command to add one: the runner merges its entry, keeping comments, other
// settings and order, backs the file up once, does nothing the second time,
// removes only its own entry, and refuses a file that is not YAML
// (TENG-3116).
func TestSetYAMLEntryMergesGooseConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(path, []byte(gooseConfig), 0o600)
	entry := gooseEntry("/opt/tap", "tap", envFlags{"OTEL_SERVICE_NAME=tap"})
	if changed, err := setYAMLEntry(path, "extensions", "tap", entry); err != nil || !changed {
		t.Fatalf("add: %v %v", changed, err)
	}
	after, _ := os.ReadFile(path)
	s := string(after)
	for _, want := range []string{"# my goose settings", "GOOSE_PROVIDER: openai # the provider", "  telara:\n", "  tap:\n", "type: stdio", "cmd: /opt/tap", "OTEL_SERVICE_NAME: tap"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Index(s, "developer:") > strings.Index(s, "telara:") || strings.Index(s, "telara:") > strings.Index(s, "tap:") {
		t.Errorf("order changed:\n%s", s)
	}
	if b, _ := os.ReadFile(path + ".tap-backup"); string(b) != gooseConfig {
		t.Error("no backup of the original")
	}
	c, _ := agents.Lookup("goose")
	if !connectedAt(c, path) {
		t.Error("the registry does not read the entry as connected")
	}
	if changed, err := setYAMLEntry(path, "extensions", "tap", entry); err != nil || changed {
		t.Fatalf("second add changed=%v %v", changed, err)
	}
	if changed, err := setYAMLEntry(path, "extensions", "tap", nil); err != nil || !changed {
		t.Fatalf("remove: %v %v", changed, err)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "tap:") || !strings.Contains(string(b), "telara:") {
		t.Fatalf("remove:\n%s", b)
	}
	os.WriteFile(path, []byte("extensions: [unclosed\n"), 0o600)
	if _, err := setYAMLEntry(path, "extensions", "tap", entry); err == nil {
		t.Fatal("a file that is not YAML was rewritten")
	}
}

// connectedAt checks Connected with a home whose config.yaml is path.
func connectedAt(c agents.Client, path string) bool {
	home, _ := os.MkdirTemp("", "goose-home-")
	defer os.RemoveAll(home)
	dst := filepath.Join(home, filepath.FromSlash(c.MCP.Path))
	os.MkdirAll(filepath.Dir(dst), 0o755)
	b, _ := os.ReadFile(path)
	os.WriteFile(dst, b, 0o600)
	return c.Connected(home, "tap")
}

// Live with Goose: tap install --client goose into a temporary home, then
// real Goose, through the bridge, starts the installed runner as its
// extension and lists the runner's own tools. The person's configuration is
// never touched.
func TestInstallIsSeenByGoose(t *testing.T) {
	if testing.Short() {
		t.Skip("runs goose")
	}
	gooseBin, err := exec.LookPath("goose")
	if err != nil {
		if h, _ := os.UserHomeDir(); h != "" {
			if p := filepath.Join(h, ".local", "bin", "goose"); fileIsThere(p) {
				gooseBin = p
			}
		}
	}
	if gooseBin == "" {
		t.Skip("not run: goose is not on this machine")
	}
	home := t.TempDir()
	tap := filepath.Join(t.TempDir(), "tap")
	build := exec.Command("go", "build", "-o", tap, "./host")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	cfg := filepath.Join(home, ".config", "goose", "config.yaml")
	os.MkdirAll(filepath.Dir(cfg), 0o755)
	os.WriteFile(cfg, []byte("GOOSE_PROVIDER: openai\nGOOSE_MODEL: none\n"), 0o600)
	inst := exec.Command(tap, "install", "--client", "goose")
	inst.Env = append(os.Environ(), "HOME="+home)
	if out, err := inst.CombinedOutput(); err != nil {
		t.Fatalf("install: %v %s", err, out)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", filepath.Dir(gooseBin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	g, err := bridge.NewGoose()
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	inv, err := g.Inventory()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range inv {
		if tool.Server == "tap" {
			names = append(names, tool.Name)
		}
	}
	if len(names) == 0 {
		t.Fatalf("goose lists no tools of the installed runner: %v", inv)
	}
	t.Logf("goose lists the runner's tools: %v", names)
}

func fileIsThere(p string) bool { _, err := os.Stat(p); return err == nil }
