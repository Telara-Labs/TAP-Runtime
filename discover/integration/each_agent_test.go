package integration

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/history"
	"gitlab.com/telara-labs/tap-runtime/discover/primitive"
)

// scripted3Home rebuilds one agent's captured history under a fresh home:
// files are copied where the agent keeps them, a <db>.sql becomes <db>, and
// Crush's per-project store is registered in its projects.json.
func scripted3Home(t *testing.T, agent string) string {
	t.Helper()
	src := filepath.Join("../history/testdata/scripted3", agent)
	root := t.TempDir()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		dst := filepath.Join(root, rel)
		if strings.HasSuffix(dst, ".sql") {
			buildStore(t, p, strings.TrimSuffix(dst, ".sql"))
			return nil
		}
		os.MkdirAll(filepath.Dir(dst), 0o755)
		return os.WriteFile(dst, mustRead(p), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	if agent == "crush" {
		work := filepath.Join(root, "work")
		b, _ := json.Marshal(map[string]any{"projects": []any{map[string]any{"path": work, "data_dir": filepath.Join(work, ".crush")}}})
		os.MkdirAll(filepath.Join(home, ".local", "share", "crush"), 0o755)
		os.WriteFile(filepath.Join(home, ".local", "share", "crush", "projects.json"), b, 0o644)
	}
	return home
}

// TENG-3124: every agent that could run here ran the scripted task three
// times, each a new session searching with a different JQL
// (testdata/scripted3, real runs captured redacted). Discovery over one
// agent's history alone, read through its default reader at its usual
// place under home, finds the search > lookup primitive with an execution
// from each of the three sessions. This is what `tap discover --client X`
// does on that agent's machine.
func TestEachAgentAloneFindsTheScriptedPrimitive(t *testing.T) {
	agents, err := os.ReadDir("../history/testdata/scripted3")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, a := range agents {
		names = append(names, a.Name())
	}
	sort.Strings(names)
	if len(names) < 10 {
		t.Fatalf("%d agents captured: %v", len(names), names)
	}
	for _, agent := range names {
		t.Run(agent, func(t *testing.T) {
			home := scripted3Home(t, agent)
			rs, err := history.DefaultReaders([]string{agent}, home)
			if err != nil || len(rs) != 1 {
				t.Fatalf("readers %v %v", rs, err)
			}
			ss, err := rs[0].Read(time.Time{})
			if err != nil || len(ss) != 3 {
				t.Fatalf("%d sessions, %v", len(ss), err)
			}
			res := primitive.Discover(ss, nil)
			var found *primitive.Primitive
			var got []string
			for i := range res.Primitives {
				p := &res.Primitives[i]
				got = append(got, strings.Join(p.Steps, " > "))
				if strings.Join(p.Steps, " > ") == "mcp:search_issues > mcp:get_issue" {
					found = p
				}
			}
			if found == nil {
				t.Fatalf("no search > lookup primitive; found %v", got)
			}
			sessions := map[string]bool{}
			for _, ex := range found.Executions {
				sessions[ex.Session] = true
				if ex.Client != rs[0].Client() {
					t.Errorf("execution from %s", ex.Client)
				}
			}
			if len(sessions) != 3 {
				t.Fatalf("executions from %d sessions", len(sessions))
			}
		})
	}
}
