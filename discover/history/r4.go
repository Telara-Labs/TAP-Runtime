package history

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
	"github.com/Telara-Labs/TAP-Runtime/discover/util"
)

// R4 readers: agents that keep sessions in a SQLite database
// (OpenCode and the Kilo CLI built on it, Goose, Crush) or one JSON document
// per session (Continue). Each was read from a real run of the agent on
// 2026-10-02 (testdata/<agent>), and each is read through the system sqlite3
// read-only, WAL included (util.SQLiteURI), as the Cursor readers are.

// errStoreSchema marks a store without the tables or columns a reader
// queries: a store the agent has not created yet, or another version's. The
// reader counts it unreadable; it does not fail the run.
var errStoreSchema = errors.New("the store does not have the expected tables")

// sqliteRows runs one query against a store, read-only.
func sqliteRows(bin, db, sql string) ([]map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), util.SQLiteReadTimeout)
	defer cancel()
	out, err := util.SQLiteQuery(ctx, bin, db, sql)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "no such table") || strings.Contains(msg, "no such column") {
			return nil, fmt.Errorf("%w: %s", errStoreSchema, msg)
		}
		return nil, err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil
	}
	var rows []map[string]any
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	return rows, dec.Decode(&rows)
}

// unreadableStore counts a store without the expected schema as unreadable
// and drops the error; any other error is returned.
func unreadableStore(st *trace.ReadStats, err error) error {
	if errors.Is(err, errStoreSchema) {
		st.UnreadableFiles++
		return nil
	}
	return err
}

func sqliteBin(client string) (string, error) {
	p, err := util.SQLiteBin()
	if err != nil {
		return "", fmt.Errorf("%s: %w: sqlite3 is not installed", client, ErrUnavailable)
	}
	return p, nil
}

func rowString(r map[string]any, k string) string {
	switch v := r[k].(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	}
	return ""
}

func rowInt(r map[string]any, k string) int64 {
	if n, ok := r[k].(json.Number); ok {
		i, _ := n.Int64()
		return i
	}
	return 0
}

// --- OpenCode and the Kilo CLI -------------------------------------------

// OpenCodeDB reads an OpenCode-schema database: session, message (data:
// {role, time}) and part (data: text | tool {tool, callID, state {status,
// input, output, error}} | step-start | step-finish {tokens}). A step's
// tokens belong to the tool parts in that step. OpenCode names an MCP tool
// <server>_<tool>; the server names come from the agent's configuration
// (the mcp object of its JSON config files), so the split is never a guess.
type OpenCodeDB struct {
	ID      string   // the registry client: "opencode" or "kilo"
	DB      string   // opencode.db / kilo.db
	Configs []string // config files that name the MCP servers
}

func (r OpenCodeDB) Client() string { return r.ID }

func (r OpenCodeDB) Read(since time.Time) ([]trace.Session, error) {
	ss, _, err := r.ReadWithStats(since)
	return ss, err
}

// openCodeServers lists the MCP server names configured for the agent.
func openCodeServers(configs []string) []string {
	var out []string
	for _, f := range configs {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var c struct {
			MCP map[string]json.RawMessage `json:"mcp"`
		}
		if json.Unmarshal(b, &c) == nil {
			for k := range c.MCP {
				out = append(out, k)
			}
		}
	}
	// Longest first, so "jira_cloud" wins over "jira".
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

func (r OpenCodeDB) ReadWithStats(since time.Time) ([]trace.Session, trace.ReadStats, error) {
	var st trace.ReadStats
	if _, err := os.Stat(r.DB); err != nil {
		return nil, st, nil
	}
	bin, err := sqliteBin(r.ID)
	if err != nil {
		return nil, st, err
	}
	sessions, err := sqliteRows(bin, r.DB, `SELECT id, time_created FROM session`)
	if err != nil {
		return nil, st, unreadableStore(&st, err)
	}
	msgs, err := sqliteRows(bin, r.DB, `SELECT id, session_id, time_created, data FROM message ORDER BY time_created, id`)
	if err != nil {
		return nil, st, unreadableStore(&st, err)
	}
	parts, err := sqliteRows(bin, r.DB, `SELECT id, message_id, session_id, time_created, data FROM part ORDER BY time_created, id`)
	if err != nil {
		return nil, st, unreadableStore(&st, err)
	}
	servers := openCodeServers(r.Configs)
	role := map[string]string{}
	for _, m := range msgs {
		var d struct{ Role string }
		if json.Unmarshal([]byte(rowString(m, "data")), &d) == nil {
			role[rowString(m, "id")] = d.Role
		}
	}
	bySession := map[string][]map[string]any{}
	for _, p := range parts {
		bySession[rowString(p, "session_id")] = append(bySession[rowString(p, "session_id")], p)
	}
	var out []trace.Session
	for _, sr := range sessions {
		id := rowString(sr, "id")
		a := NewAssembler(r.ID, id)
		a.S.Start = time.UnixMilli(rowInt(sr, "time_created")).UTC()
		if a.S.Start.Before(since) {
			continue
		}
		step := 0
		for _, p := range bySession[id] {
			var d struct {
				Type   string `json:"type"`
				Text   string `json:"text"`
				Tool   string `json:"tool"`
				CallID string `json:"callID"`
				State  struct {
					Status string                     `json:"status"`
					Input  map[string]json.RawMessage `json:"input"`
					Output json.RawMessage            `json:"output"`
					Error  json.RawMessage            `json:"error"`
				} `json:"state"`
				Tokens *struct {
					Input, Output, Reasoning float64
					Cache                    struct{ Read, Write float64 }
				} `json:"tokens"`
			}
			if json.Unmarshal([]byte(rowString(p, "data")), &d) != nil {
				a.Skip()
				continue
			}
			at := time.UnixMilli(rowInt(p, "time_created")).UTC()
			switch d.Type {
			case "text":
				if role[rowString(p, "message_id")] == "user" {
					a.Add(UserText{Text: openCodeUserText(d.Text)})
				}
			case "step-start":
				step++
			case "step-finish":
				if t := d.Tokens; t != nil {
					a.Add(TurnUsage{Turn: fmt.Sprintf("step%d", step), Usage: trace.Usage{
						Fresh: t.Input + t.Cache.Write, Cached: t.Cache.Read, Output: t.Output + t.Reasoning}})
				}
			case "tool":
				c := trace.Call{Session: id, ID: d.CallID, Time: at}
				OpenCodeTool(&c, d.Tool, d.State.Input, servers)
				a.Add(ToolCall{Key: d.CallID, Turn: fmt.Sprintf("step%d", step), Call: c})
				res := ToolResult{Key: d.CallID, Nth: -1, Text: ResultText(d.State.Output)}
				switch d.State.Status {
				case "completed":
				case "error":
					res.IsError, res.Text = true, ResultText(d.State.Error)
				default:
					res.HasOutcome = true // pending or running when written
				}
				a.Add(res)
			}
		}
		s := a.Finish()
		if len(s.Calls) > 0 {
			s.SourceDigest = digestOf(r.DB, id)
			out = append(out, s)
		}
	}
	sortSessions(out)
	return out, st, nil
}

// openCodeUserText unquotes a prompt OpenCode stored as a JSON string.
func openCodeUserText(t string) string {
	var s string
	if strings.HasPrefix(t, `"`) && json.Unmarshal([]byte(t), &s) == nil {
		return s
	}
	return t
}

// OpenCodeTool decodes OpenCode's tool names: bash is the shell; a name
// starting with a configured MCP server's name and "_" is that server's tool.
func OpenCodeTool(c *trace.Call, name string, input map[string]json.RawMessage, servers []string) {
	if name == "bash" {
		c.Tool, c.Command = "shell", RawString(input["command"])
		return
	}
	for _, s := range servers {
		if tool, ok := strings.CutPrefix(name, s+"_"); ok && tool != "" {
			MCPCall(c, s, tool, input)
			return
		}
	}
	c.Tool, c.Args, c.RawArgs = name, Flatten(input), RawKeys(input)
}

// digestOf names one session of a shared store for frozen corpora.
func digestOf(store, session string) string {
	return hexSum(store + "\x00" + session + "\x00" + FileDigest(store))
}

// --- Goose ---------------------------------------------------------------

// Goose reads Goose sessions: <Dir>/sessions.db, table messages (role,
// content_json, created_timestamp in seconds) with toolRequest {id,
// toolCall {value {name, arguments}}, _meta.goose_extension} and
// toolResponse {id, toolResult {status, value {content, isError}}}. A tool is
// named <extension>__<tool>; the developer extension's shell is the shell.
// Token use is per request in usage_ledger.
type Goose struct{ Dir string }

func (Goose) Client() string { return "goose" }

func (r Goose) Read(since time.Time) ([]trace.Session, error) {
	ss, _, err := r.ReadWithStats(since)
	return ss, err
}

func (r Goose) ReadWithStats(since time.Time) ([]trace.Session, trace.ReadStats, error) {
	var st trace.ReadStats
	db := filepath.Join(r.Dir, "sessions.db")
	if _, err := os.Stat(db); err != nil {
		return nil, st, nil
	}
	bin, err := sqliteBin("goose")
	if err != nil {
		return nil, st, err
	}
	rows, err := sqliteRows(bin, db, `SELECT session_id, role, content_json, created_timestamp FROM messages ORDER BY session_id, id`)
	if err != nil {
		return nil, st, unreadableStore(&st, err)
	}
	bySession := map[string]*Assembler{}
	var order []string
	for _, row := range rows {
		sid := rowString(row, "session_id")
		a := bySession[sid]
		if a == nil {
			a = NewAssembler("goose", sid)
			bySession[sid] = a
			order = append(order, sid)
		}
		at := time.Unix(rowInt(row, "created_timestamp"), 0).UTC()
		FirstTime(&a.S, at)
		var content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ID       string `json:"id"`
			ToolCall *struct {
				Value struct {
					Name      string                     `json:"name"`
					Arguments map[string]json.RawMessage `json:"arguments"`
				} `json:"value"`
			} `json:"toolCall"`
			ToolResult *struct {
				Status string `json:"status"`
				Value  struct {
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
					IsError bool `json:"isError"`
				} `json:"value"`
				Error string `json:"error"`
			} `json:"toolResult"`
			Meta map[string]json.RawMessage `json:"_meta"`
		}
		if json.Unmarshal([]byte(rowString(row, "content_json")), &content) != nil {
			a.Skip()
			continue
		}
		for _, p := range content {
			switch {
			case p.Type == "text" && rowString(row, "role") == "user":
				a.Add(UserText{Text: p.Text})
			case p.Type == "toolRequest" && p.ToolCall != nil:
				c := trace.Call{Session: sid, ID: p.ID, Time: at}
				GooseTool(&c, p.ToolCall.Value.Name, p.ToolCall.Value.Arguments, jsonString(p.Meta["goose_extension"]))
				a.Add(ToolCall{Key: p.ID, Call: c})
			case p.Type == "toolResponse" && p.ToolResult != nil:
				var b []string
				for _, c := range p.ToolResult.Value.Content {
					b = append(b, c.Text)
				}
				text := strings.Join(b, "\n")
				if text == "" {
					text = p.ToolResult.Error
				}
				a.Add(ToolResult{Key: p.ID, Nth: -1, Text: text, IsError: p.ToolResult.Status != "success" || p.ToolResult.Value.IsError})
			}
		}
	}
	var out []trace.Session
	for _, sid := range order {
		s := bySession[sid].Finish()
		if len(s.Calls) == 0 || s.Start.Before(since) {
			continue
		}
		s.SourceDigest = digestOf(db, sid)
		out = append(out, s)
	}
	sortSessions(out)
	return out, st, nil
}

// GooseTool decodes Goose's <extension>__<tool> names: the extension is the
// MCP server, except the built-in developer extension, whose shell tool is
// the shell and whose other tools are built-ins.
func GooseTool(c *trace.Call, name string, args map[string]json.RawMessage, extension string) {
	ext, tool, ok := strings.Cut(name, "__")
	if !ok {
		ext, tool = extension, name
	}
	switch {
	case tool == "shell" && (ext == "developer" || ext == ""):
		c.Tool, c.Command = "shell", RawString(args["command"])
	case ext == "" || ext == "developer" || ext == "extensionmanager":
		c.Tool, c.Args, c.RawArgs = tool, Flatten(args), RawKeys(args)
	default:
		MCPCall(c, ext, tool, args)
	}
}

// --- Crush ---------------------------------------------------------------

// Crush reads Crush sessions: one database per project,
// <project>/.crush/crush.db, the projects listed in <Dir>/projects.json.
// Table messages (role user|assistant|tool, parts JSON: text {text},
// tool_call {id, name, input}, tool_result {tool_call_id, content,
// is_error}). Crush names an MCP tool mcp_<server>_<tool>; server names come
// from crush.json, so the split is never a guess; bash is the shell.
type Crush struct {
	Dir     string   // ~/.local/share/crush, holding projects.json
	Configs []string // crush.json files that name the MCP servers
}

func (Crush) Client() string { return "crush" }

func (r Crush) Read(since time.Time) ([]trace.Session, error) {
	ss, _, err := r.ReadWithStats(since)
	return ss, err
}

func (r Crush) ReadWithStats(since time.Time) ([]trace.Session, trace.ReadStats, error) {
	var st trace.ReadStats
	b, err := os.ReadFile(filepath.Join(r.Dir, "projects.json"))
	if err != nil {
		return nil, st, nil
	}
	var pj struct {
		Projects []struct {
			Path    string `json:"path"`
			DataDir string `json:"data_dir"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(b, &pj); err != nil {
		return nil, st, err
	}
	var servers []string
	for _, f := range r.Configs {
		if b, err := os.ReadFile(f); err == nil {
			var c struct {
				MCP map[string]json.RawMessage `json:"mcp"`
			}
			if json.Unmarshal(b, &c) == nil {
				for k := range c.MCP {
					servers = append(servers, k)
				}
			}
		}
	}
	sort.Slice(servers, func(i, j int) bool { return len(servers[i]) > len(servers[j]) })
	var out []trace.Session
	for _, p := range pj.Projects {
		dir := p.DataDir
		if dir == "" {
			dir = filepath.Join(p.Path, ".crush")
		}
		db := filepath.Join(dir, "crush.db")
		if _, err := os.Stat(db); err != nil {
			continue
		}
		ss, err := readCrushDB(db, servers, since)
		if err != nil {
			st.UnreadableFiles++
			continue
		}
		out = append(out, ss...)
	}
	sortSessions(out)
	return out, st, nil
}

func readCrushDB(db string, servers []string, since time.Time) ([]trace.Session, error) {
	bin, err := sqliteBin("crush")
	if err != nil {
		return nil, err
	}
	rows, err := sqliteRows(bin, db, `SELECT session_id, role, parts, created_at FROM messages ORDER BY session_id, created_at, rowid`)
	if err != nil {
		return nil, err
	}
	bySession := map[string]*Assembler{}
	var order []string
	for _, row := range rows {
		sid := rowString(row, "session_id")
		a := bySession[sid]
		if a == nil {
			a = NewAssembler("crush", sid)
			bySession[sid] = a
			order = append(order, sid)
		}
		at := time.Unix(rowInt(row, "created_at"), 0).UTC()
		FirstTime(&a.S, at)
		var parts []struct {
			Type string `json:"type"`
			Data struct {
				Text       string `json:"text"`
				ID         string `json:"id"`
				Name       string `json:"name"`
				Input      string `json:"input"`
				ToolCallID string `json:"tool_call_id"`
				Content    string `json:"content"`
				IsError    bool   `json:"is_error"`
			} `json:"data"`
		}
		if json.Unmarshal([]byte(rowString(row, "parts")), &parts) != nil {
			a.Skip()
			continue
		}
		for _, p := range parts {
			switch p.Type {
			case "text":
				if rowString(row, "role") == "user" {
					a.Add(UserText{Text: p.Data.Text})
				}
			case "tool_call":
				var args map[string]json.RawMessage
				_ = json.Unmarshal([]byte(p.Data.Input), &args)
				c := trace.Call{Session: sid, ID: p.Data.ID, Time: at}
				CrushTool(&c, p.Data.Name, args, servers)
				a.Add(ToolCall{Key: p.Data.ID, Call: c})
			case "tool_result":
				a.Add(ToolResult{Key: p.Data.ToolCallID, Nth: -1, Text: p.Data.Content, IsError: p.Data.IsError})
			}
		}
	}
	var out []trace.Session
	for _, sid := range order {
		s := bySession[sid].Finish()
		if len(s.Calls) == 0 || s.Start.Before(since) {
			continue
		}
		s.SourceDigest = digestOf(db, sid)
		out = append(out, s)
	}
	return out, nil
}

// CrushTool decodes Crush's tool names: bash is the shell; mcp_<server>_<tool>
// with a configured server is that server's tool.
func CrushTool(c *trace.Call, name string, args map[string]json.RawMessage, servers []string) {
	if name == "bash" {
		c.Tool, c.Command = "shell", RawString(args["command"])
		return
	}
	if rest, ok := strings.CutPrefix(name, "mcp_"); ok {
		for _, s := range servers {
			if tool, ok := strings.CutPrefix(rest, s+"_"); ok && tool != "" {
				MCPCall(c, s, tool, args)
				return
			}
		}
	}
	c.Tool, c.Args, c.RawArgs = name, Flatten(args), RawKeys(args)
}

// --- Continue ------------------------------------------------------------

// Continue reads Continue CLI sessions: <Dir>/<id>.json {sessionId, history:
// [{message {role, content, toolCalls, usage}, toolCallStates [{toolCallId,
// toolCall {function {name}}, parsedArgs, status, output}]}]}. Continue
// records MCP tools by their bare name, with no server; the server is set
// only when the configuration names exactly one MCP server. Bash is the
// shell. Continue records no error flag: a failure shows only in the text.
type Continue struct {
	Dir     string   // ~/.continue/sessions
	Configs []string // config.yaml files naming MCP servers
}

func (Continue) Client() string { return "continue" }

func (r Continue) Read(since time.Time) ([]trace.Session, error) {
	ss, _, err := r.ReadWithStats(since)
	return ss, err
}

// continueServers lists the "- name:" entries under mcpServers in
// config.yaml files.
func continueServers(configs []string) []string {
	var out []string
	for _, f := range configs {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		in := false
		for _, line := range strings.Split(string(b), "\n") {
			t := strings.TrimSpace(line)
			switch {
			case len(line) > 0 && line[0] != ' ' && line[0] != '-':
				in = strings.HasPrefix(t, "mcpServers:")
			case in && strings.HasPrefix(t, "- name:"):
				out = append(out, strings.Trim(strings.TrimSpace(strings.TrimPrefix(t, "- name:")), `"'`))
			}
		}
	}
	return out
}

func (r Continue) ReadWithStats(since time.Time) ([]trace.Session, trace.ReadStats, error) {
	var st trace.ReadStats
	files, _ := filepath.Glob(filepath.Join(r.Dir, "*.json"))
	server := ""
	if s := continueServers(r.Configs); len(s) == 1 {
		server = s[0]
	}
	var out []trace.Session
	for _, f := range files {
		if filepath.Base(f) == "sessions.json" {
			continue // the index, not a session
		}
		info, err := os.Stat(f)
		if err != nil || info.ModTime().Before(since) {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			st.UnreadableFiles++
			continue
		}
		var doc struct {
			SessionID string `json:"sessionId"`
			History   []struct {
				Message struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
					Usage   *struct {
						Prompt     float64 `json:"prompt_tokens"`
						Completion float64 `json:"completion_tokens"`
						Details    struct {
							Cached float64 `json:"cached_tokens"`
						} `json:"prompt_tokens_details"`
					} `json:"usage"`
				} `json:"message"`
				ToolCallStates []struct {
					ToolCallID string `json:"toolCallId"`
					ToolCall   struct {
						Function struct {
							Name string `json:"name"`
						} `json:"function"`
					} `json:"toolCall"`
					ParsedArgs map[string]json.RawMessage `json:"parsedArgs"`
					Status     string                     `json:"status"`
					Output     json.RawMessage            `json:"output"`
				} `json:"toolCallStates"`
			} `json:"history"`
		}
		if json.Unmarshal(b, &doc) != nil {
			st.UnreadableFiles++
			continue
		}
		id := doc.SessionID
		if id == "" {
			id = strings.TrimSuffix(filepath.Base(f), ".json")
		}
		a := NewAssembler("continue", id)
		a.S.Start = info.ModTime().UTC() // the session records no time of its own
		for i, h := range doc.History {
			m := h.Message
			if m.Role == "user" {
				a.Add(UserText{Text: geminiText(m.Content)})
			}
			turn := ""
			if u := m.Usage; u != nil && len(h.ToolCallStates) > 0 {
				turn = fmt.Sprintf("h%d", i)
				a.Add(TurnUsage{Turn: turn, Usage: trace.Usage{Fresh: u.Prompt - u.Details.Cached, Cached: u.Details.Cached, Output: u.Completion}})
			}
			for _, tc := range h.ToolCallStates {
				c := trace.Call{Session: id, ID: tc.ToolCallID, Time: a.S.Start}
				switch name := tc.ToolCall.Function.Name; {
				case name == "Bash":
					c.Tool, c.Command = "shell", RawString(tc.ParsedArgs["command"])
				case server != "" && !continueBuiltin(name):
					MCPCall(&c, server, name, tc.ParsedArgs)
				default:
					c.Tool, c.Args, c.RawArgs = name, Flatten(tc.ParsedArgs), RawKeys(tc.ParsedArgs)
				}
				a.Add(ToolCall{Key: tc.ToolCallID, Turn: turn, Call: c})
				a.Add(ToolResult{Key: tc.ToolCallID, Nth: -1, Text: continueOutput(tc.Output), IsError: tc.Status == "errored"})
			}
		}
		s := a.Finish()
		if len(s.Calls) == 0 {
			continue
		}
		s.SourceDigest = FileDigest(f)
		out = append(out, s)
	}
	sortSessions(out)
	return out, st, nil
}

// continueOutput is a tool's result: Continue stores it as context items
// [{name, description, content}], where an MCP tool's content is the
// server's own content array as a string.
func continueOutput(raw json.RawMessage) string {
	var items []struct {
		Content *string `json:"content"`
	}
	if json.Unmarshal(raw, &items) != nil || len(items) == 0 {
		return ResultText(raw)
	}
	var b []string
	for _, it := range items {
		if it.Content == nil {
			return ResultText(raw)
		}
		c := *it.Content
		if strings.HasPrefix(strings.TrimSpace(c), "[") {
			if t := geminiText(json.RawMessage(c)); t != "" {
				c = t
			}
		}
		b = append(b, c)
	}
	return strings.Join(b, "\n")
}

// continueBuiltin reports a Continue built-in tool: they are named in
// PascalCase (Bash, Read, Edit, Write, ...), MCP tools by their server.
func continueBuiltin(name string) bool {
	return name != "" && name[0] >= 'A' && name[0] <= 'Z'
}

func hexSum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
