package discover

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ClaudeCode reads Claude Code transcripts: <Dir>/<project>/<session>.jsonl.
// Claude Code deletes transcripts older than its cleanupPeriodDays setting
// (30 by default), so this history is short unless the user raised it.
type ClaudeCode struct{ Dir string }

func (ClaudeCode) Client() string { return "claude-code" }

func (r ClaudeCode) Read(since time.Time) ([]Session, error) {
	files, err := filepath.Glob(filepath.Join(r.Dir, "*", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	var out []Session
	for _, f := range files {
		if info, err := os.Stat(f); err != nil || info.ModTime().Before(since) {
			continue
		}
		s, err := readClaudeFile(f)
		if err != nil || len(s.Calls) == 0 || s.Start.Before(since) {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

type claudeLine struct {
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

type claudeBlock struct {
	Type  string                     `json:"type"`
	ID    string                     `json:"id"`
	Name  string                     `json:"name"`
	Input map[string]json.RawMessage `json:"input"`
}

func readClaudeFile(path string) (s Session, err error) {
	// One file the parser cannot follow is skipped, not the whole run.
	defer func() {
		if r := recover(); r != nil {
			s, err = Session{}, fmt.Errorf("%s: unreadable: %v", path, r)
		}
	}()
	fh, err := os.Open(path)
	if err != nil {
		return Session{}, err
	}
	defer fh.Close()
	s = Session{Client: "claude-code", ID: strings.TrimSuffix(filepath.Base(path), ".jsonl")}
	// One model response is written as several lines (one per content
	// block) that repeat its usage; it is counted once, split over the
	// tool calls it made.
	type turn struct {
		usage Usage
		calls []int
	}
	turns := map[string]*turn{}
	byUseID := map[string]int{} // tool_use id -> call index
	var order []string
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var ln claudeLine
		if json.Unmarshal(sc.Bytes(), &ln) != nil {
			continue
		}
		if s.Start.IsZero() && !ln.Timestamp.IsZero() {
			s.Start = ln.Timestamp
		}
		if ln.Type == "user" && !ln.IsMeta {
			if text := claudeUserText(ln.Message.Content); isRequest(text) {
				s.addRequest(text)
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
					ci, ok := byUseID[r.ToolUseID]
					if r.Type != "tool_result" || !ok {
						continue
					}
					s.Calls[ci].Outcome = OutcomeOK
					if r.IsError {
						s.Calls[ci].Outcome = OutcomeFailed
					}
					s.Calls[ci].OutIDs = outputIDs(claudeUserText(r.Content))
				}
			}
			continue
		}
		if ln.Type != "assistant" || len(ln.Message.Content) == 0 || ln.Message.Content[0] != '[' {
			continue
		}
		var blocks []claudeBlock
		if json.Unmarshal(ln.Message.Content, &blocks) != nil {
			continue
		}
		tr := turns[ln.Message.ID]
		if tr == nil {
			tr = &turn{}
			if u := ln.Message.Usage; u != nil {
				tr.usage = Usage{Fresh: u.Input + u.CacheCreate, Cached: u.CacheRead, Output: u.Output}
			}
			turns[ln.Message.ID] = tr
			order = append(order, ln.Message.ID)
		}
		for _, b := range blocks {
			if b.Type != "tool_use" {
				continue
			}
			tr.calls = append(tr.calls, len(s.Calls))
			if b.ID != "" {
				byUseID[b.ID] = len(s.Calls)
			}
			c := Call{Client: s.Client, Session: s.ID, Time: ln.Timestamp, Request: s.request()}
			switch {
			case b.Name == "Bash":
				c.Tool, c.Command = "shell", rawString(b.Input["command"])
			case strings.HasPrefix(b.Name, "mcp__"):
				c.Tool, c.Args, c.RawArgs = "mcp:"+lastSegment(b.Name), flatten(b.Input), rawKeys(b.Input)
			default:
				c.Tool, c.Args, c.RawArgs = b.Name, flatten(b.Input), rawKeys(b.Input)
			}
			s.Calls = append(s.Calls, c)
		}
	}
	for ti, id := range order {
		tr := turns[id]
		if tr.usage.Total() == 0 || len(tr.calls) == 0 {
			continue
		}
		for _, ci := range tr.calls {
			s.Calls[ci].Tokens = tr.usage.scale(1 / float64(len(tr.calls)))
			s.Calls[ci].Turn, s.Calls[ci].Measured = ti, true
		}
	}
	return s, sc.Err()
}

// claudeUserText is the typed text of a user line: the string content, or
// its text blocks. A line that only carries tool results has none.
func claudeUserText(raw json.RawMessage) string {
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

// lastSegment is the tool part of mcp__<server>__<tool>.
func lastSegment(name string) string {
	parts := strings.Split(name, "__")
	return parts[len(parts)-1]
}

// rawString decodes a JSON string, or returns the raw JSON of anything else.
func rawString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// flatten turns structured arguments into strings: strings as themselves,
// anything else as its JSON. Values are capped at maxArg bytes: long enough
// to keep a browser script or a patch whole (normalize reads inside them),
// short enough that one pasted file does not dominate memory.
func flatten(in map[string]json.RawMessage) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		s := rawString(v)
		s = truncateUTF8(s, maxArg)
		out[k] = s
	}
	return out
}

const maxArg = 32 << 10

// rawKeys lists the arguments whose JSON value is not a string.
func rawKeys(in map[string]json.RawMessage) map[string]bool {
	out := map[string]bool{}
	for k, v := range in {
		if t := bytes.TrimSpace(v); len(t) > 0 && t[0] != '"' {
			out[k] = true
		}
	}
	return out
}
