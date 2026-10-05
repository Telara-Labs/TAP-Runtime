package launch

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemToolsRemainAvailableBesideUnrelatedTap(t *testing.T) {
	systemBin := t.TempDir()
	for _, name := range []string{"tap", "node", "curl", "sh"} {
		if err := os.WriteFile(filepath.Join(systemBin, exe(name)), []byte("unrelated system command"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", systemBin)
	// Apply the PATH that a fresh acceptance machine gives its commands.
	t.Setenv("PATH", strings.Join(newMachine(t).envPath(), string(os.PathListSeparator)))
	for _, name := range []string{"node", "curl", "sh"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Errorf("unrelated tap command hid the system's %s: %v", name, err)
		}
	}
}

func (m *machine) envPath() []string {
	for _, value := range m.env() {
		if strings.HasPrefix(value, "PATH=") {
			return filepath.SplitList(strings.TrimPrefix(value, "PATH="))
		}
	}
	return nil
}
