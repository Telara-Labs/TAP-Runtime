package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const httpTestToken = "a-token-only-for-protocol-tests-1234567890"

type httpTestClient struct {
	t            *testing.T
	url, session string
	next         int
}

func (c *httpTestClient) post(v any) *http.Response {
	c.t.Helper()
	b, _ := json.Marshal(v)
	r, _ := http.NewRequest("POST", c.url+"/mcp", bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer "+httpTestToken)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", "2025-06-18")
	if c.session != "" {
		r.Header.Set("Mcp-Session-Id", c.session)
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(r)
	if err != nil {
		c.t.Fatal(err)
	}
	return resp
}

func (c *httpTestClient) call(method string, params any, answer func(rpcMessage) any) rpcMessage {
	c.t.Helper()
	c.next++
	id := c.next
	resp := c.post(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		c.t.Fatalf("%s: HTTP %d %s", method, resp.StatusCode, b)
	}
	if session := resp.Header.Get("Mcp-Session-Id"); session != "" {
		c.session = session
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 4096), 1<<20)
	for sc.Scan() {
		if !strings.HasPrefix(sc.Text(), "data: ") {
			continue
		}
		var m rpcMessage
		if err := json.Unmarshal([]byte(strings.TrimPrefix(sc.Text(), "data: ")), &m); err != nil {
			c.t.Fatal(err)
		}
		if m.Method != "" {
			if answer == nil {
				c.t.Fatalf("unexpected server request: %s", m.Method)
			}
			r := c.post(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": answer(m)})
			r.Body.Close()
			if r.StatusCode != 202 {
				c.t.Fatalf("approval response HTTP %d", r.StatusCode)
			}
			continue
		}
		if m.ID != nil && string(*m.ID) == stringMustJSON(id) {
			return m
		}
	}
	c.t.Fatalf("%s: no result: %v", method, sc.Err())
	return rpcMessage{}
}

func stringMustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func httpHarness(t *testing.T, template *server) (*httpMCP, *httptest.Server) {
	t.Helper()
	h, err := newHTTPMCP(httpTestToken, []string{"https://allowed.example"}, template)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(func() { h.close(); ts.Close() })
	return h, ts
}

func (c *httpTestClient) initialize(name string, elicit bool) {
	caps := map[string]any{}
	if elicit {
		caps["elicitation"] = map[string]any{}
	}
	m := c.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "clientInfo": map[string]any{"name": name, "version": "test"}, "capabilities": caps}, nil)
	if m.Error != nil || c.session == "" {
		c.t.Fatalf("initialize: %+v session=%q", m, c.session)
	}
}

func TestHTTPAuthenticationOriginAndSessionIsolation(t *testing.T) {
	h, ts := httpHarness(t, &server{journal: io.Discard})
	for _, c := range []struct {
		token, origin string
		want          int
	}{
		{"", "", 401}, {"wrong", "", 401}, {httpTestToken, "https://evil.example", 403},
		{httpTestToken, "https://allowed.example", 405}, {httpTestToken, "", 405},
	} {
		r, _ := http.NewRequest("GET", ts.URL+"/mcp", nil)
		if c.token != "" {
			r.Header.Set("Authorization", "Bearer "+c.token)
		}
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Fatalf("authentication/origin: HTTP%d want%d", resp.StatusCode, c.want)
		}
	}
	a := &httpTestClient{t: t, url: ts.URL}
	a.initialize("Cursor", true)
	b := &httpTestClient{t: t, url: ts.URL}
	b.initialize("Claude Web", false)
	if a.session == b.session {
		t.Fatal("sessions share identity")
	}
	if h.sessions[a.session].s.clientName != "Cursor" || !h.sessions[a.session].s.canElicit || h.sessions[b.session].s.canElicit {
		t.Fatal("client identity/capabilities leaked")
	}
	m := a.call("tools/list", map[string]any{}, nil)
	if m.Error != nil || !strings.Contains(string(m.Result), "tap_run") {
		t.Fatalf("tools/list: %+v", m)
	}
	h.expire(time.Now().Add(21 * time.Minute))
	resp := a.post(map[string]any{"jsonrpc": "2.0", "id": 99, "method": "ping"})
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("expired session HTTP%d", resp.StatusCode)
	}
}

func TestHTTPInvalidMessages(t *testing.T) {
	_, ts := httpHarness(t, &server{journal: io.Discard})
	c := &httpTestClient{t: t, url: ts.URL}
	for _, v := range []any{
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": nil},
		map[string]any{"jsonrpc": "1.0", "id": 1, "method": "initialize"},
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"},
	} {
		resp := c.post(v)
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("invalid message HTTP%d", resp.StatusCode)
		}
	}
}

func TestHTTPApprovalRepliesAndCeiling(t *testing.T) {
	inDir(t)
	catalog := t.TempDir()
	pkg := writePackage(t, writeManifest, "echo first > out/a.txt && echo written\necho second > out/b.txt && echo written-again || echo declined\n")
	identity := stageLivePackage(t, catalog, pkg)
	// This is a protocol test against the real sandbox and temp files. It
	// deliberately does not establish live client/service acceptance.
	_, ts := httpHarness(t, &server{journal: io.Discard, catalogRoot: catalog, interpDir: interpreterStore(t), runsDir: t.TempDir()})
	c := &httpTestClient{t: t, url: ts.URL}
	c.initialize("protocol-test", true)
	prompts := 0
	m := c.call("tools/call", map[string]any{"name": "tap_run", "arguments": identity}, func(m rpcMessage) any {
		if m.Method != "elicitation/create" {
			t.Fatalf("unexpected request %s", m.Method)
		}
		prompts++
		if prompts < 3 {
			return map[string]any{"action": "accept", "content": map[string]any{"approve": true, "limit": 1}}
		}
		return map[string]any{"action": "decline"}
	})
	if prompts != 3 || !strings.Contains(string(m.Result), "declined") || !strings.Contains(string(m.Result), "1 refused") {
		t.Fatalf("prompts=%d result=%s", prompts, m.Result)
	}
	var result struct {
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(m.Result, &result); err != nil || !result.IsError {
		t.Fatalf("a refused workflow was reported as successful: %s (%v)", m.Result, err)
	}
	if b, err := os.ReadFile(filepath.Join("out", "a.txt")); err != nil || strings.TrimSpace(string(b)) != "first" {
		t.Fatalf("approved first write %s %v", b, err)
	}
	if _, err := os.ReadFile(filepath.Join("out", "b.txt")); err == nil {
		t.Fatal("declined second write happened")
	}
	// Request queue/approval routing remained usable after completion.
	if m = c.call("ping", map[string]any{}, nil); m.Error != nil {
		t.Fatal(m.Error)
	}
}

func TestHTTPCancellationDoesNotLeakApprovalAcrossSessions(t *testing.T) {
	inDir(t)
	catalog := t.TempDir()
	pkg := writePackage(t, writeManifest, "echo unwanted > out/a.txt\n")
	identity := stageLivePackage(t, catalog, pkg)
	h, ts := httpHarness(t, &server{journal: io.Discard, catalogRoot: catalog, interpDir: interpreterStore(t), runsDir: t.TempDir()})
	a := &httpTestClient{t: t, url: ts.URL}
	a.initialize("first", true)
	b := &httpTestClient{t: t, url: ts.URL}
	b.initialize("second", true)
	resp := a.post(map[string]any{"jsonrpc": "2.0", "id": 77, "method": "tools/call", "params": map[string]any{"name": "tap_run", "arguments": identity}})
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	var prompt rpcMessage
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(sc.Text(), "data: ")), &prompt); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if prompt.Method != "elicitation/create" {
		t.Fatalf("no approval prompt: %+v", prompt)
	}
	// A response from another authenticated client session cannot grant it.
	wrong := b.post(map[string]any{"jsonrpc": "2.0", "id": prompt.ID, "result": map[string]any{"action": "accept", "content": map[string]any{"approve": true}}})
	wrong.Body.Close()
	s := h.sessions[a.session].s
	s.mu.Lock()
	pending := len(s.pending)
	s.mu.Unlock()
	if pending != 1 {
		t.Fatal("other session answered the approval")
	}
	cancel := a.post(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": 77}})
	cancel.Body.Close()
	for sc.Scan() {
	} // cancellation must unblock the original approval wait
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(filepath.Join("out", "a.txt")); err == nil {
		t.Fatal("cancelled write executed")
	}
	if m := a.call("ping", map[string]any{}, nil); m.Error != nil {
		t.Fatalf("session unusable after cancellation: %v", m.Error)
	}
}
