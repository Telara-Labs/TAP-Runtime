package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

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
		cmd := exec.Command("go", "build", "-o", filepath.Join(shStore, "sh.wasm"), "../guest-sh")
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

// client is a minimal MCP client: the other end of the wire.
type client struct {
	t      *testing.T
	in     io.WriteCloser
	sc     *bufio.Scanner
	answer func(params map[string]any) map[string]any // how it answers an elicitation
	asked  []string
	done   chan error
}

func startServer(t *testing.T, elicitation bool, answer func(map[string]any) map[string]any) *client {
	t.Helper()
	store := interpreterStore(t)
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	c := &client{t: t, in: cw, sc: bufio.NewScanner(cr), answer: answer, done: make(chan error, 1)}
	c.sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	go func() {
		c.done <- serve(sr, sw, []string{"--interpreters", store, "--journal", filepath.Join(t.TempDir(), "journal.jsonl")})
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
	r := c.call("tools/call", map[string]any{"name": "tap_run", "arguments": map[string]any{"package": pkg}})
	content, _ := r["content"].([]any)
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	return text
}

const writeManifest = `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: test, name: writer, version: 0.1.0}
entrypoint: main.sh
files:
  - {path: out, access: write}
commands:
  - {command: touch, effect: write}
`

const writeScript = `echo first > out/a.txt && echo "a written" || echo "a refused"
echo second > out/a.txt && echo "a written again" || echo "a refused again"
echo third > out/b.txt && echo "b written" || echo "b refused"
echo hi > /tmp/tap-serve-escape.txt || echo "outside refused"
`

var (
	accept = func(map[string]any) map[string]any {
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

func TestServeListsOneTool(t *testing.T) {
	c := startServer(t, true, accept)
	r := c.call("tools/list", map[string]any{})
	tools, _ := r["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "tap_run" {
		t.Fatalf("tools = %v", r)
	}
}

func TestServeAsksOncePerActionAndWritesOnYes(t *testing.T) {
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
	// Two files, three writes: a.txt is asked about once, b.txt once. The
	// path outside is refused before anybody is asked.
	if len(c.asked) != 2 {
		t.Fatalf("the person was asked %d times, want 2:\n%s", len(c.asked), strings.Join(c.asked, "\n---\n"))
	}
	if !strings.Contains(c.asked[0], "out/a.txt") || !strings.Contains(c.asked[0], "writer") {
		t.Errorf("the prompt does not say what would happen: %q", c.asked[0])
	}
	if _, err := os.Stat("/tmp/tap-serve-escape.txt"); err == nil {
		t.Fatal("a file was written outside the declared directory")
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
			if len(c.asked) != 2 {
				t.Errorf("asked %d times, want 2: a refusal is remembered for the run", len(c.asked))
			}
		})
	}
}

func TestServeNeverAsksAClientThatCannotShowAPrompt(t *testing.T) {
	dir := inDir(t)
	c := startServer(t, false, accept) // it would say yes, if asked
	out := c.run(writePackage(t, writeManifest, writeScript))
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
	out := c.run(writePackage(t, strings.Replace(writeManifest, "{path: out, access: write}", "{path: in, access: read}", 1), "cat in/x.txt\n"))
	if !strings.Contains(out, "content") || len(c.asked) != 0 {
		t.Fatalf("asked %d, output:\n%s", len(c.asked), out)
	}
}

func TestServeRefusesToolsUnderAClientThatCannotDispatch(t *testing.T) {
	inDir(t)
	c := startServer(t, true, accept)
	pkg := writePackage(t, `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: test, name: needs-tools, version: 0.1.0}
entrypoint: main.sh
tools:
  - {alias: search, capability: gmail.threads.search, effect: read}
`, "tap call search\n")
	if out := c.run(pkg); !strings.Contains(out, "cannot lend its connections") {
		t.Fatalf("got:\n%s", out)
	}
}
