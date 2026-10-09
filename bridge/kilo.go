package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"path"
	"strings"
	"sync"

	"github.com/Telara-Labs/TAP-Runtime/bind"
)

// Kilo is the HTTP client for an OpenCode-family session's routes
// (configuration, MCP status, health, call-tool), reached through the TAP
// relay plugin in the person's own OpenCode or Kilo session
// (sessionrelay.go). The relay runs each tool on its own connection to the
// server, so the runner's effect gate is the only approval. A tool the
// person's configuration switches off (tools: {name: false}) or denies
// (permission: {name: "deny"}) is denied.
//
// On its own Kilo lists no MCP server's tools: the runner gives it the tools
// that may fill the primitive's declared capabilities, and those of servers
// the session reports connected are the inventory. SessionRelay lists them
// from the relay instead.
type Kilo struct {
	cmd     *exec.Cmd
	base    string
	pass    string
	dir     string
	version string
	client  *http.Client

	mu     sync.Mutex
	pins   []bind.Tool
	config map[string]json.RawMessage
}

// PinnedOnly is a bridge whose client cannot list its tools: the runner
// gives it the tools a primitive pins or that it resolved for the
// primitive's capabilities, and only those can bind.
type PinnedOnly interface {
	Bridge
	UsePins(tools []bind.Tool)
}

func (k *Kilo) request(method, p string, body any) ([]byte, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	u := k.base + p
	if k.dir != "" {
		sep := "?"
		if strings.Contains(p, "?") {
			sep = "&"
		}
		u += sep + "directory=" + url.QueryEscape(k.dir)
	}
	req, err := http.NewRequest(method, u, r)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth("kilo", k.pass)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("kilo %s %s: %s: %s", method, p, resp.Status, strings.TrimSpace(string(b)))
	}
	return b, nil
}

func (k *Kilo) get(p string) ([]byte, error) { return k.request(http.MethodGet, p, nil) }

func (k *Kilo) Client() (string, string) { return "kilo", k.version }
func (k *Kilo) HasSchemas() bool         { return false }

// UsePins gives the bridge the tools that may fill the primitive's
// capabilities, with whatever the runner knows of their effects.
func (k *Kilo) UsePins(tools []bind.Tool) {
	k.mu.Lock()
	k.pins = append([]bind.Tool(nil), tools...)
	k.mu.Unlock()
}

// Inventory is the given tools whose server Kilo reports connected.
func (k *Kilo) Inventory() ([]bind.Tool, error) {
	b, err := k.get("/mcp")
	if err != nil {
		return nil, err
	}
	var status map[string]struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(b, &status); err != nil {
		return nil, fmt.Errorf("kilo /mcp: %w", err)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []bind.Tool
	for _, t := range k.pins {
		if status[t.Server].Status == "connected" {
			if t.Annotated == "" {
				t.Annotated = bind.Unknown
			}
			out = append(out, t)
		}
	}
	return out, nil
}

// Denied reports a tool the person's Kilo configuration switches off
// (tools: {"<server>_<tool>": false}) or denies (permission:
// {"<server>_<tool>": "deny"}). Keys may use * as a wildcard; the most
// specific matching key decides.
func (k *Kilo) Denied(t bind.Tool) (bool, error) {
	k.mu.Lock()
	cfg := k.config
	k.mu.Unlock()
	if cfg == nil {
		b, err := k.get("/config")
		if err != nil {
			return false, err
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			return false, fmt.Errorf("kilo /config: %w", err)
		}
		k.mu.Lock()
		k.config = cfg
		k.mu.Unlock()
	}
	name := t.Server + "_" + t.Name
	var tools map[string]bool
	_ = json.Unmarshal(cfg["tools"], &tools)
	if on, ok := mostSpecific(tools, name); ok && !on {
		return true, nil
	}
	var perms map[string]json.RawMessage
	_ = json.Unmarshal(cfg["permission"], &perms)
	plain := map[string]string{}
	for key, v := range perms {
		var s string
		if json.Unmarshal(v, &s) == nil {
			plain[key] = s
		}
	}
	if p, ok := mostSpecific(plain, name); ok && p == "deny" {
		return true, nil
	}
	return false, nil
}

// mostSpecific is the value of the longest key matching name, where * in a
// key matches any run of characters.
func mostSpecific[V any](m map[string]V, name string) (V, bool) {
	var best V
	bestLen, found := -1, false
	for key, v := range m {
		if ok, _ := path.Match(key, name); ok && len(key) > bestLen {
			best, bestLen, found = v, len(key), true
		}
	}
	return best, found
}

// Call runs one MCP tool through Kilo. A result Kilo marks isError is an
// error, so a failure is never read back as output.
func (k *Kilo) Call(t bind.Tool, args map[string]any) (string, error) {
	if args == nil {
		args = map[string]any{}
	}
	b, err := k.request(http.MethodPost, "/experimental/mcp/call-tool", map[string]any{"server": t.Server, "name": t.Name, "arguments": args})
	if err != nil {
		return "", err
	}
	var r map[string]any
	if err := json.Unmarshal(b, &r); err != nil {
		return "", fmt.Errorf("kilo call-tool: %w", err)
	}
	return callResult(t, r)
}

// Close stops the server the bridge started, and everything it started:
// kilo is a launcher, and the server is its child.
func (k *Kilo) Close() {
	if k.cmd != nil && k.cmd.Process != nil {
		killTree(k.cmd)
		k.cmd.Wait()
		k.cmd = nil
	}
}
