package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
	var out []trace.Session
	st, err := r.each(since, p, nil, func(s trace.Session) error { out = append(out, s); return nil })
	if err != nil {
		return nil, st, err
	}
	return out, st, nil
}

// Each passes Claude Code's sessions on in file order, a chunk of files at a
// time.
func (r ClaudeCode) Each(since time.Time, yield func(trace.Session) error) error {
	_, err := r.each(since, nil, nil, yield)
	return err
}

// EachWith is Each for a read with options (see ReadOptions).
func (r ClaudeCode) EachWith(since time.Time, o ReadOptions, yield func(trace.Session) error) error {
	_, err := r.each(since, nil, &o, yield)
	return err
}

func (r ClaudeCode) each(since time.Time, p trace.Progress, o *ReadOptions, yield func(trace.Session) error) (trace.ReadStats, error) {
	var st trace.ReadStats
	files, err := filepath.Glob(filepath.Join(r.Dir, "*", "*.jsonl"))
	if err != nil {
		return st, err
	}
	files = changedSince(files, since)
	cache, parse := "claude-code", parseClaude
	if o != nil {
		cache, parse = "", claudeParser(o)
	}
	err = ParseFilesEach(files, cache, nil, p, parse, func(_ int, r Parsed) error {
		if r.Err != nil {
			st.UnreadableFiles++
			return nil
		}
		if s := r.Session; len(s.Calls) > 0 && !s.Start.Before(since) {
			return yield(s)
		}
		return nil
	})
	return st, err
}

type ClaudeLine struct {
	Type      string    `json:"type"`
	IsMeta    bool      `json:"isMeta"`
	SessionID string    `json:"sessionId"`
	Timestamp time.Time `json:"timestamp"`
	// The session's settings as of this line.
	Cwd       string `json:"cwd"`
	GitBranch string `json:"gitBranch"`
	Version   string `json:"version"`
	// Model is the message's model (message.model). Decoding a line does
	// not fill it; ParseClaudeLine does.
	Model   string `json:"-"`
	Message struct {
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
	Text  string                     `json:"text"`
	ID    string                     `json:"id"`
	Name  string                     `json:"name"`
	Input map[string]json.RawMessage `json:"input"`
}

func ReadClaudeFile(path string) (trace.Session, error) { return ParseFile(path, parseClaude) }

// ReadClaudeFileWith is ReadClaudeFile for a read with options.
func ReadClaudeFileWith(path string, o ReadOptions) (trace.Session, error) {
	return ParseFile(path, claudeParser(&o))
}

func parseClaude(path string, fh io.Reader) (trace.Session, error) {
	return parseClaudeWith(path, fh, nil)
}

func claudeParser(o *ReadOptions) FileParser {
	return func(path string, fh io.Reader) (trace.Session, error) { return parseClaudeWith(path, fh, o) }
}

// newClaudeAssembler starts the session of the transcript at path.
func newClaudeAssembler(path string, o *ReadOptions) *Assembler {
	id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if o == nil {
		return NewAssembler("claude-code", id)
	}
	return NewAssemblerWith("claude-code", id, *o)
}

// ParseClaudeLine decodes one transcript line, Model included.
func ParseClaudeLine(b []byte) (ClaudeLine, error) {
	var ln ClaudeLine
	if err := json.Unmarshal(b, &ln); err != nil {
		return ln, err
	}
	var m struct {
		Message struct {
			Model string `json:"model"`
		} `json:"message"`
	}
	if json.Unmarshal(b, &m) == nil {
		ln.Model = m.Message.Model
	}
	return ln, nil
}

// claudeLine applies one transcript line.
func claudeLine(a *Assembler, b []byte, o *ReadOptions) {
	var ln ClaudeLine
	var err error
	if o != nil && o.Conversation {
		ln, err = ParseClaudeLine(b)
	} else {
		err = json.Unmarshal(b, &ln)
	}
	if err != nil {
		a.Skip()
		return
	}
	FirstTime(&a.S, ln.Timestamp)
	for _, e := range claudeEvents(ln, a.S.ID, o) {
		a.Add(e)
	}
}

func parseClaudeWith(path string, fh io.Reader, o *ReadOptions) (s trace.Session, err error) {
	// One file the parser cannot follow is skipped, not the whole run.
	defer func() {
		if r := recover(); r != nil {
			s, err = trace.Session{}, fmt.Errorf("%s: unreadable: %v", path, r)
		}
	}()
	a := newClaudeAssembler(path, o)
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		claudeLine(a, sc.Bytes(), o)
	}
	return a.Finish(), sc.Err()
}

// ClaudeEvents decodes one transcript line. One model response is written as
// several lines (one per content block) that each repeat its usage; the
// assembler counts it once, split over the tool calls it made.
func ClaudeEvents(ln ClaudeLine, session string) []Event { return claudeEvents(ln, session, nil) }

// ClaudeEventsWith is ClaudeEvents for a read with options: it also
// decodes the model's prose and the session's settings, and redacts tool
// arguments before they are flattened and bounded.
func ClaudeEventsWith(ln ClaudeLine, session string, o ReadOptions) []Event {
	return claudeEvents(ln, session, &o)
}

func claudeEvents(ln ClaudeLine, session string, o *ReadOptions) []Event {
	var out []Event
	conv := o != nil && o.Conversation
	if conv && (ln.Cwd != "" || ln.GitBranch != "" || ln.Version != "" || ln.Model != "") {
		out = append(out, SessionMeta{Cwd: ln.Cwd, Branch: ln.GitBranch, Version: ln.Version, Model: ln.Model})
	}
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
		return out
	}
	var blocks []ClaudeBlock
	if json.Unmarshal(ln.Message.Content, &blocks) != nil {
		return out
	}
	var usage trace.Usage
	if u := ln.Message.Usage; u != nil {
		usage = trace.Usage{Fresh: u.Input + u.CacheCreate, Cached: u.CacheRead, Output: u.Output}
	}
	out = append(out, TurnUsage{Turn: "msg:" + ln.Message.ID, Usage: usage})
	for _, b := range blocks {
		if conv && b.Type == "text" {
			out = append(out, AssistantText{Text: b.Text, Turn: "msg:" + ln.Message.ID})
			continue
		}
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
