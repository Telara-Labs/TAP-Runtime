package history

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// GeminiCLI reads Gemini CLI sessions (TENG-3117):
// <Dir>/<project>/chats/session-*.jsonl, replay logs (ReplayGemini). A
// message is {id, timestamp, type: user|gemini|info|error, content, toolCalls,
// tokens}; each tool call is {id, name, args, result (function response
// parts), status: success|error|cancelled}. MCP tools are named
// mcp_<server>_<tool>, split at the first underscore after the prefix as
// Gemini itself does (gemini-cli parseMcpToolName). Older versions wrote one
// JSON document per session (session-*.json), read the same way.
type GeminiCLI struct{ Dir string }

func (GeminiCLI) Client() string { return "gemini-cli" }

func (r GeminiCLI) Read(since time.Time) ([]trace.Session, error) {
	ss, _, err := r.ReadWithStats(since)
	return ss, err
}

func (r GeminiCLI) ReadWithStats(since time.Time) ([]trace.Session, trace.ReadStats, error) {
	var st trace.ReadStats
	var files []string
	for _, pat := range []string{"session-*.jsonl", "session-*.json"} {
		m, err := filepath.Glob(filepath.Join(r.Dir, "*", "chats", pat))
		if err != nil {
			return nil, st, err
		}
		files = append(files, m...)
	}
	var out []trace.Session
	for _, f := range files {
		if info, err := os.Stat(f); err != nil || info.ModTime().Before(since) {
			continue
		}
		s, err := ReadGeminiFile(f)
		if err != nil {
			st.UnreadableFiles++
			continue
		}
		if len(s.Calls) == 0 || s.Start.Before(since) {
			continue
		}
		s.SourceDigest = FileDigest(f)
		out = append(out, s)
	}
	sortSessions(out)
	return out, st, nil
}

// GeminiMessage is one Gemini CLI message.
type GeminiMessage struct {
	ID        string          `json:"id"`
	Timestamp time.Time       `json:"timestamp"`
	Type      string          `json:"type"`
	Content   json.RawMessage `json:"content"`
	ToolCalls []struct {
		ID        string                     `json:"id"`
		Name      string                     `json:"name"`
		Args      map[string]json.RawMessage `json:"args"`
		Result    json.RawMessage            `json:"result"`
		Status    string                     `json:"status"`
		Timestamp time.Time                  `json:"timestamp"`
		Display   json.RawMessage            `json:"resultDisplay"`
	} `json:"toolCalls"`
	Tokens *struct {
		Input, Output, Cached, Thoughts float64
	} `json:"tokens"`
}

// ReadGeminiFile reads one Gemini CLI session.
func ReadGeminiFile(path string) (s trace.Session, err error) {
	defer func() {
		if r := recover(); r != nil {
			s, err = trace.Session{}, fmt.Errorf("%s: unreadable: %v", path, r)
		}
	}()
	fh, err := os.Open(path)
	if err != nil {
		return trace.Session{}, err
	}
	defer fh.Close()
	var log GeminiLog
	skipped := 0
	if strings.HasSuffix(path, ".json") {
		var doc struct {
			SessionID string            `json:"sessionId"`
			StartTime json.RawMessage   `json:"startTime"`
			Messages  []json.RawMessage `json:"messages"`
		}
		if err := json.NewDecoder(fh).Decode(&doc); err != nil {
			return trace.Session{}, err
		}
		log = GeminiLog{Meta: map[string]json.RawMessage{"startTime": doc.StartTime}, Messages: doc.Messages}
		log.Meta["sessionId"], _ = json.Marshal(doc.SessionID)
	} else if log, skipped, err = ReplayGemini(fh); err != nil {
		return trace.Session{}, err
	}
	id := jsonString(log.Meta["sessionId"])
	if id == "" {
		id = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	a := NewAssembler("gemini-cli", id)
	if t, err := time.Parse(time.RFC3339Nano, jsonString(log.Meta["startTime"])); err == nil {
		a.S.Start = t.UTC()
	}
	for _, raw := range log.Messages {
		var m GeminiMessage
		if json.Unmarshal(raw, &m) != nil {
			a.Skip()
			continue
		}
		FirstTime(&a.S, m.Timestamp)
		for _, e := range GeminiEvents(m, id) {
			a.Add(e)
		}
	}
	s = a.Finish()
	s.Skipped += skipped
	return s, nil
}

// GeminiEvents decodes one message.
func GeminiEvents(m GeminiMessage, session string) []Event {
	var out []Event
	switch m.Type {
	case "user":
		out = append(out, UserText{Text: geminiText(m.Content)})
	case "gemini":
		turn := ""
		if m.Tokens != nil && len(m.ToolCalls) > 0 {
			turn = "msg:" + m.ID
			fresh := m.Tokens.Input - m.Tokens.Cached
			if fresh < 0 {
				fresh = 0
			}
			out = append(out, TurnUsage{Turn: turn, Usage: trace.Usage{Fresh: fresh, Cached: m.Tokens.Cached, Output: m.Tokens.Output + m.Tokens.Thoughts}})
		}
		for _, tc := range m.ToolCalls {
			at := tc.Timestamp
			if at.IsZero() {
				at = m.Timestamp
			}
			c := trace.Call{Session: session, ID: tc.ID, Time: at}
			GeminiTool(&c, tc.Name, tc.Args)
			out = append(out, ToolCall{Key: tc.ID, Turn: turn, Call: c})
			r := ToolResult{Key: tc.ID, Nth: -1, Text: geminiResult(tc.Result, tc.Display)}
			switch tc.Status {
			case "success":
			case "error", "cancelled":
				r.IsError = true
			default: // still running when written
				r.HasOutcome = true
			}
			out = append(out, r)
		}
	}
	return out
}

// GeminiTool decodes Gemini CLI's tool names: run_shell_command is the
// shell, mcp_<server>_<tool> an MCP tool.
func GeminiTool(c *trace.Call, name string, args map[string]json.RawMessage) {
	switch {
	case name == "run_shell_command":
		c.Tool, c.Command = "shell", RawString(args["command"])
	case strings.HasPrefix(name, "mcp_"):
		if server, tool, ok := strings.Cut(strings.TrimPrefix(name, "mcp_"), "_"); ok && server != "" && tool != "" {
			MCPCall(c, server, tool, args)
			return
		}
		fallthrough
	default:
		c.Tool, c.Args, c.RawArgs = name, Flatten(args), RawKeys(args)
	}
}

// geminiText is a message's text: a string, or its text parts.
func geminiText(raw json.RawMessage) string {
	if s := jsonString(raw); s != "" {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &parts)
	var b []string
	for _, p := range parts {
		if p.Text != "" {
			b = append(b, p.Text)
		}
	}
	return strings.Join(b, "\n")
}

// geminiResult is a tool result's text: the function response's output (or
// error), else what was displayed.
func geminiResult(result, display json.RawMessage) string {
	var parts []struct {
		FunctionResponse *struct {
			Response map[string]json.RawMessage `json:"response"`
		} `json:"functionResponse"`
		Text string `json:"text"`
	}
	if json.Unmarshal(result, &parts) == nil {
		var b []string
		for _, p := range parts {
			switch {
			case p.FunctionResponse != nil:
				for _, k := range []string{"output", "error", "llmContent"} {
					if v, ok := p.FunctionResponse.Response[k]; ok {
						b = append(b, ResultText(v))
						break
					}
				}
			case p.Text != "":
				b = append(b, p.Text)
			}
		}
		if len(b) > 0 {
			return strings.Join(b, "\n")
		}
	}
	return jsonString(display)
}

// sortSessions orders sessions by start, then id.
func sortSessions(out []trace.Session) {
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Start.Equal(out[j].Start) {
			return out[i].Start.Before(out[j].Start)
		}
		return out[i].ID < out[j].ID
	})
}
