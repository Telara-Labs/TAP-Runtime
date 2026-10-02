package integration

import (
	"encoding/json"
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

// scriptedSessions reads the scripted task (plan §6.4) as each agent that
// ran it for real recorded it (TENG-3124): Claude Code, OpenCode, the Kilo
// CLI, Goose, Crush, Continue and the Cline CLI.
func scriptedSessions(t *testing.T) map[string]trace.Session {
	t.Helper()
	td := "../history/testdata/"
	dir := t.TempDir()
	out := map[string]trace.Session{}
	keep := func(client string) func([]trace.Session, error) {
		return func(ss []trace.Session, err error) {
			if err != nil {
				t.Fatalf("%s: %v", client, err)
			}
			// The session that ran the whole task: the one with the most calls.
			var best trace.Session
			for _, s := range ss {
				if len(s.Calls) > len(best.Calls) {
					best = s
				}
			}
			out[client] = best
		}
	}
	keep("claude-code")(history.ClaudeCode{Dir: td + "scripted/claude"}.Read(time.Time{}))
	db := filepath.Join(dir, "opencode.db")
	buildStore(t, td+"opencode/opencode.sql", db)
	keep("opencode")(history.OpenCodeDB{ID: "opencode", DB: db, Configs: []string{td + "opencode/opencode.json"}}.Read(time.Time{}))
	db = filepath.Join(dir, "kilo.db")
	buildStore(t, td+"kilo/kilo.sql", db)
	keep("kilo")(history.OpenCodeDB{ID: "kilo", DB: db, Configs: []string{td + "kilo/kilo.json"}}.Read(time.Time{}))
	gdir := filepath.Join(dir, "goose")
	buildStore(t, td+"goose/sessions.sql", filepath.Join(gdir, "sessions.db"))
	keep("goose")(history.Goose{Dir: gdir}.Read(time.Time{}))
	work := filepath.Join(dir, "crush")
	buildStore(t, td+"crush/work/.crush/crush.sql", filepath.Join(work, ".crush", "crush.db"))
	pj, _ := json.Marshal(map[string]any{"projects": []any{map[string]any{"path": work, "data_dir": filepath.Join(work, ".crush")}}})
	os.WriteFile(filepath.Join(dir, "projects.json"), pj, 0o644)
	keep("crush")(history.Crush{Dir: dir, Configs: []string{td + "crush/crush.json"}}.Read(time.Time{}))
	keep("continue")(history.Continue{Dir: td + "continue/sessions", Configs: []string{td + "continue/config.yaml"}}.Read(time.Time{}))
	keep("cline")(history.ClineCLI{Dir: td + "cline-cli/sessions", Configs: []string{td + "cline-cli/cline_mcp_settings.json"}}.Read(time.Time{}))
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
