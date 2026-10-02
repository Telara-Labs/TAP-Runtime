package history

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// Antigravity reads Google Antigravity conversations (TENG-3112):
// <Dir>/<conversation>/.system_generated/logs/transcript_full.jsonl, one
// step per line:
//
//	{step_index, source, type, status, created_at, content, tool_calls: [{name, args}]}
//
// USER_INPUT steps hold the person's message inside <USER_REQUEST>. A
// PLANNER_RESPONSE step may make one tool call; its result is the very next
// step, step_index+1, a GENERIC step (or an ERROR_MESSAGE, a failure). Some
// result steps are never written (the index is skipped), so results are
// paired by index, never by order: on this machine 131 of 136 calls had
// their result at k+1 and the other 5 had no step k+1 at all. MCP calls go
// through the dispatcher call_mcp_tool{ServerName, ToolName, Arguments}.
type Antigravity struct{ Dir string }

func (Antigravity) Client() string { return "antigravity" }

func (r Antigravity) Read(since time.Time) ([]trace.Session, error) {
	files, err := filepath.Glob(filepath.Join(r.Dir, "*", ".system_generated", "logs", "transcript_full.jsonl"))
	if err != nil {
		return nil, err
	}
	var out []trace.Session
	for _, f := range files {
		if info, err := os.Stat(f); err != nil || info.ModTime().Before(since) {
			continue
		}
		s, err := ReadAntigravityFile(f)
		if err != nil || len(s.Calls) == 0 || s.Start.Before(since) {
			continue
		}
		s.SourceDigest = FileDigest(f)
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Start.Equal(out[j].Start) {
			return out[i].Start.Before(out[j].Start)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// AntigravityStep is one transcript line.
type AntigravityStep struct {
	Index     int       `json:"step_index"`
	Source    string    `json:"source"`
	Type      string    `json:"type"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	Content   string    `json:"content"`
	ToolCalls []struct {
		Name string                     `json:"name"`
		Args map[string]json.RawMessage `json:"args"`
	} `json:"tool_calls"`
}

// The conversation folder is three levels above the transcript.
func antigravityConversation(path string) string {
	return filepath.Dir(filepath.Dir(filepath.Dir(path)))
}

// ReadAntigravityFile reads one conversation's transcript.
func ReadAntigravityFile(path string) (s trace.Session, err error) {
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
	conv := antigravityConversation(path)
	d := AntigravityDecoder{Conversation: conv}
	a := NewAssembler("antigravity", filepath.Base(conv))
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var st AntigravityStep
		if json.Unmarshal(sc.Bytes(), &st) != nil {
			continue // a torn last line while the conversation is written
		}
		FirstTime(&a.S, st.CreatedAt)
		for _, e := range d.Events(st, a.S.ID) {
			a.Add(e)
		}
	}
	return a.Finish(), sc.Err()
}

// AntigravityDecoder turns steps into events.
type AntigravityDecoder struct {
	Conversation string // the conversation folder, where large outputs are saved
}

func antigravityKey(step int) string { return "step" + strconv.Itoa(step) }

// Bookkeeping fields Antigravity adds to every call's arguments for its UI.
var antigravityUIArgs = []string{"toolAction", "toolSummary"}

func (d *AntigravityDecoder) Events(st AntigravityStep, session string) []Event {
	switch st.Type {
	case "USER_INPUT":
		text, ok := Envelope(st.Content, "USER_REQUEST")
		if !ok {
			text = st.Content
		}
		return []Event{UserText{Text: text}}
	case "GENERIC", "ERROR_MESSAGE":
		// Answers the call made by the step before, if there was one; an
		// error after a step with no call ("stream interrupted") answers
		// nothing and is dropped by the assembler.
		r := ToolResult{Key: antigravityKey(st.Index - 1), Nth: 0, Text: d.output(st.Content), IsError: st.Type == "ERROR_MESSAGE"}
		if st.Status == "RUNNING" { // never finished: its outcome is not known
			r.HasOutcome = true
		}
		return []Event{r}
	}
	var out []Event
	for _, tc := range st.ToolCalls {
		key := antigravityKey(st.Index)
		c := trace.Call{Session: session, Time: st.CreatedAt}
		args := Without(tc.Args, antigravityUIArgs...)
		switch {
		case tc.Name == "run_command":
			c.Tool, c.Command = "shell", RawString(args["CommandLine"])
		case tc.Name == "call_mcp_tool" && Dispatcher(&c, args, "ServerName", "ToolName", "Arguments"):
		default:
			c.Tool, c.Args, c.RawArgs = tc.Name, Flatten(args), RawKeys(args)
		}
		out = append(out, ToolCall{Key: key, Call: c})
	}
	return out
}

var antigravitySaved = regexp.MustCompile(`The output was large and was saved to: (file://\S+)`)

// maxSavedOutput bounds how much of a saved large output is read.
const maxSavedOutput = 64 << 10

// output is a result's text. A large output is saved to a file inside the
// conversation folder and the step only names it; that file is read, never
// one outside the folder.
func (d *AntigravityDecoder) output(content string) string {
	m := antigravitySaved.FindStringSubmatch(content)
	if m == nil || d.Conversation == "" {
		return content
	}
	u, err := url.Parse(m[1])
	if err != nil || u.Scheme != "file" {
		return content
	}
	p := filepath.Clean(u.Path)
	conv, err := filepath.EvalSymlinks(d.Conversation)
	if err != nil {
		return content
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil || !strings.HasPrefix(real, conv+string(filepath.Separator)) {
		return content
	}
	f, err := os.Open(real)
	if err != nil {
		return content
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSavedOutput))
	if err != nil {
		return content
	}
	return content + "\n" + string(b)
}
