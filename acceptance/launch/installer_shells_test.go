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

// Name both shells explicitly: an ordinary sh invocation does not establish
// which shell parsed the installer on a particular acceptance runner.
func TestPublishedInstallerBashAndDash(t *testing.T) {
	v := version(t)
	if runtime.GOOS == "windows" {
		t.Skip("the Unix installer is not used on Windows")
	}
	for _, name := range []string{"bash", "dash"} {
		t.Run(name, func(t *testing.T) {
			shell, err := exec.LookPath(name)
			if err != nil {
				if runtime.GOOS == "linux" {
					t.Fatalf("Linux installer acceptance requires %s: %v", name, err)
				}
				t.Skipf("%s is not installed on this platform", name)
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
			cmd := exec.CommandContext(ctx, shell, "-s", "--", "--client", "none", "--dir", into)
			cmd.Env, cmd.Stdin, cmd.WaitDelay = m.env(), bytes.NewReader(script), time.Second
			out, err := cmd.CombinedOutput()
			t.Logf("published installer piped into %s (%s):\n%s", name, shell, indent(string(out)))
			if err != nil {
				t.Fatalf("%s install: %v", name, err)
			}
			want(t, m.run("", filepath.Join(into, "tap"), "version"), 0, "tap "+v)

			start, end := bytes.LastIndex(script, []byte("\n{\n")), bytes.LastIndexByte(script, '}')
			if start < 0 || end <= start {
				t.Fatal("published installer lacks its final brace group")
			}
			truncatedDir := filepath.Join(m.home, "truncated")
			for cut := start; cut <= end; cut++ {
				cutCtx, cutCancel := context.WithTimeout(context.Background(), 5*time.Second)
				truncated := exec.CommandContext(cutCtx, shell, "-s", "--", "--client", "none", "--dir", truncatedDir)
				truncated.Env, truncated.Stdin, truncated.WaitDelay = m.env(), bytes.NewReader(script[:cut]), time.Second
				out, _ := truncated.CombinedOutput()
				timedOut := cutCtx.Err()
				cutCancel()
				if timedOut != nil || strings.Contains(string(out), "downloading") || strings.Contains(string(out), "installed") {
					t.Fatalf("%s executed an incomplete installer at byte %d: %v\n%s", name, cut, timedOut, out)
				}
			}
			if _, err := os.Stat(filepath.Join(truncatedDir, "tap")); !os.IsNotExist(err) {
				t.Fatalf("truncated installer created a runner: %v", err)
			}
			t.Logf("%s: all %d incomplete final-invocation prefixes were inactive", name, end-start+1)
		})
	}
}
