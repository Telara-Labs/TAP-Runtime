//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

// detach starts cmd in a session of its own, so it outlives the agent
// session that started it and no signal to that session reaches it.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

// shortSocketDir is a directory for the runner's socket when the cache
// directory's path is too long for one: in the system's temporary directory,
// named for this user, and refused unless it is this user's alone.
func shortSocketDir() (string, error) {
	dir := filepath.Join(os.TempDir(), "tap-"+strconv.Itoa(os.Getuid()))
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || int(st.Uid) != os.Getuid() || info.Mode().Perm() != 0o700 {
		return "", fmt.Errorf("%s is not a private directory of this user", dir)
	}
	return dir, nil
}
