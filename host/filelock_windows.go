//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFile waits for an exclusive lock on f. The lock is released when f is
// closed or the process ends, so a crashed reader never holds it.
func lockFile(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &windows.Overlapped{})
}

// tryLockFile takes an exclusive lock on f, or fails at once if another
// process holds it.
func tryLockFile(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
}
