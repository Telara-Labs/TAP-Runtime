package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A real WASI guest exercises the native stderr writer and host-command
// lifetime separately from the interpreters' JSON result protocol.
const diagnosticsGuest = `package main
import("bytes";"encoding/json";"os";"time")
func main() {
 mode:=os.Args[1]
 if mode=="pending" {
  json.NewEncoder(os.Stdout).Encode(map[string]any{"id":"owned","method":"exec","command":"sleep","args":[]string{"3"}})
  time.Sleep(100*time.Millisecond)
 } else {
  chunk:=bytes.Repeat([]byte("synthetic diagnostic\n"),256)
  for i:=0;i<4000;i++ {os.Stderr.Write(chunk)}
  if mode=="memory" {os.Stderr.Write([]byte("Memory"));os.Stderr.Write([]byte("Error\n"));os.Exit(2)}
 }
 json.NewEncoder(os.Stdout).Encode(map[string]any{"method":"return","stdout":"complete\n","stderr":"result diagnostic\n","exit":0})
}`

func TestRealGuestDiagnosticsAndPendingProgramLifetime(t *testing.T) {
	work := t.TempDir()
	source, binary := filepath.Join(work, "main.go"), filepath.Join(work, "main.wasm")
	if err := os.WriteFile(source, []byte(diagnosticsGuest), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", binary, source)
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm") // existing Go target settings
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile real WASI diagnostics guest: %v\n%s", err, out)
	}
	manifest := "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.test, name: diagnostics, version: 0.1.0}\nexecution: {entrypoint: main.wasm}\ncommands:\n  - {command: sleep, args: [\"3\"], effect: read}\n"
	if err := os.WriteFile(filepath.Join(work, "primitive.yaml"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	run := func(mode string) (*Result, error) {
		return Run(context.Background(), Options{Package: work, Args: []string{mode}, Journal: io.Discard, CacheDir: cache, RunsDir: t.TempDir()})
	}
	res, err := run("complete")
	if err != nil || res.Exit != 0 || res.Stdout != "complete\n" || res.Stderr != "result diagnostic\n" {
		t.Fatalf("bounded native diagnostics altered the result: %+v %v", res, err)
	}
	if _, err := run("memory"); err == nil || !strings.Contains(err.Error(), "ran out of memory") {
		t.Fatalf("late memory failure diagnostic was lost: %v", err)
	}
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep is not installed; diagnostics cases completed")
	}
	start := time.Now()
	res, err = run("pending")
	if err != nil || res.Stdout != "complete\n" {
		t.Fatalf("completed guest's result changed while canceling pending work: %+v %v", res, err)
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("a finished guest waited for its pending host program: %s", elapsed)
	}
}
