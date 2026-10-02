// Package client is the one list of coding agents discover knows about: their
// names, how to tell one is installed, where their skills folders are, how
// the TAP MCP server is connected to them, and whether the runner can borrow
// their connections. It holds data only and imports nothing from discover,
// so every other package can use it (TENG-3108).
//
// Each capability a call site needs (read history, write a pointer, launch
// the agent) is a field here. A call site looks the client up and checks the
// field; it never switches on a client name.
package client

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Client is one coding agent.
type Client struct {
	ID      string   // the one name used everywhere, e.g. "claude-code"
	Aliases []string // other accepted names, e.g. "claude"
	Name    string   // for menus, e.g. "Claude Code"

	// Markers are paths under home; any one existing means the agent is
	// installed for this user. A marker may be a glob, and may start with
	// $APPDATA/ or $LOCALAPPDATA/ (Windows; resolved by AppData). Detection
	// never runs a program or touches the network.
	Markers []string

	// Source says where each fact in this entry was checked: skills folders
	// and markers, history location, MCP configuration (TENG-3125).
	Source string

	// History is true when discover has a reader for this agent's session
	// history (history.Readers keys). A test keeps the two in step.
	History bool

	Skills SkillsPaths // where pointer SKILL.md folders go; empty = none
	MCP    MCPConfig   // how the TAP MCP server is connected to it
	// Bridge is true when the runner can make a primitive's tool calls
	// through this agent's own connections and approvals (host openBridge).
	Bridge bool
	// Transcript is a glob under home for one session's append-only log,
	// with {session} standing for the session ID; empty when the agent keeps
	// sessions in a database. Handoffs cite transcript lines through it.
	Transcript string
	// Launch is the argv that starts the agent on a prompt (the prompt is
	// appended), or nil when it has no command line.
	Launch []string
}

// SkillsPaths are the agent's skills folders: Global under home, Project
// under the project directory. Either may be empty.
type SkillsPaths struct {
	Global  string
	Project string
}

// MCPKind says how the TAP MCP server is registered with an agent.
type MCPKind string

const (
	MCPNone      MCPKind = ""          // not supported yet
	MCPCommand   MCPKind = "command"   // the agent's own `mcp add` command
	MCPJSONFile  MCPKind = "json-file" // one entry merged into a JSON file under home
	MCPExtension MCPKind = "extension" // our editor extension registers it
)

// MCPConfig is how the TAP MCP server is connected. Path is the file under
// home where the agent keeps its servers and Key the object (or TOML table)
// that holds them; for MCPJSONFile the runner writes there, for MCPCommand
// the agent's own command does and Path is only read to see whether TAP is
// connected. For MCPExtension, Path is a glob of the installed extension.
type MCPConfig struct {
	Kind MCPKind
	Path string
	Key  string
}

// Capabilities a call site can ask for, named in errors.
const (
	CapHistory = "reading session history"
	CapSkills  = "skills folders"
	CapLaunch  = "starting from the command line"
)

var registry = []Client{
	{ID: "claude-code", Aliases: []string{"claude"}, Name: "Claude Code",
		Source:  "skills folders and markers: npx skills 1.5.18 agent table (vercel-labs/skills dist/cli.mjs); history and MCP (claude mcp add): on disk 2026-10-02",
		Markers: []string{".claude"}, History: true, Transcript: ".claude/projects/*/{session}.jsonl",
		Skills: SkillsPaths{Global: ".claude/skills", Project: ".claude/skills"},
		MCP:    MCPConfig{Kind: MCPCommand, Path: ".claude.json", Key: "mcpServers"}, Bridge: true, Launch: []string{"claude"}},
	{ID: "codex", Name: "Codex",
		Source:  "skills folders and markers: npx skills 1.5.18 agent table (vercel-labs/skills dist/cli.mjs); history and MCP (codex mcp add): on disk 2026-10-02",
		Markers: []string{".codex"}, History: true, Transcript: ".codex/sessions/*/*/*/*{session}.jsonl",
		Skills: SkillsPaths{Global: ".codex/skills", Project: ".agents/skills"},
		MCP:    MCPConfig{Kind: MCPCommand, Path: ".codex/config.toml", Key: "mcp_servers"}, Bridge: true, Launch: []string{"codex"}},
	{ID: "cursor", Aliases: []string{"cursor-ide"}, Name: "Cursor",
		Source:  "skills folders and markers: npx skills 1.5.18 agent table (vercel-labs/skills dist/cli.mjs); history globalStorage/state.vscdb and MCP ~/.cursor/mcp.json (mcpServers): on disk 2026-10-02",
		Markers: []string{"Library/Application Support/Cursor", ".config/Cursor", "$APPDATA/Cursor"}, History: true,
		Skills: SkillsPaths{Global: ".cursor/skills", Project: ".agents/skills"},
		MCP:    MCPConfig{Kind: MCPJSONFile, Path: ".cursor/mcp.json", Key: "mcpServers"}},
	{ID: "cursor-cli", Aliases: []string{"cursor-agent"}, Name: "Cursor CLI",
		Source:  "skills folders and markers: npx skills 1.5.18 agent table (vercel-labs/skills dist/cli.mjs); history ~/.cursor/chats store.db: on disk 2026-10-02; MCP shares ~/.cursor/mcp.json",
		Markers: []string{".cursor/chats"}, History: true, Transcript: ".cursor/chats/*/{session}/store.db",
		Skills: SkillsPaths{Global: ".cursor/skills", Project: ".agents/skills"},
		MCP:    MCPConfig{Kind: MCPJSONFile, Path: ".cursor/mcp.json", Key: "mcpServers"},
		Launch: []string{"cursor-agent"}},
	{ID: "antigravity", Name: "Antigravity",
		Source:  "skills folders and markers: npx skills 1.5.18 agent table (vercel-labs/skills dist/cli.mjs); history brain/ and conversations/: on disk 2026-10-02; hooks: contract in the shipped language_server",
		Markers: []string{".gemini/antigravity"}, History: true, Transcript: ".gemini/antigravity/brain/{session}/.system_generated/logs/transcript_full.jsonl",
		Skills: SkillsPaths{Global: ".gemini/antigravity/skills", Project: ".agents/skills"}},
	{ID: "gemini-cli", Aliases: []string{"gemini"}, Name: "Gemini CLI",
		Source:  "skills folders and markers: npx skills 1.5.18 agent table (vercel-labs/skills dist/cli.mjs) (marker narrowed: ~/.gemini also holds Antigravity); history: gemini-cli source + on disk; MCP settings.json mcpServers: on disk",
		Markers: []string{".gemini/tmp", ".gemini/settings.json"},
		Skills:  SkillsPaths{Global: ".gemini/skills", Project: ".agents/skills"},
		MCP:     MCPConfig{Kind: MCPJSONFile, Path: ".gemini/settings.json", Key: "mcpServers"},
		Bridge:  true, Launch: []string{"gemini", "-i"}},
	{ID: "qwen-code", Aliases: []string{"qwen"}, Name: "Qwen Code",
		Source:  "skills folders and markers: npx skills 1.5.18 agent table (vercel-labs/skills dist/cli.mjs); history: Gemini CLI fork format per vshulcz/deja-vu docs/registry/qwen.md (not seen on disk)",
		Markers: []string{".qwen"},
		Skills:  SkillsPaths{Global: ".qwen/skills", Project: ".qwen/skills"}},
	{ID: "vscode-copilot", Aliases: []string{"vscode", "github-copilot"}, Name: "GitHub Copilot in VS Code",
		Source:  "skills folders and markers: npx skills 1.5.18 agent table (vercel-labs/skills dist/cli.mjs) (github-copilot skills); marker globalStorage/github.copilot-chat and history chatSessions: on disk 2026-10-02 + microsoft/vscode chatService.ts",
		Markers: []string{"Library/Application Support/Code/User/globalStorage/github.copilot-chat", ".config/Code/User/globalStorage/github.copilot-chat", "$APPDATA/Code/User/globalStorage/github.copilot-chat"},
		Skills:  SkillsPaths{Global: ".copilot/skills", Project: ".agents/skills"},
		MCP:     MCPConfig{Kind: MCPExtension, Path: ".vscode/extensions/telara-labs.tap-vscode-*"}, Bridge: true},
	{ID: "copilot-cli", Aliases: []string{"copilot"}, Name: "GitHub Copilot CLI",
		Source:  "skills folders and markers: npx skills 1.5.18 agent table (vercel-labs/skills dist/cli.mjs) (shares ~/.copilot); history session-state/<id>/events.jsonl: github/copilot-cli issues; MCP ~/.copilot/mcp-config.json: GitHub Copilot CLI docs (not seen on disk)",
		Markers: []string{".copilot/session-state"},
		Skills:  SkillsPaths{Global: ".copilot/skills", Project: ".agents/skills"},
		MCP:     MCPConfig{Kind: MCPCommand, Path: ".copilot/mcp-config.json", Key: "mcpServers"}},
	{ID: "windsurf", Name: "Windsurf",
		Source:  "skills folders and markers: npx skills 1.5.18 agent table (vercel-labs/skills dist/cli.mjs); MCP ~/.codeium/windsurf/mcp_config.json and transcript hook: docs.devin.ai/desktop (not installed here)",
		Markers: []string{".codeium/windsurf"},
		Skills:  SkillsPaths{Global: ".codeium/windsurf/skills", Project: ".windsurf/skills"},
		MCP:     MCPConfig{Kind: MCPJSONFile, Path: ".codeium/windsurf/mcp_config.json", Key: "mcpServers"}},
	{ID: "cline", Name: "Cline",
		Source:  "skills folders and markers: npx skills 1.5.18 agent table (vercel-labs/skills dist/cli.mjs); history tasks/<id>/api_conversation_history.json: vshulcz/deja-vu cline.go (not installed here)",
		Markers: []string{".cline"},
		Skills:  SkillsPaths{Global: ".agents/skills", Project: ".agents/skills"}},
	{ID: "roo", Aliases: []string{"roo-code"}, Name: "Roo Code",
		Source:  "skills folders and markers: npx skills 1.5.18 agent table (vercel-labs/skills dist/cli.mjs); history: Roo-Code source task-persistence/apiMessages.ts (not installed here)",
		Markers: []string{".roo"},
		Skills:  SkillsPaths{Global: ".roo/skills", Project: ".roo/skills"}},
	{ID: "kilo", Aliases: []string{"kilocode", "kilo-code"}, Name: "Kilo Code",
		Source:  "skills folders and markers: npx skills 1.5.18 agent table (vercel-labs/skills dist/cli.mjs); history: Roo fork, vshulcz/deja-vu kilo.go (not installed here)",
		Markers: []string{".kilocode"},
		Skills:  SkillsPaths{Global: ".kilocode/skills", Project: ".kilocode/skills"}},
	{ID: "opencode", Name: "OpenCode",
		Source:  "skills folders and markers: npx skills 1.5.18 agent table (vercel-labs/skills dist/cli.mjs); history opencode.db: schema seen on disk (empty) + vshulcz/deja-vu opencode.go",
		Markers: []string{".config/opencode", ".local/share/opencode"},
		Skills:  SkillsPaths{Global: ".config/opencode/skills", Project: ".agents/skills"}},
	{ID: "zed", Name: "Zed",
		Source:  "skills folders and markers: npx skills 1.5.18 agent table (vercel-labs/skills dist/cli.mjs); history threads.db: zed source crates/agent/src/db.rs (not installed here)",
		Markers: []string{"Library/Application Support/Zed", ".config/zed", ".local/share/zed", "$LOCALAPPDATA/Zed"},
		Skills:  SkillsPaths{Global: ".agents/skills", Project: ".agents/skills"}},
	{ID: "goose", Name: "Goose",
		Source:  "skills folders and markers: npx skills 1.5.18 agent table (vercel-labs/skills dist/cli.mjs); history sessions.db: goose source session_manager.rs (not installed here)",
		Markers: []string{".config/goose", ".local/share/goose", "$APPDATA/Block/goose"},
		Skills:  SkillsPaths{Global: ".config/goose/skills", Project: ".goose/skills"}},
	{ID: "crush", Name: "Crush",
		Source:  "skills folders and markers: npx skills 1.5.18 agent table (vercel-labs/skills dist/cli.mjs); history crush.db: crush source initial migration (not installed here)",
		Markers: []string{".config/crush", ".local/share/crush"},
		Skills:  SkillsPaths{Global: ".config/crush/skills", Project: ".crush/skills"}},
	{ID: "continue", Name: "Continue",
		Source:  "skills folders and markers: npx skills 1.5.18 agent table (vercel-labs/skills dist/cli.mjs); history ~/.continue/sessions: vshulcz/deja-vu registry (not installed here)",
		Markers: []string{".continue"},
		Skills:  SkillsPaths{Global: ".continue/skills", Project: ".continue/skills"}},
	{ID: "amp", Name: "Amp",
		Source:  "skills folders and markers: npx skills 1.5.18 agent table (vercel-labs/skills dist/cli.mjs); history: amp threads export, vshulcz/deja-vu amp.go (not installed here)",
		Markers: []string{".config/amp", ".local/share/amp"},
		Skills:  SkillsPaths{Global: ".config/agents/skills", Project: ".agents/skills"}},
	{ID: "aider", Name: "Aider",
		Source:  "markers .aider.chat.history.md / .aider.conf.yml: aider source io.py; aider reads no skills folder (npx skills lists the separate AiderDesk)",
		Markers: []string{".aider.chat.history.md", ".aider.conf.yml"}},
}

// All returns every known client, in registry order.
func All() []Client { return append([]Client(nil), registry...) }

// Lookup finds a client by ID or alias, ignoring case and surrounding space.
func Lookup(name string) (Client, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, c := range registry {
		if c.ID == name {
			return c, true
		}
		for _, a := range c.Aliases {
			if a == name {
				return c, true
			}
		}
	}
	return Client{}, false
}

// Detected returns the clients installed for the user whose home is home,
// in registry order.
func Detected(home string) []Client {
	var out []Client
	for _, c := range registry {
		if c.InstalledUnder(home) {
			out = append(out, c)
		}
	}
	return out
}

// InstalledUnder reports whether one of c's markers exists under home.
func (c Client) InstalledUnder(home string) bool {
	for _, m := range c.Markers {
		if ms, _ := filepath.Glob(MarkerPath(m, home)); len(ms) > 0 {
			return true
		}
	}
	return false
}

// MarkerPath resolves a marker: under home, or under the Windows roaming or
// local application data folder for $APPDATA/ and $LOCALAPPDATA/.
func MarkerPath(m, home string) string {
	for prefix, fallback := range map[string]string{"$APPDATA/": "AppData/Roaming", "$LOCALAPPDATA/": "AppData/Local"} {
		if rest, ok := strings.CutPrefix(m, prefix); ok {
			return filepath.Join(AppData(prefix[1:len(prefix)-1], home, fallback), filepath.FromSlash(rest))
		}
	}
	return filepath.Join(home, filepath.FromSlash(m))
}

// AppData is a Windows application data folder: the existing APPDATA or
// LOCALAPPDATA variable when set (read, never set here), else its default
// place under home.
func AppData(name, home, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return filepath.Join(home, filepath.FromSlash(fallback))
}

// SkillsDir is where c reads skills: under home, or under the project dir.
func (c Client) SkillsDir(project bool, home, projectDir string) (string, error) {
	rel, base := c.Skills.Global, home
	if project {
		rel, base = c.Skills.Project, projectDir
	}
	if rel == "" {
		return "", Unsupported(c.ID, CapSkills)
	}
	return filepath.Join(base, filepath.FromSlash(rel)), nil
}

// IDs returns the IDs of the clients that pass keep (all when keep is nil).
func IDs(keep func(Client) bool) []string {
	var out []string
	for _, c := range registry {
		if keep == nil || keep(c) {
			out = append(out, c.ID)
		}
	}
	return out
}

// Resolve turns a comma-separated list of names into clients that have
// capability has. "all" means every such client; "detected" (or an empty
// list) means every such client installed under home. An explicitly named
// client without the capability is an error; detected ones without it are
// left out.
func Resolve(list, home, capability string, has func(Client) bool) ([]Client, error) {
	var out []Client
	seen := map[string]bool{}
	add := func(c Client) {
		if !seen[c.ID] {
			seen[c.ID] = true
			out = append(out, c)
		}
	}
	for _, name := range strings.Split(list, ",") {
		switch name = strings.ToLower(strings.TrimSpace(name)); name {
		case "":
			if strings.TrimSpace(list) != "" {
				continue
			}
			fallthrough
		case "detected":
			for _, c := range Detected(home) {
				if has(c) {
					add(c)
				}
			}
		case "all":
			for _, c := range registry {
				if has(c) {
					add(c)
				}
			}
		default:
			c, ok := Lookup(name)
			if !ok {
				return nil, Unknown(name, capability, has)
			}
			if !has(c) {
				return nil, Unsupported(c.ID, capability)
			}
			add(c)
		}
	}
	return out, nil
}

// Unknown is the error for a name that is no client. It lists the clients
// that have the capability, from the registry.
func Unknown(name, capability string, has func(Client) bool) error {
	return fmt.Errorf("unknown client %q; for %s use one of: %s (or all, detected)", name, capability, strings.Join(sortedIDs(has), ", "))
}

// Unsupported is the error for a client that lacks a capability.
func Unsupported(id, capability string) error {
	return fmt.Errorf("client %q does not support %s yet", id, capability)
}

func sortedIDs(has func(Client) bool) []string {
	ids := IDs(has)
	sort.Strings(ids)
	return ids
}

// HasHistory, HasSkills and HasLaunch are the capability checks call sites use.
func HasHistory(c Client) bool { return c.History }
func HasSkills(c Client) bool  { return c.Skills.Global != "" || c.Skills.Project != "" }
func HasLaunch(c Client) bool  { return len(c.Launch) > 0 }

// TranscriptPath finds the log file of one session of client name under
// home, or "" when there is none (unknown client, database store, missing).
func TranscriptPath(name, session, home string) string {
	c, ok := Lookup(name)
	if !ok || c.Transcript == "" || session == "" || strings.ContainsAny(session, `*?[]/\`) {
		return ""
	}
	pattern := filepath.Join(home, filepath.FromSlash(strings.ReplaceAll(c.Transcript, "{session}", session)))
	m, _ := filepath.Glob(pattern)
	if len(m) == 0 {
		return ""
	}
	sort.Strings(m)
	return m[0]
}

// Connected reports whether the TAP MCP server is registered with c under
// name, from c's own configuration under home. An agent whose configuration
// cannot be read this way reports false.
func (c Client) Connected(home, name string) bool {
	if c.MCP.Path == "" {
		return false
	}
	p := filepath.Join(home, filepath.FromSlash(c.MCP.Path))
	if c.MCP.Kind == MCPExtension {
		m, _ := filepath.Glob(p)
		return len(m) > 0
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	if strings.HasSuffix(p, ".toml") {
		// A TOML table header: [mcp_servers.tap] or [mcp_servers."tap"].
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "["+c.MCP.Key+"."+name+"]" || line == "["+c.MCP.Key+".\""+name+"\"]" {
				return true
			}
		}
		return false
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(b, &doc) != nil {
		return false
	}
	var servers map[string]json.RawMessage
	if json.Unmarshal(doc[c.MCP.Key], &servers) != nil {
		return false
	}
	_, ok := servers[name]
	return ok
}

// HasMCP reports whether the TAP MCP server can be connected to c at all.
func HasMCP(c Client) bool { return c.MCP.Kind != MCPNone }

// CapMCP names the capability in errors.
const CapMCP = "connecting the TAP MCP server"
