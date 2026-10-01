// Package history reads the session history agent clients keep on this machine:
// Claude Code transcripts, Codex rollouts, Cursor's chat store, and frozen corpora.
package history

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// ClaudeCode reads Claude Code transcripts: <Dir>/<project>/<session>.jsonl.
// Claude Code deletes transcripts older than its cleanupPeriodDays setting
// (30 by default), so this history is short unless the user raised it.
type ClaudeCode struct{ Dir string }

func (ClaudeCode) Client() string { return "claude-code" }

func (r ClaudeCode) Read(since time.Time) ([]trace.Session, error) {
	files, err := filepath.Glob(filepath.Join(r.Dir, "*", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	var out []trace.Session
	for _, f := range files {
		if info, err := os.Stat(f); err != nil || info.ModTime().Before(since) {
			continue
		}
		s, err := ReadClaudeFile(f)
		if err != nil || len(s.Calls) == 0 || s.Start.Before(since) {
			continue
		}
		s.SourceDigest = FileDigest(f)
		out = append(out, s)
	}
	return out, nil
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
	s = trace.Session{Client: "claude-code", ID: strings.TrimSuffix(filepath.Base(path), ".jsonl")}
	// One model response is written as several lines (one per content
	// block) that repeat its usage; it is counted once, split over the
	// tool calls it made.
	type turn struct {
		usage trace.Usage
		calls []int
	}
	turns := map[string]*turn{}
	byUseID := map[string]int{} // tool_use id -> call index
	var order []string
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var ln ClaudeLine
		if json.Unmarshal(sc.Bytes(), &ln) != nil {
			continue
		}
		if s.Start.IsZero() && !ln.Timestamp.IsZero() {
			s.Start = ln.Timestamp
		}
		if ln.Type == "user" && !ln.IsMeta {
			if text := ClaudeUserText(ln.Message.Content); trace.IsRequest(text) {
				role := "user"
				if IsClaudeContinuationSummary(text) {
					role = "synthetic_context"
				}
				s.AddRequestWithRole(text, role)
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
					text := ClaudeUserText(r.Content)
					s.Calls[ci].Outcome = trace.OutcomeOK
					if r.IsError || trace.ResultOutcome(text) == trace.OutcomeFailed {
						s.Calls[ci].Outcome = trace.OutcomeFailed
					}
					s.Calls[ci].OutIDs, s.Calls[ci].OutCtx, s.Calls[ci].OutPaths = trace.OutputRefsPaths(text)
					s.Calls[ci].OutCollections = trace.ResultCollections(text)
					s.Calls[ci].Output = trace.TruncateUTF8(text, 600)
					s.Calls[ci].OutTokens = trace.OutputTokens(text)
				}
			}
			continue
		}
		if ln.Type != "assistant" || len(ln.Message.Content) == 0 || ln.Message.Content[0] != '[' {
			continue
		}
		var blocks []ClaudeBlock
		if json.Unmarshal(ln.Message.Content, &blocks) != nil {
			continue
		}
		tr := turns[ln.Message.ID]
		if tr == nil {
			tr = &turn{}
			if u := ln.Message.Usage; u != nil {
				tr.usage = trace.Usage{Fresh: u.Input + u.CacheCreate, Cached: u.CacheRead, Output: u.Output}
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
			c := trace.Call{Client: s.Client, Session: s.ID, ID: b.ID, Time: ln.Timestamp, Request: s.Request()}
			switch {
			case b.Name == "Bash":
				c.Tool, c.Command = "shell", RawString(b.Input["command"])
			case strings.HasPrefix(b.Name, "mcp__"):
				c.Tool, c.Args, c.RawArgs = "mcp:"+LastSegment(b.Name), Flatten(b.Input), RawKeys(b.Input)
				c.MCPServer, c.MCPTool, _ = strings.Cut(strings.TrimPrefix(b.Name, "mcp__"), "__")
			default:
				c.Tool, c.Args, c.RawArgs = b.Name, Flatten(b.Input), RawKeys(b.Input)
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
			s.Calls[ci].Tokens = tr.usage.Scale(1 / float64(len(tr.calls)))
			s.Calls[ci].Turn, s.Calls[ci].Measured = ti, true
		}
	}
	return s, sc.Err()
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

// Codex reads Codex CLI rollouts: <Dir>/YYYY/MM/DD/rollout-*.jsonl.
//
// Codex has recorded calls in three shapes over time, and all three are read:
// a function_call to exec_command or an MCP tool (arguments are JSON), a
// local_shell_call (argv array), and since mid-2026 a custom_tool_call to
// "exec" whose input is JavaScript calling tools.<name>({...}) one or more
// times.
type Codex struct{ Dir string }

func (Codex) Client() string { return "codex" }

func (r Codex) Read(since time.Time) ([]trace.Session, error) {
	var files []string
	err := filepath.WalkDir(r.Dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if p == r.Dir {
				return err
			}
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(p, ".jsonl") {
			files = append(files, p)
		}
		return nil
	})
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []trace.Session
	ids := map[string]bool{}
	for _, f := range files {
		if info, err := os.Stat(f); err != nil || info.ModTime().Before(since) {
			continue
		}
		s, err := ReadCodexFile(f)
		if err != nil || len(s.Calls) == 0 || s.Start.Before(since) {
			continue
		}
		s.SourceDigest = FileDigest(f)
		// A sub-rollout can open with its parent's meta. Session identity
		// must be unique, so a later file claiming a used id is named by
		// its own file.
		if ids[s.ID] {
			s.ID = strings.TrimSuffix(filepath.Base(f), ".jsonl")
			for i := range s.Calls {
				s.Calls[i].Session = s.ID
			}
		}
		ids[s.ID] = true
		out = append(out, s)
	}
	return out, nil
}

type CodexLine struct {
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Payload   struct {
		Type         string          `json:"type"`
		ID           string          `json:"id"`
		ThreadSource string          `json:"thread_source"`
		Role         string          `json:"role"`
		CallID       string          `json:"call_id"`
		Output       json.RawMessage `json:"output"`
		Content      json.RawMessage `json:"content"`
		Message      string          `json:"message"`
		Name         string          `json:"name"`
		Namespace    string          `json:"namespace"`
		Arguments    json.RawMessage `json:"arguments"`
		Input        string          `json:"input"`
		Action       struct {
			Command []string `json:"command"`
		} `json:"action"`
	} `json:"payload"`
}

func ReadCodexFile(path string) (s trace.Session, err error) {
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
	s = trace.Session{Client: "codex", ID: strings.TrimSuffix(filepath.Base(path), ".jsonl")}
	turn, turnStart := 0, 0
	metaSeen := false
	automationSession := false
	byCallID := map[string][]int{}    // call_id -> calls it produced
	execInputs := map[string]string{} // functions.exec source, for result attribution
	curID := ""
	add := func(c trace.Call) {
		c.Request = s.Request()
		if curID != "" {
			byCallID[curID] = append(byCallID[curID], len(s.Calls))
		}
		s.Calls = append(s.Calls, c)
	}
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 128<<20)
	for sc.Scan() {
		b := sc.Bytes()
		// Most lines are token counts and reasoning; skip them unparsed.
		if JsonHasAny(b, `"token_count"`) {
			// Usage of the model turn that issued the calls since the last one.
			var tc struct {
				Payload struct {
					Info *struct {
						Last struct {
							Input  float64 `json:"input_tokens"`
							Cached float64 `json:"cached_input_tokens"`
							Output float64 `json:"output_tokens"`
						} `json:"last_token_usage"`
					} `json:"info"`
				} `json:"payload"`
			}
			if json.Unmarshal(b, &tc) == nil && tc.Payload.Info != nil {
				l := tc.Payload.Info.Last
				trace.Spread(s.Calls, turnStart, turn, trace.Usage{Fresh: l.Input - l.Cached, Cached: l.Cached, Output: l.Output})
				turn++
				turnStart = len(s.Calls)
			}
			continue
		}
		if !JsonHasAny(b, `"session_meta"`, `"function_call"`, `"custom_tool_call"`, `"local_shell_call"`, `"role":"user"`, `"role": "user"`, `_call_output"`) {
			continue
		}
		var ln CodexLine
		if json.Unmarshal(b, &ln) != nil {
			continue
		}
		p := ln.Payload
		curID = p.CallID
		switch {
		case p.Type == "message" && p.Role == "user":
			var blocks []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			_ = json.Unmarshal(p.Content, &blocks)
			for _, bl := range blocks {
				if bl.Type == "input_text" && trace.IsRequest(bl.Text) {
					if automationSession && CodexInjectedAutomationContext(bl.Text) {
						s.AddRequestWithRole(bl.Text, "synthetic_context")
					} else {
						s.AddRequest(bl.Text)
					}
					break
				}
			}
		case ln.Type == "session_meta":
			// A forked or resumed rollout also carries its parent's meta
			// later in the file; the first one is this file's own.
			if p.ID != "" && !metaSeen {
				s.ID, metaSeen = p.ID, true
				automationSession = p.ThreadSource == "automation"
			}
			if s.Start.IsZero() {
				s.Start = ln.Timestamp
			}
		case strings.HasSuffix(p.Type, "_call_output"):
			// Scheduled runs receive their task in a pre-run automation record.
			// The only user message before it can be an injected AGENTS.md
			// wrapper. Keep the task at request zero so the calls and frozen
			// episode identity still refer to the same work.
			if p.Type == "function_call_output" && p.Name == "automation_update" && len(s.Calls) == 0 {
				if prompt := CodexAutomationPrompt(RawString(p.Output)); prompt != "" {
					onlyHarness := true
					for i, request := range s.Requests {
						if !trace.IsHarness(request) && (i >= len(s.RequestRoles) || s.RequestRoles[i] != "synthetic_context") {
							onlyHarness = false
							break
						}
					}
					if onlyHarness {
						s.Requests = []string{trace.TruncateUTF8(prompt, 4000)}
						s.RequestRoles = []string{"scheduled"}
					}
				}
			}
			indices := byCallID[p.CallID]
			// Only a literal Promise.allSettled array with a direct indexed
			// display proves which nested call produced each result. A combined
			// or transformed display still leaves every nested result unknown.
			if len(indices) > 1 {
				if source, nested := execInputs[p.CallID]; nested {
					if results, ok := CodexExecIndexedResults(source, p.Output, len(indices)); ok {
						for i, ci := range indices {
							CodexRecordResult(&s.Calls[ci], results[i].Text, results[i].Failed)
						}
					}
				}
				continue
			}
			if len(indices) != 1 {
				continue
			}
			text := CodexOutputText(p.Output)
			failed := false
			if source, nested := execInputs[p.CallID]; nested {
				if !CodexExecPassesThroughResult(source) {
					continue
				}
				var ok bool
				text, failed, ok = CodexExecResultText(p.Output)
				if !ok {
					continue
				}
			}
			CodexRecordResult(&s.Calls[indices[0]], text, failed)
		case p.Type == "function_call":
			var args map[string]json.RawMessage
			_ = json.Unmarshal([]byte(RawString(p.Arguments)), &args)
			add(CodexCall(s, ln.Timestamp, p.Namespace, p.Name, args))
		case p.Type == "local_shell_call":
			add(trace.Call{Client: "codex", Session: s.ID, Time: ln.Timestamp, Tool: "shell", Command: strings.Join(p.Action.Command, " ")})
		case p.Type == "custom_tool_call" && p.Name == "exec":
			execInputs[p.CallID] = p.Input
			for _, inner := range JsToolCalls(p.Input) {
				add(CodexCall(s, ln.Timestamp, "", inner.Name, inner.Args))
			}
		case p.Type == "custom_tool_call":
			// apply_patch and other freeform tools: the input is the argument.
			add(trace.Call{Client: "codex", Session: s.ID, Time: ln.Timestamp, Tool: p.Name, Args: map[string]string{"input": Truncate(p.Input, MaxArg)}})
		}
	}
	if s.Start.IsZero() && len(s.Calls) > 0 {
		s.Start = s.Calls[0].Time
	}
	for i := range s.Calls {
		s.Calls[i].Session = s.ID
	}
	if len(s.Requests) == 0 && len(s.Calls) > 0 {
		s.Requests = []string{""}
	}
	return s, sc.Err()
}

func CodexAutomationPrompt(output string) string {
	if !strings.HasPrefix(output, "Automation: ") || !strings.Contains(output, "\nAutomation ID: ") {
		return ""
	}
	_, prompt, ok := strings.Cut(output, "\n\n")
	if !ok {
		return ""
	}
	return strings.TrimSpace(prompt)
}

// Codex can prepend plugin and environment context to an injected AGENTS.md
// message. Classify only the complete envelope in an automation session; a
// real user request following the envelope must remain a user request.
func CodexInjectedAutomationContext(text string) bool {
	t := strings.TrimSpace(text)
	if trace.IsHarness(t) {
		return true
	}
	return strings.HasPrefix(t, "<recommended_plugins>") &&
		strings.Contains(t, "# AGENTS.md instructions for ") &&
		strings.Contains(t, "<environment_context>") &&
		strings.HasSuffix(t, "</environment_context>") &&
		!strings.Contains(t, "## My request for Codex:")
}

func JsonHasAny(b []byte, subs ...string) bool {
	for _, s := range subs {
		if strings.Contains(string(b), s) {
			return true
		}
	}
	return false
}

func CodexCall(s trace.Session, t time.Time, namespace, name string, args map[string]json.RawMessage) trace.Call {
	c := trace.Call{Client: "codex", Session: s.ID, Time: t}
	// Codex writes a tool as namespace "mcp__server__" + name "tool", as
	// "mcp__server" + "tool", or as "mcp__server__app" + "_tool". All three
	// must join into mcp__server__...tool.
	// Codex writes an MCP tool as namespace + name in several shapes:
	// "mcp__server__" + "tool", "mcp__server" + "tool" or + "_tool" (a
	// server-only namespace), and "mcp__server__app" + "_tool" (an app
	// inside a server, whose tool is app_tool).
	full := namespace + name
	if ns := strings.TrimSuffix(namespace, "__"); strings.HasPrefix(ns, "mcp__") {
		parts := strings.Split(strings.TrimPrefix(ns, "mcp__"), "__")
		tool := strings.TrimPrefix(name, "_")
		if len(parts) > 1 && strings.HasPrefix(name, "_") {
			tool = parts[len(parts)-1] + name
		}
		full = "mcp__" + parts[0] + "__" + tool
	}
	switch {
	case name == "exec_command" || name == "shell":
		c.Tool, c.Command = "shell", RawString(args["cmd"])
		if c.Command == "" {
			c.Command = RawString(args["command"])
		}
		var argv []string
		if json.Unmarshal(args["command"], &argv) == nil {
			c.Command = strings.Join(argv, " ")
		}
	case strings.HasPrefix(full, "mcp__"):
		c.Tool, c.Args, c.RawArgs = "mcp:"+Undouble(strings.TrimPrefix(LastSegment(full), "_")), Flatten(args), RawKeys(args)
		c.MCPServer, c.MCPTool, _ = strings.Cut(strings.TrimPrefix(full, "mcp__"), "__")
	default:
		c.Tool, c.Args, c.RawArgs = name, Flatten(args), RawKeys(args)
	}
	return c
}

type JsCall struct {
	Name string
	Args map[string]json.RawMessage
}

var JsToolRe = regexp.MustCompile(`tools\.([A-Za-z0-9_]+)\s*\(`)

// JsToolCalls finds each tools.<name>({...}) in a Codex exec body. The object
// literal is JavaScript, not JSON, so only its top-level string-valued keys are
// recovered; other values are recorded as their source text.
func JsToolCalls(src string) []JsCall {
	var out []JsCall
	for _, m := range JsToolRe.FindAllStringSubmatchIndex(src, -1) {
		name := src[m[2]:m[3]]
		rest := strings.TrimSpace(src[m[1]:])
		args := map[string]json.RawMessage{}
		switch {
		case strings.HasPrefix(rest, "{"):
			fields, quoted := JsObjectFieldsKinds(rest)
			for k, v := range fields {
				// An unquoted value that is valid JSON (a number, true, an
				// object literal with quoted keys) keeps its type.
				if !quoted[k] && json.Valid([]byte(v)) {
					args[k] = json.RawMessage(v)
					continue
				}
				b, _ := json.Marshal(v)
				args[k] = b
			}
		case rest != "" && trace.IsQuote([]rune(rest)[0]):
			// tools.apply_patch(`*** Begin Patch ...`): one string argument.
			v, _ := trace.ReadJSString([]rune(rest), 0)
			b, _ := json.Marshal(v)
			args["input"] = b
		}
		out = append(out, JsCall{Name: name, Args: args})
	}
	return out
}

// JsObjectFields reads the top-level key: value pairs of a JavaScript object
// literal starting at src[0] == '{'. String values are unquoted; anything else
// is kept as its source text. Input it cannot follow ends the scan early.
func JsObjectFields(src string) map[string]string {
	out, _ := JsObjectFieldsKinds(src)
	return out
}

// JsObjectFieldsKinds is jsObjectFields that also reports which values were
// string literals.
func JsObjectFieldsKinds(src string) (map[string]string, map[string]bool) {
	out := map[string]string{}
	quoted := map[string]bool{}
	rs := []rune(src)
	if len(rs) == 0 || rs[0] != '{' {
		return out, quoted
	}
	i := 1
	for {
		for i < len(rs) && (IsSpace(rs[i]) || rs[i] == ',') {
			i++
		}
		if i >= len(rs) || rs[i] == '}' {
			return out, quoted
		}
		var key string
		switch {
		case trace.IsQuote(rs[i]):
			key, i = trace.ReadJSString(rs, i)
		case IsIdentStart(rs[i]):
			j := i
			for j < len(rs) && (IsIdentStart(rs[j]) || (rs[j] >= '0' && rs[j] <= '9')) {
				j++
			}
			key, i = string(rs[i:j]), j
		default:
			// A spread, a computed key or something else: skip one value. A
			// stray closing bracket cannot be skipped this way; stop there.
			j := trace.ScanJSValue(rs, i)
			if j <= i {
				return out, quoted
			}
			i = j
			continue
		}
		for i < len(rs) && IsSpace(rs[i]) {
			i++
		}
		if i >= len(rs) || rs[i] != ':' {
			// Shorthand property {cmd}: the value is a variable of that name.
			out[key] = key
			continue
		}
		i++
		for i < len(rs) && IsSpace(rs[i]) {
			i++
		}
		if i >= len(rs) {
			return out, quoted
		}
		if trace.IsQuote(rs[i]) {
			out[key], i = trace.ReadJSString(rs, i)
			quoted[key] = true
			continue
		}
		j := trace.ScanJSValue(rs, i)
		out[key] = strings.TrimSpace(string(rs[i:j]))
		if j <= i {
			return out, quoted
		}
		i = j
	}
}

func IsSpace(c rune) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func IsIdentStart(c rune) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func Truncate(s string, n int) string { return trace.TruncateUTF8(s, n) }

// CodexOutputText is a call output's text: a string, or the text parts of a
// content list.
func CodexOutputText(raw json.RawMessage) string {
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str
	}
	var parts []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &parts)
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

var CodexPassThroughPrefix = regexp.MustCompile(`(?s)^\s*(?:// @exec:[^\n]*\n\s*)?(?:const|let)\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*=\s*await\s+`)

var CodexIndexedPrefix = regexp.MustCompile(`^(?:const|let)\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*=\s*await\s+Promise\.allSettled\(\s*\[`)

var CodexIndexedTail = regexp.MustCompile(`^([A-Za-z_$][A-Za-z0-9_$]*)\.forEach\(\(([A-Za-z_$][A-Za-z0-9_$]*),([A-Za-z_$][A-Za-z0-9_$]*)\)=>text\(JSON\.stringify\(\{([^{}]+)\}\)\)\)$`)

func CodexRecordResult(call *trace.Call, text string, failed bool) {
	call.Outcome = trace.ResultOutcome(text)
	if failed {
		call.Outcome = trace.OutcomeFailed
	}
	call.OutIDs, call.OutCtx, call.OutPaths = trace.OutputRefsPaths(text)
	call.OutCollections = trace.ResultCollections(text)
	call.Output = trace.TruncateUTF8(text, 600)
	call.OutTokens = trace.OutputTokens(text)
}

type CodexIndexedResult struct {
	Text   string
	Failed bool
}

// Prove that the source runs exactly one tool call per literal array item.
// The display parsers below then prove how each stable array index is printed.
func CodexIndexedArray(src string) (int, string, string, bool) {
	src = strings.TrimSpace(src)
	if strings.HasPrefix(src, "// @exec:") {
		_, rest, ok := strings.Cut(src, "\n")
		if !ok {
			return 0, "", "", false
		}
		src = strings.TrimSpace(rest)
	}
	prefix := CodexIndexedPrefix.FindStringSubmatchIndex(src)
	if prefix == nil || prefix[0] != 0 {
		return 0, "", "", false
	}
	resultName := src[prefix[2]:prefix[3]]
	pos, count := prefix[1], 0
	for {
		for pos < len(src) && (src[pos] == ' ' || src[pos] == '\t' || src[pos] == '\n' || src[pos] == '\r') {
			pos++
		}
		if pos >= len(src) {
			return 0, "", "", false
		}
		if src[pos] == ']' {
			pos++
			break
		}
		match := JsToolRe.FindStringIndex(src[pos:])
		if match == nil || match[0] != 0 {
			return 0, "", "", false
		}
		open := pos + match[1] - 1
		end := CodexJSCallEnd(src, open)
		if end < 0 {
			return 0, "", "", false
		}
		count++
		pos = end
		for pos < len(src) && (src[pos] == ' ' || src[pos] == '\t' || src[pos] == '\n' || src[pos] == '\r') {
			pos++
		}
		if pos < len(src) && src[pos] == ',' {
			pos++
			continue
		}
		if pos < len(src) && src[pos] == ']' {
			pos++
			break
		}
		return 0, "", "", false
	}
	if count < 2 || count != len(JsToolRe.FindAllStringIndex(src, -1)) || pos >= len(src) || src[pos] != ')' {
		return 0, "", "", false
	}
	tail := strings.TrimSpace(src[pos+1:])
	tail = strings.TrimPrefix(tail, ";")
	tail = strings.Join(strings.Fields(tail), "")
	tail = strings.TrimSuffix(tail, ";")
	return count, resultName, tail, true
}

// The two accepted displays retain the array index and either print the
// settled record itself or its fulfilled tool value without transforming it.
func CodexIndexedSource(src string) (int, string, bool) {
	count, resultName, tail, ok := CodexIndexedArray(src)
	if !ok {
		return 0, "", false
	}
	return CodexIndexedDisplay(count, resultName, tail, false)
}

func CodexIndexedValueSource(src string) (int, string, bool) {
	count, resultName, tail, ok := CodexIndexedArray(src)
	if !ok {
		return 0, "", false
	}
	return CodexIndexedDisplay(count, resultName, tail, true)
}

func CodexIndexedDisplay(count int, resultName, tail string, valueOnly bool) (int, string, bool) {
	match := CodexIndexedTail.FindStringSubmatch(tail)
	if match == nil || match[1] != resultName {
		return 0, "", false
	}
	itemName, indexName := match[2], match[3]
	fields := strings.Split(match[4], ",")
	if len(fields) != 2 {
		return 0, "", false
	}
	indexKey := ""
	resultFound := false
	for _, field := range fields {
		resultExpr := itemName
		if valueOnly {
			resultExpr = itemName + `.status==="fulfilled"?` + itemName + `.value:` + itemName + `.reason`
		}
		if field == "result:"+resultExpr || valueOnly && field == "result:"+strings.Replace(resultExpr, `"fulfilled"`, `'fulfilled'`, 1) {
			resultFound = true
			continue
		}
		key, value, explicit := strings.Cut(field, ":")
		if !explicit {
			key, value = field, field
		}
		if value != indexName || key != "i" && key != "check" && key != "index" {
			return 0, "", false
		}
		indexKey = key
	}
	if indexKey == "" || !resultFound {
		return 0, "", false
	}
	return count, indexKey, true
}

// The cell display must have one header and exactly one unmodified JSON
// result per array position. Missing, duplicated, or transformed blocks make
// attribution fail for the whole cell.
func CodexExecIndexedResults(src string, raw json.RawMessage, want int) ([]CodexIndexedResult, bool) {
	count, indexKey, ok := CodexIndexedSource(src)
	valueOnly := false
	if !ok {
		count, indexKey, ok = CodexIndexedValueSource(src)
		valueOnly = ok
	}
	if !ok || count != want {
		return nil, false
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil || len(blocks) != count+1 || !CodexTextBlock(blocks[0].Type) ||
		!strings.HasPrefix(strings.TrimSpace(blocks[0].Text), "Script completed\n") {
		return nil, false
	}
	out := make([]CodexIndexedResult, count)
	seen := make([]bool, count)
	for _, block := range blocks[1:] {
		if !CodexTextBlock(block.Type) {
			return nil, false
		}
		var row map[string]json.RawMessage
		if json.Unmarshal([]byte(block.Text), &row) != nil || len(row) != 2 || len(row["result"]) == 0 {
			return nil, false
		}
		var index int
		if json.Unmarshal(row[indexKey], &index) != nil || index < 0 || index >= count || seen[index] {
			return nil, false
		}
		if valueOnly {
			var valueOK bool
			out[index].Text, out[index].Failed, valueOK = CodexStrictToolValueText(row["result"])
			if !valueOK {
				return nil, false
			}
			seen[index] = true
			continue
		}
		var settled struct {
			Status string          `json:"status"`
			Value  json.RawMessage `json:"value"`
		}
		if json.Unmarshal(row["result"], &settled) != nil {
			return nil, false
		}
		result := CodexIndexedResult{}
		switch settled.Status {
		case "fulfilled":
			if len(settled.Value) == 0 {
				return nil, false
			}
			var valueOK bool
			result.Text, result.Failed, valueOK = CodexToolValueText(string(settled.Value))
			if !valueOK {
				return nil, false
			}
		case "rejected":
			result.Failed = true
		default:
			return nil, false
		}
		out[index], seen[index] = result, true
	}
	for _, present := range seen {
		if !present {
			return nil, false
		}
	}
	return out, true
}

// CodexExecPassesThroughResult accepts only a single awaited tool call whose
// return value is printed directly. A cell that slices, summarizes, combines,
// or otherwise transforms a result cannot prove per-call output provenance.
func CodexExecPassesThroughResult(src string) bool {
	if len(JsToolRe.FindAllStringIndex(src, -1)) != 1 {
		return false
	}
	prefix := CodexPassThroughPrefix.FindStringSubmatchIndex(src)
	if prefix == nil {
		return false
	}
	name := src[prefix[2]:prefix[3]]
	call := JsToolRe.FindStringIndex(src[prefix[1]:])
	if call == nil || call[0] != 0 {
		return false
	}
	open := prefix[1] + call[1] - 1
	end := CodexJSCallEnd(src, open)
	if end < 0 {
		return false
	}
	tail := strings.Join(strings.Fields(src[end:]), "")
	tail = strings.TrimPrefix(tail, ";")
	tail = strings.TrimSuffix(tail, ";")
	return tail == "text("+name+")" || tail == "text(JSON.stringify("+name+"))"
}

// CodexJSCallEnd finds the closing parenthesis of one tools.<name>(...) call.
// Quotes are skipped; template literals are rejected because they may contain
// executable substitutions that need a real JavaScript parser to assess.
func CodexJSCallEnd(src string, open int) int {
	if open < 0 || open >= len(src) || src[open] != '(' {
		return -1
	}
	depth := 0
	quote := byte(0)
	escaped := false
	for i := open; i < len(src); i++ {
		ch := src[i]
		if quote != 0 {
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == quote {
				quote = 0
			}
			continue
		}
		switch ch {
		case '`':
			return -1
		case '\'', '"':
			quote = ch
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

// CodexExecResultText unwraps the cell's display envelope and then mirrors
// the TAP bridge's resultText contract: structuredContent wins over text.
func CodexExecResultText(raw json.RawMessage) (string, bool, bool) {
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil || len(blocks) < 1 || len(blocks) > 2 || !CodexTextBlock(blocks[0].Type) {
		return "", false, false
	}
	cell := strings.TrimSpace(blocks[0].Text)
	if !strings.HasPrefix(cell, "Script completed\n") {
		return "", false, false
	}
	body := ""
	if len(blocks) == 2 {
		if !CodexTextBlock(blocks[1].Type) {
			return "", false, false
		}
		body = blocks[1].Text
	} else {
		_, body, _ = strings.Cut(cell, "\nOutput:\n")
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return "", false, false
	}
	return CodexToolValueText(body)
}

func CodexToolValueText(body string) (string, bool, bool) {
	if strings.TrimSpace(body) == "" {
		return "", false, false
	}
	var envelope struct {
		StructuredContent json.RawMessage `json:"structuredContent"`
		Content           []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if json.Unmarshal([]byte(body), &envelope) == nil {
		if len(envelope.StructuredContent) > 0 && string(envelope.StructuredContent) != "null" {
			return string(envelope.StructuredContent), envelope.IsError, true
		}
		if len(envelope.Content) > 0 {
			var parts []string
			for _, part := range envelope.Content {
				if part.Type == "text" {
					parts = append(parts, part.Text)
				}
			}
			if len(parts) == 0 {
				return "", false, false
			}
			return strings.Join(parts, "\n"), envelope.IsError, true
		}
	}
	return body, false, true
}

// A fulfilled-value display has erased Promise status. Accept it only when
// the displayed object still proves it is an unchanged tool-result envelope.
// A rejected reason or arbitrary JSON value cannot establish that provenance.
func CodexStrictToolValueText(raw json.RawMessage) (string, bool, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return "", false, false
	}
	if len(fields["structuredContent"]) > 0 && string(fields["structuredContent"]) != "null" {
		return CodexToolValueText(string(raw))
	}
	if len(fields["content"]) > 0 {
		var content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(fields["content"], &content) != nil || len(content) == 0 {
			return "", false, false
		}
		return CodexToolValueText(string(raw))
	}
	if len(fields["output"]) > 0 && len(fields["exit_code"]) > 0 {
		var output string
		if json.Unmarshal(fields["output"], &output) != nil {
			return "", false, false
		}
		var exitCode int
		if json.Unmarshal(fields["exit_code"], &exitCode) != nil {
			return "", false, false
		}
		return output, exitCode != 0, true
	}
	return "", false, false
}

func CodexTextBlock(kind string) bool { return kind == "text" || kind == "input_text" }

// Undouble collapses an app name that Codex's app connectors prepend to a
// tool whose name already starts with it: codex_apps' telara_telara_task_list
// is Telara's telara_task_list. Only an exact doubling is collapsed.
func Undouble(tool string) string {
	if p, rest, ok := strings.Cut(tool, "_"); ok && strings.HasPrefix(rest, p+"_") {
		return rest
	}
	return tool
}

// DefaultReaders returns readers for the named clients at their usual
// places under home.
func DefaultReaders(clients []string, home string) ([]trace.Reader, error) {
	var out []trace.Reader
	for _, c := range clients {
		switch strings.TrimSpace(c) {
		case "claude-code":
			out = append(out, ClaudeCode{Dir: filepath.Join(home, ".claude", "projects")})
		case "codex":
			out = append(out, Codex{Dir: filepath.Join(home, ".codex", "sessions")})
		case "cursor":
			out = append(out, Cursor{DB: CursorStateDB(home)})
		case "":
		default:
			return nil, fmt.Errorf("unknown client %q (want claude-code, codex or cursor)", c)
		}
	}
	return out, nil
}

// CursorStateDB is where Cursor keeps its chat store on this OS.
func CursorStateDB(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb")
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "Cursor", "User", "globalStorage", "state.vscdb")
		}
		return filepath.Join(home, "AppData", "Roaming", "Cursor", "User", "globalStorage", "state.vscdb")
	default:
		return filepath.Join(home, ".config", "Cursor", "User", "globalStorage", "state.vscdb")
	}
}

// Cursor reads Cursor's chat store, globalStorage/state.vscdb, a SQLite
// database. No SQLite driver is a dependency of this module, so it runs the
// system sqlite3 read-only with immutable=1: the file is never written or
// locked, and a missing sqlite3 makes this reader unavailable, not the run.
//
// Each message ("bubble") is a row bubbleId:<composer>:<bubble>; a tool call
// is a bubble with toolFormerData. A conversation ("composer") lists its
// bubbles in order in fullConversationHeadersOnly. Early 2025 conversations
// instead hold their messages inline in composerData.conversation.
type Cursor struct {
	DB      string
	SQLite3 string // path to sqlite3; "" looks it up on PATH
}

func (Cursor) Client() string { return "cursor" }

// ErrUnavailable reports that a store exists but cannot be read here.
var ErrUnavailable = errors.New("unavailable")

// CursorArgs keeps each argument's JSON type: text stays text (capped),
// numbers, booleans, objects and arrays pass through as JSON.
const CursorArgs = `CASE WHEN json_valid(%[1]s) THEN (SELECT json_group_object(a.key, CASE WHEN a.type = 'text' THEN substr(a.value, 1, 32768) ELSE json(a.value) END) FROM json_each(%[1]s) a) ELSE '{}' END`

var (
	// The table is aliased c throughout: inside the json_each subquery a bare
	// "value" would name json_each's own column, not the row's.
	CursorBubbleSQL = `SELECT substr(c.key, 10, 36) AS composer, substr(c.key, 47) AS bubble,
  json_extract(c.value, '$.toolFormerData.name') AS name,
  ` + fmt.Sprintf(CursorArgs, `coalesce(json_extract(c.value, '$.toolFormerData.rawArgs'), json_extract(c.value, '$.toolFormerData.params'))`) + ` AS args,
  json_extract(c.value, '$.createdAt') AS created,
  json_extract(c.value, '$.toolFormerData.status') AS status,
  substr(CAST(json_extract(c.value, '$.toolFormerData.result') AS TEXT), 1, 2048) AS result
FROM cursorDiskKV c WHERE c.key LIKE 'bubbleId:%' AND json_extract(c.value, '$.toolFormerData.name') IS NOT NULL;`

	CursorComposerSQL = `SELECT substr(c.key, 14) AS composer, json_extract(c.value, '$.createdAt') AS created,
  (SELECT json_group_array(json_extract(h.value, '$.bubbleId')) FROM json_each(c.value, '$.fullConversationHeadersOnly') h) AS headers
FROM cursorDiskKV c WHERE c.key LIKE 'composerData:%';`

	// The user's messages (bubble type 1), in both storage forms.
	CursorUserSQL = `SELECT substr(c.key, 10, 36) AS composer, substr(c.key, 47) AS bubble,
  substr(json_extract(c.value, '$.text'), 1, 4000) AS text
FROM cursorDiskKV c WHERE c.key LIKE 'bubbleId:%' AND json_extract(c.value, '$.type') = 1;`

	CursorInlineUserSQL = `SELECT substr(c.key, 14) AS composer, CAST(j.key AS TEXT) AS bubble,
  substr(json_extract(j.value, '$.text'), 1, 4000) AS text
FROM cursorDiskKV c, json_each(c.value, '$.conversation') j
WHERE c.key LIKE 'composerData:%' AND json_extract(j.value, '$.type') = 1;`

	CursorInlineSQL = `SELECT substr(c.key, 14) AS composer, CAST(j.key AS TEXT) AS bubble,
  json_extract(j.value, '$.toolFormerData.name') AS name,
  ` + fmt.Sprintf(CursorArgs, `coalesce(json_extract(j.value, '$.toolFormerData.rawArgs'), json_extract(j.value, '$.toolFormerData.params'))`) + ` AS args,
  NULL AS created,
  json_extract(j.value, '$.toolFormerData.status') AS status,
  substr(CAST(json_extract(j.value, '$.toolFormerData.result') AS TEXT), 1, 65536) AS result
FROM cursorDiskKV c, json_each(c.value, '$.conversation') j
WHERE c.key LIKE 'composerData:%' AND json_extract(j.value, '$.toolFormerData.name') IS NOT NULL;`
)

type CursorRow struct {
	Composer string          `json:"composer"`
	Bubble   string          `json:"bubble"`
	Name     string          `json:"name"`
	Args     string          `json:"args"`
	Created  json.RawMessage `json:"created"`
	Status   string          `json:"status"`
	Result   string          `json:"result"`
	Headers  string          `json:"headers"`
	Text     string          `json:"text"`
}

func (r Cursor) Read(since time.Time) ([]trace.Session, error) {
	if _, err := os.Stat(r.DB); os.IsNotExist(err) {
		return nil, nil
	}
	bin := r.SQLite3
	if bin == "" {
		p, err := exec.LookPath("sqlite3")
		if err != nil {
			return nil, fmt.Errorf("cursor: %w: sqlite3 is not installed", ErrUnavailable)
		}
		bin = p
	}
	query := func(sql string) ([]CursorRow, error) {
		cmd := exec.Command(bin, "-readonly", "-json", "file:"+r.DB+"?immutable=1", sql)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("cursor: sqlite3: %v: %s", err, strings.TrimSpace(stderr.String()))
		}
		var rows []CursorRow
		if len(bytes.TrimSpace(out)) == 0 {
			return nil, nil
		}
		return rows, json.Unmarshal(out, &rows)
	}
	composers, err := query(CursorComposerSQL)
	if err != nil {
		return nil, err
	}
	bubbles, err := query(CursorBubbleSQL)
	if err != nil {
		return nil, err
	}
	inline, err := query(CursorInlineSQL)
	if err != nil {
		return nil, err
	}
	users, err := query(CursorUserSQL)
	if err != nil {
		return nil, err
	}
	inlineUsers, err := query(CursorInlineUserSQL)
	if err != nil {
		return nil, err
	}

	convs := map[string]*CursorConv{}
	for _, c := range composers {
		cv := &CursorConv{Start: CursorTime(c.Created), Order: map[string]int{}}
		var hs []string
		_ = json.Unmarshal([]byte(c.Headers), &hs)
		for i, h := range hs {
			cv.Order[h] = i
		}
		convs[c.Composer] = cv
	}
	add := func(row CursorRow, pos int) {
		cv := convs[row.Composer]
		if cv == nil {
			return
		}
		call := CursorCall(row)
		call.Session = row.Composer
		cv.Calls = append(cv.Calls, CursorPlaced{pos, call})
		raw, _ := json.Marshal(row)
		cv.Raw = append(cv.Raw, string(raw))
	}
	for _, b := range bubbles {
		pos, ok := convs[b.Composer].OrderOf(b.Bubble)
		if !ok {
			// A bubble the header list does not name: order it by time after the named ones.
			pos = 1<<30 + int(CursorTime(b.Created).Unix()%(1<<30))
		}
		add(b, pos)
	}
	for _, b := range inline {
		i, _ := strconv.Atoi(b.Bubble)
		add(b, i)
	}
	addUser := func(row CursorRow, pos int) {
		if cv := convs[row.Composer]; cv != nil {
			cv.Users = append(cv.Users, CursorUser{pos, row.Text})
			raw, _ := json.Marshal(row)
			cv.Raw = append(cv.Raw, string(raw))
		}
	}
	for _, u := range users {
		if pos, ok := convs[u.Composer].OrderOf(u.Bubble); ok {
			addUser(u, pos)
		}
	}
	for _, u := range inlineUsers {
		i, _ := strconv.Atoi(u.Bubble)
		addUser(u, i)
	}

	var out []trace.Session
	for id, cv := range convs {
		if len(cv.Calls) == 0 || cv.Start.Before(since) {
			continue
		}
		sort.SliceStable(cv.Calls, func(i, j int) bool { return cv.Calls[i].Pos < cv.Calls[j].Pos })
		sort.SliceStable(cv.Users, func(i, j int) bool { return cv.Users[i].Pos < cv.Users[j].Pos })
		s := trace.Session{Client: "cursor", ID: id, Start: cv.Start}
		sort.Strings(cv.Raw)
		h := sha256.New()
		for _, r := range cv.Raw {
			h.Write([]byte(r))
			h.Write([]byte{0})
		}
		s.SourceDigest = hex.EncodeToString(h.Sum(nil))
		u := 0
		for _, c := range cv.Calls {
			for u < len(cv.Users) && cv.Users[u].Pos < c.Pos {
				if trace.IsRequest(cv.Users[u].Text) {
					s.AddRequest(cv.Users[u].Text)
				}
				u++
			}
			c.Call.Request = s.Request()
			s.Calls = append(s.Calls, c.Call)
		}
		out = append(out, s)
	}
	// Conversations come out of a map; later passes take sessions in order.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Start.Equal(out[j].Start) {
			return out[i].Start.Before(out[j].Start)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

type CursorPlaced struct {
	Pos  int
	Call trace.Call
}

type CursorConv struct {
	Raw   []string // the rows read for it, for its source digest
	Start time.Time
	Order map[string]int
	Calls []CursorPlaced
	Users []CursorUser
}

type CursorUser struct {
	Pos  int
	Text string
}

// orderOf is the bubble's position in its conversation's header list.
func (c *CursorConv) OrderOf(bubble string) (int, bool) {
	if c == nil {
		return 0, false
	}
	i, ok := c.Order[bubble]
	return i, ok
}

func CursorCall(row CursorRow) trace.Call {
	c := trace.Call{Client: "cursor", Time: CursorTime(row.Created)}
	c.OutIDs, c.OutCtx, c.OutPaths = trace.OutputRefsPaths(row.Result)
	c.OutCollections = trace.ResultCollections(row.Result)
	c.Output = trace.TruncateUTF8(row.Result, 600)
	c.OutTokens = trace.OutputTokens(row.Result)
	switch row.Status {
	case "completed":
		c.Outcome = trace.OutcomeOK
	case "error", "cancelled":
		c.Outcome = trace.OutcomeFailed
	}
	var args map[string]json.RawMessage
	_ = json.Unmarshal([]byte(row.Args), &args)
	switch {
	case strings.HasPrefix(row.Name, "run_terminal"):
		c.Tool, c.Command = "shell", RawString(args["command"])
	case strings.HasPrefix(row.Name, "mcp-"):
		parts := strings.Split(row.Name, "-")
		args = CursorMCPArgs(args)
		c.Tool, c.Args, c.RawArgs = "mcp:"+parts[len(parts)-1], Flatten(args), RawKeys(args)
	default:
		c.Tool, c.Args, c.RawArgs = row.Name, Flatten(args), RawKeys(args)
	}
	return c
}

// CursorMCPArgs returns the arguments the MCP tool received. Cursor records
// an MCP call as an envelope, {name, args, toolCallId, providerIdentifier,
// serverIdentifier, ...} in rawArgs, or {tools: [{name, parameters}]} with
// the arguments as a JSON string in params. The envelope is Cursor's
// bookkeeping, not the tool's input.
func CursorMCPArgs(args map[string]json.RawMessage) map[string]json.RawMessage {
	inner := args["args"]
	if inner == nil {
		var tools []struct {
			Parameters json.RawMessage `json:"parameters"`
		}
		if json.Unmarshal(args["tools"], &tools) != nil || len(tools) != 1 {
			return args
		}
		inner = tools[0].Parameters
	} else if args["toolCallId"] == nil && args["providerIdentifier"] == nil && args["serverIdentifier"] == nil && args["toolName"] == nil {
		// A tool whose own argument is named "args".
		return args
	}
	var s string
	if json.Unmarshal(inner, &s) == nil {
		inner = json.RawMessage(s)
	}
	var out map[string]json.RawMessage
	if json.Unmarshal(inner, &out) != nil {
		return map[string]json.RawMessage{}
	}
	return out
}

// CursorTime reads createdAt, which Cursor has written both as epoch
// milliseconds and as an RFC 3339 string.
func CursorTime(raw json.RawMessage) time.Time {
	var ms int64
	if json.Unmarshal(raw, &ms) == nil && ms > 0 {
		return time.UnixMilli(ms).UTC()
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// ManifestEntry is one frozen session.
type ManifestEntry struct {
	Client string `json:"client"`
	ID     string `json:"id"`
	Digest string `json:"digest"`
	Calls  int    `json:"calls"`
}

// Manifest lists the sessions of a frozen corpus.
type Manifest struct {
	Cutoff   time.Time       `json:"cutoff"`
	Sessions []ManifestEntry `json:"sessions"`
	// Digest covers every entry, so two manifests can be compared at a glance.
	Digest string `json:"digest"`
}

// ErrCorpusChanged is returned when a frozen session is missing or differs.
var ErrCorpusChanged = errors.New("frozen corpus changed")

// SessionDigest identifies a session's input: the digest of what its reader
// read (SourceDigest) when there is one, so a parser change never reads as
// changed input; otherwise the sha256 of the session's JSON encoding.
func SessionDigest(s trace.Session) string {
	if s.SourceDigest != "" {
		return s.SourceDigest
	}
	b, _ := json.Marshal(s)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// LastActivity is the latest time a session records.
func LastActivity(s trace.Session) time.Time {
	t := s.Start
	for _, c := range s.Calls {
		if c.Time.After(t) {
			t = c.Time
		}
	}
	return t
}

// BuildManifest freezes the sessions whose last recorded activity is before
// cutoff; sessions still in progress are left out.
func BuildManifest(ss []trace.Session, cutoff time.Time) Manifest {
	m := Manifest{Cutoff: cutoff.UTC()}
	for _, s := range ss {
		if len(s.Calls) == 0 || !LastActivity(s).Before(cutoff) {
			continue
		}
		m.Sessions = append(m.Sessions, ManifestEntry{Client: s.Client, ID: s.ID, Digest: SessionDigest(s), Calls: len(s.Calls)})
	}
	sort.Slice(m.Sessions, func(i, j int) bool {
		if m.Sessions[i].Client != m.Sessions[j].Client {
			return m.Sessions[i].Client < m.Sessions[j].Client
		}
		return m.Sessions[i].ID < m.Sessions[j].ID
	})
	h := sha256.New()
	for _, e := range m.Sessions {
		fmt.Fprintf(h, "%s\x00%s\x00%s\n", e.Client, e.ID, e.Digest)
	}
	m.Digest = hex.EncodeToString(h.Sum(nil))
	return m
}

// FrozenReader returns only the sessions a manifest lists for its client,
// and fails if one is missing or reads differently than when frozen.
type FrozenReader struct {
	Inner    trace.Reader
	Manifest Manifest
	// DropChanged leaves out a session that changed or disappeared since
	// the freeze, instead of failing, and lists it in Dropped. A live
	// client store keeps appending to and deleting sessions; an evaluation
	// that uses this must report what was dropped.
	DropChanged bool
	Dropped     *[]string
}

func (f FrozenReader) Client() string { return f.Inner.Client() }

func (f FrozenReader) Read(since time.Time) ([]trace.Session, error) {
	want := map[string]string{}
	for _, e := range f.Manifest.Sessions {
		if e.Client == f.Inner.Client() {
			if _, dup := want[e.ID]; dup {
				return nil, fmt.Errorf("%w: %s: session id %s is listed twice; the reader must give each session a unique id", ErrCorpusChanged, e.Client, e.ID)
			}
			want[e.ID] = e.Digest
		}
	}
	ss, err := f.Inner.Read(time.Time{})
	if err != nil {
		return nil, err
	}
	var out []trace.Session
	var changed []string
	for _, s := range ss {
		d, ok := want[s.ID]
		if !ok {
			continue
		}
		delete(want, s.ID)
		if SessionDigest(s) != d {
			changed = append(changed, s.ID)
			continue
		}
		if !s.Start.Before(since) || since.IsZero() {
			out = append(out, s)
		}
	}
	for id := range want {
		changed = append(changed, id+" (missing)")
	}
	if len(changed) > 0 && f.DropChanged {
		sort.Strings(changed)
		if f.Dropped != nil {
			for _, id := range changed {
				*f.Dropped = append(*f.Dropped, f.Inner.Client()+"/"+id)
			}
		}
		return out, nil
	}
	if len(changed) > 0 {
		sort.Strings(changed)
		return nil, fmt.Errorf("%w: %s: %d session(s): %v", ErrCorpusChanged, f.Inner.Client(), len(changed), FirstN(changed, 5))
	}
	return out, nil
}

func FirstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// FileDigest is the sha256 of a file's bytes, "" if it cannot be read.
func FileDigest(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}
