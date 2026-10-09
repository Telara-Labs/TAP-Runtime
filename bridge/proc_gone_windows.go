//go:build windows

package bridge

import (
	"os/exec"
	"time"
)

// awaitGroupGone is a no-op on Windows: killTree's taskkill /T returns once
// the tree is gone.
func awaitGroupGone(*exec.Cmd, time.Duration) {}
