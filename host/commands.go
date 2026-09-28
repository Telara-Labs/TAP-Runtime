package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// command is one entry of the manifest's commands block: a host program the
// runner may start on the primitive's behalf.
//
//   - command: kubectl
//     globals: ["--context minikube", "-n <any>"]
//     args: [get]
//     effect: read
//     env: [KUBECONFIG]
//
// args is the subcommand. It must be the first thing on the command line
// after any globals. globals are the only flags allowed before it: a flag
// followed by a literal allows that value and no other, and a flag followed
// by <any> allows any value. A flag with nothing after it takes no value.
// What follows the subcommand is not bounded.
type command struct {
	Command string   `yaml:"command"`
	Globals []string `yaml:"globals"`
	Args    []string `yaml:"args"`
	Effect  string   `yaml:"effect"`
	Env     []string `yaml:"env"`
}

// arbitraryCode lists invocations that run code the manifest cannot describe.
// Whatever the author declared, these are treated as destructive. This is a
// hand-maintained list, which doc 34 section 13.4 names as a weakness.
var arbitraryCode = [][]string{
	{"bash"}, {"sh"}, {"zsh"}, {"env"}, {"xargs"},
	{"docker", "run"}, {"docker", "exec"},
	{"kubectl", "exec"}, {"kubectl", "run"},
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

// resolve finds the declared command an invocation falls under. The longest
// matching subcommand wins, so `kubectl delete` does not fall under a
// declared bare `kubectl`.
func resolve(m *manifest, name string, args []string) (*command, string) {
	var best *command
	var bestRest []string
	for i := range m.Commands {
		c := &m.Commands[i]
		if c.Command != name {
			continue
		}
		rest, ok := stripGlobals(c.Globals, args)
		if !ok || !hasPrefix(rest, c.Args) {
			continue
		}
		if best == nil || len(c.Args) > len(best.Args) {
			best, bestRest = c, rest
		}
	}
	if best == nil {
		return nil, ""
	}
	effect := best.Effect
	argv := append([]string{name}, bestRest...)
	for _, p := range arbitraryCode {
		if hasPrefix(argv, p) {
			effect = "destructive"
		}
	}
	return best, effect
}

// environFor builds the environment of one host program from the runner's
// own: the base names and the names this command declares, nothing else.
func environFor(c *command, lookup func(string) (string, bool)) (env []string, names []string) {
	seen := map[string]bool{}
	for _, n := range append(append([]string{}, baseEnv...), c.Env...) {
		if seen[n] {
			continue
		}
		seen[n] = true
		if v, ok := lookup(n); ok {
			env = append(env, n+"="+v)
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return env, names
}

func runCommand(m *manifest, rq request, approve bool, journal io.Writer) reply {
	line := strings.TrimSpace(rq.Command + " " + strings.Join(rq.Args, " "))
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
		return reply{Refused: effect + " command needs approval"}
	}
	path, err := exec.LookPath(rq.Command)
	if err != nil {
		logf("  ABSENT   %s  (program not on this machine)", line)
		record("refused_absent", nil)
		return reply{Refused: "program not installed on this machine"}
	}
	t0 := time.Now()
	cmd := exec.Command(path, rq.Args...)
	var names []string
	cmd.Env, names = environFor(decl, os.LookupEnv)
	cwd, _ := os.Getwd()
	cmd.Dir = cwd
	if rq.Stdin != "" {
		cmd.Stdin = strings.NewReader(rq.Stdin)
	}
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
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
	logf("  run      %s  [%s] exit=%d in=%dB out=%dB %s", line, effect, exit, len(rq.Stdin), so.Len(), time.Since(t0).Round(time.Millisecond))
	record("ran", map[string]any{"exit": exit, "stdin_bytes": len(rq.Stdin), "stdout_bytes": so.Len(),
		"ms": time.Since(t0).Milliseconds(), "cwd": cwd, "env": names})
	return reply{Stdout: so.String(), Stderr: se.String(), Exit: exit}
}
