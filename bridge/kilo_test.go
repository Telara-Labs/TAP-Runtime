package bridge

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/bind"
)

// fakeSession serves the routes a TAP relay answers, over a Unix socket, as
// an OpenCode or Kilo session with one tracker server would.
func fakeSession(t *testing.T, config map[string]any) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "tapk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/global/health", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, map[string]any{"healthy": true, "version": "7.8.3"})
	})
	mux.HandleFunc("/config", func(w http.ResponseWriter, _ *http.Request) { reply(w, config) })
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, map[string]any{"tracker": map[string]any{"status": "connected"}})
	})
	mux.HandleFunc("/experimental/mcp/call-tool", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Server, Name string
			Arguments    map[string]any
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Name == "get_issue" {
			reply(w, map[string]any{"content": []any{map[string]any{"type": "text", "text": "issue ABC-99 does not exist"}}, "isError": true})
			return
		}
		reply(w, map[string]any{"content": []any{map[string]any{"type": "text", "text": `{"issues":[{"key":"ABC-12"}]}`}}})
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return sock
}

var trackerConfig = map[string]any{"mcp": map[string]any{"tracker": map[string]any{"type": "local", "command": []string{"tracker-mcp"}}}}

// Through a session's routes the bridge says which servers the session is
// configured with and whether each is connected, calls a tool, and returns
// a failed call as an error.
func TestKiloOverASessionsRoutes(t *testing.T) {
	k, err := newKiloOverRelay(fakeSession(t, trackerConfig), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, v := k.Client(); v != "7.8.3" {
		t.Fatalf("version %q", v)
	}
	servers, err := k.ConfiguredServers()
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].Name != "tracker" || !servers[0].StatusKnown || !servers[0].Connected || servers[0].Command == "" {
		t.Fatalf("configured servers: %+v", servers)
	}
	out, err := k.Call(bind.Tool{Server: "tracker", Name: "search_issues"}, map[string]any{"jql": "status = open"})
	if err != nil || !strings.Contains(out, "ABC-12") {
		t.Fatalf("search: %q %v", out, err)
	}
	if _, err := k.Call(bind.Tool{Server: "tracker", Name: "get_issue"}, map[string]any{"issue_key": "ABC-99"}); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("a failed call: %v", err)
	}
}

// A tool the person's configuration switches off or denies is denied; the
// most specific key decides.
func TestKiloDeniesWhatTheSessionsConfigurationDenies(t *testing.T) {
	cfg := map[string]any{
		"mcp":        trackerConfig["mcp"],
		"tools":      map[string]any{"tracker_get_issue": false},
		"permission": map[string]any{"tracker_*": "deny", "tracker_search_issues": "allow"},
	}
	k, err := newKiloOverRelay(fakeSession(t, cfg), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for tool, want := range map[string]bool{"get_issue": true, "search_issues": false, "update_issue": true} {
		if d, err := k.Denied(bind.Tool{Server: "tracker", Name: tool}); err != nil || d != want {
			t.Errorf("%s: denied %v %v, want %v", tool, d, err, want)
		}
	}
}

func TestMostSpecific(t *testing.T) {
	m := map[string]string{"*": "ask", "tracker_*": "deny", "tracker_get_issue": "allow"}
	for name, want := range map[string]string{"tracker_get_issue": "allow", "tracker_x": "deny", "other": "ask"} {
		if got, ok := mostSpecific(m, name); !ok || got != want {
			t.Errorf("%s = %q %v, want %q", name, got, ok, want)
		}
	}
}
