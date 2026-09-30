package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var version = "dev"

// installCommand is `tap install`: it registers this runner with a client
// as an MCP server, using the client's own command to do it, so the format
// of the client's configuration is the client's business.
//
//	tap install --client claude [--scope user] [--print]
//	tap install --client codex [--print]
//	tap install --client gemini [--print]
//	... [--env OTEL_EXPORTER_OTLP_ENDPOINT=URL --env OTEL_EXPORTER_OTLP_HEADERS=...]
//
// --env hands the runner the standard OpenTelemetry variables through the
// client's own configuration, so a run reports to that collector (rulings 26
// and 35). Only OTEL_* names are accepted: those are the only variables the
// runner reads for telemetry, and nothing else is to be configured this way.
//
// It is the one setup step a person takes. With --print it changes nothing
// and shows what it would run.
func installCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	client := fs.String("client", "", "claude, codex or gemini")
	scope := fs.String("scope", "user", "for claude: local, user or project")
	print := fs.Bool("print", false, "show the command and change nothing")
	name := fs.String("name", "tap", "name the client will know the runner by")
	var env envFlags
	fs.Var(&env, "env", "OTEL_* variable for the runner, as NAME=VALUE; repeatable")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		fmt.Fprintln(stderr, "cannot tell where this program is:", err)
		return 1
	}
	argv, err := installArgv(*client, *scope, *name, self, env)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	var settings string
	if *client == "gemini" {
		// Gemini lends its connections through a hook (relay.go), which
		// lives in its settings file beside its MCP servers.
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		settings = filepath.Join(home, ".gemini", "settings.json")
	}
	if settings != "" {
		// Gemini keeps its MCP servers and its hooks in one settings file.
		// Both are written there directly, as its own `mcp add` would.
		if *print {
			fmt.Fprintf(stdout, "add to %s: mcpServers.%s runs %s serve --name %s%s; hooks.AfterTool runs %s\n", settings, *name, self, *name, env.masked(), geminiHookCommand(self))
			return 0
		}
		if err := addGemini(settings, *name, self, env); err != nil {
			fmt.Fprintln(stderr, "not installed:", err)
			return 1
		}
		fmt.Fprintf(stdout, "Added %s and its hook to %s. Start a new Gemini CLI session to use it.\n", *name, settings)
		return 0
	}
	if *print {
		fmt.Fprintln(stdout, strings.Join(maskEnvArgs(argv), " "))
		return 0
	}
	if _, err := exec.LookPath(argv[0]); err != nil {
		fmt.Fprintf(stderr, "%s is not on this machine\n", argv[0])
		return 1
	}
	// A client refuses to add a name it already has, and an entry left from
	// an earlier install may point at an old path (the runner was called
	// tap-runtime before). Remove it first; a failure only means there was
	// none.
	if rm := removeArgv(*client, *scope, *name); rm != nil {
		_ = exec.Command(rm[0], rm[1:]...).Run()
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// removeArgv is the client's command to drop a registration, so installing
// again replaces it instead of failing on the existing name.
func removeArgv(client, scope, name string) []string {
	switch client {
	case "claude":
		return []string{"claude", "mcp", "remove", "--scope", scope, name}
	case "codex":
		return []string{"codex", "mcp", "remove", name}
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
	case "gemini":
		// Written to Gemini's settings by addGemini; shown for --print.
		return []string{"gemini-settings", name, self, "serve", "--name", name}, nil
	case "":
		return nil, fmt.Errorf("say which client: --client claude, --client codex or --client gemini")
	}
	return nil, fmt.Errorf("client %q cannot lend its connections, so there is nothing to install into", client)
}

// envFlags is the repeatable --env NAME=VALUE flag. Only OTEL_* names are
// accepted (ruling 35).
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
