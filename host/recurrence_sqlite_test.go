package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Use each client's actual SQLite row format and the real MCP subprocess.
// The same connection must see a session added after its first search;
// the second session starts only one second after the first.
func TestRapidRecurrenceThroughSQLiteAndSharedMCP(t *testing.T) {
	exe := buildTap(t)
	for _, agent := range []string{"opencode", "crush"} {
		t.Run(agent, func(t *testing.T) {
			home, env := isolatedHome(t)
			t.Cleanup(func() { killRunner(home, t) })
			config := filepath.Join(home, ".config", agent, agent+".json")
			if err := os.MkdirAll(filepath.Dir(config), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(config, []byte(`{"mcp":{"tap":{"command":"tap"}}}`), 0600); err != nil {
				t.Fatal(err)
			}
			var path string
			if agent == "opencode" {
				path = filepath.Join(home, ".local/share/opencode/opencode.db")
			} else {
				path = filepath.Join(home, "work/.crush/crush.db")
				dir := filepath.Join(home, ".local/share/crush")
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				b, _ := json.Marshal(map[string]any{"projects": []any{map[string]any{"path": filepath.Join(home, "work"), "data_dir": filepath.Dir(path)}}})
				if err := os.WriteFile(filepath.Join(dir, "projects.json"), b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			runSQL := func(query string, args ...any) {
				t.Helper()
				if _, err := db.Exec(query, args...); err != nil {
					t.Fatal(query, err)
				}
			}
			runSQL("PRAGMA journal_mode=WAL")
			runSQL("PRAGMA wal_autocheckpoint=0")
			if agent == "opencode" {
				runSQL("CREATE TABLE session (id TEXT, time_created INTEGER, time_updated INTEGER)")
				runSQL("CREATE TABLE message (id TEXT, session_id TEXT, time_created INTEGER, time_updated INTEGER, data TEXT)")
				runSQL("CREATE TABLE part (id TEXT, message_id TEXT, session_id TEXT, time_created INTEGER, time_updated INTEGER, data TEXT)")
			} else {
				runSQL("CREATE TABLE sessions (id TEXT, updated_at INTEGER)")
				runSQL("CREATE TABLE messages (session_id TEXT, role TEXT, parts TEXT, created_at INTEGER, updated_at INTEGER)")
			}
			query := "gitlab runner commit release readiness"
			text := "Can you check whether GitLab Runner commit abc123 is ready to release after v19.4.0?"
			first := time.Now().Add(-2 * time.Second).Truncate(time.Second)
			writeSession := func(id string, at time.Time) {
				t.Helper()
				args := map[string]any{"query": query}
				if agent == "opencode" {
					runSQL("INSERT INTO session VALUES (?,?,?)", id, at.UnixMilli(), at.UnixMilli())
					for _, row := range []struct {
						role string
						part any
					}{
						{"user", map[string]any{"type": "text", "text": text}},
						{"assistant", map[string]any{"type": "tool", "tool": "tap_tap_search", "callID": id + "-call", "state": map[string]any{"status": "running", "input": args}}},
					} {
						mid := id + row.role
						data, _ := json.Marshal(map[string]any{"role": row.role})
						part, _ := json.Marshal(row.part)
						stamp := at.UnixMilli()
						if row.role == "assistant" {
							stamp++
						}
						runSQL("INSERT INTO message VALUES (?,?,?,?,?)", mid, id, stamp, stamp, string(data))
						runSQL("INSERT INTO part VALUES (?,?,?,?,?,?)", mid, mid, id, stamp, stamp, string(part))
					}
				} else {
					runSQL("INSERT INTO sessions VALUES (?,?)", id, at.Unix())
					input, _ := json.Marshal(args)
					user, _ := json.Marshal([]any{map[string]any{"type": "text", "data": map[string]any{"text": text}}})
					tool, _ := json.Marshal([]any{map[string]any{"type": "tool_call", "data": map[string]any{"id": id + "-call", "name": "mcp_tap_tap_search", "input": string(input)}}})
					runSQL("INSERT INTO messages VALUES (?,?,?,?,?)", id, "user", string(user), at.Unix(), at.Unix())
					runSQL("INSERT INTO messages VALUES (?,?,?,?,?)", id, "assistant", string(tool), at.Unix(), at.Unix())
				}
			}
			writeSession("first", first)
			c := startProcClient(t, exe, env, home, agent, "--interpreters", interpreterStore(t), "--catalog-root", t.TempDir())
			if err := c.initialize(); err != nil {
				t.Fatal(err)
			}
			search := func() string {
				t.Helper()
				r, err := c.call("tools/call", map[string]any{"name": "tap_search", "arguments": map[string]any{"query": query}})
				if err != nil {
					t.Fatal(err)
				}
				if r["isError"] == true {
					t.Fatal(r)
				}
				return toolText(t, r)
			}
			if note := search(); strings.Contains(note, "Want me to save") {
				t.Fatalf("first session offered: %s", note)
			}
			// Complete the earlier call in place, as each client does. It must not
			// remain an ambiguous pending search in the incremental history cache.
			if agent == "opencode" {
				result, _ := json.Marshal(map[string]any{"type": "tool", "tool": "tap_tap_search", "callID": "first-call", "state": map[string]any{"status": "completed", "input": map[string]any{"query": query}, "output": "nothing saved"}})
				runSQL("UPDATE part SET data=?, time_updated=? WHERE id='firstassistant'", string(result), time.Now().UnixMilli())
			} else {
				result, _ := json.Marshal([]any{map[string]any{"type": "tool_result", "data": map[string]any{"tool_call_id": "first-call", "name": "mcp_tap_tap_search", "content": "nothing saved"}}})
				runSQL("INSERT INTO messages VALUES (?,?,?,?,?)", "first", "tool", string(result), time.Now().Unix(), time.Now().Unix())
			}
			writeSession("second", first.Add(time.Second))
			if note := search(); !strings.Contains(note, "1 earlier session") || !strings.Contains(note, agent+"/first/0") || !strings.Contains(note, "Want me to save") {
				t.Fatalf("fresh one-second recurrence lost: %s", note)
			}
		})
	}
}
