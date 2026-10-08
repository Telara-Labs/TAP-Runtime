package history

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

// R5 readers. Both fixtures are synthetic (testdata/SYNTHETIC.md):
// Copilot CLI and Zed need a sign-in this machine does not have.

// CopilotCLI reads GitHub Copilot CLI sessions:
// <Dir>/<id>/events.jsonl, one event per line {type, data, id, timestamp}:
// session.start {sessionId, context.cwd}, user.message {content},
// tool.execution_start {toolCallId, toolName, arguments},
// tool.execution_complete {toolCallId, success, result {content}}. Copilot
// sets success true on failed commands too, so the outcome comes from the
// result text. Known defects, tolerated: a result with raw newlines splits a
// line (the pieces are skipped and counted), a call with no complete event
// stays without a result. bash is the shell; an MCP tool is split by the
// server names in mcp-config.json (<server>-<tool> or <server>__<tool>).
type CopilotCLI struct {
	Dir     string   // ~/.copilot/session-state
	Configs []string // mcp-config.json
}

func (CopilotCLI) Client() string { return "copilot-cli" }

func (r CopilotCLI) Read(since time.Time) ([]trace.Session, error) {
	ss, _, err := r.ReadWithStats(since)
	return ss, err
}

func (r CopilotCLI) ReadWithStats(since time.Time) ([]trace.Session, trace.ReadStats, error) {
	var st trace.ReadStats
	files, _ := filepath.Glob(filepath.Join(r.Dir, "*", "events.jsonl"))
	servers := mcpServerNames(r.Configs, "mcpServers")
	var out []trace.Session
	// Tool names decode against the configured MCP servers, so they are part
	// of each file's fingerprint.
	parse := func(path string, r io.Reader) (trace.Session, error) { return parseCopilot(path, r, servers) }
	extra := func(string) string { return strings.Join(servers, "\x00") }
	for _, r := range ParseFilesWith(changedSince(files, since), "copilot-cli", extra, nil, parse) {
		if r.Err != nil {
			st.UnreadableFiles++
			continue
		}
		if s := r.Session; len(s.Calls) > 0 && !s.Start.Before(since) {
			out = append(out, s)
		}
	}
	sortSessions(out)
	return out, st, nil
}

func readCopilotFile(path string, servers []string) (trace.Session, error) {
	return ParseFile(path, func(path string, r io.Reader) (trace.Session, error) { return parseCopilot(path, r, servers) })
}

func parseCopilot(path string, fh io.Reader, servers []string) (trace.Session, error) {
	a := NewAssembler("copilot-cli", filepath.Base(filepath.Dir(path)))
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 128<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var ev struct {
			Type      string    `json:"type"`
			Timestamp time.Time `json:"timestamp"`
			Data      struct {
				SessionID  string                     `json:"sessionId"`
				Content    json.RawMessage            `json:"content"`
				ToolCallID string                     `json:"toolCallId"`
				ToolName   string                     `json:"toolName"`
				Arguments  map[string]json.RawMessage `json:"arguments"`
				Success    *bool                      `json:"success"`
				Result     struct {
					Content json.RawMessage `json:"content"`
				} `json:"result"`
				Error json.RawMessage `json:"error"`
			} `json:"data"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			a.Skip() // e.g. a result whose raw newline split the line
			continue
		}
		FirstTime(&a.S, ev.Timestamp)
		d := ev.Data
		switch ev.Type {
		case "session.start":
			if d.SessionID != "" {
				a.S.ID = d.SessionID
			}
		case "user.message":
			a.Add(UserText{Text: ResultText(d.Content)})
		case "tool.execution_start":
			c := trace.Call{ID: d.ToolCallID, Time: ev.Timestamp}
			switch name := d.ToolName; {
			case name == "bash":
				c.Tool, c.Command = "shell", RawString(d.Arguments["command"])
			default:
				if server, tool := splitServerToolSep(name, servers, "-", "__"); server != "" {
					MCPCall(&c, server, tool, d.Arguments)
				} else {
					c.Tool, c.Args, c.RawArgs = name, Flatten(d.Arguments), RawKeys(d.Arguments)
				}
			}
			a.Add(ToolCall{Key: d.ToolCallID, Call: c})
		case "tool.execution_complete":
			text := ResultText(d.Result.Content)
			if text == "" {
				text = ResultText(d.Error)
			}
			a.Add(ToolResult{Key: d.ToolCallID, Nth: -1, Text: text, IsError: d.Success != nil && !*d.Success})
		}
	}
	s := a.Finish()
	for i := range s.Calls {
		s.Calls[i].Session = s.ID
	}
	return s, sc.Err()
}

// splitServerToolSep splits <server><sep><tool> for a configured server.
func splitServerToolSep(name string, servers []string, seps ...string) (string, string) {
	for _, s := range servers {
		for _, sep := range seps {
			if tool, ok := strings.CutPrefix(name, s+sep); ok && tool != "" {
				return s, tool
			}
		}
	}
	return "", ""
}

// Zed reads Zed's agent threads: <Dir>/threads.db, table threads (id,
// updated_at, data_type zstd|json, data). Decompression calls the zstd
// command (decision D4); without it the reader is unavailable. A thread is
// {version, messages}: 0.3.0 messages are {"User": {content: [{Text}]}} or
// {"Agent": {content: [{Text} | {ToolUse {id, name, input}}], tool_results
// {id: {is_error, content {Text}}}}} or "Resume"; 0.2.0 messages are {role,
// segments}. terminal {command} is the shell. Zed records a tool by its bare
// name: an MCP tool's server is not in the thread.
type Zed struct{ Dir string }

func (Zed) Client() string { return "zed" }

func (r Zed) Read(since time.Time) ([]trace.Session, error) {
	ss, _, err := r.ReadWithStats(since)
	return ss, err
}

func (r Zed) ReadWithStats(since time.Time) ([]trace.Session, trace.ReadStats, error) {
	var st trace.ReadStats
	db := filepath.Join(r.Dir, "threads.db")
	if _, err := os.Stat(db); err != nil {
		return nil, st, nil
	}
	bin, err := sqliteBin("zed")
	if err != nil {
		return nil, st, err
	}
	zstd, zerr := exec.LookPath("zstd")
	// read decodes the named threads, or every thread when ids is nil.
	read := func(ids []string) (map[string]unitResult, error) {
		sql := `SELECT id, updated_at, data_type, hex(data) AS data FROM threads`
		if ids != nil {
			sql += ` WHERE id IN (` + sqlList(ids, false) + `)`
		}
		rows, err := sqliteRows(bin, db, sql)
		if err != nil {
			return nil, err
		}
		out := map[string]unitResult{}
		for _, row := range rows {
			id := rowString(row, "id")
			raw, err := hex.DecodeString(rowString(row, "data"))
			if err != nil {
				out[id] = unitResult{Err: err}
				continue
			}
			switch rowString(row, "data_type") {
			case "json":
			case "zstd":
				if zerr != nil {
					return nil, fmt.Errorf("zed: %w: zstd is not installed", ErrUnavailable)
				}
				cmd := exec.Command(zstd, "-d", "-c", "-q")
				cmd.Stdin = bytes.NewReader(raw)
				if raw, err = cmd.Output(); err != nil {
					out[id] = unitResult{Err: err}
					continue
				}
			default:
				out[id] = unitResult{Err: fmt.Errorf("zed: thread %s: data type %q", id, rowString(row, "data_type"))}
				continue
			}
			updated, _ := time.Parse(time.RFC3339Nano, rowString(row, "updated_at"))
			s, err := ZedThread(id, updated.UTC(), raw)
			if err != nil {
				out[id] = unitResult{Err: err}
				continue
			}
			s.SourceDigest = hexSum(db + "\x00" + id + "\x00" + string(raw))
			out[id] = unitResult{Sessions: []trace.Session{s}}
		}
		return out, nil
	}
	c := openUnitCache("zed")
	// A thread is rewritten whole with a new updated_at; its type and stored
	// length are checked too. Only changed threads are fetched and
	// decompressed.
	units := storeFingerprints(c, bin, db, `SELECT id, updated_at || ':' || data_type || ':' || length(data) AS fp FROM threads`)
	threads, err := storeRead{Cache: c, Scope: db, Units: units, Read: read}.run()
	if err != nil {
		return nil, st, unreadableStore(&st, err)
	}
	c.save()
	var out []trace.Session
	for _, res := range threads {
		if res.Err != nil {
			st.UnreadableFiles++
			continue
		}
		for _, s := range res.Sessions {
			if len(s.Calls) > 0 && !s.Start.Before(since) {
				out = append(out, s)
			}
		}
	}
	sortSessions(out)
	return out, st, nil
}

// ZedThread decodes one thread document.
func ZedThread(id string, at time.Time, doc []byte) (trace.Session, error) {
	var thread struct {
		Version  string            `json:"version"`
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(doc, &thread); err != nil {
		return trace.Session{}, err
	}
	a := NewAssembler("zed", id)
	a.S.Start = at
	for _, raw := range thread.Messages {
		if jsonString(raw) != "" {
			continue // "Resume" and other control markers
		}
		var m struct {
			User *struct {
				Content []struct {
					Text string `json:"Text"`
				} `json:"content"`
			} `json:"User"`
			Agent *struct {
				Content []struct {
					ToolUse *struct {
						ID    string                     `json:"id"`
						Name  string                     `json:"name"`
						Input map[string]json.RawMessage `json:"input"`
					} `json:"ToolUse"`
				} `json:"content"`
				ToolResults map[string]struct {
					IsError bool `json:"is_error"`
					Content struct {
						Text string `json:"Text"`
					} `json:"content"`
				} `json:"tool_results"`
			} `json:"Agent"`
			Role     string `json:"role"`
			Segments []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"segments"`
		}
		if json.Unmarshal(raw, &m) != nil {
			a.Skip()
			continue
		}
		switch {
		case m.User != nil:
			var b []string
			for _, c := range m.User.Content {
				b = append(b, c.Text)
			}
			a.Add(UserText{Text: strings.Join(b, "\n")})
		case m.Agent != nil:
			for _, c := range m.Agent.Content {
				tu := c.ToolUse
				if tu == nil {
					continue
				}
				call := trace.Call{Session: id, ID: tu.ID, Time: at}
				if tu.Name == "terminal" {
					call.Tool, call.Command = "shell", RawString(tu.Input["command"])
				} else {
					call.Tool, call.Args, call.RawArgs = tu.Name, Flatten(tu.Input), RawKeys(tu.Input)
				}
				a.Add(ToolCall{Key: tu.ID, Call: call})
				if r, ok := m.Agent.ToolResults[tu.ID]; ok {
					a.Add(ToolResult{Key: tu.ID, Nth: -1, Text: r.Content.Text, IsError: r.IsError})
				}
			}
		case m.Role == "user":
			var b []string
			for _, s := range m.Segments {
				if s.Type == "text" {
					b = append(b, s.Text)
				}
			}
			a.Add(UserText{Text: strings.Join(b, "\n")})
		}
	}
	return a.Finish(), nil
}
