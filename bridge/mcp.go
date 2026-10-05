package bridge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Telara-Labs/TAP-Runtime/bind"
)

// MCP reaches one MCP server directly, over streamable HTTP. It is the bridge
// for a runner with no client to borrow from: a service that runs primitives
// itself hands the runner the MCP endpoint it would call and the header that
// authenticates it (doc 34 section 13.20). The server decides, per call, what
// the caller may do; this bridge adds no policy of its own.
type MCP struct {
	url    string
	header http.Header
	http   *http.Client

	server  string // connection name exposed as Server for every tool
	version string // serverInfo.version

	mu      sync.Mutex
	n       int
	session string // Mcp-Session-Id, once the server assigns one
}

// maxMCPResponse bounds one response, JSON or event stream.
const maxMCPResponse = 64 << 20

// mcpProtocolVersion is the MCP revision this bridge speaks.
const mcpProtocolVersion = "2025-06-18"

// NewMCP opens a session with the server at url, sending header on every
// request.
func NewMCP(url string, header http.Header) (*MCP, error) {
	return NewMCPWithName(url, header, "")
}

// NewMCPWithName opens an MCP server over streamable HTTP. connectionName,
// when nonempty, is the local connection alias used as bind.Tool.Server; the
// endpoint's advertised tool names and all wire calls remain unchanged. An
// empty name preserves NewMCP's serverInfo.name behavior.
func NewMCPWithName(url string, header http.Header, connectionName string) (*MCP, error) {
	if err := validateMCPConnectionName(connectionName); err != nil {
		return nil, err
	}
	m := &MCP{url: url, header: header.Clone(), http: &http.Client{
		Timeout: 120 * time.Second,
		// Never follow a redirect. Go drops only Authorization, Cookie and
		// WWW-Authenticate when the host changes, so any other header from the
		// header file (an X-Api-Key, say) would go to whatever host a 30x names.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	r, err := m.call("initialize", map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "tap-runtime", "version": "0"},
	})
	if err != nil {
		return nil, err
	}
	if info, ok := r["serverInfo"].(map[string]any); ok {
		m.server, _ = info["name"].(string)
		m.version, _ = info["version"].(string)
	}
	if m.server == "" {
		m.server = "mcp"
	}
	if connectionName != "" {
		m.server = connectionName
	}
	if err := m.notify("notifications/initialized"); err != nil {
		return nil, err
	}
	return m, nil
}

func validateMCPConnectionName(name string) error {
	if name == "" {
		return nil
	}
	if strings.TrimSpace(name) == "" || strings.TrimSpace(name) != name {
		return fmt.Errorf("MCP connection name must be nonempty and have no surrounding whitespace")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return fmt.Errorf("MCP connection name must not contain control characters")
		}
	}
	return nil
}

// Client names the protocol rather than a product: what is on the other end
// is whatever MCP server the caller pointed at.
func (m *MCP) Client() (string, string) { return "mcp", m.version }
func (m *MCP) HasSchemas() bool         { return true }

func (m *MCP) Inventory() ([]bind.Tool, error) {
	var out []bind.Tool
	cursor := ""
	for {
		p := map[string]any{}
		if cursor != "" {
			p["cursor"] = cursor
		}
		r, err := m.call("tools/list", p)
		if err != nil {
			return nil, err
		}
		tools, _ := r["tools"].([]any)
		for _, tv := range tools {
			tm, _ := tv.(map[string]any)
			name, _ := tm["name"].(string)
			if name == "" {
				continue
			}
			ann, _ := tm["annotations"].(map[string]any)
			schema, _ := tm["inputSchema"].(map[string]any)
			// MCP's own annotation spelling is the *Hint form Codex passes on.
			out = append(out, bind.Tool{Server: m.server, Name: name, Annotated: codexEffect(ann), Schema: schema})
		}
		cursor, _ = r["nextCursor"].(string)
		if cursor == "" {
			return out, nil
		}
	}
}

// Denied always reports false. There is no user between the runner and the
// server to have forbidden anything; the server refuses what it will not do.
func (m *MCP) Denied(bind.Tool) (bool, error) { return false, nil }

// Call dispatches one tool. A result the server marks isError is returned as
// an error, so a refusal is never read back as the tool's output.
func (m *MCP) Call(t bind.Tool, args map[string]any) (string, error) {
	if args == nil {
		args = map[string]any{}
	}
	r, err := m.call("tools/call", map[string]any{"name": t.Name, "arguments": args})
	if err != nil {
		return "", err
	}
	if isErr, _ := r["isError"].(bool); isErr {
		return "", fmt.Errorf("%s: %s", t.Name, resultText(r))
	}
	return resultText(r), nil
}

// Close ends the session. A server that does not support ending sessions
// answers 405, which is not an error.
func (m *MCP) Close() {
	m.mu.Lock()
	session := m.session
	m.mu.Unlock()
	if session == "" {
		return
	}
	req, err := http.NewRequest(http.MethodDelete, m.url, nil)
	if err != nil {
		return
	}
	m.setHeaders(req, session)
	if resp, err := m.http.Do(req); err == nil {
		resp.Body.Close()
	}
}

func (m *MCP) setHeaders(req *http.Request, session string) {
	for k, vs := range m.header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", mcpProtocolVersion)
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
}

func (m *MCP) post(msg map[string]any) (*http.Response, error) {
	body, _ := json.Marshal(msg)
	req, err := http.NewRequest(http.MethodPost, m.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	session := m.session
	m.mu.Unlock()
	m.setHeaders(req, session)
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, err
	}
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		m.mu.Lock()
		m.session = s
		m.mu.Unlock()
	}
	return resp, nil
}

// refuseRedirect turns a 3xx into an error naming where it pointed.
func refuseRedirect(resp *http.Response) error {
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return fmt.Errorf("the server redirected (HTTP %d) to %q; the bridge does not follow redirects, so point --mcp-url at the final address", resp.StatusCode, resp.Header.Get("Location"))
	}
	return nil
}

func (m *MCP) notify(method string) error {
	resp, err := m.post(map[string]any{"jsonrpc": "2.0", "method": method})
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxMCPResponse))
	if err := refuseRedirect(resp); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: HTTP %d", method, resp.StatusCode)
	}
	return nil
}

func (m *MCP) call(method string, params any) (map[string]any, error) {
	m.mu.Lock()
	m.n++
	id := m.n
	m.mu.Unlock()
	resp, err := m.post(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	defer resp.Body.Close()
	if err := refuseRedirect(resp); err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%s: HTTP %d: %s", method, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	msg, err := readResponse(resp, id)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	if e, ok := msg["error"]; ok && e != nil {
		return nil, fmt.Errorf("%s: %v", method, e)
	}
	res, _ := msg["result"].(map[string]any)
	return res, nil
}

// readResponse returns the JSON-RPC response with the given id, from a plain
// JSON body or from an event stream. Notifications and requests the server
// interleaves on the stream are skipped.
func readResponse(resp *http.Response, id int) (map[string]any, error) {
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if ct != "text/event-stream" {
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxMCPResponse+1))
		if err != nil {
			return nil, err
		}
		if len(body) > maxMCPResponse {
			return nil, fmt.Errorf("response is larger than %d MiB; refused", maxMCPResponse>>20)
		}
		var msg map[string]any
		if err := json.Unmarshal(body, &msg); err != nil {
			return nil, fmt.Errorf("unreadable response: %w", err)
		}
		return msg, nil
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), maxMCPResponse)
	var data []string
	for sc.Scan() {
		line := sc.Text()
		if line != "" {
			if d, ok := strings.CutPrefix(line, "data:"); ok {
				data = append(data, strings.TrimPrefix(d, " "))
			}
			continue
		}
		// A blank line ends one event.
		if len(data) == 0 {
			continue
		}
		var msg map[string]any
		err := json.Unmarshal([]byte(strings.Join(data, "\n")), &msg)
		data = nil
		if err != nil {
			continue
		}
		if got, ok := msg["id"].(float64); ok && int(got) == id && msg["method"] == nil {
			return msg, nil
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	// A stream may close without the blank line after its last event.
	if len(data) > 0 {
		var msg map[string]any
		if json.Unmarshal([]byte(strings.Join(data, "\n")), &msg) == nil {
			if got, ok := msg["id"].(float64); ok && int(got) == id && msg["method"] == nil {
				return msg, nil
			}
		}
	}
	return nil, fmt.Errorf("the stream ended without a response")
}
