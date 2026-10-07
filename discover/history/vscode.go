package history

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

// VSCodeCopilot reads GitHub Copilot Chat sessions kept by VS Code
// : User/workspaceStorage/<ws>/chatSessions/<id>.jsonl (a replay
// log, ReplayVSCode; <id>.json before VS Code 1.109) and
// User/globalStorage/emptyWindowChatSessions/. The session state is
// {sessionId, creationDate, requests: [{timestamp, message: {text},
// response: [parts]}]}. A tool call is a response part of kind
// toolInvocationSerialized {toolCallId, toolId, source, resultDetails,
// resultError, isComplete, toolSpecificData}: source {type: "mcp", label}
// names the MCP server as configured, resultDetails {input, output, isError}
// holds the arguments (input, JSON text) and the result. VS Code names an MCP
// tool mcp_<server prefix>_<tool> (its McpToolNames prefixer); the prefix is
// rebuilt from the label to recover the tool's own name.
type VSCodeCopilot struct{ User string } // the VS Code User folder

func (VSCodeCopilot) Client() string { return "vscode-copilot" }

func (r VSCodeCopilot) Read(since time.Time) ([]trace.Session, error) {
	ss, _, err := r.ReadWithStats(since)
	return ss, err
}

func (r VSCodeCopilot) ReadWithStats(since time.Time) ([]trace.Session, trace.ReadStats, error) {
	var st trace.ReadStats
	var files []string
	for _, pat := range []string{
		filepath.Join(r.User, "workspaceStorage", "*", "chatSessions", "*.json*"),
		filepath.Join(r.User, "globalStorage", "emptyWindowChatSessions", "*.json*"),
	} {
		m, err := filepath.Glob(pat)
		if err != nil {
			return nil, st, err
		}
		files = append(files, m...)
	}
	var sessions []string
	for _, f := range files {
		if strings.HasSuffix(f, ".json") || strings.HasSuffix(f, ".jsonl") {
			sessions = append(sessions, f)
		}
	}
	var out []trace.Session
	seen := map[string]bool{}
	for _, r := range ParseFiles(changedSince(sessions, since), "vscode-copilot", nil, parseVSCode) {
		if r.Err != nil {
			st.UnreadableFiles++
			continue
		}
		s := r.Session
		if len(s.Calls) == 0 || s.Start.Before(since) || seen[s.ID] {
			continue // a session kept as both .json and .jsonl is read once
		}
		seen[s.ID] = true
		out = append(out, s)
	}
	sortSessions(out)
	return out, st, nil
}

// ReadVSCodeFile reads one chat session file.
func ReadVSCodeFile(path string) (trace.Session, error) { return ParseFile(path, parseVSCode) }

func parseVSCode(path string, fh io.Reader) (s trace.Session, err error) {
	defer func() {
		if r := recover(); r != nil {
			s, err = trace.Session{}, fmt.Errorf("%s: unreadable: %v", path, r)
		}
	}()
	var state any
	skipped := 0
	if strings.HasSuffix(path, ".jsonl") {
		if state, skipped, err = ReplayVSCode(fh); err != nil {
			return trace.Session{}, err
		}
	} else if err := json.NewDecoder(fh).Decode(&state); err != nil {
		return trace.Session{}, err
	}
	b, _ := json.Marshal(state)
	var doc VSCodeSession
	if err := json.Unmarshal(b, &doc); err != nil {
		return trace.Session{}, err
	}
	id := doc.SessionID
	if id == "" {
		id = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	a := NewAssembler("vscode-copilot", id)
	if doc.CreationDate > 0 {
		a.S.Start = time.UnixMilli(doc.CreationDate).UTC()
	}
	for _, req := range doc.Requests {
		for _, e := range VSCodeEvents(req, id) {
			a.Add(e)
		}
	}
	s = a.Finish()
	s.Skipped += skipped
	return s, nil
}

// VSCodeSession is the replayed chat state (the fields read here).
type VSCodeSession struct {
	SessionID    string          `json:"sessionId"`
	CreationDate int64           `json:"creationDate"`
	Requests     []VSCodeRequest `json:"requests"`
}

// VSCodeRequest is one turn: the person's message and the response parts.
type VSCodeRequest struct {
	Timestamp int64 `json:"timestamp"`
	Message   struct {
		Text string `json:"text"`
	} `json:"message"`
	Response []json.RawMessage `json:"response"`
}

type vscodeToolPart struct {
	Kind       string `json:"kind"`
	ToolCallID string `json:"toolCallId"`
	ToolID     string `json:"toolId"`
	IsComplete *bool  `json:"isComplete"`
	Source     struct {
		Type  string `json:"type"`
		Label string `json:"label"`
	} `json:"source"`
	ResultError   json.RawMessage `json:"resultError"`
	ResultDetails *struct {
		Input   string `json:"input"`
		IsError bool   `json:"isError"`
		Output  []struct {
			IsText bool   `json:"isText"`
			Value  string `json:"value"`
		} `json:"output"`
	} `json:"resultDetails"`
	ToolSpecificData *struct {
		Kind        string `json:"kind"`
		Command     string `json:"command"`
		CommandLine *struct {
			Original string `json:"original"`
		} `json:"commandLine"`
	} `json:"toolSpecificData"`
}

// VSCodeEvents decodes one request: the person's message, then each tool
// invocation with its result.
func VSCodeEvents(req VSCodeRequest, session string) []Event {
	out := []Event{UserText{Text: req.Message.Text}}
	at := time.UnixMilli(req.Timestamp).UTC()
	for _, raw := range req.Response {
		var p vscodeToolPart
		if json.Unmarshal(raw, &p) != nil || p.Kind != "toolInvocationSerialized" || p.ToolCallID == "" {
			continue
		}
		c := trace.Call{Session: session, ID: p.ToolCallID, Time: at}
		var args map[string]json.RawMessage
		if p.ResultDetails != nil && p.ResultDetails.Input != "" {
			_ = json.Unmarshal([]byte(p.ResultDetails.Input), &args)
		}
		switch {
		case p.ToolSpecificData != nil && p.ToolSpecificData.Kind == "terminal":
			c.Tool, c.Command = "shell", p.ToolSpecificData.Command
			if cl := p.ToolSpecificData.CommandLine; cl != nil && cl.Original != "" {
				c.Command = cl.Original
			}
		case p.Source.Type == "mcp" && p.Source.Label != "":
			MCPCall(&c, p.Source.Label, VSCodeMCPToolName(p.ToolID, p.Source.Label), args)
		default:
			c.Tool, c.Args, c.RawArgs = p.ToolID, Flatten(args), RawKeys(args)
		}
		out = append(out, ToolCall{Key: p.ToolCallID, Call: c})
		r := ToolResult{Key: p.ToolCallID, Nth: -1}
		if p.ResultDetails != nil {
			var b []string
			for _, o := range p.ResultDetails.Output {
				if o.IsText {
					b = append(b, o.Value)
				}
			}
			r.Text, r.IsError = strings.Join(b, "\n"), p.ResultDetails.IsError
		}
		if len(p.ResultError) > 0 && string(p.ResultError) != "null" && string(p.ResultError) != "false" {
			r.IsError = true
			if r.Text == "" {
				r.Text = ResultText(p.ResultError)
			}
		}
		if p.IsComplete != nil && !*p.IsComplete {
			r.HasOutcome = true // never finished
		}
		out = append(out, r)
	}
	return out
}

var vscodeToolPrefix = regexp.MustCompile(`[^a-z0-9_.-]+`)

// VSCodeMCPToolName recovers an MCP tool's own name from VS Code's tool id:
// "mcp_" + the server label lower-cased, cleaned and cut to 13 characters
// (less a disambiguating number), then "_" and the tool name.
func VSCodeMCPToolName(toolID, label string) string {
	base := vscodeToolPrefix.ReplaceAllString(strings.ToLower(label), "_")
	for n := 1; n <= 9; n++ {
		suffix := "_"
		if n > 1 {
			suffix = itoa(n) + "_"
		}
		keep := 18 - len("mcp_") - len(suffix)
		p := base
		if len(p) > keep {
			p = p[:keep]
		}
		if prefix := "mcp_" + p + suffix; strings.HasPrefix(toolID, prefix) {
			return strings.TrimPrefix(toolID, prefix)
		}
	}
	return toolID
}
