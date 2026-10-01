package history

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// DefaultReaders returns readers for the named clients at their usual
// places under home.
func DefaultReaders(clients []string, home string) ([]trace.Reader, error) {
	var out []trace.Reader
	for _, c := range clients {
		switch strings.TrimSpace(c) {
		case "claude-code":
			out = append(out, ClaudeCode{Dir: filepath.Join(home, ".claude", "projects")})
		case "codex":
			out = append(out, Codex{Dir: filepath.Join(home, ".codex", "sessions")})
		case "cursor":
			out = append(out, Cursor{DB: CursorStateDB(home)})
		case "":
		default:
			return nil, fmt.Errorf("unknown client %q (want claude-code, codex or cursor)", c)
		}
	}
	return out, nil
}

// CursorStateDB is where Cursor keeps its chat store on this OS.
func CursorStateDB(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb")
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "Cursor", "User", "globalStorage", "state.vscdb")
		}
		return filepath.Join(home, "AppData", "Roaming", "Cursor", "User", "globalStorage", "state.vscdb")
	default:
		return filepath.Join(home, ".config", "Cursor", "User", "globalStorage", "state.vscdb")
	}
}
