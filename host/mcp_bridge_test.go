package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// A primitive run with --mcp-url calls the MCP server directly, carrying the
// header from --mcp-header-file on every request: how a service that runs
// primitives itself hands the runner its gateway.
func TestARunCallsAnMCPServerDirectly(t *testing.T) {
	store := interpreterStore(t)
	inDir(t)

	var mu sync.Mutex
	var calls []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var msg struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &msg)
		if msg.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{"serverInfo": map[string]any{"name": "telara", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{
				map[string]any{"name": "gmail_search_threads", "annotations": map[string]any{"readOnlyHint": true}},
			}}
		case "tools/call":
			var p struct {
				Name string `json:"name"`
			}
			json.Unmarshal(msg.Params, &p)
			mu.Lock()
			calls = append(calls, p.Name)
			mu.Unlock()
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": `{"threads":[1,2,3]}`}}}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": result})
	}))
	defer ts.Close()

	pkg := t.TempDir()
	os.WriteFile(filepath.Join(pkg, "primitive.yaml"), []byte(`apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.test, name: direct, version: 0.1.0}
execution: {entrypoint: main.sh}
tools:
  - {alias: threads, capability: gmail.threads.search, effect: read}
`), 0o644)
	os.WriteFile(filepath.Join(pkg, "main.sh"), []byte(`tap call threads '{"query":"x"}' | jq -r '.threads | length'`+"\n"), 0o644)
	headers := filepath.Join(t.TempDir(), "headers")
	os.WriteFile(headers, []byte("# the gateway's key\nAuthorization: Bearer tok\n"), 0o600)

	res, err := Run(context.Background(), Options{Package: pkg, Journal: io.Discard, InterpDir: store, RunsDir: t.TempDir(),
		MCPURL: ts.URL, MCPHeaderFile: headers})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Stdout) != "3" {
		t.Fatalf("stdout %q stderr %q", res.Stdout, res.Stderr)
	}
	if len(calls) != 1 || calls[0] != "gmail_search_threads" {
		t.Fatalf("calls %v", calls)
	}

	// Without the header the server refuses, and the run says so.
	_, err = Run(context.Background(), Options{Package: pkg, Journal: io.Discard, InterpDir: store, RunsDir: t.TempDir(), MCPURL: ts.URL})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("a refused login must stop the run and say so: %v", err)
	}
}

func TestParseHeaderLines(t *testing.T) {
	h, err := parseHeaderLines("# comment\n\nAuthorization: Bearer a:b\nX-Tenant:  t1 \n")
	if err != nil {
		t.Fatal(err)
	}
	if h.Get("Authorization") != "Bearer a:b" || h.Get("X-Tenant") != "t1" {
		t.Fatalf("%v", h)
	}
	for _, bad := range []string{"Bearer token-without-a-name", "Bad Name: x", ": x"} {
		if _, err := parseHeaderLines(bad); err == nil {
			t.Errorf("%q must be refused, not dropped", bad)
		}
	}
}
