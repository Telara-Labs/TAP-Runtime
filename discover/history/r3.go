package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/discover/client"
	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

// R3 readers (TENG-3118): Cline, Roo Code and Kilo Code.
//
//   - The Cline CLI (3.x) keeps <Dir>/<id>/<id>.messages.json: Anthropic
//     blocks with a ts per message. MCP tools are named <server>__<tool>; a
//     result's content is the MCP result as JSON text {content, isError};
//     run_commands runs a list of commands, one result each. Read from a real
//     run (testdata/cline-cli).
//   - The VS Code extensions (Cline, Roo Code, Kilo Code) keep
//     globalStorage/<extension>/tasks/<id>/api_conversation_history.json, an
//     array of Anthropic messages ({role, content, ts}). MCP calls go
//     through the dispatcher use_mcp_tool {server_name, tool_name,
//     arguments}; execute_command {command} is the shell. Older Cline tasks
//     wrote the call as XML in the assistant's text
//     (<use_mcp_tool><server_name>…) and the result as the next user text
//     ("[use_mcp_tool for '…'] Result:"), paired by order. Synthetic
//     fixtures (testdata/SYNTHETIC.md): no extension is signed in here.

// ClineCLI reads the Cline CLI's sessions.
type ClineCLI struct {
	Dir     string   // ~/.cline/data/sessions
	Configs []string // cline_mcp_settings.json files that name MCP servers
}

func (ClineCLI) Client() string { return "cline" }

func (r ClineCLI) Read(since time.Time) ([]trace.Session, error) {
	ss, _, err := r.ReadWithStats(since)
	return ss, err
}

func (r ClineCLI) ReadWithStats(since time.Time) ([]trace.Session, trace.ReadStats, error) {
	var st trace.ReadStats
	files, _ := filepath.Glob(filepath.Join(r.Dir, "*", "*.messages.json"))
	servers := mcpServerNames(r.Configs, "mcpServers")
	var out []trace.Session
	for _, f := range files {
		if info, err := os.Stat(f); err != nil || info.ModTime().Before(since) {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			st.UnreadableFiles++
			continue
		}
		var doc struct {
			SessionID string           `json:"sessionId"`
			Messages  []anthropicTsMsg `json:"messages"`
		}
		if json.Unmarshal(b, &doc) != nil {
			st.UnreadableFiles++
			continue
		}
		id := doc.SessionID
		if id == "" {
			id = strings.TrimSuffix(filepath.Base(f), ".messages.json")
		}
		s := anthropicSession("cline", id, doc.Messages, servers)
		if len(s.Calls) == 0 || s.Start.Before(since) {
			continue
		}
		s.SourceDigest = FileDigest(f)
		out = append(out, s)
	}
	sortSessions(out)
	return out, st, nil
}

// ExtensionTasks reads a VS Code extension's task folders.
type ExtensionTasks struct {
	ID   string   // registry client
	Dirs []string // globalStorage/<extension>/tasks folders
}

func (r ExtensionTasks) Client() string { return r.ID }

func (r ExtensionTasks) Read(since time.Time) ([]trace.Session, error) {
	ss, _, err := r.ReadWithStats(since)
	return ss, err
}

func (r ExtensionTasks) ReadWithStats(since time.Time) ([]trace.Session, trace.ReadStats, error) {
	var st trace.ReadStats
	var out []trace.Session
	for _, dir := range r.Dirs {
		files, _ := filepath.Glob(filepath.Join(dir, "*", "api_conversation_history.json"))
		for _, f := range files {
			info, err := os.Stat(f)
			if err != nil || info.ModTime().Before(since) {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				st.UnreadableFiles++
				continue
			}
			var msgs []anthropicTsMsg
			if json.Unmarshal(b, &msgs) != nil {
				st.UnreadableFiles++
				continue
			}
			id := filepath.Base(filepath.Dir(f))
			s := anthropicSession(r.ID, id, msgs, nil)
			if s.Start.IsZero() {
				s.Start = info.ModTime().UTC()
			}
			if len(s.Calls) == 0 || s.Start.Before(since) {
				continue
			}
			s.SourceDigest = FileDigest(f)
			out = append(out, s)
		}
	}
	sortSessions(out)
	return out, st, nil
}

// ReaderSet lists several readers of one client as one: the Cline CLI and
// the Cline extension are one agent.
type ReaderSet struct {
	ID   string
	List []trace.Reader
}

func (r ReaderSet) Client() string { return r.ID }

func (r ReaderSet) Read(since time.Time) ([]trace.Session, error) {
	ss, _, err := r.ReadWithStats(since)
	return ss, err
}

func (r ReaderSet) ReadWithStats(since time.Time) ([]trace.Session, trace.ReadStats, error) {
	var st trace.ReadStats
	var out []trace.Session
	for _, x := range r.List {
		var ss []trace.Session
		var err error
		if sr, ok := x.(trace.StatReader); ok {
			var s trace.ReadStats
			ss, s, err = sr.ReadWithStats(since)
			st.UnreadableFiles += s.UnreadableFiles
		} else {
			ss, err = x.Read(since)
		}
		if err != nil {
			return out, st, err
		}
		out = append(out, ss...)
	}
	sortSessions(out)
	return out, st, nil
}

// anthropicTsMsg is an Anthropic message with a ts (ms).
type anthropicTsMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Ts      int64           `json:"ts"`
}

type anthropicBlock struct {
	Type      string                     `json:"type"`
	Text      string                     `json:"text"`
	ID        string                     `json:"id"`
	Name      string                     `json:"name"`
	Input     map[string]json.RawMessage `json:"input"`
	ToolUseID string                     `json:"tool_use_id"`
	Content   json.RawMessage            `json:"content"`
	IsError   bool                       `json:"is_error"`
}

var (
	userInputTag = regexp.MustCompile(`(?s)^\s*<user_input[^>]*>(.*?)</user_input>`)
	taskTag      = regexp.MustCompile(`(?s)<task>(.*?)</task>`)
	xmlMCP       = regexp.MustCompile(`(?s)<use_mcp_tool>\s*<server_name>(.*?)</server_name>\s*<tool_name>(.*?)</tool_name>\s*<arguments>(.*?)</arguments>\s*</use_mcp_tool>`)
	xmlCommand   = regexp.MustCompile(`(?s)<execute_command>\s*<command>(.*?)</command>`)
	xmlResult    = regexp.MustCompile(`(?s)^\s*\[(use_mcp_tool|execute_command)[^\]]*\] Result:\s*(.*)`)
)

// anthropicSession assembles Cline-family messages.
func anthropicSession(client, id string, msgs []anthropicTsMsg, servers []string) trace.Session {
	a := NewAssembler(client, id)
	var xmlPending []string // keys of XML calls still waiting, oldest first
	n := 0
	for _, m := range msgs {
		at := time.UnixMilli(m.Ts).UTC()
		if m.Ts > 0 {
			FirstTime(&a.S, at)
		}
		var blocks []anthropicBlock
		if s := jsonString(m.Content); s != "" {
			blocks = []anthropicBlock{{Type: "text", Text: s}}
		} else if json.Unmarshal(m.Content, &blocks) != nil {
			a.Skip()
			continue
		}
		for _, b := range blocks {
			switch {
			case b.Type == "text" && m.Role == "user":
				if res := xmlResult.FindStringSubmatch(b.Text); res != nil && len(xmlPending) > 0 {
					key := xmlPending[0]
					xmlPending = xmlPending[1:]
					a.Add(ToolResult{Key: key, Nth: -1, Text: strings.TrimSpace(res[2]), IsError: strings.HasPrefix(strings.TrimSpace(res[2]), "Error")})
					continue
				}
				text := b.Text
				if t := userInputTag.FindStringSubmatch(text); t != nil {
					text = t[1]
				} else if t := taskTag.FindStringSubmatch(text); t != nil {
					text = t[1]
				}
				a.Add(UserText{Text: strings.TrimSpace(text)})
			case b.Type == "text" && m.Role == "assistant":
				// Older Cline: the call is XML in the text.
				for _, x := range xmlMCP.FindAllStringSubmatch(b.Text, -1) {
					n++
					key := "xml" + itoa(n)
					var args map[string]json.RawMessage
					_ = json.Unmarshal([]byte(strings.TrimSpace(x[3])), &args)
					c := trace.Call{Session: id, Time: at}
					MCPCall(&c, strings.TrimSpace(x[1]), strings.TrimSpace(x[2]), args)
					a.Add(ToolCall{Key: key, Call: c})
					xmlPending = append(xmlPending, key)
				}
				for _, x := range xmlCommand.FindAllStringSubmatch(b.Text, -1) {
					n++
					key := "xml" + itoa(n)
					a.Add(ToolCall{Key: key, Call: trace.Call{Session: id, Time: at, Tool: "shell", Command: strings.TrimSpace(x[1])}})
					xmlPending = append(xmlPending, key)
				}
			case b.Type == "tool_use":
				for _, c := range clineCalls(b, id, at, servers) {
					a.Add(ToolCall{Key: b.ID, Call: c})
				}
			case b.Type == "tool_result":
				clineResults(a, b)
			}
		}
	}
	return a.Finish()
}

// clineCalls decodes one tool_use; run_commands makes one call per command.
func clineCalls(b anthropicBlock, session string, at time.Time, servers []string) []trace.Call {
	c := trace.Call{Session: session, ID: b.ID, Time: at}
	switch b.Name {
	case "run_commands":
		var cmds []string
		_ = json.Unmarshal(b.Input["commands"], &cmds)
		var out []trace.Call
		for _, cmd := range cmds {
			x := c
			x.Tool, x.Command = "shell", cmd
			out = append(out, x)
		}
		return out
	case "execute_command":
		c.Tool, c.Command = "shell", RawString(b.Input["command"])
	case "use_mcp_tool":
		if !Dispatcher(&c, b.Input, "server_name", "tool_name", "arguments") {
			c.Tool, c.Args, c.RawArgs = b.Name, Flatten(b.Input), RawKeys(b.Input)
		}
	default:
		server, tool := splitServerTool(b.Name, servers)
		if server != "" {
			MCPCall(&c, server, tool, b.Input)
		} else {
			c.Tool, c.Args, c.RawArgs = b.Name, Flatten(b.Input), RawKeys(b.Input)
		}
	}
	return []trace.Call{c}
}

// splitServerTool splits <server>__<tool>: by a configured server name when
// one matches, else at the first "__".
func splitServerTool(name string, servers []string) (string, string) {
	for _, s := range servers {
		if tool, ok := strings.CutPrefix(name, s+"__"); ok && tool != "" {
			return s, tool
		}
	}
	if server, tool, ok := strings.Cut(name, "__"); ok && server != "" && tool != "" {
		return server, tool
	}
	return "", ""
}

// clineResults records a tool_result: run_commands answers each command;
// an MCP result is the MCP result object as text.
func clineResults(a *Assembler, b anthropicBlock) {
	var cmds []struct {
		Result  string `json:"result"`
		Success *bool  `json:"success"`
	}
	if json.Unmarshal(b.Content, &cmds) == nil && len(cmds) > 0 && cmds[0].Success != nil && a.CallsUnder(b.ToolUseID) == len(cmds) {
		for i, r := range cmds {
			a.Add(ToolResult{Key: b.ToolUseID, Nth: i, Text: r.Result, IsError: r.Success == nil || !*r.Success})
		}
		return
	}
	text := ResultText(b.Content)
	isErr := b.IsError
	var mcp struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if json.Unmarshal([]byte(text), &mcp) == nil && mcp.Content != nil {
		var parts []string
		for _, c := range mcp.Content {
			parts = append(parts, c.Text)
		}
		text, isErr = strings.Join(parts, "\n"), isErr || mcp.IsError
	} else {
		var blocks []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(b.Content, &blocks) == nil && len(blocks) > 0 {
			var parts []string
			for _, x := range blocks {
				parts = append(parts, x.Text)
			}
			text = strings.Join(parts, "\n")
		}
	}
	a.Add(ToolResult{Key: b.ToolUseID, Nth: -1, Text: text, IsError: isErr})
}

// mcpServerNames reads the server names under key in JSON config files,
// longest first.
func mcpServerNames(configs []string, key string) []string {
	var out []string
	for _, f := range configs {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var doc map[string]json.RawMessage
		if json.Unmarshal(b, &doc) != nil {
			continue
		}
		var servers map[string]json.RawMessage
		if json.Unmarshal(doc[key], &servers) == nil {
			for k := range servers {
				out = append(out, k)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// vscodeFamilyStorage lists globalStorage/<extension>/tasks under every
// VS Code-family editor's User folder (the extensions install there too).
func vscodeFamilyStorage(home, extension string) []string {
	var out []string
	for _, editor := range []string{"Code", "Code - Insiders", "VSCodium", "Cursor", "Windsurf"} {
		for _, base := range []string{
			filepath.Join(home, "Library", "Application Support", editor, "User"),
			filepath.Join(home, ".config", editor, "User"),
			filepath.Join(appDataRoaming(home), editor, "User"),
		} {
			out = append(out, filepath.Join(base, "globalStorage", extension, "tasks"))
		}
	}
	return out
}

func appDataRoaming(home string) string { return client.AppData("APPDATA", home, "AppData/Roaming") }
