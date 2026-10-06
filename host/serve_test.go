package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// repoRoot is fixed before any test changes directory.
var repoRoot = func() string {
	wd, _ := os.Getwd()
	return filepath.Dir(wd)
}()

var (
	shOnce  sync.Once
	shStore string
	shErr   error
)

// interpreterStore builds the bash-compatible interpreter for the sandbox
// target, once, so tests run real programs in the real sandbox.
func interpreterStore(t *testing.T) string {
	t.Helper()
	shOnce.Do(func() {
		shStore, shErr = os.MkdirTemp("", "tap-store-")
		if shErr != nil {
			return
		}
		cmd := exec.Command("go", "build", "-o", filepath.Join(shStore, "sh.wasm"), "./guest-sh")
		cmd.Dir = repoRoot // tests change directory; the source does not move
		cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
		if out, err := cmd.CombinedOutput(); err != nil {
			shErr = err
			t.Logf("%s", out)
		}
	})
	if shErr != nil {
		t.Fatalf("building the interpreter: %v", shErr)
	}
	return shStore
}

func writePackage(t *testing.T, manifest, script string) string {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "primitive.yaml"), []byte(manifest), 0o644)
	os.WriteFile(filepath.Join(dir, "main.sh"), []byte(script), 0o644)
	return dir
}

// Action-gate tests admit their owned package separately from the action.
// Callers isolate userConfigDir before using this helper.
func trustTestPackage(t *testing.T, pkg string) {
	t.Helper()
	var out, errb bytes.Buffer
	if code := trustCommand([]string{pkg}, &out, &errb); code != 0 {
		t.Fatalf("trust disposable test package: %d %s", code, errb.String())
	}
}

// client is a minimal MCP client: the other end of the wire.
type client struct {
	t           *testing.T
	in          io.WriteCloser
	sc          *bufio.Scanner
	answer      func(params map[string]any) map[string]any // how it answers an elicitation
	asked       []string
	runs        string
	catalogRoot string
	trust       []string // the first-run question about a package, kept apart from the rest
	noTrust     bool
	done        chan error
}

func startServer(t *testing.T, elicitation bool, answer func(map[string]any) map[string]any) *client {
	t.Helper()
	return startServerArgs(t, elicitation, answer)
}

// startServerArgs is startServer with more flags given to serve.
func startServerArgs(t *testing.T, elicitation bool, answer func(map[string]any) map[string]any, extra ...string) *client {
	t.Helper()
	store := interpreterStore(t)
	// Each server starts with no package trusted, so one test's yes is not
	// another's.
	cfg, oldCfg := t.TempDir(), userConfigDir
	userConfigDir = func() (string, error) { return cfg, nil }
	t.Cleanup(func() { userConfigDir = oldCfg })
	runs := t.TempDir()
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	c := &client{t: t, runs: runs, catalogRoot: t.TempDir(), in: cw, sc: bufio.NewScanner(cr), answer: answer, done: make(chan error, 1)}
	c.sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	go func() {
		c.done <- serve(sr, sw, append([]string{"--interpreters", store, "--runs", runs, "--catalog-root", c.catalogRoot, "--journal", filepath.Join(t.TempDir(), "journal.jsonl")}, extra...))
		sw.Close()
	}()
	caps := map[string]any{}
	if elicitation {
		caps["elicitation"] = map[string]any{"form": map[string]any{}}
	}
	c.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": caps,
		"clientInfo": map[string]any{"name": "test-client", "version": "1"}})
	t.Cleanup(func() { cw.Close(); <-c.done })
	return c
}

var nextID = 1000

func (c *client) send(v any) {
	b, _ := json.Marshal(v)
	c.in.Write(append(b, '\n'))
}

// call sends a request and returns its result, answering any request the
// server makes in the meantime.
func (c *client) call(method string, params any) map[string]any {
	c.t.Helper()
	nextID++
	id := nextID
	c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) && c.sc.Scan() {
		var m map[string]any
		if json.Unmarshal(c.sc.Bytes(), &m) != nil {
			continue
		}
		if m["method"] == "elicitation/create" {
			p, _ := m["params"].(map[string]any)
			msg, _ := p["message"].(string)
			if strings.Contains(msg, "for the first time on this machine") {
				c.trust = append(c.trust, msg)
				ans := map[string]any{"action": "accept", "content": map[string]any{"approve": !c.noTrust}}
				c.send(map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": ans})
				continue
			}
			c.asked = append(c.asked, msg)
			c.send(map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": c.answer(p)})
			continue
		}
		if m["id"] == float64(id) {
			if e, ok := m["error"]; ok {
				c.t.Fatalf("%s: %v", method, e)
			}
			r, _ := m["result"].(map[string]any)
			return r
		}
	}
	c.t.Fatalf("%s: no answer", method)
	return nil
}

func (c *client) run(pkg string) string {
	r := c.call("tools/call", map[string]any{"name": "tap_run", "arguments": c.stagePackage(pkg)})
	content, _ := r["content"].([]any)
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	return text
}

func (c *client) stagePackage(pkg string) map[string]any {
	c.t.Helper()
	sum := sha256.Sum256([]byte(pkg))
	dest := filepath.Join(c.catalogRoot, hex.EncodeToString(sum[:8]))
	if err := os.MkdirAll(dest, 0o700); err != nil {
		c.t.Fatal(err)
	}
	for _, name := range []string{"primitive.yaml", "main.sh"} {
		b, err := os.ReadFile(filepath.Join(pkg, name))
		if err != nil {
			c.t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dest, name), b, 0o600); err != nil {
			c.t.Fatal(err)
		}
	}
	digest, m, err := packageDigest(dest)
	if err != nil {
		c.t.Fatal(err)
	}
	return map[string]any{"ref": m.Metadata.Publisher + "/" + m.Metadata.Name + "@" + m.Metadata.Version, "digest": digest}
}

const writeManifest = `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.test, name: writer, version: 0.1.0}
execution: {entrypoint: main.sh}
files:
  - {path: out, access: write}
commands:
  - {command: touch, args: ["*"], effect: write}
`

const writeScript = `echo first > out/a.txt && echo "a written" || echo "a refused"
echo second > out/a.txt && echo "a written again" || echo "a refused again"
echo third > out/b.txt && echo "b written" || echo "b refused"
echo hi > /tmp/tap-serve-escape.txt || echo "outside refused"
`

var (
	accept = func(map[string]any) map[string]any {
		return map[string]any{"action": "accept", "content": map[string]any{"approve": true, "limit": 10}}
	}
	// Ticking the box and naming no number allows one.
	acceptOne = func(map[string]any) map[string]any {
		return map[string]any{"action": "accept", "content": map[string]any{"approve": true}}
	}
	decline = func(map[string]any) map[string]any { return map[string]any{"action": "decline"} }
	cancel  = func(map[string]any) map[string]any { return map[string]any{"action": "cancel"} }
	// Accepting the form with the box left unticked is not a yes.
	unticked = func(map[string]any) map[string]any {
		return map[string]any{"action": "accept", "content": map[string]any{"approve": false}}
	}
	empty = func(map[string]any) map[string]any { return map[string]any{"action": "accept"} }
)

func TestServeListsSevenLocalTools(t *testing.T) {
	c := startServer(t, true, accept)
	r := c.call("tools/list", map[string]any{})
	tools, _ := r["tools"].([]any)
	if len(tools) != 7 || tools[0].(map[string]any)["name"] != "tap_search" || tools[4].(map[string]any)["name"] != "tap_evidence" || tools[5].(map[string]any)["name"] != "tap_save" || tools[6].(map[string]any)["name"] != "tap_result" {
		t.Fatalf("tools = %v", r)
	}
}

func TestServeAsksOncePerKindAndWritesOnYes(t *testing.T) {
	dir := inDir(t)
	os.Remove("/tmp/tap-serve-escape.txt")
	c := startServer(t, true, accept)
	out := c.run(writePackage(t, writeManifest, writeScript))
	for _, want := range []string{"a written", "a written again", "b written", "outside refused"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "out", "a.txt")); strings.TrimSpace(string(b)) != "second" {
		t.Errorf("a.txt holds %q", b)
	}
	// Three writes under one declared directory, and ten allowed: asked
	// once. The path outside is refused before anybody is asked.
	if len(c.asked) != 1 {
		t.Fatalf("the person was asked %d times, want 1:\n%s", len(c.asked), strings.Join(c.asked, "\n---\n"))
	}
	for _, want := range []string{"writer", "write files under out", "out/a.txt", "write change"} {
		if !strings.Contains(c.asked[0], want) {
			t.Errorf("the prompt does not say %q: %q", want, c.asked[0])
		}
	}
	if _, err := os.Stat("/tmp/tap-serve-escape.txt"); err == nil {
		t.Fatal("a file was written outside the declared directory")
	}
}

// the approval carries the ceiling, and reaching it
// asks again.
func TestReachingTheCeilingAsksAgain(t *testing.T) {
	inDir(t)
	c := startServer(t, true, acceptOne)
	out := c.run(writePackage(t, writeManifest, writeScript))
	if len(c.asked) != 3 {
		t.Fatalf("three writes with one allowed each time asked %d times:\n%s", len(c.asked), strings.Join(c.asked, "\n---\n"))
	}
	if strings.Contains(c.asked[0], "has made") || !strings.Contains(c.asked[1], "has made 1 of these") || !strings.Contains(c.asked[2], "has made 2 of these") {
		t.Errorf("the prompts do not say how many were made:\n%s", strings.Join(c.asked, "\n---\n"))
	}
	if !strings.Contains(out, "b written") {
		t.Errorf("output:\n%s", out)
	}

	// Yes to the first, no to the second: one change is made and no more.
	dir := inDir(t)
	n := 0
	c = startServer(t, true, func(p map[string]any) map[string]any {
		n++
		if n == 1 {
			return acceptOne(p)
		}
		return decline(p)
	})
	out = c.run(writePackage(t, writeManifest, writeScript))
	if b, _ := os.ReadFile(filepath.Join(dir, "out", "a.txt")); strings.TrimSpace(string(b)) != "first" {
		t.Fatalf("a.txt holds %q; the ceiling of one did not hold", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "out", "b.txt")); err == nil {
		t.Fatal("a change was made after the person said no")
	}
	if len(c.asked) != 2 {
		t.Fatalf("asked %d times, want 2: a no is remembered for the run", len(c.asked))
	}
	if !strings.Contains(out, "a refused again") || !strings.Contains(out, "b refused") {
		t.Errorf("the program was not told:\n%s", out)
	}
}

func TestServeWritesNothingWithoutAnExplicitYes(t *testing.T) {
	for name, answer := range map[string]func(map[string]any) map[string]any{
		"decline": decline, "cancel": cancel, "accepted with the box unticked": unticked, "accepted with no content": empty,
	} {
		t.Run(name, func(t *testing.T) {
			dir := inDir(t)
			c := startServer(t, true, answer)
			out := c.run(writePackage(t, writeManifest, writeScript))
			if !strings.Contains(out, "a refused") || !strings.Contains(out, "b refused") {
				t.Errorf("the script was not told:\n%s", out)
			}
			if entries, _ := os.ReadDir(filepath.Join(dir, "out")); len(entries) != 0 {
				t.Fatalf("%d file(s) were written", len(entries))
			}
			if len(c.asked) != 1 {
				t.Errorf("asked %d times, want 1: a no is remembered for the run", len(c.asked))
			}
		})
	}
}

func TestServeNeverAsksAClientThatCannotShowAPrompt(t *testing.T) {
	dir := inDir(t)
	c := startServer(t, false, accept) // it would say yes, if asked
	pkg := writePackage(t, writeManifest, writeScript)
	trustTestPackage(t, pkg)
	out := c.run(pkg)
	if len(c.asked) != 0 {
		t.Fatal("a client that did not advertise elicitation was sent one")
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "out")); len(entries) != 0 {
		t.Fatal("a write ran with nobody able to approve it")
	}
	if !strings.Contains(out, "cannot show an approval prompt") {
		t.Errorf("the caller was not told why:\n%s", out)
	}
}

func TestServeReadsNeedNobody(t *testing.T) {
	inDir(t)
	os.MkdirAll("in", 0o755)
	os.WriteFile("in/x.txt", []byte("content\n"), 0o644)
	c := startServer(t, false, decline)
	pkg := writePackage(t, strings.Replace(writeManifest, "{path: out, access: write}", "{path: in, access: read}", 1), "cat in/x.txt\n")
	trustTestPackage(t, pkg)
	out := c.run(pkg)
	if !strings.Contains(out, "content") || len(c.asked) != 0 {
		t.Fatalf("asked %d, output:\n%s", len(c.asked), out)
	}
}

func TestServeRefusesToolsUnderAClientThatCannotDispatch(t *testing.T) {
	inDir(t)
	c := startServer(t, true, accept)
	pkg := writePackage(t, `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.test, name: needs-tools, version: 0.1.0}
execution: {entrypoint: main.sh}
tools:
  - {alias: search, capability: gmail.threads.search, effect: read}
`, "tap call search\n")
	if out := c.run(pkg); !strings.Contains(out, "cannot lend its connections") {
		t.Fatalf("got:\n%s", out)
	}
}

// Tap_run takes a package path from the model. The first time
// a package is run on this machine through a client that can ask, the person
// is asked, and the answer is kept by the package's digest.
func TestAPackageIsAskedAboutOnceAndAgainWhenItChanges(t *testing.T) {
	inDir(t)
	c := startServer(t, true, accept)
	pkg := writePackage(t, writeManifest, writeScript)
	c.run(pkg)
	c.run(pkg)
	if len(c.trust) != 1 {
		t.Fatalf("the same package was asked about %d times, want 1", len(c.trust))
	}
	for _, want := range []string{"writer", "digest", "files: out (write)", "program: touch"} {
		if !strings.Contains(c.trust[0], want) {
			t.Errorf("the first-run question does not say %q: %s", want, c.trust[0])
		}
	}
	os.WriteFile(filepath.Join(pkg, "main.sh"), []byte(writeScript+"\n# edited\n"), 0o644)
	c.run(pkg)
	if len(c.trust) != 2 {
		t.Fatalf("an edited package was asked about %d times in all, want 2", len(c.trust))
	}
}

func TestToolOnlyPackageUsesPerCallGateWithoutPackagePrompt(t *testing.T) {
	inDir(t)
	pkg := writePackage(t, `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.test, name: tool-only, version: 0.1.0}
execution: {entrypoint: main.sh}
tools:
  - {alias: create, capability: jira.issues.create, effect: write}
`, "tap call create {}\n")
	store := &trustStore{path: filepath.Join(t.TempDir(), "trusted.json")}
	asked := false
	refusal := admitPackage(store, func(string, string, string, string, string, string) bool {
		asked = true
		return false
	}, pkg)
	if refusal != "" || asked {
		t.Fatalf("tool-only package got a separate package prompt: refusal=%q asked=%v", refusal, asked)
	}
	if refusal := admitPackage(store, nil, pkg); refusal != "" {
		t.Fatalf("tool-only package refused without a prompt callback: %s", refusal)
	}
	// Effectful tool calls still stop in callTool without an approval; see
	// TestCallGate. This test only removes the redundant whole-package ask.
}

func TestPackageWithLocalReachStillRequiresPackageTrust(t *testing.T) {
	inDir(t)
	pkg := writePackage(t, writeManifest, writeScript)
	store := &trustStore{path: filepath.Join(t.TempDir(), "trusted.json")}
	asked := false
	refusal := admitPackage(store, func(string, string, string, string, string, string) bool {
		asked = true
		return false
	}, pkg)
	if !asked || !strings.Contains(refusal, "did not agree to run") {
		t.Fatalf("package with local file/command reach skipped trust: refusal=%q asked=%v", refusal, asked)
	}
}

func TestADeclinedPackageIsNotRun(t *testing.T) {
	dir := inDir(t)
	c := startServer(t, true, accept)
	c.noTrust = true
	r := c.call("tools/call", map[string]any{"name": "tap_run", "arguments": c.stagePackage(writePackage(t, writeManifest, writeScript))})
	if r["isError"] != true {
		t.Fatalf("a package the person declined was run: %v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, "out", "a.txt")); err == nil {
		t.Fatal("a declined package wrote a file")
	}
	if len(c.asked) != 0 {
		t.Fatalf("a declined package went on to ask for changes: %v", c.asked)
	}
}

func TestAClientThatCannotAskRefusesAnUntrustedPackage(t *testing.T) {
	inDir(t)
	c := startServer(t, false, nil)
	out := c.run(writePackage(t, writeManifest, writeScript))
	if len(c.trust) != 0 || !strings.Contains(out, "did not agree to run") || !strings.Contains(out, "tap trust") {
		t.Fatalf("non-eliciting client did not refuse with owner trust instructions: %s; questions=%v", out, c.trust)
	}
}

func TestNonElicitingClientRequiresExactPackageTrust(t *testing.T) {
	inDir(t)
	pkg := writePackage(t, writeManifest, writeScript)
	store := &trustStore{path: filepath.Join(t.TempDir(), "trusted.json")}
	if refusal := admitPackage(store, nil, pkg); !strings.Contains(refusal, "tap trust") {
		t.Fatalf("untrusted local-reach package was admitted without a question: %q", refusal)
	}
	digest, _, err := packageDigest(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.add(digest, "writer", pkg); err != nil {
		t.Fatal(err)
	}
	if refusal := admitPackage(store, nil, pkg); refusal != "" {
		t.Fatalf("owner-trusted package refused: %s", refusal)
	}
	if err := os.WriteFile(filepath.Join(pkg, "main.sh"), []byte(writeScript+"\n# changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if refusal := admitPackage(store, nil, pkg); !strings.Contains(refusal, "tap trust") {
		t.Fatalf("changed package inherited its old trust: %q", refusal)
	}
}

func TestTapTrustLetsAClientThatCannotAskRunAPackageItDeclined(t *testing.T) {
	inDir(t)
	c := startServer(t, true, accept) // sets this test's config directory
	c.noTrust = true
	pkg := writePackage(t, writeManifest, writeScript)
	var out, errb bytes.Buffer
	if code := trustCommand([]string{pkg}, &out, &errb); code != 0 {
		t.Fatalf("trust: %d %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "files: out (write)") || !strings.Contains(out.String(), "trusted") {
		t.Fatalf("the command did not say what it trusted: %q", out.String())
	}
	// A client that can ask, and says no to every question, now runs it: the
	// first-run question is not asked.
	r := c.call("tools/call", map[string]any{"name": "tap_run", "arguments": c.stagePackage(pkg)})
	if body := fmt.Sprint(r["content"]); !strings.Contains(body, "a written") {
		t.Fatalf("a trusted package did not run: %v", r)
	}
	if len(c.trust) != 0 {
		t.Fatalf("a trusted package was asked about again: %v", c.trust)
	}
	out.Reset()
	trustCommand([]string{"--list"}, &out, &errb)
	if !strings.Contains(out.String(), "writer") {
		t.Fatalf("list: %q", out.String())
	}
	d, _, _ := packageDigest(pkg)
	out.Reset()
	trustCommand([]string{"--forget", d[:8]}, &out, &errb)
	if !strings.Contains(out.String(), "forgot 1") {
		t.Fatalf("forget: %q", out.String())
	}
}
