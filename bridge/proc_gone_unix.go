//go:build !windows

package bridge

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// awaitGroupGone waits, up to within, until no process is left in cmd's
// process group (started with ownGroup), so nothing it started is still
// writing when the caller goes on.
func awaitGroupGone(cmd *exec.Cmd, within time.Duration) {
	if cmd.Process == nil {
		return
	}
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-cmd.Process.Pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
