package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type stringList []string

func (v *stringList) String() string     { return strings.Join(*v, ",") }
func (v *stringList) Set(s string) error { *v = append(*v, s); return nil }

// HTTP is an additional transport for the same fixed MCP surface. A session
// has its own client identity, approval requests and cancellation context.
// Credentials belong to the configured backend, never to model arguments.
type httpMCP struct {
	mu       sync.Mutex
	token    string
	origins  map[string]bool
	template *server
	sessions map[string]*httpSession
}

type httpSession struct {
	s         *server
	ctx       context.Context
	stop      context.CancelFunc
	requests  chan struct{} // serialize requests; approval replies bypass this queue
	mu        sync.Mutex
	active    *httpStream
	requestID string
	cancel    context.CancelFunc
	last      time.Time // guarded by httpMCP.mu
}

type httpStream struct {
	messages chan []byte
	ctx      context.Context
}

func (s *httpSession) Write(p []byte) (int, error) {
	s.mu.Lock()
	stream := s.active
	s.mu.Unlock()
	if stream == nil {
		return len(p), nil
	}
	b := append([]byte(nil), p...)
	select {
	case stream.messages <- b:
	case <-stream.ctx.Done():
	}
	return len(p), nil
}

func (s *httpSession) close() {
	s.stop()
	s.s.closeAll()
}

func newHTTPMCP(token string, origins []string, template *server) (*httpMCP, error) {
	if len(token) < 32 || strings.ContainsAny(token, " \t\r\n") {
		return nil, fmt.Errorf("HTTP bearer token must contain at least 32 characters and no whitespace")
	}
	h := &httpMCP{token: token, origins: map[string]bool{}, template: template, sessions: map[string]*httpSession{}}
	for _, origin := range origins {
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
			return nil, fmt.Errorf("HTTP Origin must be an exact http(s) origin without a path")
		}
		h.origins[origin] = true
	}
	return h, nil
}

func (h *httpMCP) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, s := range h.sessions {
		s.close()
		delete(h.sessions, id)
	}
}

func (h *httpMCP) expire(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, s := range h.sessions {
		if now.Sub(s.last) > 20*time.Minute {
			s.close()
			delete(h.sessions, id)
		}
	}
}

func (h *httpMCP) create() (string, *httpSession) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.sessions) >= 64 {
		return "", nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil
	}
	id := hex.EncodeToString(b)
	ctx, stop := context.WithCancel(context.Background())
	x := &httpSession{ctx: ctx, stop: stop, last: time.Now(), requests: make(chan struct{}, 1)}
	t := h.template
	x.s = &server{out: x, pending: map[int]chan rpcMessage{}, journal: t.journal,
		interpDir: t.interpDir, cacheDir: t.cacheDir, runsDir: t.runsDir,
		retention: t.retention, payloads: t.payloads, mcpURL: t.mcpURL,
		mcpHeaderFile: t.mcpHeaderFile, mcpServerName: t.mcpServerName, noRecord: t.noRecord, catalogRoot: t.catalogRoot}
	h.sessions[id] = x
	return id, x
}

func (h *httpMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/mcp" {
		http.NotFound(w, r)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && !h.origins[origin] {
		http.Error(w, "origin is not allowed", http.StatusForbidden)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+h.token)) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && v != "2025-03-26" && v != "2025-06-18" && v != "2025-11-25" {
		http.Error(w, "unsupported MCP protocol version", http.StatusBadRequest)
		return
	}
	if r.Method == http.MethodGet {
		w.Header().Set("Allow", "POST, DELETE")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		w.Header().Set("Allow", "POST, DELETE")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	id := r.Header.Get("Mcp-Session-Id")
	h.mu.Lock()
	x := h.sessions[id]
	if x != nil {
		x.last = time.Now()
	}
	h.mu.Unlock()
	if r.Method == http.MethodDelete {
		if x == nil {
			http.Error(w, "unknown session", http.StatusNotFound)
			return
		}
		h.mu.Lock()
		delete(h.sessions, id)
		h.mu.Unlock()
		x.close()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	if accept := r.Header.Get("Accept"); !strings.Contains(accept, "application/json") || !strings.Contains(accept, "text/event-stream") {
		http.Error(w, "Accept must include application/json and text/event-stream", http.StatusNotAcceptable)
		return
	}
	var m rpcMessage
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if d.Decode(&m) != nil || m.JSONRPC != "2.0" || (m.Method == "" && m.ID == nil) {
		http.Error(w, "invalid JSON-RPC message", http.StatusBadRequest)
		return
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		http.Error(w, "expected one JSON-RPC message", http.StatusBadRequest)
		return
	}
	if id != "" && x == nil {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	if m.Method == "initialize" {
		if id != "" || m.ID == nil {
			http.Error(w, "initialize starts a new session", http.StatusBadRequest)
			return
		}
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(m.Params, &p) != nil {
			http.Error(w, "invalid initialize", http.StatusBadRequest)
			return
		}
		var params map[string]any
		if json.Unmarshal(m.Params, &params) != nil || params == nil {
			http.Error(w, "initialize params must be an object", http.StatusBadRequest)
			return
		}
		// Negotiate only revisions this transport actually implements.
		if p.ProtocolVersion != "2025-03-26" && p.ProtocolVersion != "2025-06-18" && p.ProtocolVersion != "2025-11-25" {
			params["protocolVersion"] = "2025-06-18"
			m.Params, _ = json.Marshal(params)
		}
		id, x = h.create()
		if x == nil {
			http.Error(w, "session limit reached", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Mcp-Session-Id", id)
	} else if x == nil {
		http.Error(w, "initialize a session first", http.StatusBadRequest)
		return
	}
	if m.Method == "" {
		x.s.deliver(m)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if m.ID == nil {
		if m.Method == "notifications/cancelled" {
			var p struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			if json.Unmarshal(m.Params, &p) != nil {
				http.Error(w, "invalid cancellation", http.StatusBadRequest)
				return
			}
			x.mu.Lock()
			if x.requestID == string(p.RequestID) && x.cancel != nil {
				x.cancel()
			}
			x.mu.Unlock()
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	// A second request queues without stealing the first request's prompts.
	select {
	case x.requests <- struct{}{}:
	case <-r.Context().Done():
		return
	case <-x.ctx.Done():
		http.Error(w, "session closed", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(x.ctx, 20*time.Minute)
	stream := &httpStream{messages: make(chan []byte, 16), ctx: ctx}
	x.mu.Lock()
	x.active, x.requestID, x.cancel = stream, string(*m.ID), cancel
	x.mu.Unlock()
	x.s.mu.Lock()
	x.s.requestCtx = ctx
	x.s.mu.Unlock()
	go func() {
		defer func() { <-x.requests }()
		defer cancel()
		x.s.handle(m)
		x.mu.Lock()
		x.active, x.requestID, x.cancel = nil, "", nil
		x.mu.Unlock()
		close(stream.messages)
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		cancel()
		return
	}
	flusher.Flush()
	connected := true
	// A lost connection does not imply cancellation or replay a write. Drain
	// the original request; its outcome stays in the durable run evidence.
	for b := range stream.messages {
		if connected {
			_, err := fmt.Fprintf(w, "event: message\ndata: %s\n\n", strings.TrimSpace(string(b)))
			if err != nil {
				connected = false
			} else {
				flusher.Flush()
			}
		}
	}
}

func serveHTTP(address, tokenFile string, origins []string, template *server) error {
	if tokenFile == "" {
		return fmt.Errorf("--http-listen requires --http-token-file")
	}
	info, err := os.Stat(tokenFile)
	if err != nil {
		return fmt.Errorf("reading HTTP token file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("HTTP token file must be a regular file accessible only to its owner")
	}
	b, err := os.ReadFile(tokenFile)
	if err != nil {
		return fmt.Errorf("reading HTTP token file: %w", err)
	}
	h, err := newHTTPMCP(strings.TrimSpace(string(b)), origins, template)
	if err != nil {
		return err
	}
	defer h.close()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer listener.Close()
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case now := <-ticker.C:
				h.expire(now)
			case <-finished:
				return
			}
		}
	}()
	logf("HTTP MCP   %s/mcp (authenticated)", listener.Addr())
	return (&http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 16 << 10}).Serve(listener)
}
