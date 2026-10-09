package bridge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/bind"
)

// ConfigBridge reaches a client that offers no way for another program to
// run its tools (Crush, Cursor) by opening the runner's own connection to
// each MCP server in the client's own configuration. It changes nothing in
// the client and holds no credentials: a server whose entry carries a
// secret (environment values or headers) is not used, nor is one that needs
// a sign-in the client stored, nor one the person has not approved in the
// client. Each is reported as blocked, naming what it needs.
type ConfigBridge struct {
	client  string
	version string
	wd      string
	env     []string

	mu      sync.Mutex
	servers map[string]configServer
	blocked map[string]string
	conns   map[string]*mcpConn
}

type configServer struct {
	Type     string            `json:"type"`
	Command  string            `json:"command"`
	Args     []string          `json:"args"`
	Env      map[string]string `json:"env"`
	URL      string            `json:"url"`
	Headers  map[string]string `json:"headers"`
	Disabled bool              `json:"disabled"`
}

// NewConfigBridgeIn reads client's MCP configuration ("crush", "cursor") as
// the client resolves it for the session's directory.
func NewConfigBridgeIn(client string, p Proc) (*ConfigBridge, error) {
	b := &ConfigBridge{client: client, wd: p.Wd(), env: p.Environ(), servers: map[string]configServer{}, blocked: map[string]string{}, conns: map[string]*mcpConn{}}
	home := p.Getenv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	var files []string
	var key, program string
	switch client {
	case "crush":
		cfgHome := p.Getenv("XDG_CONFIG_HOME")
		if cfgHome == "" {
			cfgHome = filepath.Join(home, ".config")
		}
		files = []string{filepath.Join(cfgHome, "crush", "crush.json"), filepath.Join(b.wd, "crush.json"), filepath.Join(b.wd, ".crush.json")}
		key, program = "mcp", "crush"
	case "cursor":
		files = []string{filepath.Join(home, ".cursor", "mcp.json"), filepath.Join(b.wd, ".cursor", "mcp.json")}
		key, program = "mcpServers", "cursor-agent"
	case "opencode", "kilo":
		// OpenCode's format, which Kilo shares: used when no TAP relay is
		// running in a session, as for a runner started outside one.
		cfgHome := p.Getenv("XDG_CONFIG_HOME")
		if cfgHome == "" {
			cfgHome = filepath.Join(home, ".config")
		}
		files = []string{filepath.Join(cfgHome, client, client+".json"), filepath.Join(b.wd, client+".json"), filepath.Join(b.wd, "."+client, client+".json")}
		key, program = "mcp", client
	default:
		return nil, fmt.Errorf("no configuration bridge for %s", client)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var doc map[string]json.RawMessage
		if json.Unmarshal(raw, &doc) != nil {
			continue
		}
		entries, ok := decodeServers(client, doc[key])
		if !ok {
			continue
		}
		for name, s := range entries {
			b.servers[name] = s // later files (the project's) win
		}
	}
	if path, err := p.LookPath(program); err == nil {
		if out, err := exec.Command(path, "--version").Output(); err == nil {
			if f := strings.Fields(string(out)); len(f) > 0 {
				b.version = strings.TrimPrefix(f[len(f)-1], "v")
			}
		}
	}
	approved := b.cursorApproved(p)
	if client == "cursor" && approved == nil {
		// Without Cursor's own list of approved servers, connecting would
		// pass over approvals the person gave or withheld.
		approved = map[string]bool{}
		for name := range b.servers {
			b.blocked[name] = "Cursor's approvals could not be read (cursor-agent mcp list); install the Cursor CLI"
		}
	}
	for name, s := range b.servers {
		switch {
		case name == "tap":
			delete(b.servers, name)
		case b.blocked[name] != "":
		case s.Disabled:
			b.blocked[name] = "disabled in " + client
		case len(s.Env) > 0 || len(s.Headers) > 0:
			b.blocked[name] = fmt.Sprintf("its %s entry carries a secret (environment or headers), which TAP does not use; run the primitive from a client that lends its connections", client)
		case approved != nil && !approved[name]:
			b.blocked[name] = "not approved in Cursor (cursor-agent mcp enable " + name + ")"
		}
	}
	return b, nil
}

// decodeServers reads a client's server entries into one shape. OpenCode
// and Kilo write {type: local|remote, command: [program, args...],
// environment, url, headers, enabled}; Crush and Cursor write {command,
// args, env, url, headers, disabled}.
func decodeServers(client string, raw json.RawMessage) (map[string]configServer, bool) {
	if client != "opencode" && client != "kilo" {
		var entries map[string]configServer
		return entries, json.Unmarshal(raw, &entries) == nil
	}
	var entries map[string]struct {
		Type        string            `json:"type"`
		Command     []string          `json:"command"`
		Environment map[string]string `json:"environment"`
		URL         string            `json:"url"`
		Headers     map[string]string `json:"headers"`
		Enabled     *bool             `json:"enabled"`
	}
	if json.Unmarshal(raw, &entries) != nil {
		return nil, false
	}
	out := map[string]configServer{}
	for name, e := range entries {
		s := configServer{Type: e.Type, Env: e.Environment, URL: e.URL, Headers: e.Headers, Disabled: e.Enabled != nil && !*e.Enabled}
		if len(e.Command) > 0 {
			s.Command, s.Args = e.Command[0], e.Command[1:]
		}
		out[name] = s
	}
	return out, true
}

// cursorApproved is the set of servers Cursor reports ready for the
// session's directory, or nil when the client is not Cursor or cannot say.
func (b *ConfigBridge) cursorApproved(p Proc) map[string]bool {
	if b.client != "cursor" {
		return nil
	}
	path, err := p.LookPath("cursor-agent")
	if err != nil {
		return nil
	}
	cmd := exec.Command(path, "mcp", "list")
	cmd.Dir = b.wd
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	ok := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		name, state, found := strings.Cut(strings.TrimSpace(line), ":")
		if found && strings.TrimSpace(state) == "ready" {
			ok[strings.TrimSpace(name)] = true
		}
	}
	return ok
}

func (b *ConfigBridge) Client() (string, string) { return b.client, b.version }
func (b *ConfigBridge) HasSchemas() bool         { return true }

// Blocked names each configured server the bridge does not use, and why.
func (b *ConfigBridge) Blocked() map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]string{}
	for k, v := range b.blocked {
		out[k] = v
	}
	return out
}

// Inventory lists the tools of every usable server, with their annotations
// and schemas.
func (b *ConfigBridge) Inventory() ([]bind.Tool, error) {
	b.mu.Lock()
	names := make([]string, 0, len(b.servers))
	for name := range b.servers {
		if _, no := b.blocked[name]; !no {
			names = append(names, name)
		}
	}
	b.mu.Unlock()
	sort.Strings(names)
	var out []bind.Tool
	for _, name := range names {
		c, err := b.conn(name)
		if err != nil {
			b.mu.Lock()
			b.blocked[name] = err.Error()
			b.mu.Unlock()
			continue
		}
		var cursor string
		for page := 0; page < 20; page++ {
			params := map[string]any{}
			if cursor != "" {
				params["cursor"] = cursor
			}
			r, err := c.request("tools/list", params)
			if err != nil {
				break
			}
			tools, _ := r["tools"].([]any)
			for _, x := range tools {
				t, _ := x.(map[string]any)
				tn, _ := t["name"].(string)
				ann, _ := t["annotations"].(map[string]any)
				schema, _ := t["inputSchema"].(map[string]any)
				if tn != "" {
					out = append(out, bind.Tool{Server: name, Name: tn, Annotated: codexEffect(ann), Schema: schema})
				}
			}
			cursor, _ = r["nextCursor"].(string)
			if cursor == "" {
				break
			}
		}
	}
	return out, nil
}

// Denied is false: a server the person disabled or did not approve is not
// connected at all, so its tools are never offered.
func (b *ConfigBridge) Denied(bind.Tool) (bool, error) { return false, nil }

// Call runs one tool on the runner's connection to its server.
func (b *ConfigBridge) Call(t bind.Tool, args map[string]any) (string, error) {
	b.mu.Lock()
	why, no := b.blocked[t.Server]
	b.mu.Unlock()
	if no {
		return "", fmt.Errorf("%s/%s: %s", t.Server, t.Name, why)
	}
	c, err := b.conn(t.Server)
	if err != nil {
		return "", fmt.Errorf("%s/%s: %w", t.Server, t.Name, err)
	}
	if args == nil {
		args = map[string]any{}
	}
	r, err := c.request("tools/call", map[string]any{"name": t.Name, "arguments": args})
	if err != nil {
		return "", fmt.Errorf("%s/%s: %w", t.Server, t.Name, err)
	}
	return callResult(t, r)
}

func (b *ConfigBridge) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.conns {
		c.close()
	}
	b.conns = map[string]*mcpConn{}
}

// conn opens, once, the runner's connection to a configured server.
func (b *ConfigBridge) conn(name string) (*mcpConn, error) {
	b.mu.Lock()
	if c := b.conns[name]; c != nil {
		b.mu.Unlock()
		return c, nil
	}
	s, ok := b.servers[name]
	b.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no MCP server named %s in %s's configuration", name, b.client)
	}
	var c *mcpConn
	var err error
	if s.URL != "" && s.Command == "" {
		c = newHTTPConn(name, s.URL, b.client)
	} else {
		c, err = newStdioConn(s.Command, s.Args, b.wd, b.env)
		if err != nil {
			return nil, err
		}
	}
	if _, err := c.request("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "tap-runtime", "version": "0"}}); err != nil {
		c.close()
		return nil, err
	}
	c.notify("notifications/initialized")
	b.mu.Lock()
	b.conns[name] = c
	b.mu.Unlock()
	return c, nil
}

// mcpConn is one MCP client connection, over stdio or streamable HTTP.
type mcpConn struct {
	request func(method string, params any) (map[string]any, error)
	notify  func(method string)
	close   func()
}

func newStdioConn(command string, args []string, dir string, env []string) (*mcpConn, error) {
	cmd := exec.Command(command, args...)
	cmd.Dir = dir
	cmd.Env = env
	ownGroup(cmd)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", command, err)
	}
	var mu sync.Mutex
	pending := map[int]chan map[string]any{}
	n := 0
	go func() {
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) != nil || m["method"] != nil {
				continue
			}
			id, ok := m["id"].(float64)
			if !ok {
				continue
			}
			mu.Lock()
			ch := pending[int(id)]
			delete(pending, int(id))
			mu.Unlock()
			if ch != nil {
				ch <- m
			}
		}
		mu.Lock()
		for id, ch := range pending {
			close(ch)
			delete(pending, id)
		}
		mu.Unlock()
	}()
	write := func(v any) error {
		b, _ := json.Marshal(v)
		mu.Lock()
		defer mu.Unlock()
		_, err := in.Write(append(b, '\n'))
		return err
	}
	return &mcpConn{
		request: func(method string, params any) (map[string]any, error) {
			ch := make(chan map[string]any, 1)
			mu.Lock()
			n++
			id := n
			pending[id] = ch
			mu.Unlock()
			if err := write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
				return nil, err
			}
			select {
			case m, ok := <-ch:
				if !ok {
					return nil, fmt.Errorf("%s: the server exited", method)
				}
				return rpcResult(method, m)
			case <-time.After(120 * time.Second):
				mu.Lock()
				delete(pending, id)
				mu.Unlock()
				return nil, fmt.Errorf("%s: no answer", method)
			}
		},
		notify: func(method string) { write(map[string]any{"jsonrpc": "2.0", "method": method}) },
		close: func() {
			in.Close()
			killTree(cmd)
			cmd.Wait()
		},
	}, nil
}

func newHTTPConn(name, url, client string) *mcpConn {
	hc := &http.Client{Timeout: 120 * time.Second}
	var mu sync.Mutex
	var session string
	n := 0
	post := func(body map[string]any) (map[string]any, error) {
		b, _ := json.Marshal(body)
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		mu.Lock()
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		mu.Unlock()
		resp, err := hc.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
			mu.Lock()
			session = s
			mu.Unlock()
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("%s needs a sign-in, which TAP does not use; run the primitive from a client that lends its connections", name)
		}
		if resp.StatusCode/100 != 2 {
			return nil, fmt.Errorf("%s: HTTP %d", name, resp.StatusCode)
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		if _, isReq := body["id"]; !isReq {
			return nil, nil
		}
		if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
			for _, line := range strings.Split(string(raw), "\n") {
				if d, ok := strings.CutPrefix(strings.TrimSpace(line), "data:"); ok {
					var m map[string]any
					if json.Unmarshal([]byte(strings.TrimSpace(d)), &m) == nil && m["id"] == body["id"] {
						return m, nil
					}
				}
			}
			return nil, fmt.Errorf("%s: no answer in the event stream", name)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		return m, nil
	}
	return &mcpConn{
		request: func(method string, params any) (map[string]any, error) {
			mu.Lock()
			n++
			id := float64(n)
			mu.Unlock()
			m, err := post(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
			if err != nil {
				return nil, err
			}
			return rpcResult(method, m)
		},
		notify: func(method string) { post(map[string]any{"jsonrpc": "2.0", "method": method}) },
		close:  func() {},
	}
}

func rpcResult(method string, m map[string]any) (map[string]any, error) {
	if e, ok := m["error"]; ok && e != nil {
		return nil, fmt.Errorf("%s: %v", method, e)
	}
	r, _ := m["result"].(map[string]any)
	return r, nil
}
