package bridge

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/bind"
)

// Claude reaches Claude Code through the control channel of its stream-json
// input. Control requests start no model turn. The channel is undocumented.
type Claude struct {
	cmd     *exec.Cmd
	in      io.WriteCloser
	version string

	mu      sync.Mutex
	n       int
	pending map[string]chan map[string]any
	gone    bool
	deny    []string
	ask     []string
	denied  bool // deny has been read
}

// NewClaude starts a second copy of Claude Code. extra is passed
// to it and exists for tests.
func NewClaude(extra ...string) (*Claude, error) {
	executable, err := ClaudeExecutable()
	if err != nil {
		return nil, err
	}
	v, err := exec.Command(executable, "--version").Output()
	if err != nil {
		return nil, fmt.Errorf("claude is not on this machine: %w", err)
	}
	args := append([]string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}, extra...)
	cmd := exec.Command(executable, args...)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting claude: %w", err)
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	c := &Claude{cmd: cmd, in: in, version: strings.Fields(string(v))[0], pending: map[string]chan map[string]any{}}
	go c.read(sc)
	if _, err := c.request("initialize", map[string]any{"hooks": map[string]any{}}); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// ClaudeExecutable returns the Claude Code executable TAP should use. It
// prefers the native per-user install and falls back to PATH, matching the
// bridge's control-channel launcher.
func ClaudeExecutable() (string, error) {
	home, _ := os.UserHomeDir()
	return resolveClaudeExecutable(home, runtime.GOOS, exec.LookPath)
}

// resolveClaudeExecutable prefers Claude Code's native per-user install and
// then falls back to the executable discoverable on PATH. The native path is
// used for both the version probe and the control-channel process, so a stale
// PATH shim cannot make TAP interrogate a different installation.
func resolveClaudeExecutable(home, goos string, lookPath func(string) (string, error)) (string, error) {
	name := "claude"
	if goos == "windows" {
		name += ".exe"
	}
	if home != "" {
		candidate := filepath.Join(home, ".local", "bin", name)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && (goos == "windows" || info.Mode().Perm()&0111 != 0) {
			return candidate, nil
		}
	}
	pathName := "claude"
	if goos == "windows" {
		pathName += ".exe"
	}
	path, err := lookPath(pathName)
	if err != nil {
		return "", fmt.Errorf("Claude Code was not found at the native install path or on PATH: %w", err)
	}
	return path, nil
}

// read hands each response to whoever asked for it. Several requests may be
// in flight; the request id on a response says which it answers.
func (c *Claude) read(sc *bufio.Scanner) {
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil || m["type"] != "control_response" {
			continue
		}
		r, _ := m["response"].(map[string]any)
		id, _ := r["request_id"].(string)
		c.mu.Lock()
		ch := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if ch != nil {
			ch <- r
		}
	}
	c.mu.Lock()
	c.gone = true
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
}

func (c *Claude) request(subtype string, fields map[string]any) (map[string]any, error) {
	req := map[string]any{"subtype": subtype}
	for k, v := range fields {
		req[k] = v
	}
	ch := make(chan map[string]any, 1)
	c.mu.Lock()
	if c.gone {
		c.mu.Unlock()
		return nil, fmt.Errorf("%s: claude has exited", subtype)
	}
	c.n++
	rid := fmt.Sprintf("tap-%d", c.n)
	c.pending[rid] = ch
	b, _ := json.Marshal(map[string]any{"type": "control_request", "request_id": rid, "request": req})
	_, err := c.in.Write(append(b, '\n'))
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("%s: claude exited before it answered", subtype)
		}
		if r["subtype"] == "error" {
			return nil, fmt.Errorf("%s: %v", subtype, r["error"])
		}
		res, _ := r["response"].(map[string]any)
		return res, nil
	case <-time.After(120 * time.Second):
		c.mu.Lock()
		delete(c.pending, rid)
		c.mu.Unlock()
		return nil, fmt.Errorf("%s: claude did not answer", subtype)
	}
}

func (c *Claude) Client() (string, string) { return "claude-code", c.version }
func (c *Claude) HasSchemas() bool         { return false }

func (c *Claude) Inventory() ([]bind.Tool, error) {
	var servers []any
	// A server reports "pending" for a moment after start and lists no tools
	// until it connects.
	for attempt := 0; attempt < 15; attempt++ {
		r, err := c.request("mcp_status", nil)
		if err != nil {
			return nil, err
		}
		servers, _ = r["mcpServers"].([]any)
		pending := false
		for _, s := range servers {
			if sm, _ := s.(map[string]any); sm["status"] == "pending" {
				pending = true
			}
		}
		if !pending {
			break
		}
		time.Sleep(time.Second)
	}
	var out []bind.Tool
	for _, s := range servers {
		sm, _ := s.(map[string]any)
		if sm["status"] != "connected" {
			continue
		}
		name, _ := sm["name"].(string)
		tools, _ := sm["tools"].([]any)
		for _, t := range tools {
			tm, _ := t.(map[string]any)
			tn, _ := tm["name"].(string)
			ann, _ := tm["annotations"].(map[string]any)
			out = append(out, bind.Tool{Server: name, Name: tn, Annotated: claudeEffect(ann)})
		}
	}
	return out, nil
}

// AwaitLateServers waits for the claude.ai connectors. Claude Code fetches
// them from the account after it starts answering, so an early mcp_status can
// list only local servers. It returns once a claude.ai server is listed and
// none is pending, or at d; true means the inventory is worth reading again.
func (c *Claude) AwaitLateServers(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		r, err := c.request("mcp_status", nil)
		if err != nil {
			return false
		}
		servers, _ := r["mcpServers"].([]any)
		pending, connectors := false, false
		for _, s := range servers {
			sm, _ := s.(map[string]any)
			if sm["status"] == "pending" {
				pending = true
			}
			if name, _ := sm["name"].(string); strings.HasPrefix(name, "claude.ai ") {
				connectors = true
			}
		}
		if connectors && !pending {
			return true
		}
		if time.Now().After(deadline) {
			return connectors
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// loadRules reads the user's permission rules once.
func (c *Claude) loadRules() error {
	c.mu.Lock()
	read := c.denied
	c.mu.Unlock()
	if read {
		return nil
	}
	r, err := c.request("list_permission_rules", nil)
	if err != nil {
		return err
	}
	state, _ := r["state"].(map[string]any)
	rules, _ := state["rules"].([]any)
	deny, ask := splitRules(rules)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deny, c.ask = append(c.deny, deny...), append(c.ask, ask...)
	c.denied = true
	return nil
}

// splitRules separates Claude Code's permission rules into the ones that
// forbid a tool and the ones that make it ask first.
func splitRules(rules []any) (deny, ask []string) {
	for _, x := range rules {
		rm, _ := x.(map[string]any)
		s, ok := rm["rule"].(string)
		if !ok {
			continue
		}
		switch rm["behavior"] {
		case "deny":
			deny = append(deny, s)
		case "ask":
			ask = append(ask, s)
		}
	}
	return deny, ask
}

func (c *Claude) Denied(t bind.Tool) (bool, error) {
	if err := c.loadRules(); err != nil {
		return false, err
	}
	q := claudeName(t.Server, t.Name)
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, rule := range c.deny {
		if ruleCovers(rule, q) {
			return true, nil
		}
	}
	return false, nil
}

// Asks reports whether the user has an "ask" rule for the tool: Claude Code
// would stop and ask them before using it.
func (c *Claude) Asks(t bind.Tool) (bool, error) {
	if err := c.loadRules(); err != nil {
		return false, err
	}
	q := claudeName(t.Server, t.Name)
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, rule := range c.ask {
		if ruleCovers(rule, q) {
			return true, nil
		}
	}
	return false, nil
}

func (c *Claude) Call(t bind.Tool, args map[string]any) (string, error) {
	if args == nil {
		args = map[string]any{}
	}
	r, err := c.request("mcp_call", map[string]any{"tool": claudeName(t.Server, t.Name), "arguments": args})
	if err != nil {
		return "", err
	}
	return resultText(r), nil
}

func (c *Claude) Close() {
	c.in.Close()
	_ = c.cmd.Process.Kill()
	_ = c.cmd.Wait()
}

// resultText reduces the shapes clients return to one string. Structured
// content is preferred: some connectors put the data there and a bare
// "Action completed." in the text.
func resultText(r map[string]any) string {
	if sc, ok := r["structuredContent"]; ok && sc != nil {
		b, _ := json.Marshal(sc)
		return string(b)
	}
	switch cv := r["content"].(type) {
	case string:
		return cv
	case []any:
		var parts []string
		for _, b := range cv {
			if bm, _ := b.(map[string]any); bm["type"] == "text" {
				if s, ok := bm["text"].(string); ok {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	b, _ := json.Marshal(r)
	return string(b)
}
