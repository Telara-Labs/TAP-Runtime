package bridge

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/bind"
)

// Kilo reaches the Kilo CLI through its own server, `kilo serve`, started
// for this run. POST /experimental/mcp/call-tool runs one tool of a
// connected MCP server through Kilo's live client, with no model turn
// Kilo exposes the route only with its experimental flag on,
// which the bridge sets on the server it starts and nowhere else.
//
// The route skips Kilo's own permission prompt, so, as for Goose, the
// runner's effect gate is the only approval. A tool the person's Kilo
// configuration switches off (tools: {name: false}) or denies
// (permission: {name: "deny"}) is denied.
//
// Kilo lists no MCP server's tools, so a primitive that runs through Kilo
// pins each tool it uses ({server, tool}), as for Gemini CLI. The pinned
// tools of servers Kilo reports connected are the inventory.
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
// gives it the tools a primitive pins, and only those can bind.
type PinnedOnly interface {
	Bridge
	UsePins(tools []bind.Tool)
}

var kiloListening = regexp.MustCompile(`listening on (http://127\.0\.0\.1:\d+)`)

func NewKilo() (*Kilo, error) { return newKilo("kilo", nil) }

// newKilo starts bin serve; env, when set, replaces the process environment.
func newKilo(bin string, env []string) (*Kilo, error) {
	secret := make([]byte, 24)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	k := &Kilo{pass: hex.EncodeToString(secret), client: &http.Client{Timeout: 120 * time.Second}}
	k.dir, _ = os.Getwd()
	// Kilo takes --port 0 as its default port (4096), which another Kilo
	// server may hold: ask the system for a free one.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	cmd := exec.Command(bin, "serve", "--port", strconv.Itoa(port), "--hostname", "127.0.0.1")
	if env == nil {
		env = os.Environ()
	}
	// Kilo's own flag and the server's password, for this server only.
	cmd.Env = append(env, "KILO_EXPERIMENTAL_MCP_APPS=true", "KILO_SERVER_PASSWORD="+k.pass)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = cmd.Stdout
	ownGroup(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("kilo is not on this machine: %w", err)
	}
	k.cmd = cmd
	found := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if m := kiloListening.FindStringSubmatch(sc.Text()); m != nil {
				select {
				case found <- m[1]:
				default:
				}
			}
		}
	}()
	select {
	case k.base = <-found:
	case <-time.After(60 * time.Second):
		k.Close()
		return nil, fmt.Errorf("kilo serve did not say where it listens within 60 seconds")
	}
	if v, err := k.get("/global/health"); err == nil {
		var h struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(v, &h) == nil {
			k.version = h.Version
		}
	}
	if k.version == "" {
		if b, err := exec.Command(bin, "--version").Output(); err == nil {
			k.version = strings.TrimSpace(string(b))
		}
	}
	return k, nil
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

// UsePins gives the bridge the tools the primitive pins.
func (k *Kilo) UsePins(tools []bind.Tool) {
	k.mu.Lock()
	k.pins = append([]bind.Tool(nil), tools...)
	k.mu.Unlock()
}

// Inventory is the pinned tools whose server Kilo reports connected.
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
			t.Annotated = bind.Unknown
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
	if isErr, _ := r["isError"].(bool); isErr {
		return "", fmt.Errorf("%s/%s: %s", t.Server, t.Name, resultText(r))
	}
	return resultText(r), nil
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
