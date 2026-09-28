package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStripTypes(t *testing.T) {
	js, err := stripTypes(`
interface Thread { id: string; unread: boolean }
enum Window { Day = 1, Week = 7 }
function unread(ts: Thread[]): number { return ts.filter((t: Thread) => t.unread).length }
const w: Window = Window.Week;
print(unread([{id: "a", unread: true}] as Thread[]), w satisfies number);
`, "main.ts")
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"interface", ": Thread", ": number", "satisfies", " as Thread"} {
		if strings.Contains(js, gone) {
			t.Errorf("%q survived:\n%s", gone, js)
		}
	}
	if _, err := stripTypes("const x: number = ;", "main.ts"); err == nil || !strings.Contains(err.Error(), "main.ts:1") {
		t.Fatalf("a syntax error was not reported with its place: %v", err)
	}
}

// Runs a TypeScript primitive in the real sandbox, calling a tool.
func TestATypeScriptPrimitiveRuns(t *testing.T) {
	store := interpreterStore(t)
	if _, _, _, err := obtain(store, "main.ts"); err != nil {
		t.Skipf("the JavaScript interpreter could not be obtained: %v", err)
	}
	inDir(t)
	pkg := t.TempDir()
	os.WriteFile(filepath.Join(pkg, "primitive.yaml"), []byte(`apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.test, name: typed, version: 0.1.0}
execution: {entrypoint: main.ts}
tools:
  - {alias: search, capability: gmail.threads.search, effect: read}
`), 0o644)
	os.WriteFile(filepath.Join(pkg, "main.ts"), []byte(`
interface Result { ok: boolean }
enum Level { Low = 1, High = 9 }
class Counter { constructor(private n: number = 0) {} add(by: number): number { this.n += by; return this.n } }
const r = tap.call("search", { query: "x" }) as Result;
const c = new Counter();
c.add(Level.High);
print(r.ok, c.add(1), [1, 2, 3].map((v: number): number => v * 2).join("+"));
`), 0o644)
	b := gmail()
	res, err := Run(context.Background(), Options{Package: pkg, Journal: io.Discard, InterpDir: store, RunsDir: t.TempDir(), Bridge: b})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Stdout) != "true 10 2+4+6" {
		t.Fatalf("got %q, stderr %q", res.Stdout, res.Stderr)
	}
	if len(b.calls) != 1 {
		t.Fatalf("%d tool calls", len(b.calls))
	}

	os.WriteFile(filepath.Join(pkg, "main.ts"), []byte("const x: = 1"), 0o644)
	if _, err := Run(context.Background(), Options{Package: pkg, Journal: io.Discard, InterpDir: store, RunsDir: t.TempDir(), Bridge: gmail()}); err == nil || !strings.Contains(err.Error(), "not valid TypeScript") {
		t.Fatalf("a TypeScript file that does not parse was run: %v", err)
	}
}
