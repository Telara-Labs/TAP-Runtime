//go:build windows

package rebuild

import "os/exec"

// Windows retains CommandContext's direct-process cancellation and WaitDelay.
func configureBuildProcess(cmd *exec.Cmd) {}
