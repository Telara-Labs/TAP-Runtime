package bridge

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Telara-Labs/TAP-Runtime/bind"
)

// VSCode reaches Visual Studio Code, and so GitHub Copilot's tools, through
// the TAP extension (vscode/ in this repository). The extension starts this
// runner as an MCP server and gives it a private local socket; over it the
// runner lists the editor's language model tools, the MCP servers the user
// connected included, and calls them with vscode.lm.invokeTool. The editor
// makes each call with its own connection. Such a call carries no chat
// invocation token, so VS Code does not show it in the chat: a tool that asks
// for confirmation puts up a modal dialog instead, which cancellation does
// not close. The runner therefore asks the person itself, with an MCP
// elicitation that VS Code shows in the chat that called tap_run.
type VSCode struct {
	conn net.Conn
	sc   *bufio.Scanner

	mu      sync.Mutex
	n       int
	version string
	askSet  map[string]bool
	// editorNames maps a tool as the runner names it (server and the
	// server's own tool name) back to the name VS Code calls it by.
	editorNames map[[2]string]string
}

// NewVSCode connects to the extension's socket.
func NewVSCode(socket string) (*VSCode, error) {
	c, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("the TAP extension for VS Code is not reachable at %s: %w", socket, err)
	}
	v := &VSCode{conn: c, sc: bufio.NewScanner(c)}
	v.sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	r, err := v.request(map[string]any{"op": "hello"})
	if err != nil {
		c.Close()
		return nil, err
	}
	v.version, _ = r["version"].(string)
	return v, nil
}

// request sends one request and reads its answer. One at a time: the
// extension answers in order.
func (v *VSCode) request(req map[string]any) (map[string]any, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.n++
	req["id"] = v.n
	b, _ := json.Marshal(req)
	v.conn.SetDeadline(time.Now().Add(10 * time.Minute))
	if _, err := v.conn.Write(append(b, '\n')); err != nil {
		return nil, fmt.Errorf("the TAP extension went away: %w", err)
	}
	if !v.sc.Scan() {
		return nil, fmt.Errorf("the TAP extension closed the connection")
	}
	var r map[string]any
	if err := json.Unmarshal(v.sc.Bytes(), &r); err != nil {
		return nil, fmt.Errorf("the TAP extension answered something unreadable: %w", err)
	}
	if e, _ := r["error"].(string); e != "" {
		return nil, fmt.Errorf("%s", e)
	}
	return r, nil
}

func (v *VSCode) Client() (string, string) { return "vscode", v.version }
func (v *VSCode) HasSchemas() bool         { return true }

// Denied reports false: VS Code gives an extension no way to read which tools
// the user has switched off in the chat tools picker.
func (v *VSCode) Denied(bind.Tool) (bool, error) { return false, nil }

// Asks reports whether the user's chat.tools.eligibleForAutoApproval setting
// lists the tool as false, which makes VS Code ask them before every use.
func (v *VSCode) Asks(t bind.Tool) (bool, error) {
	v.mu.Lock()
	cached := v.askSet
	v.mu.Unlock()
	if cached == nil {
		r, err := v.request(map[string]any{"op": "rules"})
		if err != nil {
			return false, err
		}
		cached = map[string]bool{}
		list, _ := r["ask"].([]any)
		for _, x := range list {
			if s, ok := x.(string); ok {
				cached[s] = true
			}
		}
		v.mu.Lock()
		v.askSet = cached
		v.mu.Unlock()
	}
	t.Name = v.editorName(t)
	for key := range cached {
		if askKeyCovers(key, t) {
			return true, nil
		}
	}
	return false, nil
}

// askKeyCovers reports whether a key of chat.tools.eligibleForAutoApproval
// names the tool. VS Code keys the setting by a tool's reference name: runTask
// for a built-in tool the editor lists as run_task, and "<server>/<tool>" for
// an MCP tool, or "<server>/*" for a whole server (read from the shipped VS
// Code 1.138 source, and the built-in names seen in a live VS Code 1.140). The
// editor lists an MCP tool as mcp_<server>_<tool>. Names are compared without
// case or punctuation. This is a best match, not the editor's own lookup.
func askKeyCovers(key string, t bind.Tool) bool {
	server, tool := "", key
	if i := strings.LastIndex(key, "/"); i >= 0 {
		server, tool = key[:i], key[i+1:]
	}
	name, want := plain(t.Name), plain(tool)
	// The editor lists an MCP tool as mcp_<server>_<tool> and tags it only
	// "mcp" (seen in a live VS Code 1.140), so the server is read from the
	// name as well as from a tag that names it.
	inServer := func() bool {
		return strings.EqualFold(server, t.Server) || strings.HasPrefix(name, "mcp"+plain(server))
	}
	if tool == "*" {
		return server != "" && inServer()
	}
	if server != "" && !inServer() {
		return false
	}
	if server == "" {
		// A bare name is a built-in tool's, or an MCP tool whose reference name
		// carries no server. Asking too often is the safe way to be wrong.
		return name == want || (strings.HasPrefix(name, "mcp") && strings.HasSuffix(name, want))
	}
	return name == want || strings.HasSuffix(name, want)
}

// plain lowers a name and drops everything but letters and digits.
func plain(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
func (v *VSCode) Close() { v.conn.Close() }

// ownTool matches the runner's own tools as the editor lists them, so a
// primitive can never bind to tap_run and start itself.
var ownTool = regexp.MustCompile(`(^|_)tap_(run|result)$`)

func (v *VSCode) Inventory() ([]bind.Tool, error) {
	r, err := v.request(map[string]any{"op": "tools"})
	if err != nil {
		return nil, err
	}
	list, _ := r["tools"].([]any)
	var out []bind.Tool
	names := map[[2]string]string{}
	for _, x := range list {
		t, _ := x.(map[string]any)
		name, _ := t["name"].(string)
		if name == "" || ownTool.MatchString(name) {
			continue
		}
		schema, _ := t["inputSchema"].(map[string]any)
		server, tool := splitEditorName(name, t)
		names[[2]string{server, tool}] = name
		out = append(out, bind.Tool{Server: server, Name: tool, Schema: schema, Annotated: bind.Unknown})
	}
	v.mu.Lock()
	v.editorNames = names
	v.mu.Unlock()
	return out, nil
}

// splitEditorName names an MCP tool by its server and the server's own tool
// name, as every other client does, so pins, gateway adapters and capability
// matching see the same names in VS Code. VS Code lists an MCP tool as
// mcp_<server>_<tool>, where <server> is the server's own name lowercased,
// with characters other than letters, digits, "_", "." and "-" made "_", and
// cut to 13 characters (VS Code's McpPrefixGenerator); a dot in the tool's
// name becomes "_". A tag "mcp:<server>" names the server outright. Without
// one, the server is read up to the first "_", which is wrong only for a
// server whose own name has one; the tool is then still called by its VS
// Code name. Built-in tools keep their name, under "vscode".
func splitEditorName(name string, t map[string]any) (server, tool string) {
	server = serverOf(t)
	rest, isMCP := strings.CutPrefix(name, "mcp_")
	if !isMCP {
		return server, name
	}
	if server != "vscode" {
		if after, ok := strings.CutPrefix(rest, editorPrefix(server)+"_"); ok && after != "" {
			return server, after
		}
		return server, name
	}
	if i := strings.Index(rest, "_"); i > 0 && i < len(rest)-1 {
		return rest[:i], rest[i+1:]
	}
	return server, name
}

var notInPrefix = regexp.MustCompile(`[^a-z0-9_.-]+`)

// editorPrefix is a server name as VS Code writes it inside a tool name.
func editorPrefix(server string) string {
	s := notInPrefix.ReplaceAllString(strings.ToLower(server), "_")
	if len(s) > 13 {
		s = s[:13]
	}
	return s
}

// editorName is the name VS Code calls a tool by.
func (v *VSCode) editorName(t bind.Tool) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	if n, ok := v.editorNames[[2]string{t.Server, t.Name}]; ok {
		return n
	}
	return t.Name
}

// serverOf names the tool's source as the editor tags it, or "vscode" when
// it says nothing. The editor's tool name is what a call uses; the server
// only groups tools for a pin and for the receipt.
func serverOf(t map[string]any) string {
	tags, _ := t["tags"].([]any)
	for _, x := range tags {
		if s, _ := x.(string); strings.HasPrefix(s, "mcp:") {
			return strings.TrimPrefix(s, "mcp:")
		}
	}
	if s, _ := t["source"].(string); s != "" {
		return s
	}
	return "vscode"
}

func (v *VSCode) Call(t bind.Tool, args map[string]any) (string, error) {
	if args == nil {
		args = map[string]any{}
	}
	r, err := v.request(map[string]any{"op": "call", "name": v.editorName(t), "input": args})
	if err != nil {
		return "", err
	}
	text, _ := r["text"].(string)
	return text, nil
}
