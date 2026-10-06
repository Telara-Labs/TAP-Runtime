package main

import (
	"encoding/json"
	"flag"
	"fmt"
	agents "github.com/Telara-Labs/TAP-Runtime/discover/client"
	"github.com/Telara-Labs/TAP-Runtime/discover/pack"
	"github.com/Telara-Labs/TAP-Runtime/discover/termart"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var version = "dev"

// installCommand is `tap install`: it connects this runner to agents as an
// MCP server. Agents come from discover's registry: an agent
// with its own `mcp add` (Claude Code, Codex, Copilot CLI) is asked to add
// it, so the format of its configuration stays its business; an agent that
// keeps servers in a JSON file (Cursor, Windsurf) gets one entry merged in;
// Gemini CLI also gets the hook its bridge needs; VS Code is connected by the
// TAP extension.
//
//	tap install --client claude-code|codex|...|all|detected [--scope user] [--print] [--remove]
//	... [--env OTEL_EXPORTER_OTLP_ENDPOINT=URL --env OTEL_EXPORTER_OTLP_HEADERS=...]
//
// --env hands the runner the standard OpenTelemetry variables through the
// client's own configuration, so a run reports to that collector. Only OTEL_* names are accepted: those are the only variables the
// runner reads for telemetry, and nothing else is to be configured this way.
//
// It is the one setup step a person takes. With --print it changes nothing
// and shows what it would do. With all or detected, an agent whose program
// is not on this machine is reported and skipped.
func installCommand(args []string, stdout, stderr io.Writer) int {
	return install(args, stdout, stderr, false)
}

// install is installCommand; animate draws the setup diagram when stdout is a
// wide terminal and the run changes something.
func install(args []string, stdout, stderr io.Writer, animate bool) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	client := fs.String("client", "", "agents to connect: "+strings.Join(agents.IDs(agents.HasMCP), ", ")+", all, or detected (installed here)")
	scope := fs.String("scope", "user", "for Claude Code: local, user or project")
	print := fs.Bool("print", false, "show what would change and change nothing")
	remove := fs.Bool("remove", false, "remove the runner from these agents instead")
	name := fs.String("name", "tap", "name the client will know the runner by")
	var env envFlags
	fs.Var(&env, "env", "OTEL_* variable for the runner, as NAME=VALUE; repeatable")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*client) == "" {
		fmt.Fprintf(stderr, "say which agents: --client %s, all or detected\n", strings.Join(agents.IDs(agents.HasMCP), ", "))
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	targets, err := agents.Resolve(*client, home, agents.CapMCP, agents.HasMCP)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if len(targets) == 0 {
		fmt.Fprintf(stdout, "No agent TAP can connect to is installed here.\nInstall one of %s, then run: tap setup\n", strings.Join(agents.IDs(agents.HasMCP), ", "))
		return 0
	}
	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		fmt.Fprintln(stderr, "cannot tell where this program is:", err)
		return 1
	}
	// Named agents must succeed; with all or detected, a missing program is
	// reported and skipped.
	explicit := true
	for _, n := range strings.Split(*client, ",") {
		if n = strings.TrimSpace(n); n == "all" || n == "detected" {
			explicit = false
		}
	}
	code := 0
	var pointerTargets []pack.Target
	var board *setupBoard
	if animate && !*print && !*remove && termart.Terminal(stdout) && termart.Wide(stdout) {
		board = newSetupBoard(stdout, targets)
	}
	for i, c := range targets {
		out, errs := board.begin(i, stdout, stderr)
		rc := installOne(c, home, self, *name, *scope, env, *print, *remove, explicit, out, errs)
		if rc > code {
			code = rc
		}
		if rc == 0 && !*remove && agents.HasSkills(c) && c.Bridge && (*print || c.Connected(home, *name)) {
			pointerTargets = append(pointerTargets, pack.Target{Client: c})
		}
		if rc == 0 && agents.HasSkills(c) && c.Bridge {
			src := syncAuthorSkill(c, home, *print, *remove, out, errs)
			if src > code {
				code = src
			}
			if src > rc {
				rc = src
			}
		}
		board.end(i, rc)
	}
	board.stop(stdout, stderr)
	if !*remove && len(pointerTargets) > 0 {
		collection, err := pack.CollectionDir()
		if err != nil {
			fmt.Fprintln(stderr, "saved primitive pointers:", err)
			return 1
		}
		if rc := syncCollectionPointers(home, collection, pointerTargets, *print, stdout, stderr); rc > code {
			code = rc
		}
	}
	return code
}

// Reconnect existing saved primitives when a new client is installed. The
// collection is the only package source; arbitrary project folders and
// foreign skill folders are never imported or overwritten by setup.
func syncCollectionPointers(home, collection string, targets []pack.Target, print bool, stdout, stderr io.Writer) int {
	entries, err := os.ReadDir(collection)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "saved primitive pointers:", err)
		return 1
	}
	code := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(collection, entry.Name())
		if _, err := os.Stat(filepath.Join(dir, pack.SavedMarker)); os.IsNotExist(err) {
			continue
		}
		marker, err := pack.ReadMarker(dir)
		if err != nil {
			fmt.Fprintf(stderr, "saved primitive %s: %v\n", entry.Name(), err)
			code = 1
			continue
		}
		id, err := pack.ReadIdentity(dir)
		if err != nil || marker.Name != strings.Split(id.Ref, "@")[0] {
			fmt.Fprintf(stderr, "saved primitive %s: invalid identity or marker (%v)\n", entry.Name(), err)
			code = 1
			continue
		}
		if print {
			for _, target := range targets {
				root, err := target.Client.SkillsDir(false, home, "")
				if err != nil {
					fmt.Fprintln(stderr, err)
					code = 1
					continue
				}
				fmt.Fprintf(stdout, "%s: would reconcile saved primitive %s at %s\n", target.Client.Name, id.Ref, filepath.Join(root, id.Name))
			}
			continue
		}
		results, err := pack.WritePointers(dir, targets, false, home, "")
		fmt.Fprint(stdout, pack.FormatPointers(results))
		if err != nil {
			fmt.Fprintf(stderr, "saved primitive %s: %v\n", entry.Name(), err)
			code = 1
		}
	}
	return code
}

// commandName is the program and argv shape of an agent with its own
// `mcp add`, under the runner's older client names.
func commandName(c agents.Client) string {
	switch c.ID {
	case "claude-code":
		return "claude"
	case "copilot-cli":
		return "copilot"
	}
	return c.ID
}

func installOne(c agents.Client, home, self, name, scope string, env envFlags, print, remove, explicit bool, stdout, stderr io.Writer) int {
	switch c.MCP.Kind {
	case agents.MCPExtension:
		if remove {
			fmt.Fprintf(stdout, "%s: connected by the TAP extension, if it is installed; uninstall the extension in %s to disconnect it.\n", c.Name, c.Name)
			return 0
		}
		fmt.Fprintf(stdout, "%s: connected by the TAP extension (tap-vscode-<version>.vsix from the release); nothing to change here.\n", c.Name)
		return 0
	case agents.MCPYAMLFile:
		path := filepath.Join(home, filepath.FromSlash(c.MCP.Path))
		if print {
			verb := "add to"
			if remove {
				verb = "remove from"
			}
			fmt.Fprintf(stdout, "%s %s: %s.%s runs %s serve --name %s%s\n", verb, path, c.MCP.Key, name, self, name, env.masked())
			return 0
		}
		var e any = gooseEntry(self, name, env)
		if remove {
			e = nil
		}
		changed, err := setYAMLEntry(path, c.MCP.Key, name, e)
		switch {
		case err != nil:
			fmt.Fprintln(stderr, "not changed:", err)
			return 1
		case !changed && remove:
			fmt.Fprintf(stdout, "%s: %s is not connected.\n", c.Name, name)
		case !changed:
			fmt.Fprintf(stdout, "%s: %s already as wanted.\n", c.Name, path)
		case remove:
			fmt.Fprintf(stdout, "%s: removed %s from %s.\n", c.Name, name, path)
		default:
			fmt.Fprintf(stdout, "%s: added %s to %s. Start a new %s session to use it.\n", c.Name, name, path, c.Name)
		}
		return 0
	case agents.MCPJSONFile:
		path := filepath.Join(home, filepath.FromSlash(c.MCP.Path))
		if c.ID == "gemini-cli" {
			// Gemini lends its connections through a hook (relay.go), which
			// lives in its settings file beside its MCP servers.
			if print {
				fmt.Fprintf(stdout, "add to %s: mcpServers.%s runs %s serve --name %s%s; hooks.AfterTool runs %s\n", path, name, self, name, env.masked(), geminiHookCommand(self))
				return 0
			}
			if remove {
				changed, err := removeGemini(path, name)
				if err != nil {
					fmt.Fprintln(stderr, "not removed:", err)
					return 1
				}
				if changed {
					fmt.Fprintf(stdout, "%s: removed %s and its hook from %s.\n", c.Name, name, path)
				} else {
					fmt.Fprintf(stdout, "%s: %s is not connected.\n", c.Name, name)
				}
				return 0
			}
			if err := addGemini(path, name, self, env); err != nil {
				fmt.Fprintln(stderr, "not installed:", err)
				return 1
			}
			fmt.Fprintf(stdout, "Added %s and its hook to %s. Start a new Gemini CLI session to use it.\n", name, path)
			return 0
		}
		entry := mcpEntry(self, name, env)
		if c.MCP.Local {
			entry = localMCPEntry(self, name, env)
		}
		if print {
			verb := "add to"
			if remove {
				verb = "remove from"
			}
			fmt.Fprintf(stdout, "%s %s: %s.%s runs %s serve --name %s%s\n", verb, path, c.MCP.Key, name, self, name, env.masked())
			return 0
		}
		var e any = entry
		if remove {
			e = nil
		}
		changed, err := setMCPEntry(path, c.MCP.Key, name, e)
		if err != nil {
			fmt.Fprintln(stderr, "not changed:", err)
			return 1
		}
		if c.ID == "windsurf" {
			// Windsurf keeps transcripts only while this hook is set
			// (discover reads them).
			hooks := filepath.Join(home, ".codeium", "windsurf", "hooks.json")
			hc, err := addWindsurfHook(hooks, self, remove)
			if err != nil {
				fmt.Fprintln(stderr, "not changed:", err)
				return 1
			}
			changed = changed || hc
		}
		switch {
		case !changed && remove:
			fmt.Fprintf(stdout, "%s: %s is not connected.\n", c.Name, name)
		case !changed:
			fmt.Fprintf(stdout, "%s: %s already as wanted.\n", c.Name, path)
		case remove:
			fmt.Fprintf(stdout, "%s: removed %s from %s.\n", c.Name, name, path)
		default:
			fmt.Fprintf(stdout, "%s: added %s to %s. Start a new %s session to use it.\n", c.Name, name, path, c.Name)
		}
		return 0
	}
	// MCPCommand: the agent's own command.
	host := commandName(c)
	argv, err := installArgv(host, scope, name, self, env)
	if remove {
		argv, err = removeArgv(host, scope, name), nil
	}
	if err != nil || argv == nil {
		fmt.Fprintln(stderr, c.Name+":", err)
		return 2
	}
	if print {
		fmt.Fprintln(stdout, strings.Join(maskEnvArgs(argv), " "))
		return 0
	}
	if _, err := exec.LookPath(argv[0]); err != nil {
		if !explicit {
			fmt.Fprintf(stdout, "%s: skipped, %s is not on this machine.\n", c.Name, argv[0])
			return 0
		}
		fmt.Fprintf(stderr, "%s is not on this machine\n", argv[0])
		return 1
	}
	if !remove {
		// A client refuses to add a name it already has, and an entry left
		// from an earlier install may point at an old path (the runner was
		// called tap-runtime before). Remove it first; a failure only means
		// there was none.
		if rm := removeArgv(host, scope, name); rm != nil {
			_ = exec.Command(rm[0], rm[1:]...).Run()
		}
	}
	if remove {
		// The agent's remove fails when there is nothing to remove, which is
		// what removing everywhere expects to find on most agents.
		out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
		switch {
		case err == nil:
			fmt.Fprintf(stdout, "%s: removed %s.\n", c.Name, name)
		case !explicit:
			fmt.Fprintf(stdout, "%s: %s is not connected (%s).\n", c.Name, name, firstLine(out, err))
		default:
			fmt.Fprintf(stderr, "%s: %s\n", c.Name, firstLine(out, err))
			return 1
		}
		return 0
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(stderr, c.Name+":", err)
		return 1
	}
	return 0
}

// firstLine is the first non-empty line a command printed, or its error.
func firstLine(out []byte, err error) string {
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return err.Error()
}

// mcpEntry is the runner's server entry for a JSON configuration.
func mcpEntry(self, name string, env envFlags) map[string]any {
	entry := map[string]any{"command": self, "args": []any{"serve", "--name", name}}
	if len(env) > 0 {
		vars := map[string]any{}
		for _, kv := range env {
			k, v, _ := strings.Cut(kv, "=")
			vars[k] = v
		}
		entry["env"] = vars
	}
	return entry
}

// localMCPEntry is the runner's server entry in the OpenCode family's
// configuration (Kilo CLI, OpenCode): a local server whose command is one
// array, with its environment under "environment".
func localMCPEntry(self, name string, env envFlags) map[string]any {
	entry := map[string]any{"type": "local", "command": []any{self, "serve", "--name", name}, "enabled": true}
	if e, ok := mcpEntry(self, name, env)["env"]; ok {
		entry["environment"] = e
	}
	return entry
}

// removeArgv is the client's command to drop a registration, so installing
// again replaces it instead of failing on the existing name.
func removeArgv(client, scope, name string) []string {
	switch client {
	case "claude":
		return []string{"claude", "mcp", "remove", "--scope", scope, name}
	case "codex":
		return []string{"codex", "mcp", "remove", name}
	case "copilot":
		return []string{"copilot", "mcp", "remove", name}
	}
	return nil
}

func geminiHookCommand(self string) string {
	return shellQuote(self) + " hook gemini"
}

func shellQuote(s string) string {
	if !strings.ContainsAny(s, " '\"$`\\") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// addGemini registers the runner as an MCP server in Gemini's settings and
// adds its hook, keeping everything else in the file as it was. Running it
// twice leaves one of each. A file that is not plain JSON is left alone and
// the reason given, rather than rewritten.
func addGemini(path, name, self string, env envFlags) error {
	command := geminiHookCommand(self)
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	settings := map[string]any{}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &settings); err != nil {
			return fmt.Errorf("%s is not plain JSON (%v); add the hook by hand: hooks.AfterTool = [{\"matcher\": \".*\", \"hooks\": [{\"name\": \"tap\", \"type\": \"command\", \"command\": %q, \"timeout\": 600000}]}]", path, err, command)
		}
	}
	servers, _ := settings["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	entry := map[string]any{"command": self, "args": []any{"serve", "--name", name}}
	if len(env) > 0 {
		vars := map[string]any{}
		for _, kv := range env {
			k, v, _ := strings.Cut(kv, "=")
			vars[k] = v
		}
		entry["env"] = vars
	}
	servers[name] = entry
	settings["mcpServers"] = servers
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	var kept []any
	groups, _ := hooks["AfterTool"].([]any)
	for _, g := range groups {
		if gm, ok := g.(map[string]any); ok {
			if hs, ok := gm["hooks"].([]any); ok && len(hs) == 1 {
				if h, ok := hs[0].(map[string]any); ok && h["name"] == "tap" {
					continue
				}
			}
		}
		kept = append(kept, g)
	}
	kept = append(kept, map[string]any{
		"matcher": ".*",
		"hooks": []any{map[string]any{
			"name": "tap", "type": "command", "command": command, "timeout": 600000,
			"description": "Carries TAP primitive tool calls; does nothing for other calls.",
		}},
	})
	hooks["AfterTool"] = kept
	settings["hooks"] = hooks
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if len(raw) > 0 {
		if err := os.WriteFile(path+".tap-backup", raw, 0o600); err != nil {
			return err
		}
	}
	return os.WriteFile(path, append(out, '\n'), 0o600)
}

// removeGemini takes the runner's server entry and its hook out of Gemini's
// settings, keeping everything else. A hook left behind would run a program
// that may no longer exist on every Gemini tool call.
func removeGemini(path, name string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return false, nil
	}
	settings := map[string]any{}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return false, fmt.Errorf("%s is not plain JSON (%v); it was left unchanged", path, err)
	}
	changed := false
	if servers, ok := settings["mcpServers"].(map[string]any); ok {
		if _, ok := servers[name]; ok {
			delete(servers, name)
			changed = true
		}
	}
	if hooks, ok := settings["hooks"].(map[string]any); ok {
		groups, _ := hooks["AfterTool"].([]any)
		kept := []any{}
		for _, g := range groups {
			if gm, ok := g.(map[string]any); ok {
				if hs, ok := gm["hooks"].([]any); ok && len(hs) == 1 {
					if h, ok := hs[0].(map[string]any); ok && h["name"] == "tap" {
						changed = true
						continue
					}
				}
			}
			kept = append(kept, g)
		}
		if changed && groups != nil {
			hooks["AfterTool"] = kept
		}
	}
	if !changed {
		return false, nil
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(path+".tap-backup", raw, 0o600); err != nil {
		return false, err
	}
	return true, os.WriteFile(path, append(out, '\n'), 0o600)
}

func installArgv(client, scope, name, self string, env envFlags) ([]string, error) {
	switch client {
	case "claude":
		switch scope {
		case "local", "user", "project":
		default:
			return nil, fmt.Errorf("scope %q is not one Claude Code has", scope)
		}
		argv := []string{"claude", "mcp", "add", "--scope", scope}
		for _, kv := range env {
			argv = append(argv, "-e", kv)
		}
		return append(argv, name, "--", self, "serve"), nil
	case "codex":
		argv := []string{"codex", "mcp", "add"}
		for _, kv := range env {
			argv = append(argv, "--env", kv)
		}
		return append(argv, name, "--", self, "serve"), nil
	case "copilot":
		argv := []string{"copilot", "mcp", "add"}
		for _, kv := range env {
			argv = append(argv, "--env", kv)
		}
		return append(argv, name, "--", self, "serve"), nil
	case "gemini":
		// Written to Gemini's settings by addGemini; shown for --print.
		return []string{"gemini-settings", name, self, "serve", "--name", name}, nil
	case "":
		return nil, fmt.Errorf("say which client: --client claude, --client codex or --client gemini")
	}
	return nil, fmt.Errorf("client %q has no mcp add command; tap install writes its configuration instead", client)
}

// envFlags is the repeatable --env NAME=VALUE flag. Only OTEL_* names are
// accepted.
type envFlags []string

func (e *envFlags) String() string { return strings.Join(*e, ",") }

func (e *envFlags) Set(v string) error {
	k, _, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("--env takes NAME=VALUE, got %q", v)
	}
	if !strings.HasPrefix(k, "OTEL_") {
		return fmt.Errorf("--env %s: only OpenTelemetry (OTEL_*) variables are passed to the runner this way", k)
	}
	*e = append(*e, v)
	return nil
}

// masked lists the names only: a header value can carry a credential.
func (e envFlags) masked() string {
	if len(e) == 0 {
		return ""
	}
	var names []string
	for _, kv := range e {
		k, _, _ := strings.Cut(kv, "=")
		names = append(names, k)
	}
	return " with env " + strings.Join(names, ", ")
}

// maskEnvArgs hides the value of each NAME=VALUE that follows -e or --env.
func maskEnvArgs(argv []string) []string {
	out := append([]string(nil), argv...)
	for i := 1; i < len(out); i++ {
		if out[i-1] == "-e" || out[i-1] == "--env" {
			if k, _, ok := strings.Cut(out[i], "="); ok {
				out[i] = k + "=***"
			}
		}
	}
	return out
}
