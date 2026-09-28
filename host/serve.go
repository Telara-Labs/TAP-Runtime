package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sync"
)

// serve runs the runner as an MCP server over standard input and output, so
// a client can start a primitive as a tool and so the runner can ask the
// person at that client to approve a write (ruling 14).
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
	retention := fs.Int("retention-days", 30, "remove the records of runs older than this many days; 0 keeps them for ever")
	if err := fs.Parse(args); err != nil {
		return err
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
	s := &server{out: out, pending: map[int]chan rpcMessage{}, journal: journal, interpDir: *interpDir, cacheDir: *cacheDir, runsDir: *runsDir, retention: *retention}
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
	mu        sync.Mutex
	out       io.Writer
	next      int
	pending   map[int]chan rpcMessage
	closed    bool
	journal   io.Writer
	interpDir string
	cacheDir  string
	runsDir   string
	retention int

	clientName    string
	clientVersion string
	canElicit     bool
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
	s.mu.Unlock()
	s.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	m, ok := <-ch
	return m, ok
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
	m, ok := s.ask("elicitation/create", map[string]any{
		"message": fmt.Sprintf("The primitive %q wants to: %s.\n\nThis is a %s change. Waiting now:\n\n%s%s\n\nNothing further is done until you answer.",
			a.Primitive, a.Kind, a.Effect, a.Example, so),
		"requestedSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"approve": map[string]any{"type": "boolean", "title": "Allow this", "description": a.Kind, "default": false},
				"limit": map[string]any{"type": "integer", "title": "How many times", "minimum": 1, "default": 1,
					"description": "After this many you are asked again."},
			},
			"required": []string{"approve"},
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
	// A person who ticks the box and names no number has agreed to one.
	limit := r.Content.Limit
	if limit < 1 {
		limit = 1
	}
	return Grant{OK: true, Limit: limit}
}

var runTool = map[string]any{
	"name":        "tap_run",
	"description": "Run a TAP primitive: a package with a primitive.yaml and one program. The program runs in a sandbox and can only do what its primitive.yaml declares. Any change it wants to make is shown to the user for approval first.",
	"inputSchema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"package": map[string]any{"type": "string", "description": "Path to the primitive's directory."},
			"args":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Arguments passed to the program."},
		},
		"required": []string{"package"},
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
		s.reply(m.ID, map[string]any{"tools": []any{runTool}})
	case "tools/call":
		var p struct {
			Name      string `json:"name"`
			Arguments struct {
				Package string   `json:"package"`
				Args    []string `json:"args"`
			} `json:"arguments"`
		}
		if json.Unmarshal(m.Params, &p) != nil || p.Name != "tap_run" || p.Arguments.Package == "" {
			s.fail(m.ID, -32602, "tap_run needs a package")
			return
		}
		s.mu.Lock()
		name, canElicit := s.clientName, s.canElicit
		s.mu.Unlock()
		var approve Approver
		if canElicit {
			approve = s.elicit
		}
		res, err := Run(context.Background(), Options{
			Package: p.Arguments.Package, Args: p.Arguments.Args, Journal: s.journal, Approve: approve,
			InterpDir: s.interpDir, CacheDir: s.cacheDir, RunsDir: s.runsDir, RetentionDays: s.retention, Client: clientFor(name),
		})
		if err != nil {
			s.reply(m.ID, map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "refused: " + err.Error()}}})
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
		s.reply(m.ID, map[string]any{"isError": res.Exit != 0, "content": []any{map[string]any{"type": "text", "text": text}}})
	default:
		s.fail(m.ID, -32601, "method not found")
	}
}

// clientFor maps the name a client gives in the MCP handshake to the bridge
// that can borrow its connections. An unknown name is passed through, and
// openBridge refuses it if the primitive needs tools.
func clientFor(name string) string {
	switch name {
	case "claude-code":
		return "claude"
	case "codex-mcp-client":
		return "codex"
	}
	if name == "" {
		return "unknown"
	}
	return name
}
