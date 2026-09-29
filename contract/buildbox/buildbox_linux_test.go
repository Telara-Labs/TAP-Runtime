//go:build linux

package buildbox

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The box under test is a real one: real namespaces, a real toolchain root
// copied from this machine, and the real helper. Where the kernel refuses an
// unprivileged user namespace these tests skip and say so; TAP_BUILDBOX_REQUIRED
// is not consulted, because a test must not depend on an environment variable
// to decide what it proves. CI runs them where a box can be made.
var box Box

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "tap-box-test-")
	if err != nil {
		panic(err)
	}
	code := func() int {
		defer os.RemoveAll(dir)
		helper := filepath.Join(dir, "tap-buildbox")
		if out, err := exec.Command("go", "build", "-o", helper, "../cmd/tap-buildbox").CombinedOutput(); err != nil {
			fmt.Printf("building the helper: %v\n%s", err, out)
			return 1
		}
		root := filepath.Join(dir, "root")
		os.MkdirAll(root, 0o755)
		for _, d := range []string{"bin", "lib", "usr", "etc", "sbin"} {
			if _, err := os.Stat("/" + d); err != nil {
				continue
			}
			// Links kept, ownership not: this user owns the copy. A file
			// this user cannot read is one the box does not need.
			exec.Command("cp", "-RP", "/"+d, root+"/").Run()
		}
		box = Box{Helper: helper, Root: root}
		return m.Run()
	}()
	os.Exit(code)
}

func need(t *testing.T) {
	t.Helper()
	if err := box.Available(); err != nil {
		t.Skipf("no build box here: %v", err)
	}
}

func run(t *testing.T, dir, command string) (string, error) {
	t.Helper()
	out, err := box.Run(context.Background(), dir, command)
	return string(out), err
}

func TestTheBoxHasNoNetwork(t *testing.T) {
	need(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port
	// Control: from outside the box the listener answers.
	if c, err := net.DialTimeout("tcp", l.Addr().String(), time.Second); err != nil {
		t.Fatalf("control failed: the listener cannot be reached from outside the box: %v", err)
	} else {
		c.Close()
	}
	out, err := run(t, t.TempDir(), fmt.Sprintf("nc -w 2 127.0.0.1 %d </dev/null && echo REACHED", port))
	if err == nil || strings.Contains(out, "REACHED") {
		t.Fatalf("the box reached a port of the machine it runs on: %q", out)
	}
}

func TestTheBoxCannotSeeTheFilesOfWhatMadeIt(t *testing.T) {
	need(t)
	secret := filepath.Join(t.TempDir(), "tls.key")
	os.WriteFile(secret, []byte("PRIVATE"), 0o644)
	work := t.TempDir()
	out, err := run(t, work, "cat "+secret+" 2>&1; ls / ; ls /proc 2>/dev/null | grep -c '^[0-9]' ")
	if strings.Contains(out, "PRIVATE") {
		t.Fatalf("the box read a file outside its root: %q", out)
	}
	if err != nil && !strings.Contains(out, "No such file") {
		t.Fatalf("%v: %q", err, out)
	}
	if !strings.Contains(out, "No such file") {
		t.Fatalf("the file should not exist inside the box: %q", out)
	}
	t.Logf("what the box sees:\n%s", out)
}

func TestTheBoxWritesOnlyInThePackage(t *testing.T) {
	need(t)
	work := t.TempDir()
	out, err := run(t, work, "echo made > made.txt && for p in /x /usr/x /bin/x /etc/x; do touch $p 2>/dev/null && echo WROTE $p; done; true")
	if err != nil {
		t.Fatalf("%v: %q", err, out)
	}
	if strings.Contains(out, "WROTE") {
		t.Fatalf("the box wrote outside the package: %q", out)
	}
	b, err := os.ReadFile(filepath.Join(work, "made.txt"))
	if err != nil || strings.TrimSpace(string(b)) != "made" {
		t.Fatalf("what the box wrote in the package is not there: %v %q", err, b)
	}
}

func TestTheBoxIsGivenNothingOfTheEnvironment(t *testing.T) {
	need(t)
	t.Setenv("VAULT_TOKEN", "PRIVATE")
	out, err := run(t, t.TempDir(), "env")
	if err != nil {
		t.Fatalf("%v: %q", err, out)
	}
	if strings.Contains(out, "PRIVATE") || strings.Contains(out, "VAULT_TOKEN") {
		t.Fatalf("the box was given the caller's environment: %q", out)
	}
	if !strings.Contains(out, "GOPROXY=off") {
		t.Fatalf("the box was not given its own environment: %q", out)
	}
}

func TestABuildThatTakesTooLongIsStoppedAndLeavesNothingRunning(t *testing.T) {
	need(t)
	b := box
	b.Timeout = time.Second
	t0 := time.Now()
	out, err := b.Run(context.Background(), t.TempDir(), "sleep 31 & sleep 32")
	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("a build that takes too long was not stopped: %v %q", err, out)
	}
	if time.Since(t0) > 10*time.Second {
		t.Fatalf("stopping took %s", time.Since(t0))
	}
	time.Sleep(300 * time.Millisecond)
	ps, _ := exec.Command("sh", "-c", "ps -o args 2>/dev/null || ps").Output()
	if strings.Contains(string(ps), "sleep 31") || strings.Contains(string(ps), "sleep 32") {
		t.Fatalf("a process of the box outlived it:\n%s", ps)
	}
}

func TestTheBoxBuildsAGoProgramWithoutANetwork(t *testing.T) {
	need(t)
	work := t.TempDir()
	os.MkdirAll(filepath.Join(work, "src"), 0o755)
	os.WriteFile(filepath.Join(work, "go.mod"), []byte("module example.test/p\n\ngo 1.24\n"), 0o644)
	os.WriteFile(filepath.Join(work, "src", "main.go"), []byte("package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"built in a box\") }\n"), 0o644)
	const build = "GOOS=wasip1 GOARCH=wasm go build -trimpath -buildvcs=false -ldflags=-buildid= -o p.wasm ./src"
	t0 := time.Now()
	out, err := run(t, work, build)
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(work, "p.wasm"))
	if err != nil || len(b) < 8 || string(b[:4]) != "\x00asm" {
		t.Fatalf("the build did not produce a wasm module: %v", err)
	}
	t.Logf("built %d bytes in %s", len(b), time.Since(t0).Round(time.Millisecond))

	// A program that needs a module from the network cannot be built, and
	// says why at once.
	os.WriteFile(filepath.Join(work, "src", "main.go"), []byte("package main\n\nimport _ \"golang.org/x/text/language\"\n\nfunc main() {}\n"), 0o644)
	out, err = run(t, work, build)
	if err == nil {
		t.Fatalf("a build that needs the network succeeded: %s", out)
	}
	t.Logf("a build that needs the network: %s", strings.TrimSpace(out))
}
