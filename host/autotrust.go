package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
	"gopkg.in/yaml.v3"
)

// An agent that cannot show TAP's prompts (no MCP elicitation: OpenCode,
// Kilo, Crush, Gemini CLI, goose run) gave no way to approve a saved
// primitive, so a primitive that only read gitlab.com was refused until the
// person ran tap trust, and the agent redid its steps by hand. The person
// already decided, in the agent's own settings, what its model may do
// without asking. A primitive that only reads may do the same there and no
// more: it fetches the web unasked only where the agent fetches the web
// unasked, and runs a read-only program (git log) only where the agent runs
// shell commands unasked. The approval comes from the agent, not from TAP.
// Anything that changes something is still asked, or refused where nobody
// can be asked, and a program that runs other code (bash -c, git -c) counts
// as destructive on every call, whatever the manifest says.

// readsOnly says whether a manifest declares nothing but reads.
func readsOnly(m *mf.Manifest) bool {
	if m == nil {
		return false
	}
	for _, c := range m.Commands {
		if c.Effect != "read" {
			return false
		}
	}
	for _, t := range m.Tools {
		if t.Effect != "read" {
			return false
		}
	}
	for _, f := range m.Files {
		if f.Access != "read" {
			return false
		}
	}
	for _, f := range m.Fetch {
		for _, method := range f.Methods {
			if u := strings.ToUpper(method); u != "GET" && u != "HEAD" {
				return false
			}
		}
	}
	return true
}

// readFetchKinds are the gate kinds of a manifest's read fetches.
func readFetchKinds(m *mf.Manifest) []string {
	var kinds []string
	for _, f := range m.Fetch {
		kinds = append(kinds, "send GET requests to "+f.Origin, "send HEAD requests to "+f.Origin)
	}
	return kinds
}

// What an agent's own settings may let its model do without asking.
const (
	unaskedWeb   = "web"
	unaskedShell = "shell"
)

// agentReadsWebUnasked reports whether an agent's own configuration lets its
// model fetch the web without asking the person, and where that was read.
func agentReadsWebUnasked(clientName, home string) (bool, string) {
	return agentDoesUnasked(clientName, home, unaskedWeb, 0)
}

// agentDoesUnasked reports whether an agent's own configuration lets its
// model use the web (unaskedWeb) or run shell commands (unaskedShell)
// without asking the person, and where that was read. Unknown agents, and
// settings that ask, deny or allow only some commands, are no. agentPid is
// the process that started the session (0: this process's parent).
func agentDoesUnasked(clientName, home, what string, agentPid int) (bool, string) {
	c, ok := historyClient(clientName)
	if !ok {
		return false, "unknown agent " + clientName
	}
	switch c.ID {
	case "opencode", "kilo":
		// permission.webfetch and permission.bash: "allow" (the default),
		// "ask" or "deny"; bash may also map command patterns to those.
		key := map[string]string{unaskedWeb: "webfetch", unaskedShell: "bash"}[what]
		file := filepath.Join(home, ".config", c.ID, c.ID+".json")
		var cfg struct {
			Permission json.RawMessage `json:"permission"`
		}
		if b, err := os.ReadFile(file); err == nil {
			json.Unmarshal(b, &cfg)
		}
		var perm map[string]any
		if json.Unmarshal(cfg.Permission, &perm) != nil {
			var all string
			if json.Unmarshal(cfg.Permission, &all) == nil && all != "" && all != "allow" {
				return false, fmt.Sprintf("%s permission is %q", file, all)
			}
			return true, fmt.Sprintf("%s leaves %s allowed (its default)", file, key)
		}
		switch v := perm[key].(type) {
		case nil:
			return true, fmt.Sprintf("%s leaves %s allowed (its default)", file, key)
		case string:
			if v != "allow" {
				return false, fmt.Sprintf("%s permission.%s is %q", file, key, v)
			}
			return true, fmt.Sprintf("%s permission.%s is allow", file, key)
		case map[string]any:
			if s, _ := v["*"].(string); s == "allow" && len(v) == 1 {
				return true, fmt.Sprintf("%s permission.%s allows every command", file, key)
			}
			return false, fmt.Sprintf("%s permission.%s allows only some commands", file, key)
		}
		return false, fmt.Sprintf("%s permission.%s cannot be read", file, key)
	case "crush":
		// Crush asks for every tool unless permissions.allowed_tools lists it.
		file := filepath.Join(home, ".config", "crush", "crush.json")
		var cfg struct {
			Permissions struct {
				AllowedTools []string `json:"allowed_tools"`
			} `json:"permissions"`
		}
		if b, err := os.ReadFile(file); err == nil {
			json.Unmarshal(b, &cfg)
		}
		want := map[string][]string{unaskedWeb: {"fetch", "agentic_fetch"}, unaskedShell: {"bash"}}[what]
		for _, t := range cfg.Permissions.AllowedTools {
			for _, w := range want {
				if t == w {
					return true, file + " allows " + t
				}
			}
		}
		return false, fmt.Sprintf("%s does not allow %s without asking", file, strings.Join(want, " or "))
	case "goose":
		// GOOSE_MODE auto runs every tool without asking.
		file := filepath.Join(home, ".config", "goose", "config.yaml")
		var cfg struct {
			Mode string `yaml:"GOOSE_MODE"`
		}
		if b, err := os.ReadFile(file); err == nil {
			yaml.Unmarshal(b, &cfg)
		}
		if cfg.Mode == "auto" {
			return true, file + " GOOSE_MODE is auto"
		}
		return false, fmt.Sprintf("%s GOOSE_MODE is %q", file, cfg.Mode)
	case "gemini-cli":
		// Gemini CLI asks before a shell command or a web fetch unless it was
		// started in yolo mode (--yolo, --approval-mode yolo) or its settings
		// list the tool under tools.allowed.
		if args, ok := geminiCommandLine(agentPid); ok && geminiYolo(args) {
			return true, "Gemini CLI was started in yolo mode"
		}
		file := filepath.Join(home, ".gemini", "settings.json")
		var cfg struct {
			Tools struct {
				Allowed []string `json:"allowed"`
			} `json:"tools"`
		}
		if b, err := os.ReadFile(file); err == nil {
			json.Unmarshal(b, &cfg)
		}
		want := map[string]string{unaskedWeb: "web_fetch", unaskedShell: "run_shell_command"}[what]
		for _, t := range cfg.Tools.Allowed {
			if t == want {
				return true, file + " tools.allowed lists " + want
			}
		}
		return false, fmt.Sprintf("Gemini CLI asks before %s (not in yolo mode, and %s does not list it under tools.allowed)", want, file)
	}
	return false, c.Name + " is not known to allow this without asking"
}

// geminiYolo reports whether Gemini CLI's own command line runs every tool
// without asking.
func geminiYolo(args []string) bool {
	for i, a := range args {
		switch {
		case a == "--yolo", a == "-y", a == "--approval-mode=yolo":
			return true
		case a == "--approval-mode" && i+1 < len(args) && args[i+1] == "yolo":
			return true
		}
	}
	return false
}

// processArgs is the command line of a process and its parent's pid. A
// variable so tests can describe a process tree.
var processArgs = func(pid int) (args []string, parent int, ok bool) {
	if runtime.GOOS == "linux" {
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			return nil, 0, false
		}
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return nil, 0, false
		}
		// The second field is the command name in parentheses, which can
		// hold spaces; the parent pid is the second field after it.
		s := string(stat)
		fields := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
		if len(fields) < 2 {
			return nil, 0, false
		}
		parent, _ = strconv.Atoi(fields[1])
		return strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00"), parent, true
	}
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("ps", "-o", "ppid=", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return nil, 0, false
		}
		fields := strings.Fields(string(out))
		if len(fields) < 2 {
			return nil, 0, false
		}
		parent, _ = strconv.Atoi(fields[0])
		return fields[1:], parent, true
	}
	return nil, 0, false
}

// geminiCommandLine finds the Gemini CLI process that started the session,
// from pid up through the next few ancestors (a wrapper script may sit
// between them), and returns its arguments. pid 0 is this process's parent;
// a shared runner is given the relay's parent instead (shared.go).
func geminiCommandLine(pid int) ([]string, bool) {
	if pid == 0 {
		pid = os.Getppid()
	}
	for i := 0; i < 5 && pid > 1; i++ {
		args, parent, ok := processArgs(pid)
		if !ok {
			return nil, false
		}
		for _, a := range args {
			if base := filepath.Base(a); base == "gemini" || strings.Contains(a, "gemini-cli") {
				return args, true
			}
		}
		pid = parent
	}
	return nil, false
}

// autoTrustReads decides whether a package may run without a TAP prompt in
// an agent that cannot show one, and says why. It may only when it reads,
// and the agent's own settings let its model do each kind of thing the
// package does (fetch the web, run a program) without asking.
func autoTrustReads(m *mf.Manifest, clientName, home string, agentPid int) (bool, string) {
	if !readsOnly(m) {
		return false, "it declares more than reads"
	}
	var because []string
	if len(m.Fetch) > 0 {
		ok, why := agentDoesUnasked(clientName, home, unaskedWeb, agentPid)
		if !ok {
			return false, why
		}
		because = append(because, why)
	}
	if len(m.Commands) > 0 {
		ok, why := agentDoesUnasked(clientName, home, unaskedShell, agentPid)
		if !ok {
			return false, why
		}
		because = append(because, why)
	}
	if len(because) == 0 {
		return true, "it only reads"
	}
	return true, strings.Join(because, "; ")
}
