package history

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// QwenCode reads Qwen Code sessions (TENG-3117):
// <Dir>/<sanitized project path>/chats/<sessionId>.jsonl (not *.ledger.jsonl),
// as qwen-code 0.24 writes them (ChatRecordingService). Unlike Gemini CLI,
// which it was forked from, each line is a record
// {uuid, parentUuid, sessionId, timestamp, type: user|assistant|tool_result|system, message},
// where message is Gemini content ({role, parts}): assistant parts hold
// functionCall {id, name, args}, tool_result parts functionResponse {id,
// response}, and an assistant record carries usageMetadata. Records form a
// tree: after a rewind the next record hangs off an earlier parent, so the
// session is the chain from the last record back to the root. MCP tools are
// named mcp__<server>__<tool>, as in Claude Code.
type QwenCode struct{ Dir string }

func (QwenCode) Client() string { return "qwen-code" }

func (r QwenCode) Read(since time.Time) ([]trace.Session, error) {
	ss, _, err := r.ReadWithStats(since)
	return ss, err
}

func (r QwenCode) ReadWithStats(since time.Time) ([]trace.Session, trace.ReadStats, error) {
	var st trace.ReadStats
	files, err := filepath.Glob(filepath.Join(r.Dir, "*", "chats", "*.jsonl"))
	if err != nil {
		return nil, st, err
	}
	var out []trace.Session
	for _, f := range files {
		if strings.HasSuffix(f, ".ledger.jsonl") {
			continue
		}
		if info, err := os.Stat(f); err != nil || info.ModTime().Before(since) {
			continue
		}
		s, err := ReadQwenFile(f)
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

// QwenRecord is one line of a Qwen Code session.
type QwenRecord struct {
	UUID       string    `json:"uuid"`
	ParentUUID *string   `json:"parentUuid"`
	SessionID  string    `json:"sessionId"`
	Timestamp  time.Time `json:"timestamp"`
	Type       string    `json:"type"`
	Subtype    string    `json:"subtype"`
	Message    *struct {
		Parts []struct {
			Text         string `json:"text"`
			FunctionCall *struct {
				ID   string                     `json:"id"`
				Name string                     `json:"name"`
				Args map[string]json.RawMessage `json:"args"`
			} `json:"functionCall"`
			FunctionResponse *struct {
				ID       string                     `json:"id"`
				Response map[string]json.RawMessage `json:"response"`
			} `json:"functionResponse"`
		} `json:"parts"`
	} `json:"message"`
	Usage *struct {
		Prompt   float64 `json:"promptTokenCount"`
		Output   float64 `json:"candidatesTokenCount"`
		Cached   float64 `json:"cachedContentTokenCount"`
		Thoughts float64 `json:"thoughtsTokenCount"`
	} `json:"usageMetadata"`
	ToolCallResult *struct {
		CallID string `json:"callId"`
		Status string `json:"status"`
	} `json:"toolCallResult"`
}

// ReadQwenFile reads one session: the active chain of its record tree.
func ReadQwenFile(path string) (s trace.Session, err error) {
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
	byUUID := map[string]QwenRecord{}
	var last string
	skipped := 0
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 128<<20)
	for sc.Scan() {
		var rec QwenRecord
		if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.UUID == "" {
			if len(strings.TrimSpace(sc.Text())) > 0 {
				skipped++
			}
			continue
		}
		byUUID[rec.UUID] = rec
		last = rec.UUID
	}
	if err := sc.Err(); err != nil {
		return trace.Session{}, err
	}
	// Walk the active branch back from the newest record.
	var chain []QwenRecord
	for id, seen := last, map[string]bool{}; id != "" && !seen[id]; {
		seen[id] = true
		rec, ok := byUUID[id]
		if !ok {
			break
		}
		chain = append(chain, rec)
		if rec.ParentUUID == nil {
			break
		}
		id = *rec.ParentUUID
	}
	id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if len(chain) > 0 && chain[0].SessionID != "" {
		id = chain[0].SessionID
	}
	a := NewAssembler("qwen-code", id)
	for i := len(chain) - 1; i >= 0; i-- {
		rec := chain[i]
		FirstTime(&a.S, rec.Timestamp)
		for _, e := range QwenEvents(rec, id) {
			a.Add(e)
		}
	}
	s = a.Finish()
	s.Skipped += skipped
	return s, nil
}

// QwenEvents decodes one record.
func QwenEvents(rec QwenRecord, session string) []Event {
	if rec.Message == nil {
		return nil
	}
	var out []Event
	switch rec.Type {
	case "user":
		if rec.Subtype != "" {
			return nil // goal runtime and other injected turns
		}
		var b []string
		for _, p := range rec.Message.Parts {
			if p.Text != "" {
				b = append(b, p.Text)
			}
		}
		out = append(out, UserText{Text: strings.Join(b, "\n")})
	case "assistant":
		turn := ""
		if u := rec.Usage; u != nil {
			turn = "rec:" + rec.UUID
			out = append(out, TurnUsage{Turn: turn, Usage: trace.Usage{Fresh: u.Prompt - u.Cached, Cached: u.Cached, Output: u.Output + u.Thoughts}})
		}
		for _, p := range rec.Message.Parts {
			if fc := p.FunctionCall; fc != nil {
				c := trace.Call{Session: session, ID: fc.ID, Time: rec.Timestamp}
				name, args := fc.Name, fc.Args
				// Qwen Code 0.24 loads tools on demand: tool_call {name,
				// arguments} runs the named tool, arguments as a JSON string
				// (TENG-3162). The call is the named tool's.
				if inner := RawString(fc.Args["name"]); name == "tool_call" && inner != "" {
					name, args = inner, map[string]json.RawMessage{}
					raw := fc.Args["arguments"]
					if str := RawString(raw); str != "" && strings.HasPrefix(strings.TrimSpace(str), "{") {
						raw = json.RawMessage(str)
					}
					_ = json.Unmarshal(raw, &args)
				}
				if name == "run_shell_command" {
					c.Tool, c.Command = "shell", RawString(args["command"])
				} else {
					c.Tool, c.Command, c.Args, c.RawArgs, c.MCPServer, c.MCPTool = DoubleUnderscore(name, args)
				}
				out = append(out, ToolCall{Key: fc.ID, Turn: turn, Call: c})
			}
		}
	case "tool_result":
		for _, p := range rec.Message.Parts {
			fr := p.FunctionResponse
			if fr == nil {
				continue
			}
			text, isErr := "", false
			if v, ok := fr.Response["output"]; ok {
				text = ResultText(v)
			} else if v, ok := fr.Response["error"]; ok {
				text, isErr = ResultText(v), true
			}
			if rec.ToolCallResult != nil && rec.ToolCallResult.Status == "error" {
				isErr = true
			}
			out = append(out, ToolResult{Key: fr.ID, Nth: -1, Text: text, IsError: isErr})
		}
	}
	return out
}
