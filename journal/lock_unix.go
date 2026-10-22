//go:build !windows

package journal

import (
	"os"

	"golang.org/x/sys/unix"
)

// lockFile blocks until it holds an exclusive lock on f. The lock is released
// when f is closed or the process dies.
func lockFile(f *os.File) error {
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX)
		if err != unix.EINTR {
			return err
		}
	}
}
