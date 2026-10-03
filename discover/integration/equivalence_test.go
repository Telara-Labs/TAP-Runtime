package integration

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/history"
	"gitlab.com/telara-labs/tap-runtime/discover/primitive"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// scriptedReaders are the readers of the scripted task (plan §6.4) as each
// agent that ran it for real recorded it (TENG-3124): Claude Code, OpenCode,
// the Kilo CLI, Goose, Crush, Continue and the Cline CLI. Stores are rebuilt
// and Continue's files copied under dir, so a test may change them.
func scriptedReaders(t *testing.T, dir string) map[string]trace.Reader {
	t.Helper()
	td := "../history/testdata/"
	db := func(name, sql string) string {
		p := filepath.Join(dir, name)
		buildStore(t, td+sql, p)
		return p
	}
	gdir := filepath.Join(dir, "goose")
	buildStore(t, td+"goose/sessions.sql", filepath.Join(gdir, "sessions.db"))
	work := filepath.Join(dir, "crush")
	buildStore(t, td+"crush/work/.crush/crush.sql", filepath.Join(work, ".crush", "crush.db"))
	pj, _ := json.Marshal(map[string]any{"projects": []any{map[string]any{"path": work, "data_dir": filepath.Join(work, ".crush")}}})
	os.WriteFile(filepath.Join(dir, "projects.json"), pj, 0o644)
	cont := filepath.Join(dir, "continue")
	os.MkdirAll(cont, 0o755)
	files, _ := filepath.Glob(td + "continue/sessions/*.json")
	for _, f := range files {
		b, _ := os.ReadFile(f)
		os.WriteFile(filepath.Join(cont, filepath.Base(f)), b, 0o644)
	}
	return map[string]trace.Reader{
		"claude-code": history.ClaudeCode{Dir: td + "scripted/claude"},
		"opencode":    history.OpenCodeDB{ID: "opencode", DB: db("opencode.db", "opencode/opencode.sql"), Configs: []string{td + "opencode/opencode.json"}},
		"kilo":        history.OpenCodeDB{ID: "kilo", DB: db("kilo.db", "kilo/kilo.sql"), Configs: []string{td + "kilo/kilo.json"}},
		"goose":       history.Goose{Dir: gdir},
		"crush":       history.Crush{Dir: dir, Configs: []string{td + "crush/crush.json"}},
		"continue":    history.Continue{Dir: cont, Configs: []string{td + "continue/config.yaml"}},
		"cline":       history.ClineCLI{Dir: td + "cline-cli/sessions", Configs: []string{td + "cline-cli/cline_mcp_settings.json"}},
	}
}

// scriptedSessions is each agent's session that ran the whole task: the
// one with the most calls.
func scriptedSessions(t *testing.T) map[string]trace.Session {
	t.Helper()
	out := map[string]trace.Session{}
	for client, r := range scriptedReaders(t, t.TempDir()) {
		ss, err := r.Read(time.Time{})
		if err != nil {
			t.Fatalf("%s: %v", client, err)
		}
		var best trace.Session
		for _, s := range ss {
			if len(s.Calls) > len(best.Calls) {
				best = s
			}
		}
		out[client] = best
	}
	return out
}

// steps is a session's replayable normalized step labels, in order.
func steps(s trace.Session) []string {
	norm := trace.Normalize([]trace.Session{s})
	if len(norm) != 1 {
		return nil
	}
	var out []string
	for _, st := range norm[0].Steps {
		// Steps a primitive cannot run (Claude loading its deferred tools
		// with ToolSearch) are the agent's own bookkeeping.
		if trace.Replayable(st.Label) {
			out = append(out, st.Label)
		}
	}
	return out
}

// Cross-client equivalence (plan §6.4): the same task, recorded by seven
// agents in seven formats, reads as the same work. After trace
// normalization every agent gives the same steps, and discovery over the
// seven sessions together finds one primitive whose executions come from
// every agent.
func TestScriptedTaskReadsTheSameInEveryAgent(t *testing.T) {
	ss := scriptedSessions(t)
	if len(ss) != 7 {
		t.Fatalf("%d agents", len(ss))
	}
	want := []string{"sh:wc", "mcp:search_issues", "mcp:get_issue", "mcp:get_issue"}
	for client, s := range ss {
		got := steps(s)
		if len(got) < len(want) {
			t.Errorf("%s: steps %v", client, got)
			continue
		}
		// The main turn, in recorded order. Agents ran steps 1-3 in
		// parallel in different orders, so the three are compared as a set
		// and the dependent call must come after the search.
		head := append([]string(nil), got[:3]...)
		sort.Strings(head)
		if strings.Join(head, ",") != "mcp:get_issue,mcp:search_issues,sh:wc" || got[3] != "mcp:get_issue" {
			t.Errorf("%s: steps %v", client, got)
		}
		// Every agent named the MCP server the calls went to.
		for _, c := range s.Calls {
			if strings.HasPrefix(c.Tool, "mcp:") && c.MCPServer != "tracker" {
				t.Errorf("%s: %s on server %q", client, c.Tool, c.MCPServer)
			}
		}
	}
	// One primitive across all seven: the search followed by the lookup of
	// the key it returned.
	var all []trace.Session
	for _, s := range ss {
		all = append(all, s)
	}
	res := primitive.Discover(all, nil)
	var found *primitive.Primitive
	for i := range res.Primitives {
		p := &res.Primitives[i]
		if strings.Join(p.Steps, " > ") == "mcp:search_issues > mcp:get_issue" {
			found = p
		}
	}
	if found == nil {
		var got []string
		for _, p := range res.Primitives {
			got = append(got, strings.Join(p.Steps, " > "))
		}
		t.Fatalf("no search > lookup primitive; found %v", got)
	}
	clients := map[string]bool{}
	for _, ex := range found.Executions {
		clients[ex.Client] = true
	}
	if len(clients) != 7 {
		t.Fatalf("the primitive's executions come from %d agents: %v", len(clients), clients)
	}
}

// The new readers work under a frozen corpus (plan §6.4): a manifest built
// from their sessions reads back the same sessions through FrozenReader, and
// a session changed after the freeze is reported, not silently re-read.
func TestNewReadersFreeze(t *testing.T) {
	dir := t.TempDir()
	rs := scriptedReaders(t, dir)
	var all []trace.Session
	for client, r := range rs {
		ss, err := r.Read(time.Time{})
		if err != nil || len(ss) == 0 {
			t.Fatalf("%s: %d sessions, %v", client, len(ss), err)
		}
		for _, s := range ss {
			if s.SourceDigest == "" {
				t.Errorf("%s/%s has no SourceDigest", client, s.ID)
			}
		}
		all = append(all, ss...)
	}
	m := history.BuildManifest(all, time.Now().Add(24*time.Hour))
	if len(m.Sessions) != len(all) {
		t.Fatalf("froze %d of %d sessions", len(m.Sessions), len(all))
	}
	for client, r := range rs {
		want, _ := r.Read(time.Time{})
		got, err := history.FrozenReader{Inner: r, Manifest: m}.Read(time.Time{})
		if err != nil || len(got) != len(want) {
			t.Errorf("%s: frozen read %d of %d, %v", client, len(got), len(want), err)
		}
	}
	// A Continue session edited after the freeze.
	files, _ := filepath.Glob(filepath.Join(dir, "continue", "*.json"))
	for _, f := range files {
		if filepath.Base(f) == "sessions.json" {
			continue
		}
		b, _ := os.ReadFile(f)
		os.WriteFile(f, append(b, ' '), 0o644)
	}
	if _, err := (history.FrozenReader{Inner: rs["continue"], Manifest: m}).Read(time.Time{}); !errors.Is(err, history.ErrCorpusChanged) {
		t.Fatalf("a changed session read back as frozen: %v", err)
	}
}
