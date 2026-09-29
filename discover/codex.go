package discover

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
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

func (r Codex) Read(since time.Time) ([]Session, error) {
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
	var out []Session
	for _, f := range files {
		if info, err := os.Stat(f); err != nil || info.ModTime().Before(since) {
			continue
		}
		s, err := readCodexFile(f)
		if err != nil || len(s.Calls) == 0 || s.Start.Before(since) {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

type codexLine struct {
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Payload   struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		Message   string          `json:"message"`
		Name      string          `json:"name"`
		Namespace string          `json:"namespace"`
		Arguments json.RawMessage `json:"arguments"`
		Input     string          `json:"input"`
		Action    struct {
			Command []string `json:"command"`
		} `json:"action"`
	} `json:"payload"`
}

func readCodexFile(path string) (s Session, err error) {
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
	s = Session{Client: "codex", ID: strings.TrimSuffix(filepath.Base(path), ".jsonl")}
	turn, turnStart := 0, 0
	add := func(c Call) {
		c.Request = s.request()
		s.Calls = append(s.Calls, c)
	}
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 128<<20)
	for sc.Scan() {
		b := sc.Bytes()
		// Most lines are token counts and reasoning; skip them unparsed.
		if jsonHasAny(b, `"token_count"`) {
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
				spread(s.Calls, turnStart, turn, Usage{Fresh: l.Input - l.Cached, Cached: l.Cached, Output: l.Output})
				turn++
				turnStart = len(s.Calls)
			}
			continue
		}
		if !jsonHasAny(b, `"session_meta"`, `"function_call"`, `"custom_tool_call"`, `"local_shell_call"`, `"role":"user"`, `"role": "user"`) {
			continue
		}
		var ln codexLine
		if json.Unmarshal(b, &ln) != nil {
			continue
		}
		p := ln.Payload
		switch {
		case p.Type == "message" && p.Role == "user":
			var blocks []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			_ = json.Unmarshal(p.Content, &blocks)
			for _, bl := range blocks {
				if bl.Type == "input_text" && isRequest(bl.Text) {
					s.addRequest(bl.Text)
					break
				}
			}
		case ln.Type == "session_meta":
			if p.ID != "" {
				s.ID = p.ID
			}
			if s.Start.IsZero() {
				s.Start = ln.Timestamp
			}
		case p.Type == "function_call":
			var args map[string]json.RawMessage
			_ = json.Unmarshal([]byte(rawString(p.Arguments)), &args)
			add(codexCall(s, ln.Timestamp, p.Namespace, p.Name, args))
		case p.Type == "local_shell_call":
			add(Call{Client: "codex", Session: s.ID, Time: ln.Timestamp, Tool: "shell", Command: strings.Join(p.Action.Command, " ")})
		case p.Type == "custom_tool_call" && p.Name == "exec":
			for _, inner := range jsToolCalls(p.Input) {
				add(codexCall(s, ln.Timestamp, "", inner.name, inner.args))
			}
		case p.Type == "custom_tool_call":
			// apply_patch and other freeform tools: the input is the argument.
			add(Call{Client: "codex", Session: s.ID, Time: ln.Timestamp, Tool: p.Name, Args: map[string]string{"input": truncate(p.Input, maxArg)}})
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

func jsonHasAny(b []byte, subs ...string) bool {
	for _, s := range subs {
		if strings.Contains(string(b), s) {
			return true
		}
	}
	return false
}

func codexCall(s Session, t time.Time, namespace, name string, args map[string]json.RawMessage) Call {
	c := Call{Client: "codex", Session: s.ID, Time: t}
	// Codex writes a tool as namespace "mcp__server__" + name "tool", as
	// "mcp__server" + "tool", or as "mcp__server__app" + "_tool". All three
	// must join into mcp__server__...tool.
	full := namespace + name
	if namespace != "" && !strings.HasSuffix(namespace, "__") && !strings.HasPrefix(name, "_") {
		full = namespace + "__" + name
	}
	switch {
	case name == "exec_command" || name == "shell":
		c.Tool, c.Command = "shell", rawString(args["cmd"])
		if c.Command == "" {
			c.Command = rawString(args["command"])
		}
		var argv []string
		if json.Unmarshal(args["command"], &argv) == nil {
			c.Command = strings.Join(argv, " ")
		}
	case strings.HasPrefix(full, "mcp__"):
		c.Tool, c.Args, c.RawArgs = "mcp:"+strings.TrimPrefix(lastSegment(full), "_"), flatten(args), rawKeys(args)
	default:
		c.Tool, c.Args, c.RawArgs = name, flatten(args), rawKeys(args)
	}
	return c
}

type jsCall struct {
	name string
	args map[string]json.RawMessage
}

var jsToolRe = regexp.MustCompile(`tools\.([A-Za-z0-9_]+)\s*\(`)

// jsToolCalls finds each tools.<name>({...}) in a Codex exec body. The object
// literal is JavaScript, not JSON, so only its top-level string-valued keys are
// recovered; other values are recorded as their source text.
func jsToolCalls(src string) []jsCall {
	var out []jsCall
	for _, m := range jsToolRe.FindAllStringSubmatchIndex(src, -1) {
		name := src[m[2]:m[3]]
		rest := strings.TrimSpace(src[m[1]:])
		args := map[string]json.RawMessage{}
		switch {
		case strings.HasPrefix(rest, "{"):
			fields, quoted := jsObjectFieldsKinds(rest)
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
		case rest != "" && isQuote([]rune(rest)[0]):
			// tools.apply_patch(`*** Begin Patch ...`): one string argument.
			v, _ := readJSString([]rune(rest), 0)
			b, _ := json.Marshal(v)
			args["input"] = b
		}
		out = append(out, jsCall{name: name, args: args})
	}
	return out
}

// jsObjectFields reads the top-level key: value pairs of a JavaScript object
// literal starting at src[0] == '{'. String values are unquoted; anything else
// is kept as its source text. Input it cannot follow ends the scan early.
func jsObjectFields(src string) map[string]string {
	out, _ := jsObjectFieldsKinds(src)
	return out
}

// jsObjectFieldsKinds is jsObjectFields that also reports which values were
// string literals.
func jsObjectFieldsKinds(src string) (map[string]string, map[string]bool) {
	out := map[string]string{}
	quoted := map[string]bool{}
	rs := []rune(src)
	if len(rs) == 0 || rs[0] != '{' {
		return out, quoted
	}
	i := 1
	for {
		for i < len(rs) && (isSpace(rs[i]) || rs[i] == ',') {
			i++
		}
		if i >= len(rs) || rs[i] == '}' {
			return out, quoted
		}
		var key string
		switch {
		case isQuote(rs[i]):
			key, i = readJSString(rs, i)
		case isIdentStart(rs[i]):
			j := i
			for j < len(rs) && (isIdentStart(rs[j]) || (rs[j] >= '0' && rs[j] <= '9')) {
				j++
			}
			key, i = string(rs[i:j]), j
		default:
			// A spread, a computed key or something else: skip one value. A
			// stray closing bracket cannot be skipped this way; stop there.
			j := scanJSValue(rs, i)
			if j <= i {
				return out, quoted
			}
			i = j
			continue
		}
		for i < len(rs) && isSpace(rs[i]) {
			i++
		}
		if i >= len(rs) || rs[i] != ':' {
			// Shorthand property {cmd}: the value is a variable of that name.
			out[key] = key
			continue
		}
		i++
		for i < len(rs) && isSpace(rs[i]) {
			i++
		}
		if i >= len(rs) {
			return out, quoted
		}
		if isQuote(rs[i]) {
			out[key], i = readJSString(rs, i)
			quoted[key] = true
			continue
		}
		j := scanJSValue(rs, i)
		out[key] = strings.TrimSpace(string(rs[i:j]))
		if j <= i {
			return out, quoted
		}
		i = j
	}
}

// readJSString reads the string literal opening at rs[i] and returns its
// unescaped text and the index after its closing quote.
func readJSString(rs []rune, i int) (string, int) {
	q := rs[i]
	var b strings.Builder
	j := i + 1
	for ; j < len(rs) && rs[j] != q; j++ {
		if rs[j] == '\\' && j+1 < len(rs) {
			j++
			switch rs[j] {
			case 'n':
				b.WriteRune('\n')
			case 't':
				b.WriteRune('\t')
			default:
				b.WriteRune(rs[j])
			}
			continue
		}
		b.WriteRune(rs[j])
	}
	return b.String(), min(j+1, len(rs))
}

// scanJSValue returns the index of the ',' or '}' that ends the value
// starting at rs[i], skipping nested brackets and strings.
func scanJSValue(rs []rune, i int) int {
	depth := 0
	for i < len(rs) {
		switch c := rs[i]; {
		case isQuote(c):
			_, i = readJSString(rs, i)
			continue
		case c == '{' || c == '[' || c == '(':
			depth++
		case c == '}' || c == ']' || c == ')':
			if depth == 0 {
				return i
			}
			depth--
		case c == ',' && depth == 0:
			return i
		}
		i++
	}
	return i
}

func isQuote(c rune) bool { return c == '"' || c == '\'' || c == '`' }

func isSpace(c rune) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func isIdentStart(c rune) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
