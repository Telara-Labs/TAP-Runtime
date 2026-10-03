package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// hookCommand is `host hook gemini`: Gemini CLI runs it after every tool
// call (AfterTool), with the call on standard input. For a call that is not
// part of a TAP run it answers nothing and Gemini carries on. For one that is,
// it carries the result to the run and asks Gemini to make the next call, or,
// at the end, to call tap_result, whose short answer replaces the chain.
func hookCommand(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "windsurf" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(stderr, "tap hook windsurf:", err)
			return 0
		}
		return windsurfHook(stdin, stderr, home)
	}
	if len(args) != 1 || args[0] != "gemini" {
		fmt.Fprintln(stderr, "usage: tap hook gemini|windsurf")
		return 2
	}
	dir, err := relayDir()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	out, err := geminiAfterTool(stdin, dir)
	if err != nil {
		// A hook that fails must not break the user's session: say why on
		// standard error, which Gemini logs, and change nothing.
		fmt.Fprintln(stderr, "tap hook:", err)
		fmt.Fprintln(stdout, "{}")
		return 0
	}
	b, _ := json.Marshal(out)
	fmt.Fprintln(stdout, string(b))
	return 0
}

type geminiHookInput struct {
	Event        string         `json:"hook_event_name"`
	ToolName     string         `json:"tool_name"`
	ToolInput    map[string]any `json:"tool_input"`
	ToolResponse struct {
		LLMContent json.RawMessage `json:"llmContent"`
		Error      json.RawMessage `json:"error"`
	} `json:"tool_response"`
}

// tailCall is the output that makes Gemini run one more tool, whose result
// replaces the one just made.
func tailCall(name string, args map[string]any) map[string]any {
	return map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":       "AfterTool",
		"tailToolCallRequest": map[string]any{"name": name, "args": args},
	}}
}

func geminiAfterTool(stdin io.Reader, dir string) (map[string]any, error) {
	var in geminiHookInput
	if err := json.NewDecoder(stdin).Decode(&in); err != nil {
		return nil, fmt.Errorf("reading the hook input: %w", err)
	}
	if in.Event != "" && in.Event != "AfterTool" {
		return map[string]any{}, nil
	}
	text := geminiUnwrap(llmText(in.ToolResponse.LLMContent))

	// tap_run answered that its run waits on a call: ask Gemini to make it.
	if strings.HasSuffix(in.ToolName, "tap_run") {
		if _, call, ok := relayPending(text); ok {
			return tailCall(call.Name, call.Args), nil
		}
		return map[string]any{}, nil
	}

	// Any other call: is a run waiting on exactly this one?
	p, ok := findPending(dir, in.ToolName, in.ToolInput)
	if !ok {
		return map[string]any{}, nil
	}
	msg := hookMessage{Run: p.Run, Text: text}
	if len(in.ToolResponse.Error) > 0 && string(in.ToolResponse.Error) != "null" {
		msg.Error = errorText(in.ToolResponse.Error)
	}
	reply, err := deliver(p.Socket, msg)
	if err != nil {
		return nil, err
	}
	switch {
	case reply.Error != "":
		return nil, fmt.Errorf("%s", reply.Error)
	case reply.Finish:
		name := reply.ResultTool
		if name == "" {
			name = geminiToolName("tap", "tap_result")
		}
		return tailCall(name, map[string]any{"run": p.Run}), nil
	case reply.Call != nil:
		return tailCall(reply.Call.Name, reply.Call.Args), nil
	}
	return map[string]any{}, nil
}

// findPending looks for a run waiting on this exact call: the same tool with
// the same arguments. A call the model made on its own, with other
// arguments, is left alone.
func findPending(dir, name string, args map[string]any) (pendingFile, bool) {
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	want := canonical(args)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var p pendingFile
		if json.Unmarshal(raw, &p) != nil {
			continue
		}
		if p.Call.Name == name && canonical(p.Call.Args) == want {
			return p, true
		}
	}
	return pendingFile{}, false
}

func canonical(m map[string]any) string {
	if m == nil {
		m = map[string]any{}
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func deliver(socket string, m hookMessage) (hookReply, error) {
	c, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return hookReply{}, fmt.Errorf("the run's server is not reachable: %w", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Minute))
	b, _ := json.Marshal(m)
	if _, err := c.Write(append(b, '\n')); err != nil {
		return hookReply{}, err
	}
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	if !sc.Scan() {
		return hookReply{}, fmt.Errorf("the run's server closed without answering")
	}
	var r hookReply
	if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
		return hookReply{}, err
	}
	return r, nil
}

// llmText is the text of a tool's result as Gemini holds it: a string, one
// part, or a list of parts, each with a text field.
func llmText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	type part struct {
		Text string `json:"text"`
	}
	var parts []part
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	var one part
	if json.Unmarshal(raw, &one) == nil {
		return one.Text
	}
	return string(raw)
}

// geminiUnwrap removes the one layer Gemini CLI (0.62) puts around every MCP
// result it hands the model, <untrusted_context>\n…\n</untrusted_context>,
// so the runner reads the tool's own text (TENG-3058, found live).
func geminiUnwrap(s string) string {
	const open, close = "<untrusted_context>\n", "\n</untrusted_context>"
	if strings.HasPrefix(s, open) && strings.HasSuffix(s, close) && len(s) >= len(open)+len(close) {
		return s[len(open) : len(s)-len(close)]
	}
	return s
}

func errorText(raw json.RawMessage) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Message != "" {
		return e.Message
	}
	return strings.Trim(string(raw), `"`)
}
