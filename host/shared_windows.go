//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// detach starts cmd with no console and in a process group of its own, so
// it outlives the agent session that started it.
func detach(cmd *exec.Cmd) {
	const detachedProcess, newProcessGroup = 0x00000008, 0x00000200
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess | newProcessGroup}
}

// shortSocketDir is a directory for the runner's socket when the cache
// directory's path is too long for one. Windows cache paths are short, and
// its temporary directory is already the user's own.
func shortSocketDir() (string, error) {
	dir := filepath.Join(os.TempDir(), "tap-runner")
	return dir, os.MkdirAll(dir, 0o700)
}
