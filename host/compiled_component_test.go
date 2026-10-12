package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/internal/component"
)

// rustcFor compiles a Rust program for a WASI target with the installed
// rustc. These are real modules and components from a real toolchain; the
// test skips, saying why, where Rust or the target is not installed.
func rustcFor(t *testing.T, mainRS, target string) []byte {
	t.Helper()
	if _, err := exec.LookPath("rustc"); err != nil {
		t.Skip("rustc is not installed; install Rust with the wasm32-wasip1 and wasm32-wasip2 targets to run this test")
	}
	out := filepath.Join(t.TempDir(), "main.wasm")
	cmd := exec.Command("rustc", "--edition", "2021", "--target", target, "-C", "opt-level=s", "-C", "debuginfo=0", "-o", out, filepath.Base(mainRS))
	cmd.Dir = filepath.Dir(mainRS)
	if b, err := cmd.CombinedOutput(); err != nil {
		if strings.Contains(string(b), "can't find crate for `std`") || strings.Contains(string(b), "target may not be installed") {
			t.Skipf("the Rust %s target is not installed (rustup target add %s)", target, target)
		}
		t.Fatalf("rustc --target %s: %v\n%s", target, err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// journalRequests is what the run's journal says the broker did, without
// times, so two runs can be compared.
func journalRequests(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var e map[string]any
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("journal line %q: %v", line, err)
		}
		for _, k := range []string{"ts", "ms", "elapsed_ms", "duration_ms"} {
			delete(e, k)
		}
		out = append(out, e)
	}
	return out
}

// The Rust example's source is built twice, as a WASI Preview 1 module and
// as a WASI Preview 2 component. Both run through the same runner and must
// return the same output and make the same broker requests.
func TestTheSamePrimitiveBuiltAsModuleAndComponentBehavesTheSame(t *testing.T) {
	example, _ := filepath.Abs(filepath.Join("..", "examples", "languages", "rust"))
	p1 := rustcFor(t, filepath.Join(example, "src", "main.rs"), "wasm32-wasip1")
	p2 := rustcFor(t, filepath.Join(example, "src", "main.rs"), "wasm32-wasip2")
	if f, _ := component.Detect(p1); f != component.Module {
		t.Fatalf("wasip1 build is %q", f)
	}
	if f, _ := component.Detect(p2); f != component.Component {
		t.Fatalf("wasip2 build is %q", f)
	}
	manifest, err := os.ReadFile(filepath.Join(example, "primitive.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(filepath.Join(example, "..", "fixtures", "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	dir := inDir(t)
	os.MkdirAll(filepath.Join(dir, "examples", "languages", "fixtures"), 0o755)
	fixturePath := filepath.Join(dir, "examples", "languages", "fixtures", "tasks.json")
	os.WriteFile(fixturePath, fixture, 0o600)
	cache := t.TempDir()

	type outcome struct {
		res     *Result
		err     error
		journal []map[string]any
	}
	run := func(wasm []byte, manifest string, input string) outcome {
		pkg := t.TempDir()
		os.WriteFile(filepath.Join(pkg, "primitive.yaml"), []byte(manifest), 0o600)
		os.WriteFile(filepath.Join(pkg, "main.wasm"), wasm, 0o600)
		var j bytes.Buffer
		res, err := Run(context.Background(), Options{Package: pkg, Args: []string{input}, Journal: &j, CacheDir: cache, RunsDir: t.TempDir()})
		return outcome{res, err, journalRequests(t, j.Bytes())}
	}
	same := func(t *testing.T, a, b outcome) {
		t.Helper()
		if a.err != nil || b.err != nil {
			t.Fatalf("module: %v; component: %v", a.err, b.err)
		}
		if a.res.Stdout != b.res.Stdout || a.res.Stderr != b.res.Stderr || a.res.Exit != b.res.Exit || a.res.Ran != b.res.Ran || a.res.Refused != b.res.Refused {
			t.Fatalf("results differ:\nmodule    %+v\ncomponent %+v", a.res, b.res)
		}
		if !reflect.DeepEqual(a.journal, b.journal) {
			t.Fatalf("broker requests differ:\nmodule    %v\ncomponent %v", a.journal, b.journal)
		}
	}

	for _, c := range []struct{ input, want string }{
		{`{"status":"open"}`, `{"status":"open","count":2,"items":[{"id":"TAP-1","title":"Record terminal evidence"},{"id":"TAP-3","title":"Review extraction output"}]}`},
		{`{"status":"done"}`, `{"status":"done","count":1,"items":[{"id":"TAP-2","title":"Verify a declared file read"}]}`},
	} {
		t.Run(c.input, func(t *testing.T) {
			a, b := run(p1, string(manifest), c.input), run(p2, string(manifest), c.input)
			same(t, a, b)
			t.Logf("broker requests of both builds: %v", b.journal)
			if b.res.Stdout != c.want || b.res.Exit != 0 || b.res.Ran != 1 || b.res.Refused != 0 || len(b.journal) == 0 {
				t.Fatalf("%+v %v", b.res, b.journal)
			}
		})
	}
	t.Run("bad input fails before any request", func(t *testing.T) {
		a, b := run(p1, string(manifest), `{"status":"open","extra":true}`), run(p2, string(manifest), `{"status":"open","extra":true}`)
		same(t, a, b)
		if b.res.Exit == 0 || b.res.Ran != 0 || b.res.Stderr != "expected only status" {
			t.Fatalf("%+v", b.res)
		}
	})
	t.Run("an undeclared read is refused for both", func(t *testing.T) {
		undeclared := strings.Replace(string(manifest), "  - {path: examples/languages/fixtures/tasks.json, access: read}\n", "  - {path: elsewhere.json, access: read}\n", 1)
		if undeclared == string(manifest) {
			t.Fatal("the example's file declaration was not found")
		}
		a, b := run(p1, undeclared, `{"status":"open"}`), run(p2, undeclared, `{"status":"open"}`)
		same(t, a, b)
		if b.res.Exit == 0 || b.res.Ran != 0 || b.res.Refused != 1 || !strings.Contains(b.res.Stderr, "refused") {
			t.Fatalf("%+v", b.res)
		}
	})
}

// A component asks for the host's files, directories, sockets, environment
// and processes directly. Each attempt fails inside the guest; nothing
// reaches the host, and the run's only effect is its return frame.
const componentProbe = `
use std::io::Write;
fn kind<T>(r: std::io::Result<T>) -> String { match r { Ok(_) => "allowed".into(), Err(e) => format!("{:?}", e.kind()) } }
fn main() {
    let path = std::env::args().nth(1).unwrap_or_default();
    let report = format!(
        "{{\"file\":\"{}\",\"dir\":\"{}\",\"write\":\"{}\",\"tcp\":\"{}\",\"udp\":\"{}\",\"process\":\"{}\",\"env\":{}}}",
        kind(std::fs::read(&path)),
        kind(std::fs::read_dir("/")),
        kind(std::fs::write(format!("{}.new", path), b"x")),
        kind(std::net::TcpStream::connect("127.0.0.1:9")),
        kind(std::net::UdpSocket::bind("127.0.0.1:0")),
        kind(std::process::Command::new("/bin/sh").arg("-c").arg("echo outside").output()),
        std::env::vars().count(),
    );
    let mut out = std::io::stdout().lock();
    let frame = format!("{{\"method\":\"return\",\"stdout\":{:?},\"stderr\":\"\",\"exit\":0}}\n", report);
    out.write_all(frame.as_bytes()).unwrap();
    out.flush().unwrap();
}
`

func TestAComponentHasNoDirectFilesSocketsEnvironmentOrProcesses(t *testing.T) {
	src := filepath.Join(t.TempDir(), "main.rs")
	os.WriteFile(src, []byte(componentProbe), 0o600)
	wasm := rustcFor(t, src, "wasm32-wasip2")
	secret := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(secret, []byte("host data"), 0o600)
	t.Setenv("TAP_COMPONENT_PROBE", "should not be visible")
	pkg := t.TempDir()
	os.WriteFile(filepath.Join(pkg, "primitive.yaml"), []byte("apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.test, name: component-probe, version: 0.1.0}\nexecution: {entrypoint: main.wasm, timeoutSeconds: 20}\n"), 0o600)
	os.WriteFile(filepath.Join(pkg, "main.wasm"), wasm, 0o600)
	var j bytes.Buffer
	res, err := Run(context.Background(), Options{Package: pkg, Args: []string{secret}, Journal: &j, CacheDir: t.TempDir(), RunsDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(res.Stdout), &got); err != nil {
		t.Fatalf("%q: %v", res.Stdout, err)
	}
	t.Logf("the component's own attempts: %v", got)
	for _, k := range []string{"file", "dir", "write", "tcp", "udp", "process"} {
		if got[k] == "allowed" {
			t.Errorf("%s was allowed: %v", k, got)
		}
	}
	if got["tcp"] != "PermissionDenied" || got["udp"] != "PermissionDenied" || got["env"] != float64(0) {
		t.Errorf("sockets must be refused and the environment empty: %v", got)
	}
	if _, err := os.Stat(secret + ".new"); !os.IsNotExist(err) {
		t.Error("the component wrote a host file")
	}
	if res.Ran != 0 || res.Refused != 0 || len(journalRequests(t, j.Bytes())) != 0 {
		t.Fatalf("the probe reached the broker: %+v %s", res, j.String())
	}
}

// A component that imports wasi:http is refused before it runs: a primitive
// sends network requests through the broker's fetch, never directly.
func TestAComponentThatImportsHTTPIsRefusedBeforeItRuns(t *testing.T) {
	src := filepath.Join(t.TempDir(), "main.rs")
	os.WriteFile(src, []byte(componentProbe), 0o600)
	wasm := rustcFor(t, src, "wasm32-wasip2")
	// Rename the UDP interface (same length) so the component imports an
	// interface from wasi:http.
	renamed := bytes.ReplaceAll(wasm, []byte("wasi:sockets/udp@"), []byte("wasi:http/client@"))
	if bytes.Equal(renamed, wasm) {
		t.Fatal("the probe does not import wasi:sockets/udp")
	}
	pkg := t.TempDir()
	os.WriteFile(filepath.Join(pkg, "primitive.yaml"), []byte("apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.test, name: component-http, version: 0.1.0}\nexecution: {entrypoint: main.wasm}\n"), 0o600)
	os.WriteFile(filepath.Join(pkg, "main.wasm"), renamed, 0o600)
	_, err := Run(context.Background(), Options{Package: pkg, Args: []string{"x"}, CacheDir: t.TempDir(), RunsDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "no direct network access") {
		t.Fatalf("a wasi:http import was not refused: %v", err)
	}
}

func TestAnUnknownWasmVersionIsRefused(t *testing.T) {
	pkg := t.TempDir()
	os.WriteFile(filepath.Join(pkg, "primitive.yaml"), []byte("apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.test, name: old-component, version: 0.1.0}\nexecution: {entrypoint: main.wasm}\n"), 0o600)
	os.WriteFile(filepath.Join(pkg, "main.wasm"), []byte("\x00asm\x0c\x00\x01\x00"), 0o600)
	_, err := Run(context.Background(), Options{Package: pkg, CacheDir: t.TempDir(), RunsDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), fmt.Sprint("version 12")) {
		t.Fatalf("%v", err)
	}
}
