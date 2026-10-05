//go:build windows

package bridge

import (
	"os/exec"
	"strconv"
	"syscall"
)

// ownGroup starts cmd in a process group of its own (see proc_unix.go).
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// killTree kills cmd and every process under it.
func killTree(cmd *exec.Cmd) {
	if cmd.Process != nil {
		exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		cmd.Process.Kill()
	}
}
