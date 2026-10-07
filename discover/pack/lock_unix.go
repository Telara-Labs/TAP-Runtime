//go:build !windows

package pack

import (
	"golang.org/x/sys/unix"
	"os"
)

func lockSave(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) }
func unlockSave(f *os.File)     { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }
