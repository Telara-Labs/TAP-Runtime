package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// A host's MCP server list is the user's. TAP writes one entry in it and may
// replace or remove only an entry it wrote, never a server of the user's own
// that happens to carry the same name.

// isTapBinary says whether a command path names a TAP runner, this one or one
// installed earlier under either of its names.
func isTapBinary(command string) bool {
	if command == "" {
		return false
	}
	if self, err := os.Executable(); err == nil {
		if command == self {
			return true
		}
		if resolved, err := filepath.EvalSymlinks(self); err == nil && command == resolved {
			return true
		}
	}
	return tapBinaryName.MatchString(strings.TrimSuffix(filepath.Base(command), ".exe"))
}

// tapBinaryName is what a runner is called on disk. The npm package and the
// installer name it tap, an earlier release tap-runtime, and a release asset
// run in place keeps its version and platform (tap-0.2.34-darwin-arm64).
var tapBinaryName = regexp.MustCompile(`^tap(-runtime)?(-\d+\.\d+\.\d+[-0-9A-Za-z.]*)?$`)

// tapWrote reports whether a decoded server entry is one TAP wrote, whichever
// shape its host keeps it in. It runs a TAP runner with the serve command.
func tapWrote(entry any) bool {
	e, ok := entry.(map[string]any)
	if !ok {
		return false
	}
	var argv []string
	for _, key := range []string{"command", "cmd", "args"} {
		switch v := e[key].(type) {
		case string:
			argv = append(argv, v)
		case []any:
			for _, a := range v {
				argv = append(argv, fmt.Sprint(a))
			}
		}
	}
	for i := 0; i+1 < len(argv); i++ {
		if isTapBinary(argv[i]) && argv[i+1] == "serve" {
			return true
		}
	}
	return false
}

func notTapsEntry(path, name string) error {
	return fmt.Errorf("%s already has a server named %s that TAP did not write; it was left unchanged. Install under another name with --name", path, name)
}

var hostCommandLine = regexp.MustCompile(`(?im)^\s*"?command"?\s*[:=]\s*"?([^\s",]+)`)

// hostHoldsForeignEntry asks a command-managed host (claude, codex, copilot)
// what it has registered under name. It is true when the host has one and its
// command is not a TAP runner. A host that has none, or cannot say, is not
// foreign: there is nothing of the user's to protect.
func hostHoldsForeignEntry(host, name string) (bool, string) {
	out, err := exec.Command(host, "mcp", "get", name).CombinedOutput()
	if err != nil {
		return false, ""
	}
	m := hostCommandLine.FindSubmatch(out)
	if m == nil {
		return true, "its command could not be read"
	}
	return !isTapBinary(string(m[1])), string(m[1])
}
