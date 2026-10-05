package history

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

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
	ss, _, err := r.ReadWithStats(since)
	return ss, err
}

func (r Codex) ReadWithStats(since time.Time) ([]trace.Session, trace.ReadStats, error) {
	var st trace.ReadStats
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
		return nil, st, nil
	}
	if err != nil {
		return nil, st, err
	}
	var out []trace.Session
	ids := map[string]bool{}
	for _, f := range files {
		if info, err := os.Stat(f); err != nil || info.ModTime().Before(since) {
			continue
		}
		s, err := ReadCodexFile(f)
		if err != nil {
			st.UnreadableFiles++
			continue
		}
		if len(s.Calls) == 0 || s.Start.Before(since) {
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
	return out, st, nil
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
		// Result is an mcp_tool_call_end event's {Ok: {content, isError}}
		// or {Err: "..."}.
		Result json.RawMessage `json:"result"`
	} `json:"payload"`
}

func ReadCodexFile(path string) (res trace.Session, err error) {
	// One file the parser cannot follow is skipped, not the whole run.
	defer func() {
		if r := recover(); r != nil {
			res, err = trace.Session{}, fmt.Errorf("%s: unreadable: %v", path, r)
		}
	}()
	fh, err := os.Open(path)
	if err != nil {
		return trace.Session{}, err
	}
	defer fh.Close()
	a := NewAssembler("codex", strings.TrimSuffix(filepath.Base(path), ".jsonl"))
	s := &a.S
	metaSeen := false
	automationSession := false
	execInputs := map[string]string{}      // functions.exec source, for result attribution
	mcpEnds := map[string]codexMCPResult{} // call_id -> the clean result Codex recorded
	curID := ""
	// Calls under one call_id: one, or several from one exec script.
	add := func(c trace.Call) { a.Add(ToolCall{Key: curID, Call: c}) }
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
				a.Add(TurnUsage{Usage: trace.Usage{Fresh: l.Input - l.Cached, Cached: l.Cached, Output: l.Output}})
			}
			continue
		}
		if !JsonHasAny(b, `"session_meta"`, `"function_call"`, `"custom_tool_call"`, `"local_shell_call"`, `"role":"user"`, `"role": "user"`, `_call_output"`, `"mcp_tool_call_end"`) {
			continue
		}
		var ln CodexLine
		if json.Unmarshal(b, &ln) != nil {
			a.Skip()
			continue
		}
		p := ln.Payload
		curID = p.CallID
		switch {
		case p.Type == "mcp_tool_call_end":
			// Codex records an MCP call's result here, clean, before the
			// function_call_output that carries it inside a text envelope.
			if r, ok := codexMCPEnd(p.Result); ok {
				mcpEnds[p.CallID] = r
			}
		case p.Type == "message" && p.Role == "user":
			var blocks []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			_ = json.Unmarshal(p.Content, &blocks)
			for _, bl := range blocks {
				if bl.Type == "input_text" && trace.IsRequest(bl.Text) {
					role := "user"
					if automationSession && CodexInjectedAutomationContext(bl.Text) {
						role = "synthetic_context"
					}
					a.Add(UserText{Text: bl.Text, Role: role})
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
			n := a.CallsUnder(p.CallID)
			// Only a literal Promise.allSettled array with a direct indexed
			// display proves which nested call produced each result. A combined
			// or transformed display still leaves every nested result unknown.
			if n > 1 {
				if source, nested := execInputs[p.CallID]; nested {
					if results, ok := CodexExecIndexedResults(source, p.Output, n); ok {
						for i := range n {
							a.Add(ToolResult{Key: p.CallID, Nth: i, Text: results[i].Text, IsError: results[i].Failed})
						}
					}
				}
				continue
			}
			if n != 1 {
				continue
			}
			text := CodexOutputText(p.Output)
			failed := false
			if r, ok := mcpEnds[p.CallID]; ok {
				text, failed = r.Text, r.Failed
			} else if inner, ok := CodexMCPEnvelope(text); ok {
				text = inner
			}
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
			a.Add(ToolResult{Key: p.CallID, Nth: 0, Text: text, IsError: failed})
		case p.Type == "function_call":
			var args map[string]json.RawMessage
			_ = json.Unmarshal([]byte(RawString(p.Arguments)), &args)
			add(CodexCall(*s, ln.Timestamp, p.Namespace, p.Name, args))
		case p.Type == "local_shell_call":
			add(trace.Call{Client: "codex", Session: s.ID, Time: ln.Timestamp, Tool: "shell", Command: strings.Join(p.Action.Command, " ")})
		case p.Type == "custom_tool_call" && p.Name == "exec":
			execInputs[p.CallID] = p.Input
			for _, inner := range JsToolCalls(p.Input) {
				add(CodexCall(*s, ln.Timestamp, "", inner.Name, inner.Args))
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
	return a.Finish(), sc.Err()
}

// codexMCPResult is an MCP call's result as Codex recorded it.
type codexMCPResult struct {
	Text   string
	Failed bool
}

// codexMCPEnd reads an mcp_tool_call_end result: {Ok: {content, isError}}
// or {Err: "message"}.
func codexMCPEnd(raw json.RawMessage) (codexMCPResult, bool) {
	var r struct {
		Ok *struct {
			Content           json.RawMessage `json:"content"`
			StructuredContent json.RawMessage `json:"structuredContent"`
			IsError           bool            `json:"isError"`
		} `json:"Ok"`
		Err *string `json:"Err"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &r) != nil {
		return codexMCPResult{}, false
	}
	switch {
	case r.Ok != nil:
		text := CodexOutputText(r.Ok.Content)
		if strings.TrimSpace(text) == "" && len(r.Ok.StructuredContent) > 0 && string(r.Ok.StructuredContent) != "null" {
			text = string(r.Ok.StructuredContent)
		}
		return codexMCPResult{Text: strings.TrimSuffix(text, "\n"), Failed: r.Ok.IsError}, true
	case r.Err != nil:
		return codexMCPResult{Text: *r.Err, Failed: true}, true
	}
	return codexMCPResult{}, false
}

// CodexMCPEnvelope unwraps the text Codex hands the model for a direct MCP
// call, "Wall time: X seconds\nOutput:\n" and the MCP content array as JSON,
// to the content's text. It is the fallback when no mcp_tool_call_end was
// recorded; a shell's output, which is not a content array, is left as is.
func CodexMCPEnvelope(text string) (string, bool) {
	if !strings.HasPrefix(text, "Wall time: ") {
		return "", false
	}
	_, body, ok := strings.Cut(text, "\nOutput:\n")
	if !ok {
		return "", false
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(body)), &parts) != nil || len(parts) == 0 {
		return "", false
	}
	var b []string
	for _, p := range parts {
		if p.Type == "text" {
			b = append(b, p.Text)
		}
	}
	return strings.Join(b, "\n"), true
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
