package rebuild

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRebuildSnapshotPreservesExecutableSource(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(from, "build-script"), []byte("#!/bin/sh\n"), 0755)
	if err := copyTree(from, to); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(to, "build-script"))
	if err != nil || info.Mode()&0111 == 0 {
		t.Fatalf("executable source lost: %v %v", info, err)
	}
}

func TestCanceledBuildBoundsInheritedOutputPipes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := (Here{}).Run(ctx, t.TempDir(), "sleep 5 & wait")
	if err == nil || ctx.Err() == nil {
		t.Fatalf("canceled build succeeded: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Fatalf("inherited output pipes held canceled build for %s", elapsed)
	}
}

const build = "GOOS=wasip1 GOARCH=wasm go build -trimpath -buildvcs=false -ldflags=-buildid= -o p.wasm ./src"

const yaml = `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.test, name: compiled, version: 0.1.0}
execution: {entrypoint: p.wasm}
provenance:
  source: src/
  toolchain: go
  build: "` + build + `"
`

const program = `package main

import "fmt"

func main() { fmt.Println("%s") }
`

// compiled writes a real package: Go source, and the wasm it builds to.
func compiled(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not installed")
	}
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0o755)
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.test/p\n\ngo 1.24\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte(strings.Replace(program, "%s", "honest", 1)), 0o644)
	os.WriteFile(filepath.Join(dir, "primitive.yaml"), []byte(yaml), 0o644)
	cmd := exec.Command("/bin/sh", "-c", build)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return dir
}

func TestAProgramBuiltFromItsSourceVerifies(t *testing.T) {
	dir := compiled(t)
	r, err := Verify(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Built || r.Shipped != r.Rebuilt || !strings.HasPrefix(r.Shipped, "sha256:") {
		t.Fatalf("%+v", r)
	}
	// Verifying changes nothing in the package.
	if _, err := os.Stat(filepath.Join(dir, ".home")); err == nil {
		t.Fatal("the rebuild wrote into the package")
	}
}

// The case the check exists for: the source a person reads says one thing
// and the program that ships does another.
func TestAProgramThatIsNotWhatItsSourceBuildsToIsRefused(t *testing.T) {
	dir := compiled(t)
	// The shipped program is built from other source than the package shows.
	os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte(strings.Replace(program, "%s", "something else entirely", 1)), 0o644)
	cmd := exec.Command("/bin/sh", "-c", build)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte(strings.Replace(program, "%s", "honest", 1)), 0o644)

	r, err := Verify(dir)
	if err == nil || !strings.Contains(err.Error(), "not what its source builds to") {
		t.Fatalf("a program that does not match its source verified: %v", err)
	}
	if r == nil || r.Shipped == r.Rebuilt {
		t.Fatalf("the result does not carry both digests: %+v", r)
	}
}

func TestOtherWaysARebuildIsRefused(t *testing.T) {
	for name, c := range map[string]struct {
		change func(dir string)
		want   string
	}{
		"one byte of the program changed": {func(d string) {
			b, _ := os.ReadFile(filepath.Join(d, "p.wasm"))
			b[len(b)-1] ^= 1
			os.WriteFile(filepath.Join(d, "p.wasm"), b, 0o644)
		}, "not what its source builds to"},
		"a build that produces nothing": {func(d string) {
			os.WriteFile(filepath.Join(d, "primitive.yaml"), []byte(strings.Replace(yaml, build, "true", 1)), 0o644)
		}, "did not produce"},
		"a build that fails": {func(d string) {
			os.WriteFile(filepath.Join(d, "primitive.yaml"), []byte(strings.Replace(yaml, build, "exit 3", 1)), 0o644)
		}, "failed"},
		"no build named": {func(d string) {
			os.WriteFile(filepath.Join(d, "primitive.yaml"), []byte(strings.Split(yaml, "provenance:")[0]), 0o644)
		}, "names no build"},
		"source the package does not contain": {func(d string) {
			os.WriteFile(filepath.Join(d, "primitive.yaml"), []byte(strings.Replace(yaml, "source: src/", "source: elsewhere/", 1)), 0o644)
		}, "does not contain"},
		"a symbolic link in the package": {func(d string) {
			os.Symlink("/etc", filepath.Join(d, "src", "link"))
		}, "symbolic link"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := compiled(t)
			c.change(dir)
			if _, err := Verify(dir); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error mentioning %q, got %v", c.want, err)
			}
		})
	}
}

func TestTheBuildDoesNotSeeTheCallersEnvironment(t *testing.T) {
	dir := compiled(t)
	t.Setenv("TAP_REBUILD_CANARY", "must-not-leak")
	leak := filepath.Join(t.TempDir(), "leak.txt")
	os.WriteFile(filepath.Join(dir, "primitive.yaml"), []byte(strings.Replace(yaml, build,
		"echo \\\"canary=[$TAP_REBUILD_CANARY]\\\" > "+leak+"; "+build, 1)), 0o644)
	if _, err := Verify(dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(leak); !strings.Contains(string(b), "canary=[]") {
		t.Fatalf("the build saw the caller's environment: %q", b)
	}
}

func TestASourceEntrypointIsItsOwnProof(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "primitive.yaml"), []byte(`apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.test, name: script, version: 0.1.0}
execution: {entrypoint: main.py}
`), 0o644)
	os.WriteFile(filepath.Join(dir, "main.py"), []byte("print(1)\n"), 0o644)
	r, err := Verify(dir)
	if err != nil || r.Built || r.Shipped != r.Rebuilt {
		t.Fatalf("%+v %v", r, err)
	}
}

// The verdict a publish pipeline records. With build false the author's build
// command must not run at all, which the marker file proves.
func TestVerdict(t *testing.T) {
	dir := compiled(t)
	marker := filepath.Join(t.TempDir(), "ran")
	y := strings.Replace(yaml, build, "touch "+marker+" && "+build, 1)
	os.WriteFile(filepath.Join(dir, "primitive.yaml"), []byte(y), 0o644)

	v, r, err := Verdict(dir, nil)
	if v != NotAttempted || err == nil || r == nil || r.Built {
		t.Fatalf("a compiled program, no build allowed: %s %+v %v", v, r, err)
	}
	if _, serr := os.Stat(marker); serr == nil {
		t.Fatal("the author's build ran although building was not allowed")
	}

	if v, _, err := Verdict(dir, Here{}); v != Reproduced || err != nil {
		t.Fatalf("an honest program, built: %s %v", v, err)
	}
	if _, serr := os.Stat(marker); serr != nil {
		t.Fatal("control failed: with building allowed the build did not run")
	}

	os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte(strings.Replace(program, "%s", "changed", 1)), 0o644)
	if v, _, err := Verdict(dir, Here{}); v != NotReproduced || err == nil {
		t.Fatalf("a program that is not what its source builds to: %s %v", v, err)
	}

	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "primitive.yaml"), []byte("apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.test, name: s, version: 0.1.0}\nexecution: {entrypoint: main.sh}\n"), 0o644)
	os.WriteFile(filepath.Join(src, "main.sh"), []byte("echo hi\n"), 0o644)
	if v, _, err := Verdict(src, nil); v != Reproduced || err != nil {
		t.Fatalf("a source entrypoint: %s %v", v, err)
	}
	os.Remove(filepath.Join(src, "main.sh"))
	if v, _, err := Verdict(src, nil); v != NotAttempted || err == nil {
		t.Fatalf("an entrypoint that is not in the package: %s %v", v, err)
	}
}
