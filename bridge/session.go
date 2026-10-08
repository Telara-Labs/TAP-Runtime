package bridge

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Proc is where a bridge starts its client's program: the working directory
// and environment of the agent session it serves. One runner can serve many
// sessions at once, each started in its own project with its own
// environment, so these cannot come from the runner's own process. The zero
// value uses the runner's own.
type Proc struct {
	Dir string
	Env []string // KEY=value; nil means the runner's own environment
	// ToolMeta is the host-supplied context of this MCP call, distinct from
	// model arguments. It is forwarded only for this run, never persisted as
	// bridge authority or shared between requests.
	ToolMeta json.RawMessage
}

func (p Proc) apply(cmd *exec.Cmd) {
	if p.Dir != "" {
		cmd.Dir = p.Dir
	}
	if p.Env != nil {
		cmd.Env = p.Env
	}
}

// Wd is the session's working directory.
func (p Proc) Wd() string {
	if p.Dir != "" {
		return p.Dir
	}
	wd, _ := os.Getwd()
	return wd
}

// Getenv reads the session's environment.
func (p Proc) Getenv(name string) string {
	if p.Env == nil {
		return os.Getenv(name)
	}
	for i := len(p.Env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(p.Env[i], "="); ok && k == name {
			return v
		}
	}
	return ""
}

// Environ is the session's environment.
func (p Proc) Environ() []string {
	if p.Env == nil {
		return os.Environ()
	}
	return append([]string(nil), p.Env...)
}

// LookPath finds a program on the session's PATH.
func (p Proc) LookPath(name string) (string, error) {
	if p.Env == nil || strings.ContainsAny(name, `/\`) {
		return exec.LookPath(name)
	}
	for _, dir := range filepath.SplitList(p.Getenv("PATH")) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		if path, err := exec.LookPath(filepath.Join(dir, name)); err == nil {
			return path, nil
		}
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}

// command is exec.Command for a program found on the session's PATH, started
// in the session's directory with its environment.
func (p Proc) command(name string, args ...string) *exec.Cmd {
	path := name
	if found, err := p.LookPath(name); err == nil {
		path = found
	}
	cmd := exec.Command(path, args...)
	p.apply(cmd)
	return cmd
}
