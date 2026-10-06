package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agents "github.com/Telara-Labs/TAP-Runtime/discover/client"
	"github.com/Telara-Labs/TAP-Runtime/discover/pack"
)

func saveSetupPrimitive(t *testing.T, collection string) string {
	t.Helper()
	files := map[string][]byte{
		"primitive.yaml": []byte("apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.setup, name: greeting, version: 1.0.0, description: Greet a person}\nexecution: {entrypoint: main.sh}\n"),
		"main.sh":        []byte("echo setup-ready\n"),
	}
	archive, digest, err := pack.PackFiles(files, func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	dir, _, err := pack.Install(collection, "greeting", archive, pack.Marker{Name: "dev.setup/greeting", Digest: digest, Validation: "not_run"}, "# greeting\n")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestInstallReconcilesSavedSkillsForNewClient(t *testing.T) {
	interpreterStore(t) // Build before isolating HOME; Go caches are not fixture data.
	home := t.TempDir()
	t.Setenv("HOME", home)
	collection, err := pack.CollectionDir()
	if err != nil {
		t.Fatal(err)
	}
	saved := saveSetupPrimitive(t, collection)
	var out, errOut bytes.Buffer
	if rc := installCommand([]string{"--client", "goose"}, &out, &errOut); rc != 0 {
		t.Fatalf("install %d: %s", rc, errOut.String())
	}
	goose, _ := agents.Lookup("goose")
	if !goose.Connected(home, "tap") {
		t.Fatal("Goose was not connected")
	}
	pointer := filepath.Join(home, ".config/goose/skills/greeting/SKILL.md")
	first, err := os.ReadFile(pointer)
	if err != nil {
		t.Fatal(err)
	}
	id, err := pack.ReadIdentity(saved)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), id.Ref) || !strings.Contains(string(first), id.Digest) {
		t.Fatal("skill does not name exact runnable identity")
	}
	stat, err := os.Stat(pointer)
	if err != nil {
		t.Fatal(err)
	}
	if rc := installCommand([]string{"--client", "goose"}, &out, &errOut); rc != 0 {
		t.Fatalf("repeat %d: %s", rc, errOut.String())
	}
	again, _ := os.ReadFile(pointer)
	after, _ := os.Stat(pointer)
	if !bytes.Equal(first, again) || !stat.ModTime().Equal(after.ModTime()) {
		t.Fatal("unchanged pointer rewritten")
	}
	// The configured client's pointer names a real catalog entry that runs in
	// the actual MCP server and WebAssembly interpreter, not a model fixture.
	c := startServer(t, true, accept)
	userConfigDir = func() (string, error) { return filepath.Dir(filepath.Dir(collection)), nil }
	res := toolObject(t, c.call("tools/call", map[string]any{"name": "tap_search", "arguments": map[string]any{"query": "greet"}}))
	matches, _ := res["matches"].([]any)
	if len(matches) != 1 {
		t.Fatalf("matches %#v", res)
	}
	run := c.call("tools/call", map[string]any{"name": "tap_run", "arguments": map[string]any{"ref": id.Ref, "digest": id.Digest}})
	if run["isError"] == true || !strings.Contains(toolText(t, run), "setup-ready") {
		t.Fatalf("run %#v", run)
	}
}

func TestInstallPrintAndRemoveDoNotWriteSkillPointers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	collection, err := pack.CollectionDir()
	if err != nil {
		t.Fatal(err)
	}
	saveSetupPrimitive(t, collection)
	var out, errOut bytes.Buffer
	pointer := filepath.Join(home, ".config/goose/skills/greeting/SKILL.md")
	for _, flags := range [][]string{{"--client", "goose", "--print"}, {"--client", "goose", "--remove"}} {
		if rc := installCommand(flags, &out, &errOut); rc != 0 {
			t.Fatalf("%v: %d %s", flags, rc, errOut.String())
		}
		if _, err := os.Stat(pointer); !os.IsNotExist(err) {
			t.Fatalf("%v created pointer: %v", flags, err)
		}
	}
	if !strings.Contains(out.String(), "would reconcile saved primitive") {
		t.Fatal("dry run omitted planned skill reconciliation")
	}
}

func TestSyncCollectionPointersPreservesForeignSkillAndUpdatesOtherClients(t *testing.T) {
	home := t.TempDir()
	collection := filepath.Join(t.TempDir(), "primitives")
	saveSetupPrimitive(t, collection)
	foreign := filepath.Join(home, ".codex/skills/greeting/SKILL.md")
	os.MkdirAll(filepath.Dir(foreign), 0755)
	os.WriteFile(foreign, []byte("owned by user\n"), 0644)
	codex, _ := agents.Lookup("codex")
	claude, _ := agents.Lookup("claude-code")
	var out, errOut bytes.Buffer
	if rc := syncCollectionPointers(home, collection, []pack.Target{{Client: codex}, {Client: claude}}, false, &out, &errOut); rc != 0 {
		t.Fatalf("%d: %s", rc, errOut.String())
	}
	got, _ := os.ReadFile(foreign)
	if string(got) != "owned by user\n" {
		t.Fatal("foreign skill changed")
	}
	if !strings.Contains(out.String(), "skipped") {
		t.Fatal("foreign collision not reported")
	}
	if _, err := os.Stat(filepath.Join(home, ".claude/skills/greeting/SKILL.md")); err != nil {
		t.Fatal(err)
	}
}

func TestSyncCollectionPointersReportsInvalidSavedPackage(t *testing.T) {
	collection := t.TempDir()
	dir := filepath.Join(collection, "broken")
	os.MkdirAll(dir, 0755)
	os.WriteFile(filepath.Join(dir, pack.SavedMarker), []byte("{}"), 0644)
	codex, _ := agents.Lookup("codex")
	var out, errOut bytes.Buffer
	if rc := syncCollectionPointers(t.TempDir(), collection, []pack.Target{{Client: codex}}, false, &out, &errOut); rc != 1 || !strings.Contains(errOut.String(), "broken") {
		t.Fatalf("%d: %s", rc, errOut.String())
	}
}

func TestSyncCollectionPointersReachesEachSupportedSkillClient(t *testing.T) {
	home := t.TempDir()
	collection := filepath.Join(t.TempDir(), "primitives")
	saved := saveSetupPrimitive(t, collection)
	var targets []pack.Target
	for _, id := range agents.IDs(nil) {
		c, _ := agents.Lookup(id)
		if c.Bridge && agents.HasSkills(c) {
			targets = append(targets, pack.Target{Client: c})
		}
	}
	var out, errOut bytes.Buffer
	if rc := syncCollectionPointers(home, collection, targets, false, &out, &errOut); rc != 0 {
		t.Fatalf("%d: %s", rc, errOut.String())
	}
	identity, err := pack.ReadIdentity(saved)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		root, err := target.Client.SkillsDir(false, home, "")
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(root, "greeting/SKILL.md"))
		if err != nil {
			t.Fatalf("%s: %v", target.Client.ID, err)
		}
		if !strings.Contains(string(data), identity.Ref) || !strings.Contains(string(data), identity.Digest) {
			t.Fatalf("%s wrong identity", target.Client.ID)
		}
	}
}
