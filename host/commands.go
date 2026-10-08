package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Telara-Labs/TAP-Runtime/bridge"
	"github.com/Telara-Labs/TAP-Runtime/contract/glob"
	"io"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// A command (manifest.Command) is one entry of the manifest's commands block:
// a host program the runner may start on the primitive's behalf.
//
//   - command: kubectl
//     globals: ["--context minikube", "-n <any>"]
//     args: [get]
//     effect: read
//     env: [KUBECONFIG]
//
// args are the arguments the program may be given, as bash-style patterns
// Each word is matched against one argument, and a final bare *
// matches whatever remains: [get, pods, "*"] allows `kubectl get pods -n app`
// and refuses `kubectl get secrets`. They are required: a command that
// declares none is refused at admission.
//
// globals are the only flags allowed before them: a flag followed by a
// literal allows that value and no other, a flag followed by <any> allows
// any value, and a flag with nothing after it takes no value.
//
// env names the variables of the runner's environment the program is given,
// and a name may be a pattern such as AWS_*.

// arbitraryCode lists invocations that run code the manifest cannot describe.
// Whatever the author declared, these are treated as destructive. This is a
// hand-maintained list, which is a known weakness.
var arbitraryCode = [][]string{
	{"bash"}, {"sh"}, {"zsh"}, {"env"}, {"xargs"},
	{"docker", "run"}, {"docker", "exec"},
	{"kubectl", "exec"}, {"kubectl", "run"},
	{"python"}, {"python3"}, {"node"}, {"deno"}, {"ruby"}, {"perl"}, {"php"},
	{"ssh"}, {"sudo"},
	// Programs that run another program or a script they are
	// handed, and the subcommands of build tools that run a project's code.
	{"nohup"}, {"nice"}, {"time"}, {"timeout"}, {"watch"}, {"flock"}, {"exec"}, {"eval"},
	{"busybox"}, {"command"}, {"osascript"}, {"pwsh"}, {"powershell"}, {"cmd"}, {"lua"},
	{"awk"}, {"gawk"}, {"mawk"}, {"make"}, {"gmake"}, {"npx"},
	{"npm", "run"}, {"npm", "exec"}, {"npm", "x"}, {"npm", "start"}, {"npm", "test"}, {"npm", "install"}, {"npm", "ci"},
	{"yarn", "run"}, {"yarn", "exec"}, {"pnpm", "run"}, {"pnpm", "exec"}, {"pnpm", "dlx"},
	{"pip", "install"}, {"pip3", "install"},
	{"cargo", "run"}, {"cargo", "build"}, {"cargo", "test"}, {"cargo", "install"},
	{"go", "run"}, {"go", "generate"}, {"go", "test"},
	{"git", "difftool"}, {"git", "mergetool"}, {"git", "filter-branch"}, {"git", "daemon"},
	{"git", "bisect", "run"}, {"git", "submodule", "foreach"},
	{"docker", "compose", "run"}, {"docker", "compose", "exec"},
}

// arbitraryAnywhere lists arguments that make a program run other code
// wherever they appear on its command line. find's -exec and its kin can
// follow any number of paths and tests, so a prefix cannot catch them
// .
var arbitraryAnywhere = map[string][]string{
	"find": {"-exec", "-execdir", "-ok", "-okdir"},
	// Flags that name a program for the command to run. A flag may carry its
	// value as --flag=value, which matches too.
	"git":   {"--upload-pack", "--receive-pack", "--exec", "--ext-cmd"},
	"tar":   {"--to-command", "--checkpoint-action", "--use-compress-program", "-I"},
	"rsync": {"-e", "--rsh", "--rsync-path"},
	"scp":   {"-S", "-o"},
}

// gitGlobalCode reports whether git is given configuration on its command
// line before the subcommand: `-c alias.x=!sh ...` defines an alias that runs
// a shell, `-c core.pager`, `-c core.sshCommand` and the like run programs, and
// --config-env and --exec-path point git at code. After the subcommand, -c is
// an ordinary flag (git log -c), so only the leading flags are read.
func gitGlobalCode(args []string) bool {
	for i := 0; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
		a := args[i]
		switch {
		case a == "-c", strings.HasPrefix(a, "-c") && len(a) > 2 && !strings.HasPrefix(a, "--"),
			a == "--config-env", strings.HasPrefix(a, "--config-env="),
			a == "--exec-path", strings.HasPrefix(a, "--exec-path="):
			return true
		case a == "-C", a == "--git-dir", a == "--work-tree", a == "--namespace":
			i++ // these take a value, which is not a flag
		}
	}
	return false
}

// runsArbitraryCode reports whether an invocation runs code the manifest
// cannot describe.
func runsArbitraryCode(name string, args []string) bool {
	argv := append([]string{name}, args...)
	for _, p := range arbitraryCode {
		if hasPrefix(argv, p) {
			return true
		}
	}
	for _, flag := range arbitraryAnywhere[name] {
		for _, a := range args {
			if a == flag || (strings.HasPrefix(flag, "--") && strings.HasPrefix(a, flag+"=")) {
				return true
			}
		}
	}
	return name == "git" && gitGlobalCode(args)
}

// baseEnv is what every host program is given. Everything else in the
// runner's environment is withheld unless the manifest names it, so a token
// that happens to be set where the runner started does not reach a program
// the approver never associated with it.
var baseEnv = []string{"PATH", "HOME", "USER", "LANG", "TMPDIR"}

func hasPrefix(argv, prefix []string) bool {
	if len(argv) < len(prefix) {
		return false
	}
	for i := range prefix {
		if argv[i] != prefix[i] {
			return false
		}
	}
	return true
}

// stripGlobals removes the declared globals from the front of args. It
// reports false when args opens with a flag that is not declared, or with a
// declared flag carrying a value that is not allowed.
func stripGlobals(globals, args []string) ([]string, bool) {
	type rule struct {
		takesValue bool
		any        bool
		values     map[string]bool
	}
	rules := map[string]*rule{}
	for _, g := range globals {
		f := strings.Fields(g)
		if len(f) == 0 {
			continue
		}
		r := rules[f[0]]
		if r == nil {
			r = &rule{values: map[string]bool{}}
			rules[f[0]] = r
		}
		if len(f) > 1 {
			r.takesValue = true
			if f[1] == "<any>" {
				r.any = true
			} else {
				r.values[strings.Join(f[1:], " ")] = true
			}
		}
	}
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		flag, value, inline := args[0], "", false
		if i := strings.Index(flag, "="); i > 0 {
			flag, value, inline = flag[:i], flag[i+1:], true
		}
		r := rules[flag]
		if r == nil {
			return nil, false
		}
		switch {
		case !r.takesValue:
			if inline {
				return nil, false
			}
			args = args[1:]
		case inline:
			if !r.any && !r.values[value] {
				return nil, false
			}
			args = args[1:]
		default:
			if len(args) < 2 || (!r.any && !r.values[args[1]]) {
				return nil, false
			}
			args = args[2:]
		}
	}
	return args, true
}

// resolve finds the declared command an invocation falls under. When several
// match, the one that says most wins: more words written out, then more
// words in all. So `kubectl delete pod x` falls under a declared
// [delete, "*"] and not under a declared ["*"].
func resolve(m *manifest, name string, args []string) (*command, string) {
	var best *command
	var bestRest []string
	for i := range m.Commands {
		c := &m.Commands[i]
		if c.Command != name {
			continue
		}
		// A declaration with globals names the only flags allowed before its
		// arguments. One without is matched against the whole command line:
		// its patterns say everything it allows, flags included.
		rest, ok := args, true
		if len(c.Globals) > 0 {
			rest, ok = stripGlobals(c.Globals, args)
		}
		if !ok || !glob.Args(c.Args, rest) {
			continue
		}
		if best == nil || literal(c.Args) > literal(best.Args) ||
			(literal(c.Args) == literal(best.Args) && len(c.Args) > len(best.Args)) {
			best, bestRest = c, rest
		}
	}
	if best == nil {
		return nil, ""
	}
	effect := best.Effect
	// Matching may strip permitted global flags, but those flags still reach
	// the program and must participate in its effect classification.
	if runsArbitraryCode(name, args) || runsArbitraryCode(name, bestRest) {
		effect = "destructive"
	}
	return best, effect
}

// literal counts the words of a pattern that are written out in full.
func literal(pattern []string) int {
	n := 0
	for _, w := range pattern {
		if !glob.HasMeta(w) {
			n++
		}
	}
	return n
}

// environFor builds the environment of one host program from the runner's
// own: the base names and whatever this command's declarations match,
// nothing else. environ is the runner's environment as KEY=value.
func environFor(c *command, environ []string) (env []string, names []string) {
	sort.Strings(environ)
	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			continue
		}
		give := false
		for _, b := range baseEnv {
			give = give || b == name
		}
		for _, pattern := range c.Env {
			give = give || glob.Word(pattern, name)
		}
		if give {
			env = append(env, kv)
			names = append(names, name)
		}
	}
	return env, names
}

func runCommand(ctx context.Context, p bridge.Proc, m *manifest, rq request, approve bool, journal io.Writer) reply {
	line := logCommand(rq.Command, rq.Args)
	decl, effect := resolve(m, rq.Command, rq.Args)
	entry := map[string]any{"ts": time.Now().UTC().Format(time.RFC3339Nano), "command": rq.Command, "args": rq.Args}
	record := func(outcome string, extra map[string]any) {
		entry["outcome"] = outcome
		for k, v := range extra {
			entry[k] = v
		}
		b, _ := json.Marshal(entry)
		journal.Write(append(b, '\n'))
	}
	if decl == nil {
		logf("  REFUSED  %s  (not declared)", line)
		record("refused_undeclared", nil)
		return reply{Refused: "command not declared in primitive.yaml"}
	}
	entry["effect"] = effect
	if effect != "read" && !approve {
		logf("  GATED    %s  (%s, no approval)", line, effect)
		record("gated", nil)
		return reply{Refused: effect + " command needs approval", Gated: true}
	}
	path, err := p.LookPath(rq.Command)
	if err != nil {
		logf("  ABSENT   %s  (program not on this machine)", line)
		record("refused_absent", nil)
		return reply{Refused: "program not installed on this machine"}
	}
	t0 := time.Now()
	cctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, path, rq.Args...)
	// A child may inherit a program's output pipe. Do not wait without a
	// bound for that pipe after the dispatched program has been stopped.
	cmd.WaitDelay = 250 * time.Millisecond
	var names []string
	cmd.Env, names = environFor(decl, p.Environ())
	cwd := p.Wd()
	cmd.Dir = cwd
	if rq.Stdin != "" {
		cmd.Stdin = strings.NewReader(rq.Stdin)
	}
	so, se := &cappedBuffer{max: maxCommandOutput}, &cappedBuffer{max: maxCommandOutput}
	cmd.Stdout, cmd.Stderr = so, se
	exit := 0
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		} else {
			exit = 127
			se.WriteString(err.Error())
		}
	}
	if cctx.Err() != nil {
		if ctx.Err() != nil {
			se.WriteString("\nthe runner stopped this program with the primitive")
		} else {
			se.WriteString("\nthe runner stopped this program after " + commandTimeout.String())
		}
	}
	if so.truncated || se.truncated {
		se.WriteString("\noutput past the runner's limit was dropped")
	}
	logf("  run      %s  [%s] exit=%d in=%dB out=%dB %s", line, effect, exit, len(rq.Stdin), so.Len(), time.Since(t0).Round(time.Millisecond))
	record("ran", map[string]any{"exit": exit, "stdin_bytes": len(rq.Stdin), "stdout_bytes": so.Len(),
		"ms": time.Since(t0).Milliseconds(), "cwd": cwd, "env": names})
	return reply{Stdout: so.String(), Stderr: se.String(), Exit: exit}
}
