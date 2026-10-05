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

func buildStore(t *testing.T, sqlFile, db string) {
	t.Helper()
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 is not installed")
	}
	os.MkdirAll(filepath.Dir(db), 0o755)
	cmd := exec.Command(bin, db)
	cmd.Stdin = bytes.NewReader(mustRead(sqlFile))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s: %v %s", sqlFile, err, out)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(to), 0o755)
	if err := os.WriteFile(to, mustRead(from), 0o644); err != nil {
		t.Fatal(err)
	}
}

// End to end for R3 and R4 (TENG-3118, TENG-3119): every agent's fixture in
// its real place under HOME, read by discover with no flags.
func TestDiscoverReadsR3R4Agents(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	td := "../history/testdata/"
	// OpenCode and the Kilo CLI.
	buildStore(t, td+"opencode/opencode.sql", filepath.Join(home, ".local/share/opencode/opencode.db"))
	copyFile(t, td+"opencode/opencode.json", filepath.Join(home, ".config/opencode/opencode.json"))
	buildStore(t, td+"kilo/kilo.sql", filepath.Join(home, ".local/share/kilo/kilo.db"))
	copyFile(t, td+"kilo/kilo.json", filepath.Join(home, ".config/kilo/kilo.json"))
	// Goose.
	buildStore(t, td+"goose/sessions.sql", filepath.Join(home, ".local/share/goose/sessions/sessions.db"))
	// Crush: a project database listed in projects.json.
	work := filepath.Join(home, "work")
	buildStore(t, td+"crush/work/.crush/crush.sql", filepath.Join(work, ".crush/crush.db"))
	pj, _ := json.Marshal(map[string]any{"projects": []any{map[string]any{"path": work, "data_dir": filepath.Join(work, ".crush")}}})
	os.MkdirAll(filepath.Join(home, ".local/share/crush"), 0o755)
	os.WriteFile(filepath.Join(home, ".local/share/crush/projects.json"), pj, 0o644)
	copyFile(t, td+"crush/crush.json", filepath.Join(home, ".config/crush/crush.json"))
	// Continue.
	copyTree(t, td+"continue/sessions", filepath.Join(home, ".continue/sessions"))
	copyFile(t, td+"continue/config.yaml", filepath.Join(home, ".continue/config.yaml"))
	// Cline CLI, and the Cline, Roo and Kilo extensions in VS Code.
	copyTree(t, td+"cline-cli/sessions", filepath.Join(home, ".cline/data/sessions"))
	copyFile(t, td+"cline-cli/cline_mcp_settings.json", filepath.Join(home, ".cline/data/settings/cline_mcp_settings.json"))
	user := filepath.Join(home, ".config", "Code", "User")
	if runtime.GOOS == "darwin" {
		user = filepath.Join(home, "Library", "Application Support", "Code", "User")
	}
	copyTree(t, td+"vscode-ext/Code/User", user)

	// The same scripted task in several agents gives the same step
	// sequence, which discover counts once (duplicate_sessions); what each
	// reader read shows in its calls.
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
		if c.Error != "" || c.SkippedRecords != 0 || c.UnreadableFiles != 0 {
			t.Errorf("%s: %+v", c.Client, c)
		}
	}
	sort.Strings(read)
	if got := strings.Join(read, ","); got != "cline,continue,crush,goose,kilo,opencode,roo" {
		t.Fatalf("read %s", got)
	}
	// Real runs: 5 scripted calls each (OpenCode also a 1-call first run;
	// the Cline CLI 4, one turn). Extensions: 3 per native task, 2 for the
	// older Cline XML task.
	want := map[string]int{"opencode": 6, "kilo": 5 + 3, "goose": 5, "crush": 5, "continue": 5, "cline": 4 + 3 + 2, "roo": 3}
	for k, v := range want {
		if calls[k] != v {
			t.Errorf("%s: %d calls, want %d", k, calls[k], v)
		}
	}
}
