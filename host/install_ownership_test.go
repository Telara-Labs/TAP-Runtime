package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A server of the user's own that happens to share TAP's name is not TAP's to
// replace or delete. Install refuses and says why, and remove leaves
// it as it was, byte for byte. An entry TAP wrote is still removable.
func TestInstallAndRemoveLeaveAHostOwnedEntryOfTheSameNameAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := t.TempDir()
	t.Setenv("PATH", bin)

	cursor := filepath.Join(home, ".cursor", "mcp.json")
	os.MkdirAll(filepath.Dir(cursor), 0o755)
	cursorMine := "{\n  \"mcpServers\": {\n    \"tap\": {\"command\": \"/usr/local/bin/my-own-server\", \"args\": [\"--port\", \"9\"]}\n  }\n}\n"
	os.WriteFile(cursor, []byte(cursorMine), 0o600)

	goose := filepath.Join(home, ".config", "goose", "config.yaml")
	os.MkdirAll(filepath.Dir(goose), 0o755)
	gooseMine := "extensions:\n  tap:\n    enabled: true\n    type: stdio\n    name: tap\n    cmd: /usr/local/bin/my-own-server\n    args: [--port, \"9\"]\n"
	os.WriteFile(goose, []byte(gooseMine), 0o600)

	calls := filepath.Join(t.TempDir(), "claude-calls")
	script := "#!/bin/sh\necho \"$@\" >> " + calls + "\n" +
		"if [ \"$1 $2\" = \"mcp get\" ]; then printf 'tap:\\n  Type: stdio\\n  Command: /usr/local/bin/my-own-server\\n  Args: --port 9\\n'; fi\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := installCommand([]string{"--client", "cursor,goose,claude"}, &out, &errOut); code == 0 {
		t.Fatalf("install replaced entries it did not write:\n%s", out.String())
	}
	for path, want := range map[string]string{cursor: cursorMine, goose: gooseMine} {
		if b, _ := os.ReadFile(path); string(b) != want {
			t.Errorf("install changed %s:\n%s", path, b)
		}
	}
	if !strings.Contains(errOut.String(), "did not write") {
		t.Errorf("the refusal does not say why:\n%s", errOut.String())
	}
	logged := func() string { b, _ := os.ReadFile(calls); return string(b) }
	if strings.Contains(logged(), "mcp remove") || strings.Contains(logged(), "mcp add") {
		t.Errorf("install ran the host's remove or add against a foreign entry:\n%s", logged())
	}

	out.Reset()
	errOut.Reset()
	installCommand([]string{"--client", "cursor,goose,claude", "--remove"}, &out, &errOut)
	for path, want := range map[string]string{cursor: cursorMine, goose: gooseMine} {
		if b, _ := os.ReadFile(path); string(b) != want {
			t.Errorf("remove changed %s:\n%s", path, b)
		}
	}
	if strings.Contains(logged(), "mcp remove") {
		t.Errorf("remove ran the host's remove against a foreign entry:\n%s", logged())
	}

	// An entry TAP wrote is still TAP's to remove.
	own := filepath.Join(t.TempDir(), "mcp.json")
	if _, err := setMCPEntry(own, "mcpServers", "tap", mcpEntry("/opt/tap", "tap", nil)); err != nil {
		t.Fatal(err)
	}
	if changed, err := setMCPEntry(own, "mcpServers", "tap", nil); err != nil || !changed {
		t.Fatalf("an entry TAP wrote was not removable: %v %v", changed, err)
	}
}
