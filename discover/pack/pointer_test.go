package pack

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/discover/client"
)

// savePrimitive installs a minimal package named name at version into the
// collection, as a save does.
func savePrimitive(t *testing.T, coll, name, version string) string {
	t.Helper()
	files := map[string][]byte{
		"primitive.yaml": []byte("apiVersion: tap/v3\nkind: Primitive\nmetadata:\n  name: " + name + "\n  publisher: local\n  version: " + version + "\n  description: Close stale tickets\nexecution:\n  entrypoint: main.sh\n"),
		"main.sh":        []byte("echo ok\n"),
	}
	pkg, digest, err := PackFiles(files, func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	path, _, err := Install(coll, name, pkg, Marker{Name: "local/" + name, Digest: digest, Validation: "not_run"}, "# "+name+"\n")
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func target(t *testing.T, name string, explicit bool) Target {
	t.Helper()
	c, ok := client.Lookup(name)
	if !ok {
		t.Fatalf("no client %s", name)
	}
	return Target{Client: c, Explicit: explicit}
}

func TestPointersNameThePrimitiveAndHoldNoPackage(t *testing.T) {
	home, coll := t.TempDir(), t.TempDir()
	pkg := savePrimitive(t, coll, "close-stale", "1.0.0")
	id, err := ReadIdentity(pkg)
	raw, _ := os.ReadFile(filepath.Join(pkg, "primitive.yaml"))
	sum := sha256.Sum256(append(raw, []byte("echo ok\n")...))
	if err != nil || id.Ref != "local/close-stale@1.0.0" || id.Digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("identity %+v %v", id, err)
	}
	rs, err := WritePointers(pkg, []Target{target(t, "claude-code", false), target(t, "codex", false), target(t, "gemini-cli", false)}, false, home, "")
	if err != nil || len(rs) != 3 {
		t.Fatalf("%+v %v", rs, err)
	}
	for _, r := range rs {
		if r.Mode != PointerWritten {
			t.Fatalf("%+v", r)
		}
		b, _ := os.ReadFile(filepath.Join(r.Path, "SKILL.md"))
		if !strings.Contains(string(b), "ref:    local/close-stale@1.0.0") || !strings.Contains(string(b), id.Digest) {
			t.Errorf("%s pointer lacks the ref or digest:\n%s", r.Client, b)
		}
		if ref, ok := IsPointer(r.Path); !ok || ref != id.Ref {
			t.Errorf("%s: tap-pointer = %q, %v", r.Client, ref, ok)
		}
		entries, _ := os.ReadDir(r.Path)
		if len(entries) != 1 {
			t.Errorf("%s pointer folder holds %d files; only SKILL.md belongs there", r.Client, len(entries))
		}
		if _, err := os.Stat(filepath.Join(r.Path, SavedMarker)); err == nil {
			t.Errorf("%s pointer carries a saved marker: the catalog would list it twice", r.Client)
		}
	}
	if rs[0].Path != filepath.Join(home, ".claude", "skills", "close-stale") {
		t.Errorf("claude pointer at %s", rs[0].Path)
	}
}

func TestSavingAgainChangesNothingAndANewVersionRewritesPointers(t *testing.T) {
	home, coll := t.TempDir(), t.TempDir()
	pkg := savePrimitive(t, coll, "close-stale", "1.0.0")
	ts := []Target{target(t, "claude-code", false), target(t, "codex", false)}
	first, _ := WritePointers(pkg, ts, false, home, "")
	mtimes := map[string]time.Time{}
	for _, r := range first {
		fi, _ := os.Stat(filepath.Join(r.Path, "SKILL.md"))
		mtimes[r.Path] = fi.ModTime()
	}
	time.Sleep(20 * time.Millisecond)
	if _, unchanged, err := Install(coll, "close-stale", mustRead(t, pkg), mustMarker(t, pkg), "# close-stale\n"); err != nil || !unchanged {
		t.Fatalf("collection resave: unchanged=%v %v", unchanged, err)
	}
	again, _ := WritePointers(pkg, ts, false, home, "")
	for _, r := range again {
		fi, _ := os.Stat(filepath.Join(r.Path, "SKILL.md"))
		if r.Mode != PointerUnchanged || !fi.ModTime().Equal(mtimes[r.Path]) {
			t.Errorf("%s: %s, mtime changed %v", r.Client, r.Mode, !fi.ModTime().Equal(mtimes[r.Path]))
		}
	}
	// A new version replaces the collection folder and the pointers' digest.
	pkg2 := savePrimitive(t, coll, "close-stale", "1.1.0")
	id2, _ := ReadIdentity(pkg2)
	third, _ := WritePointers(pkg2, ts, false, home, "")
	for _, r := range third {
		b, _ := os.ReadFile(filepath.Join(r.Path, "SKILL.md"))
		if r.Mode != PointerWritten || !strings.Contains(string(b), id2.Digest) || !strings.Contains(string(b), "@1.1.0") {
			t.Errorf("%s not rewritten: %s\n%s", r.Client, r.Mode, b)
		}
	}
	if entries, _ := os.ReadDir(coll); len(entries) != 1 {
		t.Errorf("collection holds %d entries, want the one primitive", len(entries))
	}
}

func TestForeignFolderIsSkippedAndOthersWritten(t *testing.T) {
	home, coll := t.TempDir(), t.TempDir()
	pkg := savePrimitive(t, coll, "close-stale", "1.0.0")
	foreign := filepath.Join(home, ".claude", "skills", "close-stale")
	os.MkdirAll(foreign, 0o755)
	os.WriteFile(filepath.Join(foreign, "SKILL.md"), []byte("---\nname: close-stale\n---\nmine\n"), 0o644)
	rs, err := WritePointers(pkg, []Target{target(t, "claude-code", false), target(t, "codex", false)}, false, home, "")
	if err != nil {
		t.Fatal(err)
	}
	if rs[0].Mode != PointerSkipped || !strings.Contains(rs[0].Reason, "not a TAP folder") {
		t.Errorf("claude: %+v", rs[0])
	}
	if b, _ := os.ReadFile(filepath.Join(foreign, "SKILL.md")); string(b) != "---\nname: close-stale\n---\nmine\n" {
		t.Error("a foreign skill was changed")
	}
	if rs[1].Mode != PointerWritten {
		t.Errorf("codex: %+v", rs[1])
	}
}

func TestAgentsSharingAFolderGetOnePointer(t *testing.T) {
	home, proj, coll := t.TempDir(), t.TempDir(), t.TempDir()
	pkg := savePrimitive(t, coll, "close-stale", "1.0.0")
	// Codex and Gemini CLI both read <project>/.agents/skills.
	rs, err := WritePointers(pkg, []Target{target(t, "codex", false), target(t, "gemini-cli", false)}, true, home, proj)
	if err != nil || len(rs) != 2 {
		t.Fatalf("%+v %v", rs, err)
	}
	shared := filepath.Join(proj, ".agents", "skills", "close-stale")
	if rs[0].Path != shared || rs[1].Path != shared || rs[0].Mode != PointerWritten || rs[1].Mode != PointerUnchanged || !strings.Contains(rs[1].Reason, "shares codex") {
		t.Fatalf("%+v", rs)
	}
	b, _ := os.ReadFile(filepath.Join(shared, "SKILL.md"))
	if !strings.Contains(string(b), "Codex") || !strings.Contains(string(b), "Gemini CLI") {
		t.Errorf("shared pointer should name both agents it runs in:\n%s", b)
	}
}

func TestPointersGoWhereThePrimitiveCanRunUnlessPicked(t *testing.T) {
	home := t.TempDir()
	for _, d := range []string{".claude", ".codex", ".cursor/chats", ".codeium/windsurf"} {
		os.MkdirAll(filepath.Join(home, d), 0o755)
	}
	ids := func(ts []Target) string {
		var out []string
		for _, t := range ts {
			out = append(out, t.Client.ID)
		}
		return strings.Join(out, ",")
	}
	// Detected: installed, able to run it (a bridge) and with TAP connected.
	// Codex has a bridge but no TAP registration: it is reported, not pointed.
	os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"mcpServers":{"tap":{"command":"tap"}}}`), 0o644)
	ts, skipped, err := ResolveTargets("detected", home)
	if err != nil || ids(ts) != "claude-code" || len(skipped) != 1 || skipped[0].Client != "codex" || !strings.Contains(skipped[0].Reason, "tap install --client codex") {
		t.Fatalf("detected = %s, skipped %+v, %v", ids(ts), skipped, err)
	}
	os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte("model = \"x\"\n\n[mcp_servers.tap]\ncommand = \"tap\"\n"), 0o644)
	ts, skipped, err = ResolveTargets("detected", home)
	if err != nil || ids(ts) != "claude-code,codex" || len(skipped) != 0 {
		t.Fatalf("detected = %s, skipped %+v, %v", ids(ts), skipped, err)
	}
	// Picked by name: a pointer even without a bridge, saying so.
	ts, err = Targets("windsurf,claude", home)
	if err != nil || ids(ts) != "windsurf,claude-code" || !ts[0].Explicit {
		t.Fatalf("named = %s, %v", ids(ts), err)
	}
	coll := t.TempDir()
	pkg := savePrimitive(t, coll, "close-stale", "1.0.0")
	rs, _ := WritePointers(pkg, ts, false, home, "")
	b, _ := os.ReadFile(filepath.Join(rs[0].Path, "SKILL.md"))
	if !strings.Contains(string(b), "cannot run TAP primitives yet") || !strings.Contains(rs[0].Reason, "cannot run in Windsurf yet") {
		t.Errorf("windsurf pointer must say it cannot run there:\n%s\n%+v", b, rs[0])
	}
	if ts, _ := Targets("none", home); len(ts) != 0 {
		t.Error("none")
	}
	if _, err := Targets("aider", home); err == nil {
		t.Error("aider has no skills folder")
	}
}

func TestMigrateSavedMovesPackagesAndLeavesPointers(t *testing.T) {
	home, proj, coll := t.TempDir(), t.TempDir(), t.TempDir()
	old := filepath.Join(home, ".claude", "skills")
	src := savePrimitive(t, old, "close-stale", "1.0.0")
	oldProj := savePrimitive(t, filepath.Join(proj, ".codex", "skills"), "triage", "2.0.0")
	foreign := filepath.Join(old, "someones-skill")
	os.MkdirAll(foreign, 0o755)
	os.WriteFile(filepath.Join(foreign, "SKILL.md"), []byte("hand written\n"), 0o644)
	srcMarker, _ := ReadMarker(src)

	rs, err := MigrateSaved(home, proj, coll)
	if err != nil || len(rs) != 2 {
		t.Fatalf("%+v %v", rs, err)
	}
	for _, r := range rs {
		if r.Mode != "moved" {
			t.Errorf("%+v", r)
		}
	}
	if m, err := ReadMarker(filepath.Join(coll, "close-stale")); err != nil || m != srcMarker {
		t.Fatalf("collection copy: %+v %v", m, err)
	}
	if _, err := ReadMarker(filepath.Join(coll, "triage")); err != nil {
		t.Fatal("project primitive not migrated")
	}
	for _, p := range []string{src, oldProj} {
		if ref, ok := IsPointer(p); !ok || ref == "" {
			t.Errorf("%s is not a pointer after migration", p)
		}
		if _, err := os.Stat(filepath.Join(p, SavedMarker)); err == nil {
			t.Errorf("%s still holds the package", p)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(foreign, "SKILL.md")); string(b) != "hand written\n" {
		t.Error("migration touched someone else's skill")
	}
	// Running it again finds nothing left to move.
	if rs, err := MigrateSaved(home, proj, coll); err != nil || len(rs) != 0 {
		t.Fatalf("second run: %+v %v", rs, err)
	}
}

func TestMigrateKeepsADifferentCollectionEntry(t *testing.T) {
	home, coll := t.TempDir(), t.TempDir()
	src := savePrimitive(t, filepath.Join(home, ".claude", "skills"), "close-stale", "1.0.0")
	savePrimitive(t, coll, "close-stale", "9.0.0")
	rs, err := MigrateSaved(home, "", coll)
	if err != nil || len(rs) != 1 || rs[0].Mode != PointerSkipped || !strings.Contains(rs[0].Reason, "different") {
		t.Fatalf("%+v %v", rs, err)
	}
	if _, err := ReadMarker(src); err != nil {
		t.Fatal("the old package was touched although it was not moved")
	}
}

func mustRead(t *testing.T, pkgDir string) []byte {
	t.Helper()
	files := map[string][]byte{}
	for _, f := range []string{"primitive.yaml", "main.sh"} {
		b, err := os.ReadFile(filepath.Join(pkgDir, f))
		if err != nil {
			t.Fatal(err)
		}
		files[f] = b
	}
	pkg, _, err := PackFiles(files, func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	return pkg
}

func mustMarker(t *testing.T, pkgDir string) Marker {
	m, err := ReadMarker(pkgDir)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
