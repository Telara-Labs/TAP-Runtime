package history

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
	"gitlab.com/telara-labs/tap-runtime/discover/util"
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
	ss, _, err := r.ReadWithStats(since)
	return ss, err
}

func (r Antigravity) ReadWithStats(since time.Time) ([]trace.Session, trace.ReadStats, error) {
	var st trace.ReadStats
	files, err := filepath.Glob(filepath.Join(r.Dir, "*", ".system_generated", "logs", "transcript_full.jsonl"))
	if err != nil {
		return nil, st, err
	}
	var out []trace.Session
	for _, f := range files {
		if info, err := os.Stat(f); err != nil || info.ModTime().Before(since) {
			continue
		}
		s, err := ReadAntigravityFile(f)
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
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Start.Equal(out[j].Start) {
			return out[i].Start.Before(out[j].Start)
		}
		return out[i].ID < out[j].ID
	})
	return out, st, nil
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
	d := AntigravityDecoder{Conversation: conv, Usage: AntigravityUsage(antigravityStateDB(conv))}
	a := NewAssembler("antigravity", filepath.Base(conv))
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	line := 0
	for sc.Scan() {
		line++
		var st AntigravityStep
		if json.Unmarshal(sc.Bytes(), &st) != nil {
			a.Skip() // a torn line, e.g. the last one while it is being written
			continue
		}
		FirstTime(&a.S, st.CreatedAt)
		for _, e := range d.Events(st, a.S.ID, line) {
			a.Add(e)
		}
	}
	return a.Finish(), sc.Err()
}

// AntigravityDecoder turns steps into events.
type AntigravityDecoder struct {
	Conversation string // the conversation folder, where large outputs are saved
	// Usage is each model generation's token use, keyed by the last step
	// index it saw: the step it produced is that index + 1.
	Usage map[int]trace.Usage
}

func antigravityKey(step int) string { return "step" + strconv.Itoa(step) }

// Bookkeeping fields Antigravity adds to every call's arguments for its UI.
var antigravityUIArgs = []string{"toolAction", "toolSummary"}

// Events decodes one step, read from line (one-based) of the transcript.
func (d *AntigravityDecoder) Events(st AntigravityStep, session string, line int) []Event {
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
		r := ToolResult{Key: antigravityKey(st.Index - 1), Nth: 0, Line: line, Text: d.output(st.Content), IsError: st.Type == "ERROR_MESSAGE"}
		if st.Status == "RUNNING" { // never finished: its outcome is not known
			r.HasOutcome = true
		}
		return []Event{r}
	}
	var out []Event
	// The generation that wrote this step saw everything up to the step
	// before it (on this machine all 144 planner steps had one).
	turn := ""
	if u, ok := d.Usage[st.Index-1]; ok && len(st.ToolCalls) > 0 {
		turn = "gen" + strconv.Itoa(st.Index-1)
		out = append(out, TurnUsage{Turn: turn, Usage: u})
	}
	for _, tc := range st.ToolCalls {
		key := antigravityKey(st.Index)
		// The step index is the call's id: its result is step index+1.
		c := trace.Call{Session: session, ID: key, Time: st.CreatedAt, Src: trace.CallSource{CallLine: line}}
		args := Without(tc.Args, antigravityUIArgs...)
		switch {
		case tc.Name == "run_command":
			c.Tool, c.Command = "shell", RawString(args["CommandLine"])
		case tc.Name == "call_mcp_tool" && Dispatcher(&c, args, "ServerName", "ToolName", "Arguments"):
		default:
			c.Tool, c.Args, c.RawArgs = tc.Name, Flatten(args), RawKeys(args)
		}
		out = append(out, ToolCall{Key: key, Turn: turn, Call: c})
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

// antigravityStateDB is the conversation's state database, kept beside the
// brain folder: <antigravity>/conversations/<id>.db.
func antigravityStateDB(conv string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(conv)), "conversations", filepath.Base(conv)+".db")
}

// AntigravityUsage reads each model generation's token use from the
// conversation's state database (table gen_metadata, one protobuf row per
// generation). Field numbers come from the descriptor shipped in
// Antigravity's language_server: the row's field 1 holds a ModelUsageStats
// at field 4 (2 input_tokens, 3 output_tokens, 4 cache_write_tokens,
// 5 cache_read_tokens) and metadata pairs at field 20, among them
// last_step_index. Without sqlite3 or the database there is no usage.
func AntigravityUsage(db string) map[int]trace.Usage {
	if _, err := os.Stat(db); err != nil {
		return nil
	}
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		return nil
	}
	raw, err := exec.Command(bin, "-readonly", "-json", util.SQLiteURI(db), `SELECT hex(data) AS data FROM gen_metadata`).Output()
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var rows []struct{ Data string }
	if json.Unmarshal(raw, &rows) != nil {
		return nil
	}
	out := map[int]trace.Usage{}
	for _, r := range rows {
		b, err := hex.DecodeString(r.Data)
		if err != nil {
			continue
		}
		if last, u, ok := AntigravityGeneration(b); ok {
			out[last] = u
		}
	}
	return out
}

// AntigravityGeneration decodes one gen_metadata row: the last step index
// the generation saw, and its token use.
func AntigravityGeneration(row []byte) (last int, u trace.Usage, ok bool) {
	last = -1
	for _, f := range ProtoFields(row) {
		if f.Num != 1 {
			continue
		}
		for _, g := range ProtoFields(f.Bytes) {
			switch g.Num {
			case 4:
				var in, out, write, read uint64
				for _, h := range ProtoFields(g.Bytes) {
					switch h.Num {
					case 2:
						in = h.Int
					case 3:
						out = h.Int
					case 4:
						write = h.Int
					case 5:
						read = h.Int
					}
				}
				u = trace.Usage{Fresh: float64(in + write), Cached: float64(read), Output: float64(out)}
			case 20:
				var k, v string
				for _, h := range ProtoFields(g.Bytes) {
					switch h.Num {
					case 1:
						k = string(h.Bytes)
					case 2:
						v = string(h.Bytes)
					}
				}
				if k == "last_step_index" {
					if n, err := strconv.Atoi(v); err == nil {
						last = n
					}
				}
			}
		}
	}
	return last, u, last >= 0 && u.Total() > 0
}
