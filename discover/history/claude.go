package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

// ClaudeCode reads Claude Code transcripts: <Dir>/<project>/<session>.jsonl.
// Claude Code deletes transcripts older than its cleanupPeriodDays setting
// (30 by default), so this history is short unless the user raised it.
type ClaudeCode struct{ Dir string }

func (ClaudeCode) Client() string { return "claude-code" }

func (r ClaudeCode) Read(since time.Time) ([]trace.Session, error) {
	ss, _, err := r.ReadWithStats(since)
	return ss, err
}

func (r ClaudeCode) ReadWithStats(since time.Time) ([]trace.Session, trace.ReadStats, error) {
	return r.read(since, nil)
}

// ReadProgress is Read, reporting each session file read.
func (r ClaudeCode) ReadProgress(since time.Time, p trace.Progress) ([]trace.Session, error) {
	ss, _, err := r.read(since, p)
	return ss, err
}

func (r ClaudeCode) read(since time.Time, p trace.Progress) ([]trace.Session, trace.ReadStats, error) {
	var st trace.ReadStats
	files, err := filepath.Glob(filepath.Join(r.Dir, "*", "*.jsonl"))
	if err != nil {
		return nil, st, err
	}
	var out []trace.Session
	files = changedSince(files, since)
	for i, f := range files {
		if p != nil {
			p(i, len(files))
		}
		s, err := ReadClaudeFile(f)
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
	if p != nil {
		p(len(files), len(files))
	}
	return out, st, nil
}

type ClaudeLine struct {
	Type      string    `json:"type"`
	IsMeta    bool      `json:"isMeta"`
	SessionID string    `json:"sessionId"`
	Timestamp time.Time `json:"timestamp"`
	Message   struct {
		ID      string          `json:"id"`
		Content json.RawMessage `json:"content"`
		Usage   *struct {
			Input       float64 `json:"input_tokens"`
			CacheCreate float64 `json:"cache_creation_input_tokens"`
			CacheRead   float64 `json:"cache_read_input_tokens"`
			Output      float64 `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

type ClaudeBlock struct {
	Type  string                     `json:"type"`
	ID    string                     `json:"id"`
	Name  string                     `json:"name"`
	Input map[string]json.RawMessage `json:"input"`
}

func ReadClaudeFile(path string) (s trace.Session, err error) {
	// One file the parser cannot follow is skipped, not the whole run.
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
	a := NewAssembler("claude-code", strings.TrimSuffix(filepath.Base(path), ".jsonl"))
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var ln ClaudeLine
		if json.Unmarshal(sc.Bytes(), &ln) != nil {
			a.Skip()
			continue
		}
		FirstTime(&a.S, ln.Timestamp)
		for _, e := range ClaudeEvents(ln, a.S.ID) {
			a.Add(e)
		}
	}
	return a.Finish(), sc.Err()
}

// ClaudeEvents decodes one transcript line. One model response is written as
// several lines (one per content block) that each repeat its usage; the
// assembler counts it once, split over the tool calls it made.
func ClaudeEvents(ln ClaudeLine, session string) []Event {
	var out []Event
	if ln.Type == "user" && !ln.IsMeta {
		if text := ClaudeUserText(ln.Message.Content); text != "" {
			role := "user"
			if IsClaudeContinuationSummary(text) {
				role = "synthetic_context"
			}
			out = append(out, UserText{Text: text, Role: role})
		}
		// Results of earlier tool calls: whether they failed, and the
		// identifiers they returned.
		var results []struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id"`
			IsError   bool            `json:"is_error"`
			Content   json.RawMessage `json:"content"`
		}
		if json.Unmarshal(ln.Message.Content, &results) == nil {
			for _, r := range results {
				if r.Type == "tool_result" && r.ToolUseID != "" {
					out = append(out, ToolResult{Key: r.ToolUseID, Nth: -1, Text: ClaudeUserText(r.Content), IsError: r.IsError})
				}
			}
		}
		return out
	}
	if ln.Type != "assistant" || len(ln.Message.Content) == 0 || ln.Message.Content[0] != '[' {
		return nil
	}
	var blocks []ClaudeBlock
	if json.Unmarshal(ln.Message.Content, &blocks) != nil {
		return nil
	}
	var usage trace.Usage
	if u := ln.Message.Usage; u != nil {
		usage = trace.Usage{Fresh: u.Input + u.CacheCreate, Cached: u.CacheRead, Output: u.Output}
	}
	out = append(out, TurnUsage{Turn: "msg:" + ln.Message.ID, Usage: usage})
	for _, b := range blocks {
		if b.Type != "tool_use" {
			continue
		}
		c := trace.Call{Session: session, ID: b.ID, Time: ln.Timestamp}
		c.Tool, c.Command, c.Args, c.RawArgs, c.MCPServer, c.MCPTool = DoubleUnderscore(b.Name, b.Input)
		out = append(out, ToolCall{Key: b.ID, Turn: "msg:" + ln.Message.ID, Call: c})
	}
	return out
}

// Claude inserts this line on context compaction. Its contents describe earlier
// work but are not a new instruction from the user.
func IsClaudeContinuationSummary(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), "This session is being continued from a previous conversation that ran out of context.")
}

// ClaudeUserText is the typed text of a user line: the string content, or
// its text blocks. A line that only carries tool results has none.
func ClaudeUserText(raw json.RawMessage) string {
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// LastSegment is the tool part of mcp__<server>__<tool>.
func LastSegment(name string) string {
	parts := strings.Split(name, "__")
	return parts[len(parts)-1]
}

// RawString decodes a JSON string, or returns the raw JSON of anything else.
func RawString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// Flatten turns structured arguments into strings: strings as themselves,
// anything else as its JSON. Values are capped at maxArg bytes: long enough
// to keep a browser script or a patch whole (normalize reads inside them),
// short enough that one pasted file does not dominate memory.
func Flatten(in map[string]json.RawMessage) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		s := RawString(v)
		s = trace.TruncateUTF8(s, MaxArg)
		out[k] = s
	}
	return out
}

const MaxArg = 32 << 10

// RawKeys lists the arguments whose JSON value is not a string.
func RawKeys(in map[string]json.RawMessage) map[string]bool {
	out := map[string]bool{}
	for k, v := range in {
		if t := bytes.TrimSpace(v); len(t) > 0 && t[0] != '"' {
			out[k] = true
		}
	}
	return out
}
