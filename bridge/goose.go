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

// Goose reaches Goose through ACP (`goose acp`), with Goose's own custom
// requests: _goose/unstable/tools/list lists a session's extension tools and
// _goose/unstable/tools/call runs one through Goose's extension manager, with
// no model turn. Goose marks both unstable.
//
// Goose runs an app tool call only in auto mode, so the bridge's session is
// put in auto mode and Goose shows no approval of its own: the runner's
// effect gate is the only approval. A tool the user set to
// never_allow is denied.
type Goose struct {
	cmd     *exec.Cmd
	in      io.WriteCloser
	session string
	version string

	mu      sync.Mutex
	n       int
	pending map[int]chan map[string]any
	gone    bool
	perm    map[string]string // qualified tool name -> Goose permission
}

func NewGoose() (*Goose, error) { return newGoose("goose", nil) }

// newGoose starts bin acp; env, when set, replaces the process environment.
func newGoose(bin string, env []string) (*Goose, error) {
	cmd := exec.Command(bin, "acp")
	cmd.Env = env
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("goose is not on this machine: %w", err)
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	g := &Goose{cmd: cmd, in: in, pending: map[int]chan map[string]any{}}
	go g.read(sc)
	r, err := g.call("initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{},
		"clientInfo":         map[string]string{"name": "tap-runtime", "version": "0"},
	})
	if err != nil {
		g.Close()
		return nil, err
	}
	if info, ok := r["agentInfo"].(map[string]any); ok {
		g.version, _ = info["version"].(string)
	}
	r, err = g.call("session/new", map[string]any{"cwd": os.TempDir(), "mcpServers": []any{}})
	if err != nil {
		g.Close()
		return nil, err
	}
	g.session, _ = r["sessionId"].(string)
	if g.session == "" {
		g.Close()
		return nil, fmt.Errorf("goose session/new returned no session id")
	}
	if _, err := g.call("session/set_mode", map[string]any{"sessionId": g.session, "modeId": "auto"}); err != nil {
		g.Close()
		return nil, err
	}
	return g, nil
}

// read hands each response to whoever asked for it.
func (g *Goose) read(sc *bufio.Scanner) {
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		idf, ok := m["id"].(float64)
		if !ok || m["method"] != nil {
			continue // a session/update notification, or a request of its own
		}
		g.mu.Lock()
		ch := g.pending[int(idf)]
		delete(g.pending, int(idf))
		g.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
	g.mu.Lock()
	g.gone = true
	for id, ch := range g.pending {
		close(ch)
		delete(g.pending, id)
	}
	g.mu.Unlock()
}

func (g *Goose) call(method string, params any) (map[string]any, error) {
	ch := make(chan map[string]any, 1)
	g.mu.Lock()
	if g.gone {
		g.mu.Unlock()
		return nil, fmt.Errorf("%s: goose has exited", method)
	}
	g.n++
	id := g.n
	g.pending[id] = ch
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	_, err := g.in.Write(append(b, '\n'))
	g.mu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case m, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("%s: goose exited before it answered", method)
		}
		if e, ok := m["error"]; ok && e != nil {
			return nil, fmt.Errorf("%s: %v", method, e)
		}
		res, _ := m["result"].(map[string]any)
		return res, nil
	case <-time.After(120 * time.Second):
		g.mu.Lock()
		delete(g.pending, id)
		g.mu.Unlock()
		return nil, fmt.Errorf("%s: goose did not answer", method)
	}
}

func (g *Goose) Client() (string, string) { return "goose", g.version }
func (g *Goose) HasSchemas() bool         { return true }

// gooseOwn are the extension types that are Goose's own tools (its shell,
// its extension manager, apps), not connections the user made. Goose 1.53
// lists a connected stdio server as type "mcp"; any type not listed here is
// a connection.
var gooseOwn = map[string]bool{"platform": true, "builtin": true, "frontend": true, "inline_python": true}

// Inventory lists the tools of each extension the user connected. Goose
// names an extension's tool <extension>__<tool>. Goose gives no
// annotations, so every effect is Unknown.
func (g *Goose) Inventory() ([]bind.Tool, error) {
	r, err := g.call("_goose/unstable/session/extensions/list", map[string]any{"sessionId": g.session})
	if err != nil {
		return nil, err
	}
	exts, _ := r["extensions"].([]any)
	perm := map[string]string{}
	var out []bind.Tool
	for _, e := range exts {
		em, _ := e.(map[string]any)
		key, _ := em["extensionKey"].(string)
		ext, _ := em["extension"].(map[string]any)
		if typ, _ := ext["type"].(string); key == "" || gooseOwn[typ] {
			continue
		}
		r, err := g.call("_goose/unstable/tools/list", map[string]any{"sessionId": g.session, "extensionName": key})
		if err != nil {
			return nil, err
		}
		tools, _ := r["tools"].([]any)
		for _, tv := range tools {
			tm, _ := tv.(map[string]any)
			name, _ := tm["name"].(string)
			tool, ok := strings.CutPrefix(name, key+"__")
			if !ok || tool == "" {
				continue
			}
			p, _ := tm["permission"].(string)
			perm[name] = p
			schema, _ := tm["inputSchema"].(map[string]any)
			out = append(out, bind.Tool{Server: key, Name: tool, Annotated: bind.Unknown, Schema: schema})
		}
	}
	g.mu.Lock()
	g.perm = perm
	g.mu.Unlock()
	return out, nil
}

// Denied reports a tool the user set to never_allow in Goose.
func (g *Goose) Denied(t bind.Tool) (bool, error) {
	g.mu.Lock()
	listed := g.perm != nil
	g.mu.Unlock()
	if !listed {
		if _, err := g.Inventory(); err != nil {
			return false, err
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.perm[t.Server+"__"+t.Name] == "never_allow", nil
}

// Call runs one extension tool through Goose. A result Goose marks isError
// is returned as an error, so a failure is never read back as output.
func (g *Goose) Call(t bind.Tool, args map[string]any) (string, error) {
	if args == nil {
		args = map[string]any{}
	}
	r, err := g.call("_goose/unstable/tools/call", map[string]any{
		"sessionId":     g.session,
		"extensionName": t.Server,
		"name":          t.Server + "__" + t.Name,
		"arguments":     args,
	})
	if err != nil {
		return "", err
	}
	if isErr, _ := r["isError"].(bool); isErr {
		return "", fmt.Errorf("%s/%s: %s", t.Server, t.Name, resultText(r))
	}
	return resultText(r), nil
}

// Close deletes the bridge's session, so it does not show in Goose's
// history, and stops Goose.
func (g *Goose) Close() {
	if g.session != "" {
		g.call("session/delete", map[string]any{"sessionId": g.session})
		g.session = ""
	}
	if g.in != nil {
		g.in.Close()
	}
	if g.cmd != nil && g.cmd.Process != nil {
		g.cmd.Process.Kill()
		g.cmd.Wait()
	}
}
