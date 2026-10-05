package bridge

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/bind"
)

// mcpServer is a small MCP server over streamable HTTP, run for real by
// net/http: initialize assigns a session, tools/list pages across two answers
// (the second as an event stream), and tools/call echoes or refuses.
type mcpServer struct {
	mu        sync.Mutex
	requests  []*http.Request
	callNames []string
	deleted   bool
}

func (s *mcpServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.Clone(r.Context()))
	s.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer good" {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodDelete {
		s.mu.Lock()
		s.deleted = true
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		return
	}
	var msg struct {
		ID     *int            `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	body, _ := io.ReadAll(r.Body)
	if json.Unmarshal(body, &msg) != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if msg.ID == nil { // a notification
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if msg.Method != "initialize" && r.Header.Get("Mcp-Session-Id") != "sess-1" {
		http.Error(w, "no session", http.StatusBadRequest)
		return
	}
	reply := func(result any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": result})
	}
	switch msg.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", "sess-1")
		reply(map[string]any{"protocolVersion": mcpProtocolVersion, "serverInfo": map[string]any{"name": "telara", "version": "9.9"}})
	case "tools/list":
		var p struct {
			Cursor string `json:"cursor"`
		}
		json.Unmarshal(msg.Params, &p)
		if p.Cursor == "" {
			reply(map[string]any{"nextCursor": "page2", "tools": []any{
				map[string]any{"name": "gmail_search_threads", "annotations": map[string]any{"readOnlyHint": true},
					"inputSchema": map[string]any{"type": "object"}},
			}})
			return
		}
		// The second page arrives as an event stream, after a notification
		// the bridge must skip.
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": map[string]any{"tools": []any{
			map[string]any{"name": "gmail_delete_draft", "annotations": map[string]any{"readOnlyHint": false, "destructiveHint": true}},
			map[string]any{"name": "gmail_create_draft"},
		}}})
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		json.Unmarshal(msg.Params, &p)
		s.mu.Lock()
		s.callNames = append(s.callNames, p.Name)
		s.mu.Unlock()
		if p.Name == "gmail_delete_draft" {
			reply(map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "denied by policy"}}})
			return
		}
		a, _ := json.Marshal(p.Arguments)
		reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": p.Name + " " + string(a)}}})
	default:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
	}
}

func TestMCPBridgeAgainstAServer(t *testing.T) {
	srv := &mcpServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	b, err := NewMCP(ts.URL, http.Header{"Authorization": {"Bearer good"}})
	if err != nil {
		t.Fatal(err)
	}
	if name, version := b.Client(); name != "mcp" || version != "9.9" {
		t.Fatalf("client %s %s", name, version)
	}
	if !Tested("mcp", "9.9") {
		t.Fatal("the MCP bridge must be admitted without an untested warning")
	}

	inv, err := b.Inventory()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bind.Effect{"gmail_search_threads": bind.Read, "gmail_delete_draft": bind.Destructive, "gmail_create_draft": bind.Unknown}
	if len(inv) != len(want) {
		t.Fatalf("inventory %+v: both pages, including the event stream, must be read", inv)
	}
	for _, tool := range inv {
		if tool.Server != "telara" {
			t.Errorf("%s: server %q, want the serverInfo name", tool.Name, tool.Server)
		}
		if tool.Annotated != want[tool.Name] {
			t.Errorf("%s: effect %v, want %v", tool.Name, tool.Annotated, want[tool.Name])
		}
	}

	out, err := b.Call(bind.Tool{Server: "telara", Name: "gmail_search_threads"}, map[string]any{"query": "x"})
	if err != nil || out != `gmail_search_threads {"query":"x"}` {
		t.Fatalf("call: %q %v", out, err)
	}
	if _, err := b.Call(bind.Tool{Server: "telara", Name: "gmail_delete_draft"}, nil); err == nil || !strings.Contains(err.Error(), "denied by policy") {
		t.Fatalf("a result marked isError must be an error that says why: %v", err)
	}

	b.Close()
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if !srv.deleted {
		t.Error("Close must end the session")
	}
	for _, r := range srv.requests {
		if r.Header.Get("Authorization") != "Bearer good" {
			t.Errorf("%s request without the header", r.Method)
		}
	}
}

func TestMCPBridgeUsesExplicitConnectionNameWithoutRenamingTools(t *testing.T) {
	srv := &mcpServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	b, err := NewMCPWithName(ts.URL, http.Header{"Authorization": {"Bearer good"}}, "telara-mcp")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	inventory, err := b.Inventory()
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory) != 3 {
		t.Fatalf("inventory has %d tools, want all advertised tools", len(inventory))
	}
	for _, tool := range inventory {
		if tool.Server != "telara-mcp" {
			t.Errorf("tool %q server=%q, want explicit connection alias", tool.Name, tool.Server)
		}
	}
	if _, err := b.Call(bind.Tool{Server: "telara-mcp", Name: "gmail_search_threads"}, map[string]any{"query": "x"}); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.callNames) != 1 || srv.callNames[0] != "gmail_search_threads" {
		t.Fatalf("wire tool names %v, want the advertised tool name unchanged", srv.callNames)
	}
}

func TestMCPBridgeValidatesExplicitConnectionName(t *testing.T) {
	for _, name := range []string{" ", " telara", "telara\nname", "telara\x00name"} {
		if _, err := NewMCPWithName("not-a-url", nil, name); err == nil || !strings.Contains(err.Error(), "connection name") {
			t.Errorf("name %q: got error %v, want a connection-name validation error", name, err)
		}
	}
}

func TestMCPBridgeReportsARefusedLogin(t *testing.T) {
	ts := httptest.NewServer(&mcpServer{})
	defer ts.Close()
	_, err := NewMCP(ts.URL, http.Header{"Authorization": {"Bearer bad"}})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err %v: a refused login must say so", err)
	}
}

// A redirect is never followed: any header in the header file would otherwise
// travel to the host the 30x names.
func TestMCPBridgeRefusesRedirects(t *testing.T) {
	var reached int
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached++ }))
	defer elsewhere.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/mcp", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	_, err := NewMCP(redirector.URL, http.Header{"X-Api-Key": {"secret"}})
	if err == nil || !strings.Contains(err.Error(), "307") || !strings.Contains(err.Error(), elsewhere.URL) {
		t.Fatalf("err %v: a redirect must be refused, naming where it pointed", err)
	}
	if reached != 0 {
		t.Fatalf("the redirect target received %d request(s)", reached)
	}
}

// A plain JSON reply is bounded like an event stream is.
func TestMCPBridgeBoundsAJSONResponse(t *testing.T) {
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"pad":"`))
		chunk := []byte(strings.Repeat("x", 1<<20))
		for i := 0; i <= maxMCPResponse>>20; i++ {
			w.Write(chunk)
		}
		w.Write([]byte(`"}}`))
	}))
	defer big.Close()
	_, err := NewMCP(big.URL, nil)
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("err %v: an oversized response must be refused and say so", err)
	}
}
