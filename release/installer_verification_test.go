package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var verificationReleaseDir = flag.String("verification-release-dir", "", "opt in to a published-release BusyBox installer test with Docker")

func TestVerificationPublishedInstallerWithBusyBoxOnLinux(t *testing.T) {
	if *verificationReleaseDir == "" {
		t.Skip("pass -verification-release-dir with a verified published release")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker unavailable")
	}
	script, err := os.ReadFile(filepath.Join(*verificationReleaseDir, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`(?m)^version=([^\r\n]+)$`).FindSubmatch(script)
	if len(match) != 2 {
		t.Fatal("published installer has no pinned version")
	}
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 15*time.Second)
	probe := exec.CommandContext(probeCtx, "docker", "version", "--format", "{{.Server.Version}}")
	probe.WaitDelay = time.Second
	probeOut, probeErr := probe.CombinedOutput()
	probeCancel()
	if probeErr != nil {
		t.Fatalf("Docker daemon prerequisite: %v %s", probeErr, probeOut)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	created, err := exec.CommandContext(ctx, "docker", "create", "--platform", "linux/amd64", "busybox:latest").CombinedOutput()
	if err != nil {
		t.Fatalf("create isolated BusyBox container: %v %s", err, created)
	}
	id := strings.TrimSpace(string(created))
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(id) {
		t.Fatalf("Docker returned an invalid container ID: %q", id)
	}
	probeName := "tap-installer-" + id[:12]
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		cleanup := exec.CommandContext(cleanupCtx, "docker", "rm", "-f", id, probeName)
		cleanup.WaitDelay = time.Second
		if out, err := cleanup.CombinedOutput(); err != nil {
			t.Logf("owned-container cleanup: %v %s", err, out)
		}
	}()
	bin := filepath.Join(t.TempDir(), "busybox")
	if b, err := exec.CommandContext(ctx, "docker", "cp", id+":/bin/busybox", bin).CombinedOutput(); err != nil {
		t.Fatalf("copy BusyBox: %v %s", err, b)
	}
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--name", probeName, "-i", "--platform", "linux/amd64", "--mount", "type=bind,source="+bin+",target=/probe/busybox,readonly", "golang:1.26", "/bin/sh", "-c", `/probe/busybox sh -s -- --client none --dir /tmp/tap-bin && /tmp/tap-bin/tap version`)
	cmd.WaitDelay = time.Second
	cmd.Stdin = bytes.NewReader(script)
	b, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(b), "tap "+string(match[1])) {
		t.Fatalf("BusyBox published installer: %v\n%s", err, b)
	}
	t.Logf("fresh-container published installer under BusyBox, with curl/checksum prerequisites: %s", b)
}

// Each shell consumes the complete installer on stdin as curl | shell would.
// It downloads the actual freshly-built runner from a local HTTP server.
func TestVerificationInstallPipedToAvailableShells(t *testing.T) {
	dir, _ := serve(t)
	script, err := os.ReadFile(filepath.Join(dir, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, shell := range []string{"sh", "dash", "bash", "busybox"} {
		t.Run(shell, func(t *testing.T) {
			bin, err := exec.LookPath(shell)
			if err != nil {
				t.Skipf("%s unavailable", shell)
			}
			into := t.TempDir()
			args := []string{"-s", "--", "--client", "none", "--dir", into}
			if shell == "busybox" {
				args = append([]string{"sh"}, args...)
			}
			cmd := exec.Command(bin, args...)
			cmd.Stdin = bytes.NewReader(script)
			out, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "installed tap 0.0.0-test") {
				t.Fatalf("piped install: %v\n%s", err, out)
			}
			got, err := exec.Command(filepath.Join(into, "tap"), "version").CombinedOutput()
			if err != nil || strings.TrimSpace(string(got)) != "tap 0.0.0-test" {
				t.Fatalf("installed binary: %v %s", err, got)
			}
		})
	}
}

// A cut after the final word "main" is valid shell syntax. The installer must
// not run from that truncated download, even though its function is complete.
func TestVerificationInstallerTruncatedAtFinalInvocationDoesNothing(t *testing.T) {
	dir, _ := serve(t)
	script, err := os.ReadFile(filepath.Join(dir, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cut := strings.LastIndex(string(script), "main \"$@\"") + len("main")
	if cut < len("main") {
		t.Fatal("final invocation missing")
	}
	home := t.TempDir()
	// These are existing client variables. Keep all changes inside this test's
	// HOME and prevent the calling client's CODEX_HOME from redirecting them.
	var env []string
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "HOME=") && !strings.HasPrefix(v, "CODEX_HOME=") {
			env = append(env, v)
		}
	}
	env = append(env, "HOME="+home)
	cmd := exec.Command("sh", "-s", "--", "--client", "none", "--dir", t.TempDir())
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(script[:cut])
	out, runErr := cmd.CombinedOutput()
	installed := filepath.Join(home, ".local", "bin", "tap")
	if _, err := os.Stat(installed); err == nil {
		t.Fatalf("a truncated script ran its installer and ignored the passed arguments; installed %s (exit %v):\n%s", installed, runErr, out)
	}
	fmt.Fprintf(os.Stderr, "tail-truncated installer exit: %v\n", runErr)
}

func TestEveryIncompleteFinalInvocationIsInactive(t *testing.T) {
	script := []byte(installSh)
	start, end := strings.LastIndex(installSh, "\n{\n"), strings.LastIndex(installSh, "}")
	if start < 0 || end <= start {
		t.Fatal("final brace group missing")
	}
	for _, shell := range []string{"sh", "dash", "bash"} {
		bin, err := exec.LookPath(shell)
		if err != nil {
			continue
		}
		for cut := start; cut <= end; cut++ {
			cmd := exec.Command(bin, "-s", "--", "--client", "none")
			cmd.Stdin = bytes.NewReader(script[:cut])
			out, _ := cmd.CombinedOutput()
			if strings.Contains(string(out), "downloading") || strings.Contains(string(out), "install:") || strings.Contains(string(out), "installed") {
				t.Fatalf("%s ran an incomplete invocation cut at %d: %s", shell, cut, out)
			}
		}
	}
}
