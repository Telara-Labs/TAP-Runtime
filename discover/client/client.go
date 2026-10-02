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
	// installed for this user. Detection never runs a program or touches
	// the network.
	Markers []string

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

// MCPConfig is how the TAP MCP server is connected. For MCPJSONFile, Path is
// the file under home and Key the object that holds servers.
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
		Markers: []string{".claude"}, History: true, Transcript: ".claude/projects/*/{session}.jsonl",
		Skills: SkillsPaths{Global: ".claude/skills", Project: ".claude/skills"},
		MCP:    MCPConfig{Kind: MCPCommand}, Bridge: true, Launch: []string{"claude"}},
	{ID: "codex", Name: "Codex",
		Markers: []string{".codex"}, History: true, Transcript: ".codex/sessions/*/*/*/*{session}.jsonl",
		Skills: SkillsPaths{Global: ".codex/skills", Project: ".agents/skills"},
		MCP:    MCPConfig{Kind: MCPCommand}, Bridge: true, Launch: []string{"codex"}},
	{ID: "cursor", Aliases: []string{"cursor-ide"}, Name: "Cursor",
		Markers: []string{"Library/Application Support/Cursor", ".config/Cursor", "AppData/Roaming/Cursor"}, History: true,
		Skills: SkillsPaths{Global: ".cursor/skills", Project: ".agents/skills"},
		MCP:    MCPConfig{Kind: MCPJSONFile, Path: ".cursor/mcp.json", Key: "mcpServers"}},
	{ID: "cursor-cli", Aliases: []string{"cursor-agent"}, Name: "Cursor CLI",
		Markers: []string{".cursor/chats"}, History: true,
		Skills: SkillsPaths{Global: ".cursor/skills", Project: ".agents/skills"},
		MCP:    MCPConfig{Kind: MCPJSONFile, Path: ".cursor/mcp.json", Key: "mcpServers"},
		Launch: []string{"cursor-agent"}},
	{ID: "antigravity", Name: "Antigravity",
		Markers: []string{".gemini/antigravity"}, History: true, Transcript: ".gemini/antigravity/brain/{session}/.system_generated/logs/transcript_full.jsonl",
		Skills: SkillsPaths{Global: ".gemini/antigravity/skills", Project: ".agents/skills"}},
	{ID: "gemini-cli", Aliases: []string{"gemini"}, Name: "Gemini CLI",
		Markers: []string{".gemini/tmp", ".gemini/settings.json"},
		Skills:  SkillsPaths{Global: ".gemini/skills", Project: ".agents/skills"},
		MCP:     MCPConfig{Kind: MCPJSONFile, Path: ".gemini/settings.json", Key: "mcpServers"},
		Bridge:  true, Launch: []string{"gemini", "-i"}},
	{ID: "qwen-code", Aliases: []string{"qwen"}, Name: "Qwen Code",
		Markers: []string{".qwen"},
		Skills:  SkillsPaths{Global: ".qwen/skills", Project: ".qwen/skills"}},
	{ID: "vscode-copilot", Aliases: []string{"vscode", "github-copilot"}, Name: "GitHub Copilot in VS Code",
		Markers: []string{"Library/Application Support/Code/User", ".config/Code/User", "AppData/Roaming/Code/User"},
		Skills:  SkillsPaths{Global: ".copilot/skills", Project: ".agents/skills"},
		MCP:     MCPConfig{Kind: MCPExtension}, Bridge: true},
	{ID: "copilot-cli", Aliases: []string{"copilot"}, Name: "GitHub Copilot CLI",
		Markers: []string{".copilot/session-state"},
		Skills:  SkillsPaths{Global: ".copilot/skills", Project: ".agents/skills"},
		MCP:     MCPConfig{Kind: MCPJSONFile, Path: ".copilot/mcp-config.json", Key: "mcpServers"}},
	{ID: "windsurf", Name: "Windsurf",
		Markers: []string{".codeium/windsurf"},
		Skills:  SkillsPaths{Global: ".codeium/windsurf/skills", Project: ".windsurf/skills"},
		MCP:     MCPConfig{Kind: MCPJSONFile, Path: ".codeium/windsurf/mcp_config.json", Key: "mcpServers"}},
	{ID: "cline", Name: "Cline",
		Markers: []string{".cline"},
		Skills:  SkillsPaths{Global: ".agents/skills", Project: ".agents/skills"}},
	{ID: "roo", Aliases: []string{"roo-code"}, Name: "Roo Code",
		Markers: []string{".roo"},
		Skills:  SkillsPaths{Global: ".roo/skills", Project: ".roo/skills"}},
	{ID: "kilo", Aliases: []string{"kilocode", "kilo-code"}, Name: "Kilo Code",
		Markers: []string{".kilocode"},
		Skills:  SkillsPaths{Global: ".kilocode/skills", Project: ".kilocode/skills"}},
	{ID: "opencode", Name: "OpenCode",
		Markers: []string{".config/opencode", ".local/share/opencode"},
		Skills:  SkillsPaths{Global: ".config/opencode/skills", Project: ".agents/skills"}},
	{ID: "zed", Name: "Zed",
		Markers: []string{"Library/Application Support/Zed", ".config/zed", ".local/share/zed"},
		Skills:  SkillsPaths{Global: ".agents/skills", Project: ".agents/skills"}},
	{ID: "goose", Name: "Goose",
		Markers: []string{".config/goose", ".local/share/goose"},
		Skills:  SkillsPaths{Global: ".config/goose/skills", Project: ".goose/skills"}},
	{ID: "crush", Name: "Crush",
		Markers: []string{".config/crush", ".local/share/crush"},
		Skills:  SkillsPaths{Global: ".config/crush/skills", Project: ".crush/skills"}},
	{ID: "continue", Name: "Continue",
		Markers: []string{".continue"},
		Skills:  SkillsPaths{Global: ".continue/skills", Project: ".continue/skills"}},
	{ID: "amp", Name: "Amp",
		Markers: []string{".config/amp", ".local/share/amp"},
		Skills:  SkillsPaths{Global: ".config/agents/skills", Project: ".agents/skills"}},
	{ID: "aider", Name: "Aider",
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
		if _, err := os.Stat(filepath.Join(home, filepath.FromSlash(m))); err == nil {
			return true
		}
	}
	return false
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
