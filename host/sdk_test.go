package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The guest SDK contract of doc 34 section 13.17: every error carries a code a
// program can branch on, in both languages, and a command the host refused to
// run raises, while a command that ran and exited non-zero is a result.
func TestErrorsCarryACodeAndARefusedCommandRaises(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed, and this runs it")
	}
	const manifest = `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.test, name: sdk, version: 0.1.0}
execution: {entrypoint: %s}
tools:
  - {alias: search, capability: gmail.threads.search, effect: read}
commands:
  - {command: git, args: ["*"], effect: read}
`
	const want = "tool refused PermissionError|Error\ncommand refused\nexit nonzero\n"
	for entry, program := range map[string]string{
		"main.py": `
try:
    tap.call("undeclared", {})
except Exception as e:
    print("tool", e.code, type(e).__name__)
try:
    tap.exec("curl", ["https://example.com"])
    print("command ran")
except PermissionError as e:
    print("command", e.code)
r = tap.exec("git", ["--no-such-flag"])
print("exit", "nonzero" if r.get("exit") else "zero")
`,
		"main.js": `
try { tap.call("undeclared", {}); } catch (e) { print("tool", e.code, e.name); }
try { tap.exec("curl", ["https://example.com"]); print("command ran"); } catch (e) { print("command", e.code); }
const r = tap.exec("git", ["--no-such-flag"]);
print("exit", r.exit ? "nonzero" : "zero");
`,
	} {
		t.Run(entry, func(t *testing.T) {
			store := interpreterStore(t)
			if _, _, _, err := obtain(store, entry); err != nil {
				t.Skipf("the interpreter could not be obtained: %v", err)
			}
			inDir(t)
			pkg := t.TempDir()
			os.WriteFile(filepath.Join(pkg, "primitive.yaml"), []byte(strings.Replace(manifest, "%s", entry, 1)), 0o644)
			os.WriteFile(filepath.Join(pkg, entry), []byte(program), 0o644)
			res, err := Run(context.Background(), Options{Package: pkg, Journal: io.Discard, InterpDir: store, RunsDir: t.TempDir(), Bridge: gmail()})
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Split(res.Stdout, "\n")
			exp := strings.Split(want, "\n")
			if len(got) != len(exp) {
				t.Fatalf("got %q, stderr %q", res.Stdout, res.Stderr)
			}
			for i := range exp {
				ok := false
				for _, alt := range strings.Split(exp[i], "|") {
					// "tool refused PermissionError|Error": the class differs by language, the code does not.
					if got[i] == alt || (i == 0 && got[i] == "tool refused "+alt) {
						ok = true
					}
				}
				if !ok {
					t.Fatalf("line %d: got %q, want %q\nstderr %q", i, got[i], exp[i], res.Stderr)
				}
			}
		})
	}
}
