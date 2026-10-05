package bridge

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/bind"
)

// Codex reaches Codex through its app-server protocol, the interface its own
// desktop app uses. Codex marks it experimental.
type Codex struct {
	cmd     *exec.Cmd
	in      io.WriteCloser
	thread  string
	version string

	mu      sync.Mutex
	n       int
	pending map[int]chan map[string]any
	gone    bool
	ruleSet map[string]codexServerRules
}

func NewCodex() (*Codex, error) {
	cmd := exec.Command("codex", "app-server")
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("codex is not on this machine: %w", err)
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	c := &Codex{cmd: cmd, in: in, pending: map[int]chan map[string]any{}}
	go c.read(sc)
	r, err := c.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "tap-runtime", "version": "0"}})
	if err != nil {
		c.Close()
		return nil, err
	}
	// userAgent reads "<client name>/<codex version> (...)".
	if ua, _ := r["userAgent"].(string); ua != "" {
		if i := strings.Index(ua, "/"); i >= 0 {
			c.version = strings.Fields(ua[i+1:])[0]
		}
	}
	r, err = c.call("thread/start", codexBridgeThreadStartParams())
	if err != nil {
		c.Close()
		return nil, err
	}
	if th, ok := r["thread"].(map[string]any); ok {
		c.thread, _ = th["id"].(string)
	}
	if v, ok := r["threadId"].(string); ok && c.thread == "" {
		c.thread = v
	}
	if c.thread == "" {
		c.Close()
		return nil, fmt.Errorf("codex thread/start returned no thread id")
	}
	return c, nil
}

// codexBridgeThreadStartParams creates the private app-server thread used to
// dispatch a primitive's calls. TAP has already applied the primitive's
// effect gate and obtained any required approval before Bridge.Call. The
// bridge thread has no person to answer Codex's separate MCP approval prompt,
// so keep that second gate off; calls still cannot reach Bridge.Call unless
// TAP's gate allowed them.
func codexBridgeThreadStartParams() map[string]any {
	return map[string]any{
		"ephemeral":      true,
		"cwd":            os.TempDir(),
		"approvalPolicy": "never",
	}
}

// read hands each response to whoever asked for it.
func (c *Codex) read(sc *bufio.Scanner) {
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		idf, ok := m["id"].(float64)
		if !ok || m["method"] != nil {
			continue // a notification, or a request of its own
		}
		c.mu.Lock()
		ch := c.pending[int(idf)]
		delete(c.pending, int(idf))
		c.mu.Unlock()
		if ch != nil {
			ch <- m
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

func (c *Codex) call(method string, params any) (map[string]any, error) {
	ch := make(chan map[string]any, 1)
	c.mu.Lock()
	if c.gone {
		c.mu.Unlock()
		return nil, fmt.Errorf("%s: codex has exited", method)
	}
	c.n++
	id := c.n
	c.pending[id] = ch
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	_, err := c.in.Write(append(b, '\n'))
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case m, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("%s: codex exited before it answered", method)
		}
		if e, ok := m["error"]; ok && e != nil {
			return nil, fmt.Errorf("%s: %v", method, e)
		}
		res, _ := m["result"].(map[string]any)
		return res, nil
	case <-time.After(120 * time.Second):
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("%s: codex did not answer", method)
	}
}

func (c *Codex) Client() (string, string) { return "codex", c.version }
func (c *Codex) HasSchemas() bool         { return true }

func (c *Codex) Inventory() ([]bind.Tool, error) {
	var out []bind.Tool
	cursor := ""
	for {
		p := map[string]any{"detail": "full", "limit": 100}
		if cursor != "" {
			p["cursor"] = cursor
		}
		r, err := c.call("mcpServerStatus/list", p)
		if err != nil {
			return nil, err
		}
		data, _ := r["data"].([]any)
		for _, s := range data {
			sm, _ := s.(map[string]any)
			name, _ := sm["name"].(string)
			tools, _ := sm["tools"].(map[string]any)
			for tn, tv := range tools {
				tm, _ := tv.(map[string]any)
				ann, _ := tm["annotations"].(map[string]any)
				schema, _ := tm["inputSchema"].(map[string]any)
				out = append(out, bind.Tool{Server: name, Name: tn, Annotated: codexEffect(ann), Schema: schema})
			}
		}
		cursor, _ = r["nextCursor"].(string)
		if cursor == "" {
			return out, nil
		}
	}
}

// rules reads the user's MCP tool rules once, through app-server's
// config/read, which answers with Codex's own merged configuration.
func (c *Codex) rules() (map[string]codexServerRules, error) {
	c.mu.Lock()
	cached := c.ruleSet
	c.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	params := map[string]any{"includeLayers": false}
	if wd, err := os.Getwd(); err == nil {
		params["cwd"] = wd
	}
	r, err := c.call("config/read", params)
	if err != nil {
		return nil, err
	}
	cfg, _ := r["config"].(map[string]any)
	rs := codexRulesFrom(cfg)
	c.mu.Lock()
	c.ruleSet = rs
	c.mu.Unlock()
	return rs, nil
}

// Denied reports whether the user's Codex configuration switches the tool off:
// its server is disabled, it is not in the server's enabled_tools, or it is in
// disabled_tools.
func (c *Codex) Denied(t bind.Tool) (bool, error) {
	rs, err := c.rules()
	if err != nil {
		return false, err
	}
	r, ok := rs[t.Server]
	return ok && r.denies(t.Name), nil
}

// Asks reports whether the user set the tool's approval_mode to "prompt".
func (c *Codex) Asks(t bind.Tool) (bool, error) {
	rs, err := c.rules()
	if err != nil {
		return false, err
	}
	return rs[t.Server].alwaysPrompts[t.Name], nil
}

func (c *Codex) Call(t bind.Tool, args map[string]any) (string, error) {
	if args == nil {
		args = map[string]any{}
	}
	r, err := c.call("mcpServer/tool/call", map[string]any{
		"server": t.Server, "tool": t.Name, "arguments": args, "threadId": c.thread,
	})
	if err != nil {
		return "", err
	}
	return resultText(r), nil
}

func (c *Codex) Close() {
	c.in.Close()
	_ = c.cmd.Process.Kill()
	_ = c.cmd.Wait()
}
