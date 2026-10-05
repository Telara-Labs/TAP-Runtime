package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func webPackage(t *testing.T, manifest, file, program string) string {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "primitive.yaml"), []byte(manifest), 0o644)
	os.WriteFile(filepath.Join(dir, file), []byte(program), 0o644)
	return dir
}

// jsStore holds a stand-in for the pinned JavaScript interpreter, so the
// test needs no network. obtain checks every read against the pin, so the
// pin is moved to the stand-in for the test.
func jsStore(t *testing.T) string {
	t.Helper()
	old := interpreters[".js"]
	t.Cleanup(func() { interpreters[".js"] = old })
	body := []byte("a stand-in interpreter")
	in := old
	in.SHA256 = digest(body)
	interpreters[".js"] = in
	store := t.TempDir()
	os.WriteFile(filepath.Join(store, in.File), body, 0o644)
	return store
}

const webHead = "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.test, name: %s, version: 0.1.0}\n"

func TestWebBuild(t *testing.T) {
	store := jsStore(t)
	pkg := webPackage(t, strings.Replace(webHead, "%s", "mail", 1)+`execution: {entrypoint: main.ts}
tools:
  - {alias: threads, capability: gmail.threads.search, effect: read, pin: {server: claude.ai Gmail, tool: search_threads}}
  - {alias: drive, capability: drive.files.search, effect: read, optional: true}
`, "main.ts", "const n: number = 1;\nprint('</script>', n);\n")
	page, caps, err := buildWebPage(store, []string{pkg})
	if err != nil {
		t.Fatal(err)
	}
	s := string(page)
	for _, left := range []string{"@QJS_B64@", "@PRELUDE@", "@TAPWEB@", "@PRIMITIVES@", "@VERSION@", "@QJS_SHA256@"} {
		if strings.Contains(s, left) {
			t.Errorf("%s was not filled in", left)
		}
	}
	if strings.Contains(s, ": number") {
		t.Error("the TypeScript program was not stripped of its types")
	}
	if strings.Count(s, "</script>") != 1 {
		t.Errorf("a closing script tag inside the program ends the page's script early: %d found", strings.Count(s, "</script>"))
	}
	if !strings.Contains(s, `"server": "Gmail"`) || strings.Contains(s, "claude.ai Gmail") {
		t.Error("the connector is not named the way claude.ai names it")
	}
	b, _ := json.Marshal(caps)
	if string(b) != `{"db":{},"mcp":{"servers":[{"server":"Gmail","tools":["search_threads"]}]},"user":{}}` {
		t.Errorf("capabilities: %s", b)
	}
}

func TestWebBuildRefuses(t *testing.T) {
	store := jsStore(t)
	for want, c := range map[string][3]string{
		"JavaScript and TypeScript only": {"execution: {entrypoint: main.sh}\n", "main.sh", "echo x\n"},
		"no machine":                     {"execution: {entrypoint: main.js}\ncommands:\n  - {command: git, args: [\"*\"], effect: read}\n", "main.js", "print(1)\n"},
		"not allowed to make":            {"execution: {entrypoint: main.js}\nfetch:\n  - {origin: \"https://example.com\"}\n", "main.js", "print(1)\n"},
		"cannot ask for approval":        {"execution: {entrypoint: main.js}\ntools:\n  - {alias: send, capability: gmail.drafts.create, effect: write, pin: {server: Gmail, tool: create_draft}}\n", "main.js", "print(1)\n"},
		"must name its connector":        {"execution: {entrypoint: main.js}\ntools:\n  - {alias: t, capability: gmail.threads.search, effect: read}\n", "main.js", "print(1)\n"},
	} {
		pkg := webPackage(t, strings.Replace(webHead, "%s", "p", 1)+c[0], c[1], c[2])
		if _, _, err := buildWebPage(store, []string{pkg}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want a refusal saying %q, got: %v", want, err)
		}
	}
}

// The page has no approval step, so a connector tool that
// says it changes state is refused even when the primitive declared it a read.
func TestTheWebPageRefusesAToolThatSaysItChangesState(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed, and this runs the page's own guard")
	}
	page, err := os.ReadFile(filepath.Join(repoRoot, "host", "webassets", "worker.html"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(page)
	i, j := strings.Index(src, "// TAP-GUARD-BEGIN"), strings.Index(src, "// TAP-GUARD-END")
	if i < 0 || j < i {
		t.Fatal("the page's guard is not marked")
	}
	script := src[i:j] + `
const calls = [];
const mk = (annotations) => ({
  describeTool: async () => ({ annotations }),
  callTool: async (s, t) => { calls.push(t); return { payload: "ok" }; },
});
(async () => {
  const out = [];
  for (const [name, ann] of [["readonly", {readOnlyHint: true}], ["destructive", {destructiveHint: true}], ["not read only", {readOnlyHint: false}], ["says nothing", undefined]]) {
    try { await guardedCall(mk(ann), "Gmail", name, {}); out.push(name + ":called"); }
    catch (e) { out.push(name + ":" + e.code); }
  }
  const broken = { describeTool: async () => { throw new Error("no schema"); }, callTool: async () => ({payload: 1}) };
  try { await guardedCall(broken, "Gmail", "no-describe", {}); out.push("no describe:called"); } catch (e) { out.push("no describe:" + e.code); }
  console.log(JSON.stringify({out, calls}));
})();
`
	b, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	var got struct {
		Out   []string `json:"out"`
		Calls []string `json:"calls"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	want := "readonly:called destructive:refused not read only:refused says nothing:refused no describe:refused"
	if strings.Join(got.Out, " ") != want {
		t.Fatalf("got %v", got.Out)
	}
	if strings.Join(got.Calls, ",") != "readonly" {
		t.Errorf("a refused tool was called: %v", got.Calls)
	}
}
