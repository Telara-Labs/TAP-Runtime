package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"gitlab.com/telara-labs/tap-runtime/discover"
	"gitlab.com/telara-labs/tap-runtime/discover/model"
)

// End to end for R5 and R6 (TENG-3120, TENG-3121): Copilot CLI, Zed,
// Windsurf (hook-captured transcripts and TAP's archive) and Aider in their
// real places under HOME, read by discover with no flags. Amp needs its own
// CLI and is covered by its reader test.
func TestDiscoverReadsR5R6Agents(t *testing.T) {
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("zstd is not installed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	td := "../history/testdata/"
	copyTree(t, td+"copilot-cli/session-state", filepath.Join(home, ".copilot/session-state"))
	copyFile(t, td+"copilot-cli/mcp-config.json", filepath.Join(home, ".copilot/mcp-config.json"))
	zed := filepath.Join(home, ".local/share/zed/threads")
	if runtime.GOOS == "darwin" {
		zed = filepath.Join(home, "Library/Application Support/Zed/threads")
	}
	buildStore(t, td+"zed/threads/threads.sql", filepath.Join(zed, "threads.db"))
	copyTree(t, td+"windsurf/windsurf-transcripts", filepath.Join(home, ".windsurf/transcripts"))
	copyTree(t, td+"windsurf/tap-archive", filepath.Join(home, ".tap/windsurf/transcripts"))
	copyFile(t, td+"aider/work/.aider.chat.history.md", filepath.Join(home, "code/work/.aider.chat.history.md"))

	var out, errOut bytes.Buffer
	if code := discover.Command([]string{"report", "--json"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var rep model.Report
	json.Unmarshal(out.Bytes(), &rep)
	calls := map[string]int{}
	var read []string
	for _, c := range rep.Clients {
		calls[c.Client] = c.Calls
		read = append(read, c.Client)
		if c.Error != "" || c.UnreadableFiles != 0 {
			t.Errorf("%s: %+v", c.Client, c)
		}
	}
	sort.Strings(read)
	if got := strings.Join(read, ","); got != "aider,copilot-cli,windsurf,zed" {
		t.Fatalf("read %s", got)
	}
	for k, v := range map[string]int{"copilot-cli": 5, "zed": 3, "windsurf": 3 + 1, "aider": 2} {
		if calls[k] != v {
			t.Errorf("%s: %d calls, want %d", k, calls[k], v)
		}
	}
}
