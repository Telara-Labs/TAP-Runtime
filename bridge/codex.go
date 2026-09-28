package bridge

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"gitlab.com/telara-labs/tap-runtime/bind"
)

// Codex reaches Codex through its app-server protocol, the interface its own
// desktop app uses. Codex marks it experimental.
type Codex struct {
	cmd     *exec.Cmd
	in      io.WriteCloser
	sc      *bufio.Scanner
	n       int
	thread  string
	version string
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
	c := &Codex{cmd: cmd, in: in, sc: sc}
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
	r, err = c.call("thread/start", map[string]any{"ephemeral": true, "cwd": os.TempDir()})
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

func (c *Codex) call(method string, params any) (map[string]any, error) {
	c.n++
	id := c.n
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) && c.sc.Scan() {
		var m map[string]any
		if json.Unmarshal(c.sc.Bytes(), &m) != nil || m["id"] != float64(id) {
			continue
		}
		if e, ok := m["error"]; ok && e != nil {
			return nil, fmt.Errorf("%s: %v", method, e)
		}
		res, _ := m["result"].(map[string]any)
		return res, nil
	}
	return nil, fmt.Errorf("%s: codex did not answer", method)
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
				out = append(out, bind.Tool{Server: name, Name: tn, Annotated: codexEffect(ann)})
			}
		}
		cursor, _ = r["nextCursor"].(string)
		if cursor == "" {
			return out, nil
		}
	}
}

// Denied always reports false. Where Codex keeps a user's tool permission
// rules, and whether app-server exposes them, has not been established.
func (c *Codex) Denied(bind.Tool) (bool, error) { return false, nil }

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
