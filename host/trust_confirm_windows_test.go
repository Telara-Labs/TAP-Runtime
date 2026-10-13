//go:build windows

package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The Windows console device remains available to a process with redirected
// stdin. Exercise that exact case in a child process so a regression that
// opens CONIN$ cannot wedge the test runner.
func TestWindowsRedirectedTrustPromptRefusesBeforeOpeningConsole(t *testing.T) {
	if len(os.Args) > 1 && os.Args[len(os.Args)-1] == "trust-prompt-helper" {
		_, err := askAtTerminal("must not open CONIN$")
		if err == nil || !strings.Contains(err.Error(), "standard input is not a terminal") {
			t.Fatalf("askAtTerminal error = %v, want redirected-input refusal", err)
		}
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestWindowsRedirectedTrustPromptRefusesBeforeOpeningConsole$", "--", "trust-prompt-helper")
	cmd.Stdin = strings.NewReader("")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("redirected trust prompt helper failed: %v\n%s", err, output.String())
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("redirected trust prompt blocked instead of refusing; child output:\n%s", output.String())
	}
}
