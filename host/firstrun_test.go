package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The first commands a new user types, on a machine with no agents and no
// history, each end with a next step rather than an internal error.
func TestFirstRunDeadEndsSayWhatToDo(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	run := func(args ...string) (string, int) {
		cmd := exec.Command(self, append([]string{tapMainArg}, args...)...)
		cmd.Env = []string{"HOME=" + home, "USERPROFILE=" + home, "APPDATA=" + home, "LOCALAPPDATA=" + home, "PATH=" + t.TempDir()}
		cmd.Dir = t.TempDir()
		out, _ := cmd.CombinedOutput()
		return string(out), cmd.ProcessState.ExitCode()
	}
	for _, c := range []struct {
		args []string
		code int
		want []string
	}{
		{[]string{"--help"}, 0, []string{"usage: tap", "tap discover", "tap setup", "tap remove"}},
		{[]string{"help"}, 0, []string{"usage: tap"}},
		{nil, 2, []string{"usage: tap", "Start with: tap discover"}},
		{[]string{"setup"}, 0, []string{"No agent TAP can connect to is installed here.", "claude-code", "then run: tap setup"}},
		{[]string{"remove"}, 0, []string{"is not connected"}},
		{[]string{"no-such-folder"}, 2, []string{`tap: unknown command "no-such-folder"`, "usage: tap", "tap discover"}},
		{[]string{"upgrade"}, 2, []string{"npm install -g @telaralabs/tap@latest", "Restart your agents"}},
		{[]string{"update"}, 2, []string{"npm install -g @telaralabs/tap@latest"}},
		{[]string{"./no-such-folder"}, 2, []string{"tap: ./no-such-folder is not a primitive folder", "tap discover"}},
	} {
		out, code := run(c.args...)
		if code != c.code {
			t.Errorf("tap %v exited %d, want %d\n%s", c.args, code, c.code, out)
		}
		for _, w := range c.want {
			if !strings.Contains(out, w) {
				t.Errorf("tap %v: output lacks %q\n%s", c.args, w, out)
			}
		}
		if strings.Contains(out, "host ") || strings.Contains(out, "panic") {
			t.Errorf("tap %v: output shows an internal name or a panic\n%s", c.args, out)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); err == nil {
		t.Error("setup with no agents installed wrote an agent's configuration")
	}
}
