package bridge

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/bind"
)

// Copilot reaches GitHub Copilot CLI through its headless JSON-RPC server
// (`copilot --headless --stdio`, the protocol GitHub's Copilot SDK speaks).
// A session started there lists the MCP tools the person connected
// (session.tools.getCurrentMetadata) and runs one through Copilot's own
// invocation pipeline (session.tools.execute), with no model turn, using
// Copilot's own connection to the server.
//
// Copilot applies the person's own permission rules to each call: a tool
// they denied is refused by Copilot. When Copilot would ask, it asks this
// client (permission.requested); the bridge approves only the call the
// runner is making at that moment, which the runner's effect gate has
// already let through, and rejects anything else.
type Copilot struct {
	cmd     *exec.Cmd
	in      io.WriteCloser
	session string
	version string

	mu      sync.Mutex
	n       int
	pending map[int]chan map[string]any
	gone    bool
	names   map[string]string // server/tool -> Copilot's own tool name
	current *copilotCall      // the call in flight, which Copilot may ask about
	callMu  sync.Mutex        // one call at a time, so a permission request has one subject
}

type copilotCall struct {
	server, tool, name string
	args               map[string]any
}

func NewCopilot() (*Copilot, error) { return NewCopilotIn(Proc{}) }

// NewCopilotIn starts Copilot CLI's headless server in a session's directory
// and environment, so it reads the same configuration as the person's Copilot.
func NewCopilotIn(p Proc) (*Copilot, error) {
	bin := "copilot"
	if path, err := p.LookPath(bin); err == nil {
		bin = path
	}
	cmd := exec.Command(bin, "--headless", "--no-auto-update", "--stdio", "--log-level", "error")
	p.apply(cmd)
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
		return nil, fmt.Errorf("copilot is not on this machine: %w", err)
	}
	c := &Copilot{cmd: cmd, in: in, pending: map[int]chan map[string]any{}}
	go c.read(bufio.NewReaderSize(out, 64*1024))
	if v, err := exec.Command(bin, "--version").Output(); err == nil {
		c.version = copilotVersion(string(v))
	}
	if _, err := c.call("ping", map[string]any{}); err != nil {
		c.Close()
		return nil, err
	}
	var id [16]byte
	rand.Read(id[:])
	r, err := c.call("session.create", map[string]any{
		"sessionId":         hex.EncodeToString(id[:]),
		"clientName":        "tap-runtime",
		"requestPermission": true,
		"workingDirectory":  p.Wd(),
	})
	if err != nil {
		c.Close()
		return nil, err
	}
	c.session, _ = r["sessionId"].(string)
	if c.session == "" {
		c.Close()
		return nil, fmt.Errorf("copilot session.create returned no session id")
	}
	// Copilot builds a session's tool list when a turn starts; the runner
	// needs it with no turn.
	if _, err := c.call("session.tools.initializeAndValidate", map[string]any{"sessionId": c.session}); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// copilotVersion reads "GitHub Copilot CLI 1.0.94." as 1.0.94.
func copilotVersion(s string) string {
	for _, f := range strings.Fields(s) {
		f = strings.TrimSuffix(f, ".")
		if len(f) > 0 && f[0] >= '0' && f[0] <= '9' && strings.Contains(f, ".") {
			return f
		}
	}
	return ""
}

// read takes each Content-Length framed message: a response goes to whoever
// asked, a permission request is answered.
func (c *Copilot) read(r *bufio.Reader) {
	for {
		m, err := readFramed(r)
		if err != nil {
			break
		}
		if m["method"] == "session.event" {
			c.event(m)
			continue
		}
		idf, ok := m["id"].(float64)
		if !ok || m["method"] != nil {
			continue
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

// readFramed reads one message framed by a Content-Length header.
func readFramed(r *bufio.Reader) (map[string]any, error) {
	n := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if n < 0 {
				continue
			}
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "content-length") {
			n, _ = strconv.Atoi(strings.TrimSpace(v))
		}
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]any{}, nil
	}
	return m, nil
}

func (c *Copilot) write(v any) error {
	b, _ := json.Marshal(v)
	_, err := c.in.Write(append([]byte(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(b))), b...))
	return err
}

// event answers Copilot's permission request: yes for the call the runner is
// making now, no for anything else.
func (c *Copilot) event(m map[string]any) {
	p, _ := m["params"].(map[string]any)
	ev, _ := p["event"].(map[string]any)
	if ev["type"] != "permission.requested" {
		return
	}
	data, _ := ev["data"].(map[string]any)
	if resolved, _ := data["resolvedByHook"].(bool); resolved {
		return
	}
	req, _ := data["permissionRequest"].(map[string]any)
	c.mu.Lock()
	cur := c.current
	c.mu.Unlock()
	result := map[string]any{"kind": "reject", "feedback": "the runner is not making this call"}
	if cur != nil && req["kind"] == "mcp" && req["serverName"] == cur.server && req["toolName"] == cur.name && sameArgs(req["args"], cur.args) {
		result = map[string]any{"kind": "approve-once"}
	}
	c.mu.Lock()
	c.n++
	id := c.n
	c.mu.Unlock()
	c.mu.Lock()
	c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": "session.permissions.handlePendingPermissionRequest",
		"params": map[string]any{"sessionId": c.session, "requestId": data["requestId"], "result": result}})
	c.mu.Unlock()
}

// sameArgs compares the arguments Copilot asks about with the ones the
// runner sent, as JSON.
func sameArgs(got any, want map[string]any) bool {
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(want)
	var x, y any
	json.Unmarshal(a, &x)
	json.Unmarshal(b, &y)
	if len(want) == 0 && (got == nil || len(a) <= 2) {
		return true
	}
	return reflect.DeepEqual(x, y)
}

func (c *Copilot) call(method string, params any) (map[string]any, error) {
	ch := make(chan map[string]any, 1)
	c.mu.Lock()
	if c.gone {
		c.mu.Unlock()
		return nil, fmt.Errorf("%s: copilot has exited", method)
	}
	c.n++
	id := c.n
	c.pending[id] = ch
	err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case m, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("%s: copilot exited before it answered", method)
		}
		if e, ok := m["error"]; ok && e != nil {
			return nil, fmt.Errorf("%s: %v", method, e)
		}
		res, _ := m["result"].(map[string]any)
		return res, nil
	case <-time.After(180 * time.Second):
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("%s: copilot did not answer", method)
	}
}

func (c *Copilot) Client() (string, string) { return "copilot", c.version }
func (c *Copilot) HasSchemas() bool         { return true }

// Inventory lists the tools of each MCP server the person connected, by the
// server's own tool names. Copilot's built-in tools (shell, file edits) are
// not connections and are left out, as is the TAP server itself.
func (c *Copilot) Inventory() ([]bind.Tool, error) {
	r, err := c.call("session.tools.getCurrentMetadata", map[string]any{"sessionId": c.session})
	if err != nil {
		return nil, err
	}
	list, _ := r["tools"].([]any)
	names := map[string]string{}
	var out []bind.Tool
	for _, x := range list {
		t, _ := x.(map[string]any)
		server, _ := t["mcpServerName"].(string)
		tool, _ := t["mcpToolName"].(string)
		name, _ := t["name"].(string)
		if server == "" || tool == "" || name == "" || server == "tap" {
			continue
		}
		schema, _ := t["input_schema"].(map[string]any)
		names[server+"/"+tool] = name
		out = append(out, bind.Tool{Server: server, Name: tool, Annotated: bind.Unknown, Schema: schema})
	}
	c.mu.Lock()
	c.names = names
	c.mu.Unlock()
	return out, nil
}

// Denied is answered by Copilot itself when the call is made: a tool the
// person denied comes back refused.
func (c *Copilot) Denied(bind.Tool) (bool, error) { return false, nil }

// Call runs one tool through the session. A result Copilot reports as a
// failure or a refusal is returned as an error.
func (c *Copilot) Call(t bind.Tool, args map[string]any) (string, error) {
	if args == nil {
		args = map[string]any{}
	}
	c.mu.Lock()
	known := c.names != nil
	c.mu.Unlock()
	if !known {
		if _, err := c.Inventory(); err != nil {
			return "", err
		}
	}
	c.mu.Lock()
	name := c.names[t.Server+"/"+t.Name]
	c.mu.Unlock()
	if name == "" {
		name = t.Server + "-" + t.Name
	}
	c.callMu.Lock()
	defer c.callMu.Unlock()
	c.mu.Lock()
	c.current = &copilotCall{server: t.Server, tool: t.Name, name: name, args: args}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.current = nil
		c.mu.Unlock()
	}()
	r, err := c.call("session.tools.execute", map[string]any{"sessionId": c.session, "name": name, "arguments": args})
	if err != nil {
		return "", err
	}
	text, _ := r["textResultForLlm"].(string)
	switch kind, _ := r["resultType"].(string); kind {
	case "success":
		return text, nil
	default:
		msg, _ := r["error"].(string)
		if msg == "" {
			msg = text
		}
		return "", fmt.Errorf("%s/%s: %s (%s)", t.Server, t.Name, msg, kind)
	}
}

// Close lets Copilot exit on its own, then stops everything it started and
// waits until it has: Copilot writes session state as it exits, and its MCP
// servers outlive it unless stopped.
func (c *Copilot) Close() {
	c.in.Close()
	exited := make(chan struct{})
	go func() { c.cmd.Wait(); close(exited) }()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
	}
	killTree(c.cmd)
	<-exited
	awaitGroupGone(c.cmd, 2*time.Second)
}
