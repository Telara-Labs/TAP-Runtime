package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

// Gemini CLI does not support MCP elicitation, so it cannot show TAP's save
// question or its first-run question. Outside yolo mode, tap_save could not
// save and a primitive that was not yet trusted could not run at all.
//
// Gemini has its own confirmation, and a BeforeTool hook can require it: a
// hook that answers {"decision": "ask"} makes Gemini show its confirmation
// for that call, with the hook's systemMessage in it, even in yolo mode and
// after the person chose "allow always" (packages/core/src/scheduler/
// scheduler.ts and hook-utils.ts in Gemini CLI 0.63). The call runs only if
// the person allows it.
//
// So `tap hook gemini`, run by Gemini before this runner's tap_save, and
// before tap_run of a package that is not yet trusted, writes a one-use
// record of that exact call and answers "ask" with TAP's own question. When
// the same call reaches the server, the record is the person's answer: the
// call can only have arrived after they allowed it.
//
// A non-interactive Gemini (gemini -p, or no terminal) never answers a
// confirmation, and a forced "ask" there waits forever (found in a live
// run). The hook asks nothing there and writes a record saying nobody was
// asked, which also replaces any record planted for the same call.

// geminiConfirmTTL bounds how long a record waits for its call: the person
// may take a while to answer, but a record never outlives one sitting.
const geminiConfirmTTL = 30 * time.Minute

type geminiConfirmRecord struct {
	Gemini int       `json:"gemini"` // the Gemini CLI process that asked
	Asked  bool      `json:"asked"`
	At     time.Time `json:"at"`
}

// geminiConfirmKey names a record by the call it is for. Only the fields
// the runner reads count: Gemini adds wait_for_previous to every call.
func geminiConfirmKey(tool string, args map[string]any) string {
	fields := map[string]any{"tool": tool}
	for _, k := range []string{"package", "ref", "digest", "args"} {
		if v, ok := args[k]; ok {
			fields[k] = v
		}
	}
	b, _ := json.Marshal(fields)
	sum := sha256.Sum256(b)
	return "confirm-" + hex.EncodeToString(sum[:16]) + ".json"
}

func writeGeminiConfirm(dir, tool string, args map[string]any, gemini int, asked bool) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(geminiConfirmRecord{Gemini: gemini, Asked: asked, At: time.Now().UTC()})
	path := filepath.Join(dir, geminiConfirmKey(tool, args))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// takeGeminiConfirm reads and removes the record for a call, and reports
// whether it says the person was asked, by the Gemini process gemini, in
// the last geminiConfirmTTL.
func takeGeminiConfirm(dir, tool string, args map[string]any, gemini int) (bool, string) {
	path := filepath.Join(dir, geminiConfirmKey(tool, args))
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, "Gemini CLI did not ask the person about this call (the tap hook for BeforeTool is not installed; run: tap install --client gemini)"
	}
	os.Remove(path)
	var r geminiConfirmRecord
	switch {
	case json.Unmarshal(raw, &r) != nil:
		return false, "the record of Gemini CLI's confirmation could not be read"
	case !r.Asked:
		return false, "Gemini CLI runs without a person to confirm this call (non-interactive)"
	case r.Gemini == 0 || r.Gemini != gemini:
		return false, "the confirmation was given in another Gemini CLI session"
	case time.Since(r.At) > geminiConfirmTTL:
		return false, "the confirmation is too old"
	}
	return true, ""
}

// geminiConfirmed reports whether the person allowed this exact call in
// Gemini CLI's own confirmation, which the tap hook required.
func (s *server) geminiConfirmed(tool string, raw json.RawMessage) (bool, string) {
	s.mu.Lock()
	name, pid := s.clientName, s.agentPid
	s.mu.Unlock()
	if !relayClient(clientFor(name)) {
		return false, ""
	}
	dir, err := relayDir()
	if err != nil {
		return false, err.Error()
	}
	var args map[string]any
	if len(raw) > 0 && json.Unmarshal(raw, &args) != nil {
		return false, "the call's arguments could not be read"
	}
	if pid == 0 {
		pid = os.Getppid()
	}
	gemini, _, ok := geminiProcess(pid)
	if !ok {
		return false, "the Gemini CLI process that started this server was not found"
	}
	return takeGeminiConfirm(dir, tool, args, gemini)
}

// geminiProcess finds the Gemini CLI process at pid or among its next few
// ancestors (a wrapper or a shell may sit between them).
func geminiProcess(pid int) (int, []string, bool) {
	for i := 0; i < 5 && pid > 1; i++ {
		args, parent, ok := processArgs(pid)
		if !ok {
			return 0, nil, false
		}
		if isGeminiProgram(args) {
			return pid, args, true
		}
		pid = parent
	}
	return 0, nil, false
}

// isGeminiProgram reports whether a command line runs Gemini CLI itself
// (node .../gemini, .../gemini-cli/...), not a program that only has the
// word as an argument, such as this hook.
func isGeminiProgram(args []string) bool {
	if len(args) == 0 {
		return false
	}
	program := args[0]
	if isNodeProgram(program) {
		// node [its own flags] script ...
		program = ""
		for _, a := range args[1:] {
			if !strings.HasPrefix(a, "-") {
				program = a
				break
			}
		}
	}
	base := filepath.Base(program)
	return base == "gemini" || base == "gemini.js" || strings.Contains(program, "@google/gemini-cli/")
}

func isNodeProgram(a string) bool {
	base := filepath.Base(a)
	return base == "node" || base == "nodejs" || base == "bun"
}

// geminiInteractive reports whether a Gemini CLI process can show its
// confirmation to a person, the way Gemini decides it (isHeadlessMode in
// packages/core/src/utils/headless.ts): not with -p, not in CI, and only
// with a terminal on standard input and output. ACP mode asks the editor.
// Unknown is no: asking where nobody can answer stalls the session.
func geminiInteractive(pid int, args []string) bool {
	for _, a := range args {
		switch {
		case a == "--acp", a == "--experimental-acp":
			return true
		case a == "-i", a == "--prompt-interactive", strings.HasPrefix(a, "--prompt-interactive="):
			return processHasTerminal(pid)
		case a == "-p", a == "--prompt", strings.HasPrefix(a, "--prompt="):
			return false
		}
	}
	if os.Getenv("CI") == "true" || os.Getenv("GITHUB_ACTIONS") == "true" {
		return false
	}
	return processHasTerminal(pid)
}

// processHasTerminal reports whether a process has a terminal on standard
// input and output. A variable so tests can describe a process.
var processHasTerminal = func(pid int) bool {
	switch runtime.GOOS {
	case "linux":
		for _, fd := range []string{"0", "1"} {
			target, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, fd))
			if err != nil || !(strings.HasPrefix(target, "/dev/pts/") || strings.HasPrefix(target, "/dev/tty")) {
				return false
			}
		}
		return true
	case "darwin":
		out, err := exec.Command("ps", "-o", "tty=", "-p", strconv.Itoa(pid)).Output()
		tty := strings.TrimSpace(string(out))
		return err == nil && tty != "" && tty != "??"
	}
	return false
}

type geminiBeforeToolInput struct {
	Event      string         `json:"hook_event_name"`
	Cwd        string         `json:"cwd"`
	ToolName   string         `json:"tool_name"`
	ToolInput  map[string]any `json:"tool_input"`
	MCPContext struct {
		ServerName string   `json:"server_name"`
		ToolName   string   `json:"tool_name"`
		Args       []string `json:"args"`
	} `json:"mcp_context"`
}

// geminiBeforeTool answers Gemini's BeforeTool hook. For this runner's
// tap_save, and tap_run of a package not yet trusted, it writes the record
// and makes Gemini ask the person; for anything else it answers nothing.
func geminiBeforeTool(raw []byte, dir string) (map[string]any, error) {
	var in geminiBeforeToolInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("reading the hook input: %w", err)
	}
	tool := in.MCPContext.ToolName
	if tool == "" {
		tool = in.ToolName
	}
	if tool != "tap_save" && tool != "tap_run" || !runsTapServe(in.MCPContext.Args) {
		return map[string]any{}, nil
	}
	gemini, gargs, found := geminiProcess(os.Getppid())
	if !found || !geminiInteractive(gemini, gargs) {
		// Nobody can answer here; make sure no earlier record speaks for
		// this call.
		return map[string]any{}, writeGeminiConfirm(dir, tool, in.ToolInput, gemini, false)
	}
	var question string
	if tool == "tap_save" {
		question = geminiSaveQuestion(in.ToolInput)
	} else {
		var ok bool
		if question, ok = geminiRunQuestion(in.Cwd, in.MCPContext.Args, in.ToolInput, gemini); !ok {
			return map[string]any{}, writeGeminiConfirm(dir, tool, in.ToolInput, gemini, false)
		}
	}
	if err := writeGeminiConfirm(dir, tool, in.ToolInput, gemini, true); err != nil {
		return nil, err
	}
	return map[string]any{"decision": "ask", "systemMessage": question}, nil
}

// runsTapServe reports whether an MCP server's arguments are this runner's.
func runsTapServe(args []string) bool {
	for _, a := range args {
		if a == "serve" {
			return true
		}
	}
	return false
}

func geminiSaveQuestion(args map[string]any) string {
	dir, _ := args["package"].(string)
	what, declared := dir, "it could not be read; the save will refuse it"
	if m, err := mf.Load(dir); err == nil {
		what = m.Metadata.Publisher + "/" + m.Metadata.Name + "@" + m.Metadata.Version + " from " + dir
		if declared = declares(m); strings.TrimSpace(declared) == "" {
			declared = "nothing beyond computing its output"
		}
	}
	return fmt.Sprintf("TAP: allow this to save the primitive %s to your TAP collection, so later sessions can find and run it? It declares:\n%s\nRunning it later still needs your agreement.", what, declared)
}

// geminiRunQuestion is the first-run question for tap_run, and false when
// there is nothing to ask: the package is trusted, needs no trust, runs
// unasked because Gemini's own settings let it do what it declares
// (autotrust.go), or is not found (tap_run then refuses it).
func geminiRunQuestion(cwd string, serveArgs []string, args map[string]any, gemini int) (string, bool) {
	ref, _ := args["ref"].(string)
	digest, _ := args["digest"].(string)
	if digest == "" || newTrustStore().has(digest) {
		return "", false
	}
	var roots []string
	for i, a := range serveArgs {
		if a == "--catalog-root" && i+1 < len(serveArgs) {
			roots = append(roots, serveArgs[i+1])
		} else if v, ok := strings.CutPrefix(a, "--catalog-root="); ok {
			roots = append(roots, v)
		}
	}
	entries, err := localCatalogIn(cwd, roots...)
	if err != nil {
		return "", false
	}
	entry, err := resolveCatalog(entries, ref, digest)
	if err != nil || entry.Manifest == nil || !needsPackageTrust(entry.Manifest) {
		return "", false
	}
	m := entry.Manifest
	if home, err := os.UserHomeDir(); err == nil {
		if ok, _ := autoTrustReads(m, "gemini-cli-mcp-client", home, gemini); ok {
			return "", false
		}
	}
	reads := ""
	if len(m.Fetch) > 0 {
		reads = " Allowing it also lets it send GET and HEAD requests to the web origins it declares, now and in later runs of this version."
	}
	return fmt.Sprintf("TAP: this is the first run of the primitive %s (digest %.12s) on this machine. It declares:\n%s\nAllow it to run?%s Each tool call it makes is confirmed like any other; any other change it would make is refused.", ref, digest, declares(m), reads), true
}
