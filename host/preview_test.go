package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/bind"
	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

type previewProbeBridge struct {
	*fakeBridge
	inventories   int
	failInventory bool
	ask           bool
}

func (b *previewProbeBridge) Inventory() ([]bind.Tool, error) {
	b.inventories++
	if b.failInventory {
		return nil, errors.New("private-endpoint-token")
	}
	return b.inv, nil
}
func (b *previewProbeBridge) Asks(bind.Tool) (bool, error) { return b.ask, nil }

func TestPreviewSingleInventoryAndClientApprovalRules(t *testing.T) {
	b := &previewProbeBridge{fakeBridge: gmail(), ask: true}
	m := &mf.Manifest{Tools: []toolDecl{
		{Alias: "one", Capability: "gmail.threads.search", Effect: "read"},
		{Alias: "two", Capability: "gmail.threads.search", Effect: "read"},
	}}
	p := previewBindings(m, b, nil)
	if b.inventories != 1 || len(p.Connections) != 2 || p.Connections[0].Effective != "write" || !*p.Connections[0].ApprovalRequired {
		t.Fatalf("client rule or inventory changed %+v", p)
	}
	b.failInventory = true
	p = previewBindings(m, b, nil)
	encoded, _ := json.Marshal(p)
	if p.Status != "inventory_unavailable" || strings.Contains(string(encoded), "private-endpoint-token") {
		t.Fatalf("inventory error leaked %s", encoded)
	}
	if _, err := (inventoryBridge{Bridge: b}).Call(bind.Tool{}, nil); err == nil || len(b.calls) != 0 {
		t.Fatal("preview execution guard failed")
	}
	if p := (&server{clientName: "gemini-cli", relay: &relayHub{}}).previewConnections(m); p.Status != "inventory_unavailable" {
		t.Fatalf("relay pins claimed live inventory %+v", p)
	}
}

func TestPreviewUsesAdmissionWithoutCallsOrBindingChanges(t *testing.T) {
	f := gmail()
	f.inv = append(f.inv, bind.Tool{Server: "other Gmail", Name: "search_threads", Annotated: bind.Read})
	store := newFileBindings(filepath.Join(t.TempDir(), "bindings.json"))
	if err := store.set("claude-code", "gmail.threads.search", "claude.ai Gmail"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(store.path)
	m := &mf.Manifest{Tools: []toolDecl{
		{Alias: "search", Capability: "gmail.threads.search", Effect: "read"},
		{Alias: "draft", Capability: "gmail.draft.create", Effect: "read", Pin: &mf.Pin{Server: "claude.ai Gmail", Tool: "create_draft"}},
		{Alias: "missing", Capability: "slack.messages.send", Effect: "write", Optional: true},
		{Alias: "denied", Capability: "gmail.draft.delete", Effect: "destructive", Pin: &mf.Pin{Server: "claude.ai Gmail", Tool: "delete_draft"}},
	}}
	f.deny["claude.ai Gmail/delete_draft"] = true
	p := previewBindings(m, f, store)
	if p.Status != "available" || len(p.Connections) != 4 || p.Truncated {
		t.Fatalf("preview = %+v", p)
	}
	read, unknown, missing, denied := p.Connections[0], p.Connections[1], p.Connections[2], p.Connections[3]
	if read.Server != "claude.ai Gmail" || read.Tool != "search_threads" || read.Effective != "read" || read.ApprovalRequired == nil || *read.ApprovalRequired {
		t.Fatalf("saved read binding = %+v", read)
	}
	if unknown.Effective != "write" || unknown.ApprovalRequired == nil || !*unknown.ApprovalRequired {
		t.Fatalf("unknown annotation lost gate = %+v", unknown)
	}
	if missing.Status != "unavailable" || !missing.Optional || missing.ApprovalRequired != nil || denied.Status != "unresolved" {
		t.Fatalf("refusals = %+v %+v", missing, denied)
	}
	after, _ := os.ReadFile(store.path)
	if string(before) != string(after) || len(f.calls) != 0 {
		t.Fatal("preview wrote bindings or executed a tool")
	}
	// The same saved choice and effect produce the same admission used by Run.
	a, err := admitWith(store, nil, m.Tools[:2], f)
	if err != nil || a.Bindings[0].Server != read.Server || a.Bindings[1].effective() != unknown.Effective {
		t.Fatalf("execution disagrees with preview: %+v %v", a, err)
	}
	if err := store.set("claude-code", "gmail.threads.search", ""); err != nil {
		t.Fatal(err)
	}
	p = previewBindings(m, f, store)
	if p.Connections[0].Status != "ambiguous" || p.Connections[0].Server != "" || p.Connections[0].ApprovalRequired != nil {
		t.Fatalf("ambiguous binding claimed resolution = %+v", p)
	}
}

func TestPreviewMarksDispatcherRuntimeCheckAndExcludesSchemas(t *testing.T) {
	f := &fakeBridge{inv: []bind.Tool{{Server: "telara", Name: "telara_execute_action", Annotated: bind.Write,
		Schema: map[string]any{"description": "private-schema-secret"}}}}
	m := &mf.Manifest{Tools: []toolDecl{
		{Alias: "search", Capability: "jira.issue.search", Effect: "read", Pin: &mf.Pin{Server: "telara", Tool: "telara_jira_search_issues"}},
		{Alias: "dynamic", Capability: "telara.execute.action", Effect: "write", Pin: &mf.Pin{Server: "telara", Tool: "telara_execute_action"}},
	}}
	p := previewBindings(m, f, nil)
	for _, row := range p.Connections {
		if row.Status != "resolved" || !row.RuntimeCheck || row.Tool != "telara_execute_action" {
			t.Fatalf("dispatcher preview = %+v", row)
		}
	}
	if p.Connections[0].Operation != "jira/search_issues" || p.Connections[1].Operation != "" {
		t.Fatalf("operation identity = %+v", p)
	}
	encoded, _ := json.Marshal(p)
	if strings.Contains(string(encoded), "private-schema-secret") || len(f.calls) != 0 {
		t.Fatal("preview leaked a schema or executed a catalog probe")
	}
}

func TestPreviewBoundsWholeIdentitiesAndRejectsInvalidDeclarations(t *testing.T) {
	f := gmail()
	m := &mf.Manifest{}
	for i := 0; i < 105; i++ {
		m.Tools = append(m.Tools, toolDecl{Alias: fmt.Sprintf("a%d", i), Capability: "gmail.threads.search", Effect: "read"})
	}
	p := previewBindings(m, f, nil)
	if !p.Truncated || len(p.Connections) == 0 || len(p.Connections) > 100 {
		t.Fatalf("unbounded preview = %+v", p)
	}
	f.inv[0].Server = "Gmail " + strings.Repeat("s", 20000)
	p = previewBindings(m, f, nil)
	encoded, _ := json.Marshal(p)
	if !p.Truncated || len(p.Connections) != 0 || len(encoded) > 16<<10 {
		t.Fatal("oversized identity was shortened or not bounded")
	}
	m.Tools[1].Alias = m.Tools[0].Alias
	if p := previewBindings(m, f, nil); p.Status != "invalid_declarations" {
		t.Fatalf("duplicate alias was resolved separately = %+v", p)
	}
	if p := (&server{mcpURL: "not-a-url-with-private-token"}).previewConnections(&mf.Manifest{Tools: m.Tools[:1]}); p.Status != "inventory_unavailable" || len(p.Connections) != 0 {
		t.Fatalf("failed connection = %+v", p)
	}
}

func TestLoadPreviewMatchesRealMCPRunWithoutExecutingTools(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			t.Error(err)
			return
		}
		if msg.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{"serverInfo": map[string]any{"name": "gmail", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "search_threads", "annotations": map[string]any{"readOnlyHint": true}}}}
		case "tools/call":
			mu.Lock()
			calls = append(calls, msg.Params.Name)
			mu.Unlock()
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": `{"threads":[1,2,3]}`}}}
		default:
			t.Errorf("unexpected method %s", msg.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": result})
	}))
	defer ts.Close()
	pkg := writePackage(t, `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.test, name: preview, version: 1.0.0}
execution: {entrypoint: main.sh}
tools:
  - {alias: threads, capability: gmail.threads.search, effect: read}
`, "tap call threads '{}'\n")
	c := startServerArgs(t, true, accept, "--mcp-url", ts.URL)
	identity := c.stagePackage(pkg)
	loaded := toolObject(t, c.call("tools/call", map[string]any{"name": "tap_load", "arguments": identity}))
	p := loaded["connection_preview"].(map[string]any)
	rows := p["connections"].([]any)
	row := rows[0].(map[string]any)
	if p["status"] != "available" || row["tool"] != "search_threads" || row["base_effect"] != "read" || row["base_approval_required"] != false {
		t.Fatalf("MCP load preview = %#v", p)
	}
	mu.Lock()
	count := len(calls)
	mu.Unlock()
	if count != 0 || len(c.asked) != 0 || len(c.trust) != 0 {
		t.Fatal("tap_load executed a tool")
	}
	res, err := Run(context.Background(), Options{Package: pkg, ExpectedDigest: identity["digest"].(string), Journal: io.Discard, InterpDir: interpreterStore(t), RunsDir: t.TempDir(), MCPURL: ts.URL})
	mu.Lock()
	defer mu.Unlock()
	if err != nil || res == nil || len(calls) != 1 || calls[0] != "search_threads" {
		t.Fatalf("real WASM/MCP execution differs: %+v %v calls %v", res, err, calls)
	}
}
