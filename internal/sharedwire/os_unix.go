//go:build !windows

package sharedwire

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

// LockFile waits for an exclusive lock on f. The lock is released when f is
// closed or the process ends, so a crashed holder never keeps it.
func LockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX) }

// TryLockFile takes an exclusive lock on f, or fails at once if another
// process holds it.
func TryLockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

// Detach starts cmd in a session of its own, so it outlives the agent
// session that started it and no signal to that session reaches it.
func Detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

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
