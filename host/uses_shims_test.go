package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// kidPackage is a real compiled primitive that prints what it is told to.
func kidPackage(t *testing.T) string { return pkgSpec{name: "kid"}.write(t) }

func runParent(t *testing.T, spec pkgSpec, store string) *Result {
	t.Helper()
	inDir(t)
	kid := kidPackage(t)
	spec.rest = "uses:\n" + use(t, "kid", kid, "read")
	spec.name = "boss"
	parent := spec.write(t)
	res, err := Run(context.Background(), Options{Package: parent, Journal: nilWriter{}, InterpDir: store, CacheDir: sharedCompileCache(t), RunsDir: t.TempDir(), catalog: catalogOf(t, kid), Approve: approveAll(t)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Exit != 0 {
		t.Fatalf("exit %d, stdout %q, stderr %q", res.Exit, res.Stdout, res.Stderr)
	}
	if len(res.Children) < 1 {
		t.Fatalf("no child ran: %+v", res)
	}
	return res
}

type nilWriter struct{}

func (nilWriter) Write(p []byte) (int, error) { return len(p), nil }

// obtainable skips a test whose interpreter cannot be had on this machine.
func obtainable(t *testing.T, entry string) string {
	t.Helper()
	dir, err := storeDir("")
	if err != nil {
		t.Skip(err)
	}
	if _, _, _, err := obtain(dir, entry); err != nil {
		t.Skipf("the interpreter for %s could not be obtained: %v", entry, err)
	}
	return ""
}

const pyRunProgram = `import json
r = tap.run("kid", {"print": "{\"answer\": 42}"})
many = tap.run_many([("kid", {"print": "{\"n\": 1}"}), ("kid", {"print": "{\"n\": 2}"}), ("kid", {"exit": 3, "print": "x"})])
try:
    tap.run("kid", {"exit": 3, "print": "x"})
    failed = "no error"
except RuntimeError as e:
    failed = e.code
print(json.dumps([r, many[0], many[1], isinstance(many[2], RuntimeError), failed], separators=(",", ":")))
`

const jsRunProgram = `const r = tap.run("kid", { print: '{"answer": 42}' });
const many = tap.runMany([["kid", { print: '{"n": 1}' }], ["kid", { print: '{"n": 2}' }], ["kid", { exit: 3, print: "x" }]]);
let failed = "no error";
try { tap.run("kid", { exit: 3, print: "x" }); } catch (e) { failed = e.code; }
print(JSON.stringify([r, many[0], many[1], many[2] instanceof Error, failed]));
`

const wantShim = `[{"answer":42},{"n":1},{"n":2},true,"failed"]`

func TestThePythonShimRunsAPrimitive(t *testing.T) {
	store := obtainable(t, "main.py")
	res := runParent(t, pkgSpec{entry: "main.py", code: []byte(pyRunProgram)}, store)
	if strings.TrimSpace(res.Stdout) != wantShim {
		t.Fatalf("got %q", res.Stdout)
	}
	if len(res.Children) != 5 {
		t.Fatalf("%d children ran, want 5", len(res.Children))
	}
}

func TestTheJavaScriptShimRunsAPrimitive(t *testing.T) {
	store := obtainable(t, "main.js")
	res := runParent(t, pkgSpec{entry: "main.js", code: []byte(jsRunProgram)}, store)
	if strings.TrimSpace(res.Stdout) != wantShim {
		t.Fatalf("got %q", res.Stdout)
	}
}

func TestTheTypeScriptShimRunsAPrimitive(t *testing.T) {
	store := obtainable(t, "main.ts")
	prog := "declare const tap: any;\nconst r: any = tap.run(\"kid\", { print: '{\"answer\": 42}' });\nprint(JSON.stringify(r));\n"
	res := runParent(t, pkgSpec{entry: "main.ts", code: []byte(prog)}, store)
	if strings.TrimSpace(res.Stdout) != `{"answer":42}` {
		t.Fatalf("got %q", res.Stdout)
	}
}

func TestTheBashGuestRunsAPrimitive(t *testing.T) {
	store := interpreterStore(t)
	prog := "tap run kid '{\"print\":\"{\\\"answer\\\":42}\"}'\ntap run kid '{\"exit\":3,\"print\":\"x\"}' || echo \"exit=$?\"\ntap run nobody || echo \"refused=$?\"\n"
	res := runParent(t, pkgSpec{entry: "main.sh", code: []byte(prog)}, store)
	want := "{\"answer\":42}\nexit=3\nrefused=126\n"
	if res.Stdout != want {
		t.Fatalf("got %q, want %q (stderr %q)", res.Stdout, want, res.Stderr)
	}
}

func TestTheGoSDKRunsAPrimitive(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "rungo.wasm")
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", bin, "./host/testdata/rungo")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building a primitive with the Go SDK: %v\n%s", err, out)
	}
	code, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	res := runParent(t, pkgSpec{entry: "main.wasm", code: code}, "")
	if strings.TrimSpace(res.Stdout) != `{"answer":42} failed` {
		t.Fatalf("got %q (stderr %q)", res.Stdout, res.Stderr)
	}
}

// The C++ header is checked by building and running its own test with the
// vendored JSON library. No WASI C++ compiler is assumed, so a primitive
// built with it is not run here.
func TestTheCppSDKRunHelperPassesItsTests(t *testing.T) {
	cxx, err := exec.LookPath("c++")
	if err != nil {
		t.Skip("no C++ compiler is installed")
	}
	bin := filepath.Join(t.TempDir(), "tap_test")
	cmd := exec.Command(cxx, "-std=c++17", "-I", "sdk/cpp", "-I", "examples/languages/cpp/vendor", "-o", bin, "sdk/cpp/tap_test.cpp")
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the C++ SDK test: %v\n%s", err, out)
	}
	if out, err := exec.Command(bin).CombinedOutput(); err != nil {
		t.Fatalf("the C++ SDK test failed: %v\n%s", err, out)
	}
}

const cppParent = `#include <iostream>
#include <string>

#include <nlohmann/json.hpp>
#include <tap.hpp>

int main() {
  tap::Client client(std::cin, std::cout);
  tap::Reply ok, bad;
  tap::Error error;
  if (!client.run("kid", {{"print", "{\"answer\":42}"}}, ok, error)) {
    client.finish("", error.detail, 1, error);
    return 0;
  }
  const bool second = client.run("kid", {{"print", "x"}, {"exit", 3}}, bad, error);
  const std::string code = second ? "ok" : (error.code == tap::ErrorCode::failed ? "failed" : "other");
  client.finish(ok.result + " " + code + "\n", "", 0, error);
  return 0;
}
`

// A C++ parent built with the header's Client::run, run in the sandbox. The
// compiler is found as the C++ language example finds it: the name in its
// compiler.json, on PATH. Without one, the test skips.
func TestACppParentRunsAPrimitiveInTheSandbox(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot, "examples", "languages", "cpp", "compiler.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Compiler string `json:"compiler"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil || cfg.Compiler == "" {
		t.Fatalf("compiler.json: %v", err)
	}
	compiler, err := exec.LookPath(cfg.Compiler)
	if err != nil {
		t.Skipf("%s is not on PATH; install wasi-sdk to build C++ for WASI", cfg.Compiler)
	}
	work := t.TempDir()
	src := filepath.Join(work, "main.cpp")
	if err := os.WriteFile(src, []byte(cppParent), 0o600); err != nil {
		t.Fatal(err)
	}
	wasm := filepath.Join(work, "main.wasm")
	cmd := exec.Command(compiler, "-std=c++17", "-fno-exceptions", "-O2",
		"-I", filepath.Join(repoRoot, "sdk", "cpp"), "-I", filepath.Join(repoRoot, "examples", "languages", "cpp", "vendor"),
		"-o", wasm, src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building a C++ primitive with Client::run: %v\n%s", err, out)
	}
	code, err := os.ReadFile(wasm)
	if err != nil {
		t.Fatal(err)
	}
	res := runParent(t, pkgSpec{entry: "main.wasm", code: code}, "")
	if strings.TrimSpace(res.Stdout) != `{"answer":42} failed` {
		t.Fatalf("got %q (stderr %q)", res.Stdout, res.Stderr)
	}
	if len(res.Children) != 2 {
		t.Fatalf("%d children were recorded, want both runs", len(res.Children))
	}
}
