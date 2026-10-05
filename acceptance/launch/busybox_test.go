package launch

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Use native BusyBox on a fresh Linux runner, avoiding a Docker prerequisite.
// The installer and runner both come from the requested published release.
func TestBusyBoxInstallScript(t *testing.T) {
	v := version(t)
	if runtime.GOOS != "linux" {
		t.Skip("native BusyBox installer acceptance runs on Linux")
	}
	busybox, err := exec.LookPath("busybox")
	if err != nil {
		t.Fatal("install BusyBox to exercise the published installer")
	}
	m := newMachine(t)
	scriptPath := filepath.Join(m.home, "install.sh")
	download(t, fmt.Sprintf("%s/releases/download/v%s/install.sh", repo, v), scriptPath)
	script, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	into := filepath.Join(m.home, "bin")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, busybox, "sh", "-s", "--", "--client", "none", "--dir", into)
	cmd.Env, cmd.Stdin, cmd.WaitDelay = m.env(), bytes.NewReader(script), time.Second
	out, err := cmd.CombinedOutput()
	t.Logf("published installer piped into BusyBox sh:\n%s", indent(string(out)))
	if err != nil {
		t.Fatalf("BusyBox install: %v", err)
	}
	want(t, m.run("", filepath.Join(into, "tap"), "version"), 0, "tap "+v)

	// Every cut inside the final invocation must remain inactive under ash.
	start, end := bytes.LastIndex(script, []byte("\n{\n")), bytes.LastIndexByte(script, '}')
	if start < 0 || end <= start {
		t.Fatal("published installer lacks its final brace group")
	}
	for cut := start; cut <= end; cut++ {
		cutCtx, cutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		truncated := exec.CommandContext(cutCtx, busybox, "sh", "-s", "--", "--client", "none", "--dir", filepath.Join(m.home, "truncated"))
		truncated.Env, truncated.Stdin, truncated.WaitDelay = m.env(), bytes.NewReader(script[:cut]), time.Second
		out, _ := truncated.CombinedOutput()
		timedOut := cutCtx.Err()
		cutCancel()
		if timedOut != nil || strings.Contains(string(out), "downloading") || strings.Contains(string(out), "installed") {
			t.Fatalf("BusyBox executed an incomplete installer at byte %d: %v\n%s", cut, timedOut, out)
		}
	}
	if _, err := os.Stat(filepath.Join(m.home, "truncated", "tap")); !os.IsNotExist(err) {
		t.Fatalf("truncated installer created a runner: %v", err)
	}
	t.Logf("all %d incomplete final-invocation prefixes were inactive", end-start+1)
}
