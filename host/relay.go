package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/bind"
	"github.com/Telara-Labs/TAP-Runtime/bridge"
	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

// A relay run is how the runner borrows the connections of a client that has
// no call-back API but lets a hook ask it to make a tool call (TENG-3058).
// Gemini CLI is the first: an AfterTool hook may return a tailToolCallRequest,
// and Gemini then makes that call itself, with its own connection and its
// own approval, and fires the hook again on its result.
//
// The program runs here, in the server the client started. When it asks for
// a tool, the run stops at that request and says so. The hook, a separate
// process the client starts after every tool call, asks the client to make
// the call, carries its result back here over a local socket, and is told
// what comes next: another call, or the end. At the end the hook has the
// client call tap_result, whose short answer replaces everything the chain
// produced, so the model sees only the primitive's output.

// relayPrefix starts the text tap_run returns while a run waits on the
// client. The hook reads it; without the hook, the model reads the rest.
const relayPrefix = "TAP_PENDING "

// relayCall is one tool call the program asked the client to make.
type relayCall struct {
	Name string         `json:"name"` // the client's own name for the tool
	Args map[string]any `json:"args"`
}

// relayEvent is what a run did next: asked for a call, or ended.
type relayEvent struct {
	call *relayCall
	done bool
}

type relayAnswer struct {
	text string
	err  string
}

type relayRun struct {
	id      string
	client  string
	hub     *relayHub
	events  chan relayEvent
	answers chan relayAnswer

	finished chan struct{}
	result   *Result
	err      error
}

// relayHub holds the runs of one server and the socket their hooks reach.
type relayHub struct {
	mu   sync.Mutex
	dir  string
	sock string
	// sockDir is the private directory a long socket path falls back to.
	sockDir string
	ln      net.Listener
	runs    map[string]*relayRun
	expiry  time.Duration
	// name is what the client calls this server, so the hook can end a
	// chain with this server's tap_result.
	name string
}

// relayDir is where a server keeps its socket and the calls its runs wait
// on, private to the user. A variable so a test can move it.
var relayDir = func() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "tap-runtime", "relay"), nil
}

func newRelayHub(dir, name string) *relayHub {
	return &relayHub{dir: dir, runs: map[string]*relayRun{}, expiry: 10 * time.Minute, name: name}
}

// listen opens the hub's socket on first use. Only this user can reach it:
// the directory is private to them.
func (h *relayHub) listen() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ln != nil {
		return nil
	}
	if err := os.MkdirAll(h.dir, 0o700); err != nil {
		return err
	}
	os.Chmod(h.dir, 0o700)
	h.sock = filepath.Join(h.dir, fmt.Sprintf("%d.sock", os.Getpid()))
	// A socket path is limited to about 100 bytes (104 on macOS). A deep
	// cache directory falls back to a short name in the system's temporary
	// directory; the pending files, which name the socket, stay in h.dir.
	if len(h.sock) > 100 {
		// A directory of its own, made private with a name nobody can guess:
		// a fixed name in the shared temporary directory could be taken by
		// another user before it is made, or be removed from under another
		// process (TENG-3104).
		d, err := os.MkdirTemp("", "tap-relay-")
		if err != nil {
			return err
		}
		h.sockDir = d
		h.sock = filepath.Join(d, "r.sock")
	}
	os.Remove(h.sock)
	ln, err := net.Listen("unix", h.sock)
	if err != nil {
		return err
	}
	h.ln = ln
	go h.accept()
	return nil
}

func (h *relayHub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ln != nil {
		h.ln.Close()
		os.Remove(h.sock)
	}
	if h.sockDir != "" {
		os.RemoveAll(h.sockDir)
	}
	for id := range h.runs {
		os.Remove(filepath.Join(h.dir, id+".json"))
	}
}

func (h *relayHub) start(client string) (*relayRun, error) {
	if err := h.listen(); err != nil {
		return nil, err
	}
	b := make([]byte, 8)
	rand.Read(b)
	r := &relayRun{id: "relay-" + hex.EncodeToString(b), client: client, hub: h,
		events: make(chan relayEvent, 1), answers: make(chan relayAnswer, 1), finished: make(chan struct{})}
	h.mu.Lock()
	h.runs[r.id] = r
	h.mu.Unlock()
	return r, nil
}

func (h *relayHub) get(id string) *relayRun {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.runs[id]
}

func (h *relayHub) forget(id string) {
	h.mu.Lock()
	delete(h.runs, id)
	h.mu.Unlock()
	os.Remove(filepath.Join(h.dir, id+".json"))
}

// pendingFile is what a hook reads to find the run waiting on a call.
type pendingFile struct {
	Run    string    `json:"run"`
	Socket string    `json:"socket"`
	Call   relayCall `json:"call"`
}

// ask is the program asking the client for one tool call. It returns when a
// hook brings the result back, or when nobody has for the hub's expiry.
func (r *relayRun) ask(call relayCall) (string, error) {
	p, _ := json.Marshal(pendingFile{Run: r.id, Socket: r.hub.sock, Call: call})
	path := filepath.Join(r.hub.dir, r.id+".json")
	if err := os.WriteFile(path, p, 0o600); err != nil {
		return "", err
	}
	defer os.Remove(path)
	r.events <- relayEvent{call: &call}
	select {
	case a := <-r.answers:
		if a.err != "" {
			return "", fmt.Errorf("%s", a.err)
		}
		return a.text, nil
	case <-time.After(r.hub.expiry):
		return "", fmt.Errorf("the client did not make the call %s within %s; is the tap hook installed? run: tap install --client %s", call.Name, r.hub.expiry, r.client)
	}
}

// finish records how the run ended and says so to whoever waits next.
func (r *relayRun) finish(res *Result, err error) {
	r.result, r.err = res, err
	close(r.finished)
	r.events <- relayEvent{done: true}
}

// hookMessage is what a hook sends: the result of the call the run waits on.
type hookMessage struct {
	Run   string `json:"run"`
	Text  string `json:"text"`
	Error string `json:"error,omitempty"`
}

// hookReply tells the hook what the client should do next.
type hookReply struct {
	Call   *relayCall `json:"call,omitempty"`
	Finish bool       `json:"finish,omitempty"`
	// ResultTool is the client's name for this server's tap_result.
	ResultTool string `json:"result_tool,omitempty"`
	Error      string `json:"error,omitempty"`
}

func (h *relayHub) accept() {
	for {
		c, err := h.ln.Accept()
		if err != nil {
			return
		}
		go h.serveHook(c)
	}
}

func (h *relayHub) serveHook(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(h.expiry))
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	if !sc.Scan() {
		return
	}
	var m hookMessage
	reply := func(v hookReply) { b, _ := json.Marshal(v); c.Write(append(b, '\n')) }
	if json.Unmarshal(sc.Bytes(), &m) != nil {
		reply(hookReply{Error: "the hook sent something that is not a result"})
		return
	}
	r := h.get(m.Run)
	if r == nil {
		reply(hookReply{Error: "no run " + m.Run + " is waiting here"})
		return
	}
	r.answers <- relayAnswer{text: m.Text, err: m.Error}
	ev := <-r.events
	if ev.done {
		reply(hookReply{Finish: true, ResultTool: geminiToolName(h.name, "tap_result")})
		return
	}
	reply(hookReply{Call: ev.call})
}

// relayBridge is the bridge a relay run gives the runner. The client does not
// say which tools it has, so only tools the primitive pins can bind, and the
// pin names the client's tool exactly.
type relayBridge struct {
	run     *relayRun
	client  string
	version string
	tools   []bind.Tool
	nameFor func(server, tool string) string
	denies  func(bind.Tool) bool
}

func newRelayBridge(r *relayRun, client, version string, decls []mf.Tool) *relayBridge {
	b := &relayBridge{run: r, client: client, version: version, nameFor: geminiToolName}
	// The person's own rules, from Gemini CLI's settings: includeTools and
	// excludeTools per server, in their user settings and in this project's.
	if home, err := os.UserHomeDir(); err == nil {
		files := []string{filepath.Join(home, ".gemini", "settings.json")}
		if wd, err := os.Getwd(); err == nil {
			files = append(files, filepath.Join(wd, ".gemini", "settings.json"))
		}
		b.denies = bridge.GeminiRules(files...)
	}
	for _, d := range decls {
		if d.Pin != nil && d.Pin.Server != "" && d.Pin.Tool != "" {
			b.tools = append(b.tools, bind.Tool{Server: d.Pin.Server, Name: d.Pin.Tool, Annotated: bind.Unknown})
		}
	}
	return b
}

func (b *relayBridge) Client() (string, string) { return b.client, b.version }
func (b *relayBridge) HasSchemas() bool         { return false }
func (b *relayBridge) Denied(t bind.Tool) (bool, error) {
	return b.denies != nil && b.denies(t), nil
}
func (b *relayBridge) Close() {}

func (b *relayBridge) Inventory() ([]bind.Tool, error) { return b.tools, nil }

func (b *relayBridge) Call(t bind.Tool, args map[string]any) (string, error) {
	if args == nil {
		args = map[string]any{}
	}
	return b.run.ask(relayCall{Name: b.nameFor(t.Server, t.Name), Args: args})
}

var geminiInvalid = regexp.MustCompile(`[^a-zA-Z0-9_\-.:]`)

// geminiToolName is Gemini CLI's name for an MCP tool: generateValidName of
// server + "_" + tool (packages/core/src/tools/mcp-tool.ts).
func geminiToolName(server, tool string) string {
	n := server + "_" + tool
	if !strings.HasPrefix(n, "mcp_") {
		n = "mcp_" + n
	}
	n = geminiInvalid.ReplaceAllString(n, "_")
	if n != "" && !regexp.MustCompile(`^[a-zA-Z_]`).MatchString(n) {
		n = "_" + n
	}
	if len(n) > 63 {
		n = n[:30] + "..." + n[len(n)-30:]
	}
	return n
}

// pendingText is what tap_run answers while its run waits on the client.
func pendingText(run string, call relayCall) string {
	b, _ := json.Marshal(map[string]any{"run": run, "call": call})
	return relayPrefix + string(b) + "\n\nThis primitive is waiting for the client to make the tool call above. " +
		"The tap hook makes it; if you are reading this, the hook is not installed. Run: tap install --client gemini"
}

// relayPending reads tap_run's answer back into the waiting run and call.
func relayPending(text string) (string, relayCall, bool) {
	i := strings.Index(text, relayPrefix)
	if i < 0 {
		return "", relayCall{}, false
	}
	line := text[i+len(relayPrefix):]
	if j := strings.IndexByte(line, '\n'); j >= 0 {
		line = line[:j]
	}
	var p struct {
		Run  string    `json:"run"`
		Call relayCall `json:"call"`
	}
	if json.Unmarshal([]byte(line), &p) != nil || p.Run == "" || p.Call.Name == "" {
		return "", relayCall{}, false
	}
	return p.Run, p.Call, true
}
