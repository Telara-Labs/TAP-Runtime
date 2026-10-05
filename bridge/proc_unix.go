//go:build !windows

package bridge

import (
	"os/exec"
	"syscall"
)

// ownGroup starts cmd in a process group of its own, so killTree reaches
// every process it starts. The kilo command is a launcher whose child is
// the server: killing the launcher alone left the server running.
func ownGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// killTree kills cmd's process group.
func killTree(cmd *exec.Cmd) {
	if cmd.Process != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
