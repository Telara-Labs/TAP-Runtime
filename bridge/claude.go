package bridge

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"gitlab.com/telara-labs/tap-runtime/bind"
)

// Claude reaches Claude Code through the control channel of its stream-json
// input. Control requests start no model turn. The channel is undocumented.
type Claude struct {
	cmd     *exec.Cmd
	in      io.WriteCloser
	sc      *bufio.Scanner
	n       int
	version string
	deny    []string
	denied  bool // deny has been read
}

// NewClaude starts a second copy of Claude Code (ruling 11). extra is passed
// to it and exists for tests.
func NewClaude(extra ...string) (*Claude, error) {
	v, err := exec.Command("claude", "--version").Output()
	if err != nil {
		return nil, fmt.Errorf("claude is not on this machine: %w", err)
	}
	args := append([]string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}, extra...)
	cmd := exec.Command("claude", args...)
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
	c := &Claude{cmd: cmd, in: in, sc: sc, version: strings.Fields(string(v))[0]}
	if _, err := c.request("initialize", map[string]any{"hooks": map[string]any{}}); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *Claude) request(subtype string, fields map[string]any) (map[string]any, error) {
	c.n++
	rid := fmt.Sprintf("tap-%d", c.n)
	req := map[string]any{"subtype": subtype}
	for k, v := range fields {
		req[k] = v
	}
	b, _ := json.Marshal(map[string]any{"type": "control_request", "request_id": rid, "request": req})
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) && c.sc.Scan() {
		var m map[string]any
		if json.Unmarshal(c.sc.Bytes(), &m) != nil || m["type"] != "control_response" {
			continue
		}
		r, _ := m["response"].(map[string]any)
		if r["request_id"] != rid {
			continue
		}
		if r["subtype"] == "error" {
			return nil, fmt.Errorf("%s: %v", subtype, r["error"])
		}
		res, _ := r["response"].(map[string]any)
		return res, nil
	}
	return nil, fmt.Errorf("%s: claude did not answer", subtype)
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

func (c *Claude) Denied(t bind.Tool) (bool, error) {
	if !c.denied {
		r, err := c.request("list_permission_rules", nil)
		if err != nil {
			return false, err
		}
		state, _ := r["state"].(map[string]any)
		rules, _ := state["rules"].([]any)
		for _, x := range rules {
			rm, _ := x.(map[string]any)
			if rm["behavior"] == "deny" {
				if s, ok := rm["rule"].(string); ok {
					c.deny = append(c.deny, s)
				}
			}
		}
		c.denied = true
	}
	q := claudeName(t.Server, t.Name)
	for _, rule := range c.deny {
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
