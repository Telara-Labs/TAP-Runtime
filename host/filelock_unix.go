//go:build !windows

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// lockFile waits for an exclusive lock on f. The lock is released when f is
// closed or the process ends, so a crashed reader never holds it.
func lockFile(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_EX) }

// tryLockFile takes an exclusive lock on f, or fails at once if another
// process holds it.
func tryLockFile(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) }
