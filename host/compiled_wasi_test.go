package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These are real Go/WASI guests, not a substitute interpreter or host process.
const compiledProbe = `package main
import("encoding/json";"os";"os/exec")
func finish(v any) { b,_:=json.Marshal(v); json.NewEncoder(os.Stdout).Encode(map[string]any{"method":"return","stdout":string(b)+"\n","exit":0}) }
func main() {
 mode,path:=os.Args[1],os.Args[2]
 if mode=="loop" {for {}}
 if mode=="probe" {
  _,e:=os.ReadFile(path)
  spawn:=exec.Command("/bin/sh","-c","echo outside").Run()
  finish(map[string]any{"env_count":len(os.Environ()),"native_read_denied":e!=nil,"process_denied":spawn!=nil});return
 }
 req:=map[string]any{"id":"one","method":mode,"path":path,"stdin":"compiled-created"}
 json.NewEncoder(os.Stdout).Encode(req)
 var reply map[string]any
 if e:=json.NewDecoder(os.Stdin).Decode(&reply);e!=nil {panic(e)}
 finish(reply)
}`

func TestCompiledWASIPreservesSandboxBrokerGatesAndTimeout(t *testing.T) {
	work := t.TempDir()
	source := filepath.Join(work, "main.go")
	binary := filepath.Join(work, "main.wasm")
	if err := os.WriteFile(source, []byte(compiledProbe), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", binary, source)
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm") // existing Go compiler target settings
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile real Go WASI: %v\n%s", err, out)
	}
	wasm, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(work, "input.txt")
	os.WriteFile(target, []byte("broker-only data"), 0600)
	base := "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.test, name: compiled-probe, version: 0.1.0}\nexecution: {entrypoint: main.wasm, timeoutSeconds: 1}\n"
	cache := t.TempDir()
	run := func(t *testing.T, mode, path, extra string, approve Approver) (*Result, error) {
		t.Helper()
		pkg := t.TempDir()
		os.WriteFile(filepath.Join(pkg, "primitive.yaml"), []byte(base+extra), 0600)
		os.WriteFile(filepath.Join(pkg, "main.wasm"), wasm, 0600)
		return Run(context.Background(), Options{Package: pkg, Args: []string{mode, path}, Journal: io.Discard, CacheDir: cache, RunsDir: t.TempDir(), Approve: approve})
	}
	t.Run("no native authority", func(t *testing.T) {
		res, err := run(t, "probe", target, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			EnvCount      int  `json:"env_count"`
			ReadDenied    bool `json:"native_read_denied"`
			ProcessDenied bool `json:"process_denied"`
		}
		if err := json.Unmarshal([]byte(res.Stdout), &got); err != nil {
			t.Fatal(err)
		}
		if got.EnvCount != 0 || !got.ReadDenied || !got.ProcessDenied || res.Ran != 0 {
			t.Fatalf("native authority leaked: %+v %+v", got, res)
		}
	})
	t.Run("declared read", func(t *testing.T) {
		res, err := run(t, "read", target, fmt.Sprintf("files: [{path: %q, access: read}]\n", target), nil)
		if err != nil || res.Ran != 1 || res.Refused != 0 || !strings.Contains(res.Stdout, "broker-only data") {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("undeclared read", func(t *testing.T) {
		res, err := run(t, "read", target, "", nil)
		if err != nil || res.Ran != 0 || res.Refused != 1 || !strings.Contains(res.Stdout, "refused") {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("unapproved and approved writes", func(t *testing.T) {
		path := filepath.Join(work, "created.txt")
		decl := fmt.Sprintf("files: [{path: %q, access: write}]\n", path)
		res, err := run(t, "write", path, decl, nil)
		if err != nil || res.Ran != 0 || res.Refused != 1 {
			t.Fatalf("%+v %v", res, err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("unapproved write landed")
		}
		asked := 0
		res, err = run(t, "write", path, decl, func(Ask) Grant { asked++; return Grant{OK: true, Limit: 1} })
		b, _ := os.ReadFile(path)
		if err != nil || asked != 1 || res.Ran != 1 || res.Refused != 0 || string(b) != "compiled-created" {
			t.Fatalf("%+v %v asked %d bytes %q", res, err, asked, b)
		}
	})
	t.Run("cancel computation", func(t *testing.T) {
		started := time.Now()
		_, err := run(t, "loop", target, "", nil)
		if err == nil || time.Since(started) > 10*time.Second {
			t.Fatalf("uncancelled WASI loop: %v %s", err, time.Since(started))
		}
	})
	t.Run("selected artifact digest", func(t *testing.T) {
		pkg := t.TempDir()
		os.WriteFile(filepath.Join(pkg, "primitive.yaml"), []byte(base), 0600)
		os.WriteFile(filepath.Join(pkg, "main.wasm"), wasm, 0600)
		expected, _, err := packageDigest(pkg)
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(pkg, "main.wasm"), append(append([]byte{}, wasm...), 0), 0600)
		_, err = Run(context.Background(), Options{Package: pkg, ExpectedDigest: expected, Args: []string{"probe", target}, Journal: io.Discard, RunsDir: t.TempDir()})
		if err == nil || !strings.Contains(err.Error(), "changed since discovery") {
			t.Fatalf("changed module accepted: %v", err)
		}
	})
}
