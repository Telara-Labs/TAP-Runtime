package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// R6 readers (TENG-3121): each agent needs a different way in.

// Windsurf reads Windsurf (Devin Desktop) conversations captured by its
// post_cascade_response_with_transcript hook (HookCapture). With the hook
// configured, Windsurf writes the whole conversation to
// ~/.windsurf/transcripts/<trajectory_id>.jsonl and keeps the 100 newest;
// `tap hook windsurf` copies each one to ~/.tap/windsurf/transcripts so
// pruning loses nothing. A line is one step, its data under a key named
// after its type: user_input {user_response}, planner_response {response},
// mcp_tool_use {mcp_server_name, mcp_tool_name, mcp_tool_arguments, status},
// run_command (command_execution {command_line}). The transcript records no
// tool result, so calls have no outcome. (docs.devin.ai/desktop/cascade/hooks;
// synthetic fixture: Windsurf needs a sign-in.)
type Windsurf struct{ Dirs []string }

func (Windsurf) Client() string { return "windsurf" }

func (r Windsurf) Read(since time.Time) ([]trace.Session, error) {
	ss, _, err := r.ReadWithStats(since)
	return ss, err
}

func (r Windsurf) ReadWithStats(since time.Time) ([]trace.Session, trace.ReadStats, error) {
	var st trace.ReadStats
	// The newest copy of a trajectory wins: Windsurf rewrites it whole.
	newest := map[string]string{}
	for _, d := range r.Dirs {
		files, _ := filepath.Glob(filepath.Join(d, "*.jsonl"))
		for _, f := range files {
			id := strings.TrimSuffix(filepath.Base(f), ".jsonl")
			if cur, ok := newest[id]; !ok || fileSize(f) > fileSize(cur) {
				newest[id] = f
			}
		}
	}
	var out []trace.Session
	for id, f := range newest {
		info, err := os.Stat(f)
		if err != nil || info.ModTime().Before(since) {
			continue
		}
		s, err := readWindsurfFile(f, id, info.ModTime().UTC())
		if err != nil {
			st.UnreadableFiles++
			continue
		}
		if len(s.Calls) == 0 {
			continue
		}
		s.SourceDigest = FileDigest(f)
		out = append(out, s)
	}
	sortSessions(out)
	return out, st, nil
}

func fileSize(p string) int64 {
	if info, err := os.Stat(p); err == nil {
		return info.Size()
	}
	return -1
}

func readWindsurfFile(path, id string, at time.Time) (trace.Session, error) {
	fh, err := os.Open(path)
	if err != nil {
		return trace.Session{}, err
	}
	defer fh.Close()
	a := NewAssembler("windsurf", id)
	a.S.Start = at // the transcript holds no times
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	n := 0
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var step struct {
			Type      string `json:"type"`
			UserInput *struct {
				UserResponse string `json:"user_response"`
			} `json:"user_input"`
			MCP *struct {
				Server string                     `json:"mcp_server_name"`
				Tool   string                     `json:"mcp_tool_name"`
				Args   map[string]json.RawMessage `json:"mcp_tool_arguments"`
				Result json.RawMessage            `json:"mcp_result"`
			} `json:"mcp_tool_use"`
			Command *struct {
				CommandLine string `json:"command_line"`
			} `json:"command_execution"`
		}
		if json.Unmarshal(sc.Bytes(), &step) != nil {
			a.Skip()
			continue
		}
		switch {
		case step.UserInput != nil:
			a.Add(UserText{Text: step.UserInput.UserResponse})
		case step.MCP != nil:
			n++
			key := fmt.Sprintf("w%d", n)
			c := trace.Call{Session: id, Time: at}
			MCPCall(&c, step.MCP.Server, step.MCP.Tool, step.MCP.Args)
			a.Add(ToolCall{Key: key, Call: c})
			if len(step.MCP.Result) > 0 {
				a.Add(ToolResult{Key: key, Nth: 0, Text: ResultText(step.MCP.Result)})
			}
		case step.Command != nil:
			n++
			a.Add(ToolCall{Key: fmt.Sprintf("w%d", n), Call: trace.Call{Session: id, Time: at, Tool: "shell", Command: step.Command.CommandLine}})
		}
	}
	return a.Finish(), sc.Err()
}

// Amp reads Amp threads through Amp's own CLI (CLIExport): since builds of
// 2026-03-31 threads live on ampcode.com, so `amp threads list --json` and
// `amp threads export <id>` (the person's own login) are the way in; older
// builds kept ~/.local/share/amp/threads/T-*.json, read directly. A thread
// is {id, created, messages: [{role, content: [text | tool_use {id, name,
// input} | tool_result {toolUseID, run {status, result}}]}]}; MCP tools are
// named mcp__<server>__<tool>. (vshulcz/deja-vu amp.go; synthetic fixture:
// Amp needs a sign-in.) Without the amp program the CLI part is skipped.
type Amp struct {
	Bin string // the amp program; "" looks it up on PATH
	Dir string // legacy threads folder
}

func (Amp) Client() string { return "amp" }

func (r Amp) Read(since time.Time) ([]trace.Session, error) {
	ss, _, err := r.ReadWithStats(since)
	return ss, err
}

func (r Amp) ReadWithStats(since time.Time) ([]trace.Session, trace.ReadStats, error) {
	var st trace.ReadStats
	docs := map[string][]byte{}
	files, _ := filepath.Glob(filepath.Join(r.Dir, "T-*.json"))
	for _, f := range files {
		if b, err := os.ReadFile(f); err == nil {
			docs[strings.TrimSuffix(filepath.Base(f), ".json")] = b
		}
	}
	bin := r.Bin
	if bin == "" {
		bin, _ = exec.LookPath("amp")
	}
	if bin != "" {
		if list, err := exec.Command(bin, "threads", "list", "--json").Output(); err == nil {
			var threads []struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(list, &threads) == nil {
				for _, t := range threads {
					if !strings.HasPrefix(t.ID, "T-") || strings.ContainsAny(t.ID, " /\\") {
						continue
					}
					b, err := exec.Command(bin, "threads", "export", t.ID).Output()
					if err != nil {
						st.UnreadableFiles++
						continue
					}
					docs[t.ID] = b
				}
			}
		}
	}
	var out []trace.Session
	for id, b := range docs {
		s, err := AmpThread(id, b)
		if err != nil {
			st.UnreadableFiles++
			continue
		}
		if len(s.Calls) == 0 || s.Start.Before(since) {
			continue
		}
		s.SourceDigest = hexSum(string(b))
		out = append(out, s)
	}
	sortSessions(out)
	return out, st, nil
}

// AmpThread decodes one exported thread.
func AmpThread(id string, b []byte) (trace.Session, error) {
	var doc struct {
		ID       string `json:"id"`
		Created  int64  `json:"created"`
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string                     `json:"type"`
				Text      string                     `json:"text"`
				ID        string                     `json:"id"`
				Name      string                     `json:"name"`
				Input     map[string]json.RawMessage `json:"input"`
				ToolUseID string                     `json:"toolUseID"`
				Run       *struct {
					Status string          `json:"status"`
					Result json.RawMessage `json:"result"`
					Error  json.RawMessage `json:"error"`
				} `json:"run"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return trace.Session{}, err
	}
	if doc.ID != "" {
		id = doc.ID
	}
	a := NewAssembler("amp", id)
	a.S.Start = time.UnixMilli(doc.Created).UTC()
	for _, m := range doc.Messages {
		for _, p := range m.Content {
			switch {
			case p.Type == "text" && m.Role == "user":
				a.Add(UserText{Text: p.Text})
			case p.Type == "tool_use":
				c := trace.Call{Session: id, ID: p.ID, Time: a.S.Start}
				if p.Name == "Bash" {
					c.Tool, c.Command = "shell", RawString(p.Input["cmd"])
					if c.Command == "" {
						c.Command = RawString(p.Input["command"])
					}
				} else {
					c.Tool, c.Command, c.Args, c.RawArgs, c.MCPServer, c.MCPTool = DoubleUnderscore(p.Name, p.Input)
				}
				a.Add(ToolCall{Key: p.ID, Call: c})
			case p.Type == "tool_result" && p.Run != nil:
				text := ResultText(p.Run.Result)
				if text == "" {
					text = ResultText(p.Run.Error)
				}
				a.Add(ToolResult{Key: p.ToolUseID, Nth: -1, Text: text, IsError: p.Run.Status == "error" || p.Run.Status == "cancelled"})
			}
		}
	}
	return a.Finish(), nil
}

// Aider reads Aider's chat history (Markdown): .aider.chat.history.md in the
// home folder and at project roots. Aider makes no MCP or tool calls; a
// `/run <command>` the person typed is a shell call, and an applied edit is
// an edit of that file. Project roots are found by a bounded walk of the
// home folder, at most six levels down, skipping hidden folders, system and
// dependency trees (TENG-3167; a fixed depth of 3 missed every project at
// ~/Desktop/Projects/<org>/<repo>). Read from a real run (testdata/aider).
type Aider struct {
	Home  string
	Depth int // how many folder levels below home to look; 0 means 6
}

// aiderSkip are folders that never hold a project's chat history: system
// and package trees, dependency and build output, caches. Hidden folders
// are skipped too (TENG-3167).
var aiderSkip = map[string]bool{"Library": true, "Applications": true, "node_modules": true, "vendor": true,
	"venv": true, "site-packages": true, "__pycache__": true, "target": true, "dist": true, "build": true, "Pictures": true, "Movies": true, "Music": true}

// aiderBudget bounds the folders visited, so a large home stays fast.
const aiderBudget = 200000

// aiderHistories finds .aider.chat.history.md files under home, at most
// depth folder levels down. Aider writes the file into the project folder,
// which is often 4 or more levels down (~/Desktop/Projects/<org>/<repo>).
func aiderHistories(home string, depth int) []string {
	var files []string
	visited := 0
	base := strings.Count(filepath.Clean(home), string(filepath.Separator))
	filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p == home {
				return nil
			}
			visited++
			name := d.Name()
			if visited > aiderBudget || aiderSkip[name] || strings.HasPrefix(name, ".") ||
				strings.Count(p, string(filepath.Separator))-base > depth {
				return fs.SkipDir
			}
			return nil
		}
		if d.Name() == ".aider.chat.history.md" {
			files = append(files, p)
		}
		return nil
	})
	sort.Strings(files)
	return files
}

func (Aider) Client() string { return "aider" }

func (r Aider) Read(since time.Time) ([]trace.Session, error) {
	ss, _, err := r.ReadWithStats(since)
	return ss, err
}

func (r Aider) ReadWithStats(since time.Time) ([]trace.Session, trace.ReadStats, error) {
	var st trace.ReadStats
	depth := r.Depth
	if depth == 0 {
		depth = 6
	}
	files := aiderHistories(r.Home, depth)
	var out []trace.Session
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil || info.ModTime().Before(since) {
			continue
		}
		ss, err := readAiderFile(f)
		if err != nil {
			st.UnreadableFiles++
			continue
		}
		for _, s := range ss {
			if len(s.Calls) > 0 && !s.Start.Before(since) {
				out = append(out, s)
			}
		}
	}
	sortSessions(out)
	return out, st, nil
}

var (
	aiderStart   = regexp.MustCompile(`^# aider chat started at (\d{4}-\d\d-\d\d \d\d:\d\d:\d\d)`)
	aiderApplied = regexp.MustCompile(`^> Applied edit to (.+?)\s*$`)
)

// readAiderFile splits a history into its chats: each "# aider chat started
// at" begins one session.
func readAiderFile(path string) ([]trace.Session, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	digest := FileDigest(path)
	var out []trace.Session
	var a *Assembler
	var at time.Time
	var pending string // a /run command waiting for its output
	var output []string
	flush := func() {
		if a != nil && pending != "" {
			a.Add(ToolResult{Key: pending, Nth: 0, Text: strings.Join(output, "\n")})
		}
		pending, output = "", nil
	}
	n := 0
	finish := func() {
		flush()
		if a != nil {
			s := a.Finish()
			s.SourceDigest = hexSum(digest + s.ID)
			out = append(out, s)
		}
	}
	for _, line := range strings.Split(string(b), "\n") {
		if m := aiderStart.FindStringSubmatch(line); m != nil {
			finish()
			at, _ = time.ParseInLocation("2006-01-02 15:04:05", m[1], time.Local)
			a = NewAssembler("aider", filepath.Dir(path)+"@"+m[1])
			a.S.Start = at.UTC()
			continue
		}
		if a == nil {
			continue
		}
		switch {
		case strings.HasPrefix(line, "#### "):
			flush()
			msg := strings.TrimSpace(strings.TrimPrefix(line, "#### "))
			if cmd, ok := strings.CutPrefix(msg, "/run "); ok {
				n++
				pending = fmt.Sprintf("a%d", n)
				a.Add(ToolCall{Key: pending, Call: trace.Call{Time: at.UTC(), Tool: "shell", Command: strings.TrimSpace(cmd)}})
				continue
			}
			if !strings.HasPrefix(msg, "/") {
				a.Add(UserText{Text: msg})
			}
		case aiderApplied.MatchString(line):
			flush()
			file := aiderApplied.FindStringSubmatch(line)[1]
			n++
			key := fmt.Sprintf("a%d", n)
			args := map[string]json.RawMessage{"file_path": json.RawMessage(fmt.Sprintf("%q", file))}
			a.Add(ToolCall{Key: key, Call: trace.Call{Time: at.UTC(), Tool: "edit", Args: Flatten(args), RawArgs: RawKeys(args)}})
			a.Add(ToolResult{Key: key, Nth: 0, Text: strings.TrimPrefix(line, "> ")})
		case pending != "" && strings.HasPrefix(line, "> "):
			output = append(output, strings.TrimPrefix(line, "> "))
		}
	}
	finish()
	return out, nil
}
