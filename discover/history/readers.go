package history

import (
	"path/filepath"
	"runtime"
	"strings"

	"gitlab.com/telara-labs/tap-runtime/discover/client"
	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// Readers maps a client ID (discover/client) to the reader of its history at
// its usual place under home. Every client marked History has an entry here
// and every entry is such a client; TestEveryHistoryClientHasAReader keeps
// the two in step.
var Readers = map[string]func(home string) trace.Reader{
	"claude-code": func(home string) trace.Reader {
		return ClaudeCode{Dir: filepath.Join(home, ".claude", "projects")}
	},
	"codex": func(home string) trace.Reader {
		return Codex{Dir: filepath.Join(home, ".codex", "sessions")}
	},
	"cursor": func(home string) trace.Reader { return Cursor{DB: CursorStateDB(home)} },
	"cursor-cli": func(home string) trace.Reader {
		return CursorCLI{Dir: filepath.Join(home, ".cursor", "chats")}
	},
	"antigravity": func(home string) trace.Reader {
		return Antigravity{Dir: filepath.Join(home, ".gemini", "antigravity", "brain")}
	},
	"gemini-cli": func(home string) trace.Reader {
		return GeminiCLI{Dir: filepath.Join(home, ".gemini", "tmp")}
	},
	"qwen-code": func(home string) trace.Reader {
		return QwenCode{Dir: filepath.Join(home, ".qwen", "projects")}
	},
	"vscode-copilot": func(home string) trace.Reader { return VSCodeCopilot{User: VSCodeUserDir(home)} },
	"cline": func(home string) trace.Reader {
		return ReaderSet{ID: "cline", List: []trace.Reader{
			ClineCLI{Dir: filepath.Join(home, ".cline", "data", "sessions"), Configs: []string{filepath.Join(home, ".cline", "data", "settings", "cline_mcp_settings.json")}},
			ExtensionTasks{ID: "cline", Dirs: vscodeFamilyStorage(home, "saoudrizwan.claude-dev")},
		}}
	},
	"roo": func(home string) trace.Reader {
		return ExtensionTasks{ID: "roo", Dirs: vscodeFamilyStorage(home, "rooveterinaryinc.roo-cline")}
	},
	"kilo": func(home string) trace.Reader {
		cfg := filepath.Join(home, ".config", "kilo")
		return ReaderSet{ID: "kilo", List: []trace.Reader{
			OpenCodeDB{ID: "kilo", DB: filepath.Join(home, ".local", "share", "kilo", "kilo.db"), Configs: []string{filepath.Join(cfg, "kilo.json"), filepath.Join(cfg, "opencode.json")}},
			ExtensionTasks{ID: "kilo", Dirs: vscodeFamilyStorage(home, "kilocode.kilo-code")},
		}}
	},
	"opencode": func(home string) trace.Reader {
		return OpenCodeDB{ID: "opencode", DB: filepath.Join(home, ".local", "share", "opencode", "opencode.db"), Configs: []string{filepath.Join(home, ".config", "opencode", "opencode.json")}}
	},
	"goose": func(home string) trace.Reader {
		return Goose{Dir: filepath.Join(home, ".local", "share", "goose", "sessions")}
	},
	"crush": func(home string) trace.Reader {
		return Crush{Dir: filepath.Join(home, ".local", "share", "crush"), Configs: []string{filepath.Join(home, ".config", "crush", "crush.json")}}
	},
	"continue": func(home string) trace.Reader {
		return Continue{Dir: filepath.Join(home, ".continue", "sessions"), Configs: []string{filepath.Join(home, ".continue", "config.yaml")}}
	},
}

// VSCodeUserDir is VS Code's User folder on this OS.
func VSCodeUserDir(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Code", "User")
	case "windows":
		return filepath.Join(client.AppData("APPDATA", home, "AppData/Roaming"), "Code", "User")
	default:
		return filepath.Join(home, ".config", "Code", "User")
	}
}

// DefaultReaders returns readers for the named clients at their usual places
// under home. Each name is a client ID or alias, "all", or "detected"; no
// names means detected (the agents installed under home).
func DefaultReaders(clients []string, home string) ([]trace.Reader, error) {
	cs, err := client.Resolve(strings.Join(clients, ","), home, client.CapHistory, client.HasHistory)
	if err != nil {
		return nil, err
	}
	var out []trace.Reader
	for _, c := range cs {
		r, ok := Readers[c.ID]
		if !ok {
			return nil, client.Unsupported(c.ID, client.CapHistory)
		}
		out = append(out, r(home))
	}
	return out, nil
}

// ReaderFor returns the history reader of one client, by ID or alias.
func ReaderFor(name, home string) (trace.Reader, error) {
	rs, err := DefaultReaders([]string{name}, home)
	if err != nil {
		return nil, err
	}
	if len(rs) != 1 {
		return nil, client.Unknown(name, client.CapHistory, client.HasHistory)
	}
	return rs[0], nil
}

// CursorStateDB is where Cursor keeps its chat store on this OS.
func CursorStateDB(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb")
	case "windows":
		return filepath.Join(client.AppData("APPDATA", home, "AppData/Roaming"), "Cursor", "User", "globalStorage", "state.vscdb")
	default:
		return filepath.Join(home, ".config", "Cursor", "User", "globalStorage", "state.vscdb")
	}
}
