package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// serve runs the runner as an MCP server over standard input and output, so
// a client can start a primitive as a tool and so the runner can ask the
// person at that client to approve a write.
//
// The approval is an MCP elicitation. The client shows it; the model that
// called the tool is never given the question and cannot answer it. A client
// that does not advertise elicitation is never asked, and every write under
// it is refused.
func serve(in io.Reader, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	journalPath := fs.String("journal", "", "append one JSON line per action")
	interpDir := fs.String("interpreters", "", "interpreter store; default is the user cache directory")
	cacheDir := fs.String("cache", "", "directory for the compiled-interpreter cache")
	runsDir := fs.String("runs", "", "directory holding one record per run; default is the user cache directory")
	otelPayloads := fs.Bool("otel-payloads", false, "with an OpenTelemetry endpoint set: also send what calls were given and what they touched")
	mcpURL := fs.String("mcp-url", "", "call this MCP server (streamable HTTP) directly instead of borrowing the client's connections")
	mcpHeaderFile := fs.String("mcp-header-file", "", "with --mcp-url: file of header lines to send, such as \"Authorization: Bearer ...\"")
	mcpServerName := fs.String("mcp-server-name", "", "with --mcp-url: logical connection name used by primitive tool pins; defaults to serverInfo.name")
	retention := fs.Int("retention-days", 30, "remove the records of runs older than this many days; 0 keeps them for ever")
	serverName := fs.String("name", "tap", "the name the client knows this server by, as given at install")
	vscodeSocket := fs.String("vscode-socket", "", "the TAP extension's socket, given by the extension that starts this server in VS Code")
	noRecord := fs.Bool("no-record", false, "keep no record of a run: tool results and requests are not written to disk, and a run cannot be resumed")
	configDir := fs.String("config-dir", "", "directory for the choices a person made (tool bindings) and the packages they trust; default is the user config directory")
	catalogRoot := fs.String("catalog-root", "", "additional local primitive collection root")
	allowPackagePath := fs.Bool("allow-package-path", false, "allow legacy model-supplied package paths; use only during migration")
	httpListen := fs.String("http-listen", "", "serve Streamable HTTP at /mcp on this address instead of stdio (for example 127.0.0.1:8765)")
	httpTokenFile := fs.String("http-token-file", "", "required with --http-listen: private file containing the incoming bearer token")
	var httpOrigins stringList
	fs.Var(&httpOrigins, "http-origin", "allowed HTTP Origin; repeat for each exact origin")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *configDir != "" {
		userConfigDir = func() (string, error) { return *configDir, nil }
	}
	var journal io.Writer = io.Discard
	if *journalPath != "" {
		f, err := os.OpenFile(*journalPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		journal = &lockedWriter{w: f}
	}
	s := &server{out: out, pending: map[int]chan rpcMessage{}, journal: journal, interpDir: *interpDir, cacheDir: *cacheDir, runsDir: *runsDir, retention: *retention, payloads: *otelPayloads, mcpURL: *mcpURL, mcpHeaderFile: *mcpHeaderFile, vscodeSocket: *vscodeSocket, noRecord: *noRecord, catalogRoot: *catalogRoot, allowPackagePath: *allowPackagePath}
	s.mcpServerName = *mcpServerName
	if *httpListen != "" {
		if *mcpURL == "" {
			return fmt.Errorf("--http-listen requires an explicitly configured --mcp-url backend")
		}
		return serveHTTP(*httpListen, *httpTokenFile, httpOrigins, s)
	}
	if dir, err := relayDir(); err == nil {
		s.relay = newRelayHub(dir, *serverName)
		defer s.relay.close()
	}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	var wg sync.WaitGroup
	for sc.Scan() {
		var m rpcMessage
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch {
		case m.Method == "" && m.ID != nil:
			s.deliver(m)
		case m.Method != "" && m.ID != nil:
			wg.Add(1)
			go func() { defer wg.Done(); s.handle(m) }()
		}
	}
	s.closeAll()
	wg.Wait()
	return sc.Err()
}

type rpcMessage struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *rpcError        `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

type server struct {
	noRecord         bool // keep no record of a run
	mu               sync.Mutex
	out              io.Writer
	next             int
	pending          map[int]chan rpcMessage
	closed           bool
	journal          io.Writer
	interpDir        string
	cacheDir         string
	mcpURL           string
	mcpHeaderFile    string
	mcpServerName    string
	runsDir          string
	catalogRoot      string
	allowPackagePath bool
	retention        int
	payloads         bool

	clientName    string
	clientVersion string
	canElicit     bool
	requestCtx    context.Context // HTTP serializes requests within each client session

	// relay holds the runs that wait on a client which makes tool calls
	// when a hook asks it to (relay.go).
	relay *relayHub
	// vscodeSocket reaches the TAP extension in VS Code (bridge/vscode.go).
	vscodeSocket string
}

func (s *server) write(v any) {
	b, _ := json.Marshal(v)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out.Write(append(b, '\n'))
}

func (s *server) reply(id *json.RawMessage, result any) {
	s.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (s *server) fail(id *json.RawMessage, code int, msg string) {
	s.write(map[string]any{"jsonrpc": "2.0", "id": id, "error": rpcError{code, msg}})
}

func (s *server) deliver(m rpcMessage) {
	var id int
	if json.Unmarshal(*m.ID, &id) != nil {
		return
	}
	s.mu.Lock()
	ch := s.pending[id]
	delete(s.pending, id)
	s.mu.Unlock()
	if ch != nil {
		ch <- m
	}
}

func (s *server) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for id, ch := range s.pending {
		close(ch)
		delete(s.pending, id)
	}
}

// ask sends a request to the client and waits for its answer.
func (s *server) ask(method string, params any) (rpcMessage, bool) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return rpcMessage{}, false
	}
	s.next++
	id := s.next
	ch := make(chan rpcMessage, 1)
	s.pending[id] = ch
	ctx := s.requestCtx
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Unlock()
	s.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	select {
	case m, ok := <-ch:
		return m, ok
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return rpcMessage{}, false
	}
}

// elicit asks the person at the client whether a primitive may make a kind
// of change, and how many times. Anything but an explicit yes is a no: a
// decline, a cancel, an error, a closed connection, or an accepted form with
// the box left unticked.
func (s *server) elicit(a Ask) Grant {
	so := ""
	if a.Done > 0 {
		so = fmt.Sprintf("\n\nIt has made %d of these in this run and has reached the number you allowed.", a.Done)
	}
	what := fmt.Sprintf("This is a %s change.", a.Effect)
	props := map[string]any{
		"approve": map[string]any{"type": "boolean", "title": "Allow this", "description": a.Kind, "default": false},
		"limit": map[string]any{"type": "integer", "title": "How many times", "minimum": 1, "default": 1,
			"description": "After this many you are asked again."},
	}
	// A read that leaves this machine changes nothing there, but its address
	// and headers can carry data out. It is asked once for the origin, with no
	// count, and what it sends is in the record.
	reads := a.Effect == "read"
	if reads {
		what = "This changes nothing, but the address and headers of a request can carry data off this machine. You are asked once for this origin in this run."
		delete(props, "limit")
	}
	m, ok := s.ask("elicitation/create", map[string]any{
		"message": fmt.Sprintf("The primitive %q wants to: %s.\n\n%s Waiting now:\n\n%s%s\n\nNothing further is done until you answer.",
			a.Primitive, a.Kind, what, a.Example, so),
		"requestedSchema": map[string]any{
			"type":       "object",
			"properties": props,
			"required":   []string{"approve"},
		},
	})
	if !ok || m.Error != nil {
		return Grant{}
	}
	var r struct {
		Action  string `json:"action"`
		Content struct {
			Approve bool `json:"approve"`
			Limit   int  `json:"limit"`
		} `json:"content"`
	}
	if json.Unmarshal(m.Result, &r) != nil || r.Action != "accept" || !r.Content.Approve {
		return Grant{}
	}
	if reads {
		return Grant{OK: true, Limit: Unlimited}
	}
	// A person who ticks the box and names no number has agreed to one.
	limit := r.Content.Limit
	if limit < 1 {
		limit = 1
	}
	return Grant{OK: true, Limit: limit}
}

// trustPackage asks the person whether this package may run on this machine.
func (s *server) trustPackage(name, publisher, version, path, digest, declared string) bool {
	m, ok := s.ask("elicitation/create", map[string]any{
		"message": fmt.Sprintf("A client asked to run the primitive %q %s from %s (digest %s) for the first time on this machine. It declares:\n\n%s\n\nA primitive can only do what it declares, and each change is asked of you separately. Run it?",
			name, strings.TrimSpace("by "+publisher+" v"+version), path, digest, declared),
		"requestedSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"approve": map[string]any{"type": "boolean", "title": "Run this primitive", "default": false}},
			"required":   []string{"approve"},
		},
	})
	if !ok || m.Error != nil {
		return false
	}
	var r struct {
		Action  string `json:"action"`
		Content struct {
			Approve bool `json:"approve"`
		} `json:"content"`
	}
	return json.Unmarshal(m.Result, &r) == nil && r.Action == "accept" && r.Content.Approve
}

// choose asks the person which of several servers should fill a capability.
// Anything but an explicit pick of one of them is a no.
func (s *server) choose(p Pick) (string, bool) {
	m, ok := s.ask("elicitation/create", map[string]any{
		"message": fmt.Sprintf("The primitive %q needs a tool for %s, and %d connected servers offer one that fits equally well. The runner does not choose between them. Which should it use? The choice is kept on this machine for %s.\n\nNothing is done until you answer.",
			p.Primitive, p.Capability, len(p.Servers), p.Client),
		"requestedSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"server": map[string]any{"type": "string", "title": "Server", "enum": p.Servers},
			},
			"required": []string{"server"},
		},
	})
	if !ok || m.Error != nil {
		return "", false
	}
	var r struct {
		Action  string `json:"action"`
		Content struct {
			Server string `json:"server"`
		} `json:"content"`
	}
	if json.Unmarshal(m.Result, &r) != nil || r.Action != "accept" || r.Content.Server == "" {
		return "", false
	}
	return r.Content.Server, true
}

var runTool = map[string]any{
	"name":        "tap_run",
	"description": "Run one exact installed TAP primitive by reference and digest. Changes still require the person's approval.",
	"inputSchema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"ref":    map[string]any{"type": "string", "description": "Exact publisher/name@version returned by tap_search."},
			"digest": map[string]any{"type": "string", "description": "Exact package digest returned by tap_search."},
			"args":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Arguments passed to the program."},
		},
		"required":             []string{"ref", "digest"},
		"additionalProperties": false,
	},
	"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": true, "openWorldHint": true},
}

func (s *server) handle(m rpcMessage) {
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
			ClientInfo      struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"clientInfo"`
			Capabilities map[string]json.RawMessage `json:"capabilities"`
		}
		json.Unmarshal(m.Params, &p)
		s.mu.Lock()
		s.clientName, s.clientVersion = p.ClientInfo.Name, p.ClientInfo.Version
		_, s.canElicit = p.Capabilities["elicitation"]
		s.mu.Unlock()
		logf("client     %s %s, elicitation=%v", p.ClientInfo.Name, p.ClientInfo.Version, s.canElicit)
		s.reply(m.ID, map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "tap-runtime", "version": version},
		})
	case "ping":
		s.reply(m.ID, map[string]any{})
	case "tools/list":
		tools := []any{searchTool, loadTool, runTool, statusTool, evidenceTool}
		s.mu.Lock()
		name := s.clientName
		s.mu.Unlock()
		if relayClient(clientFor(name)) {
			tools = append(tools, resultTool)
		}
		s.reply(m.ID, map[string]any{"tools": tools})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(m.Params, &p) != nil {
			s.fail(m.ID, -32602, "the call could not be read")
			return
		}
		s.mu.Lock()
		name, version, canElicit := s.clientName, s.clientVersion, s.canElicit
		s.mu.Unlock()
		if p.Name == "tap_result" {
			var args struct {
				Run string `json:"run"`
			}
			if json.Unmarshal(p.Arguments, &args) != nil {
				s.fail(m.ID, -32602, "tap_result needs a run")
				return
			}
			s.replyResult(m.ID, args.Run, canElicit)
			return
		}
		if p.Name != "tap_run" {
			s.handleReadTool(m.ID, p.Name, p.Arguments)
			return
		}
		var args struct {
			Ref     string   `json:"ref"`
			Digest  string   `json:"digest"`
			Args    []string `json:"args"`
			Package string   `json:"package"`
		}
		if readToolArgs(p.Arguments, &args) != nil {
			s.fail(m.ID, -32602, "tap_run arguments could not be read")
			return
		}
		if len(args.Ref) > 512 || (args.Package == "" && len(args.Digest) != 64) || len(args.Args) > 128 {
			s.toolError(m.ID, "tap_run identity or arguments exceed their limits")
			return
		}
		for _, value := range args.Args {
			if len(value) > 16<<10 {
				s.toolError(m.ID, "tap_run argument exceeds 16 KiB")
				return
			}
		}
		packagePath, err := s.resolveRunPackage(args.Ref, args.Digest, args.Package)
		if err != nil {
			s.toolError(m.ID, err.Error())
			return
		}
		var approve Approver
		var choose Chooser
		var truster Truster
		if canElicit {
			approve = s.elicit
			choose = s.choose
			truster = s.trustPackage
		}
		if why := admitPackage(newTrustStore(), truster, packagePath); why != "" {
			s.reply(m.ID, map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": why}}})
			return
		}
		o := Options{
			Package: packagePath, ExpectedDigest: args.Digest, Args: args.Args, Journal: s.journal, Approve: approve, Choose: choose,
			InterpDir: s.interpDir, CacheDir: s.cacheDir, RunsDir: s.runsDir, RetentionDays: s.retention, NoJournal: s.noRecord, TelemetryPayloads: s.payloads, Client: clientFor(name),
			MCPURL: s.mcpURL, MCPHeaderFile: s.mcpHeaderFile, MCPServerName: s.mcpServerName,
		}
		if s.vscodeSocket != "" && s.mcpURL == "" {
			// The approval stays with this runner: VS Code runs an unconfirmed
			// tool call made this way without asking anyone.
			o.VSCodeSocket = s.vscodeSocket
		}
		if relayClient(o.Client) && s.relay != nil && s.mcpURL == "" {
			s.startRelay(m.ID, o, name, version, canElicit)
			return
		}
		s.mu.Lock()
		ctx := s.requestCtx
		s.mu.Unlock()
		if ctx == nil {
			ctx = context.Background()
		}
		res, err := Run(ctx, o)
		s.replyRun(m.ID, res, err, canElicit)
	default:
		s.fail(m.ID, -32601, "method not found")
	}
}

// replyRun answers a tool call with how a run ended.
func (s *server) replyRun(id *json.RawMessage, res *Result, err error, canElicit bool) {
	if err != nil {
		s.reply(id, map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "refused: " + err.Error()}}})
		return
	}
	text := res.Stdout
	if res.Stderr != "" {
		text += "\n[stderr]\n" + res.Stderr
	}
	text += fmt.Sprintf("\n[%d action(s) run, %d refused]", res.Ran, res.Refused)
	if res.RunID != "" {
		text += "\n[run " + res.RunID + "]"
	}
	if res.Unknown > 0 {
		text += fmt.Sprintf("\n[%d change(s) have an unknown outcome and need a person to check]", res.Unknown)
	}
	if !canElicit && res.Refused > 0 {
		text += "\n[this client cannot show an approval prompt, so every change was refused]"
	}
	s.reply(id, map[string]any{"isError": res.Exit != 0 || res.Refused > 0 || res.Unknown > 0, "content": []any{map[string]any{"type": "text", "text": text}}})
}

// startRelay runs a primitive whose tool calls the client makes itself when
// its hook asks (relay.go). tap_run answers as soon as the program first
// asks for a tool, with that request; the hook takes it from there, and the
// chain ends with tap_result. A program that asks for nothing ends here.
func (s *server) startRelay(id *json.RawMessage, o Options, name, version string, canElicit bool) {
	r, err := s.relay.start(o.Client)
	if err != nil {
		s.replyRun(id, nil, fmt.Errorf("the relay could not start: %w", err), canElicit)
		return
	}
	o.relay, o.relayClient, o.relayVersion = r, name, version
	// Every tool call of a relay run is made by the client itself, through
	// its own validation and approval (Gemini sends a tail call through the
	// same confirmation as a call the model makes), so the client is the
	// gate for those. Anything the client never sees, a host program, a file
	// write or a web request, is still gated here.
	o.Approve = clientApprovesCalls(o.Approve)
	go func() {
		res, err := Run(context.Background(), o)
		r.finish(res, err)
	}()
	ev := <-r.events
	if ev.done {
		s.relay.forget(r.id)
		s.replyRun(id, r.result, r.err, canElicit)
		return
	}
	s.reply(id, map[string]any{"content": []any{map[string]any{"type": "text", "text": pendingText(r.id, *ev.call)}}})
}

// clientApprovesCalls leaves the approval of tool calls to the client, which
// makes each of them itself and confirms it the way it confirms a call from
// its own model. Every other kind of change is still asked of inner, or
// refused when there is no one to ask.
func clientApprovesCalls(inner Approver) Approver {
	return func(a Ask) Grant {
		if strings.HasPrefix(a.Kind, "call ") {
			return Grant{OK: true, Limit: Unlimited}
		}
		if inner == nil {
			return Grant{}
		}
		return inner(a)
	}
}

// replyResult answers tap_result: how the relay run ended.
func (s *server) replyResult(id *json.RawMessage, run string, canElicit bool) {
	r := s.relay.get(run)
	if r == nil {
		s.reply(id, map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "no run " + run + " is known here"}}})
		return
	}
	<-r.finished
	s.relay.forget(run)
	s.replyRun(id, r.result, r.err, canElicit)
}

var resultTool = map[string]any{
	"name":        "tap_result",
	"description": "The answer of a TAP primitive that ran through this client. The tap hook calls it at the end of a run; you do not need to.",
	"inputSchema": map[string]any{
		"type":       "object",
		"properties": map[string]any{"run": map[string]any{"type": "string", "description": "The run's id."}},
		"required":   []string{"run"},
	},
	"annotations": map[string]any{"readOnlyHint": true},
}

// relayClient reports whether a client lends its connections through a hook
// that asks it to make calls, rather than through a call-back API.
func relayClient(client string) bool { return client == "gemini" }

// clientFor maps the name a client gives in the MCP handshake to the bridge
// that can borrow its connections. An unknown name is passed through, and
// openBridge refuses it if the primitive needs tools.
func clientFor(name string) string {
	switch name {
	case "claude-code":
		return "claude"
	case "codex-mcp-client":
		return "codex"
	case "gemini-cli-mcp-client":
		return "gemini"
	case "goose-cli": // goose 1.53, also under goose acp
		return "goose"
	}
	if name == "" {
		return "unknown"
	}
	return name
}
