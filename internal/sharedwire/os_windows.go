//go:build windows

package sharedwire

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

// LockFile waits for an exclusive lock on f. The lock is released when f is
// closed or the process ends, so a crashed holder never keeps it.
func LockFile(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &windows.Overlapped{})
}

// TryLockFile takes an exclusive lock on f, or fails at once if another
// process holds it.
func TryLockFile(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
}

// Detach starts cmd with no console and in a process group of its own, so
// it outlives the agent session that started it.
func Detach(cmd *exec.Cmd) {
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
