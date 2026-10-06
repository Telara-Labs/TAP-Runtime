package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The runner imports the published Discover module, which can lag behind the
// source in discover/. Exercise the actual built CLI so a stale dependency
// cannot turn a help request into a usage error in a release.
func TestDiscoverCLIHelpThroughPublishedModule(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	bin := filepath.Join(t.TempDir(), "tap")
	build := exec.CommandContext(ctx, "go", "build", "-o", bin, "./host")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the runner: %v\n%s", err, out)
	}
	for _, tc := range []struct {
		name string
		args []string
		code int
	}{
		{"help", []string{"--help"}, 0},
		{"short help", []string{"-h"}, 0},
		{"save help", []string{"save", "--help"}, 0},
		{"brief help", []string{"brief", "--help"}, 0},
		{"validate help", []string{"validate", "--help"}, 0},
		{"unknown flag", []string{"--no-such-flag"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.CommandContext(ctx, bin, append([]string{"discover"}, tc.args...)...)
			cmd.Dir = t.TempDir()
			out, err := cmd.CombinedOutput()
			code := 0
			if err != nil {
				if exit, ok := err.(*exec.ExitError); ok {
					code = exit.ExitCode()
				} else {
					t.Fatalf("running discover: %v\n%s", err, out)
				}
			}
			if code != tc.code {
				t.Errorf("discover %v exited %d, want %d\n%s", tc.args, code, tc.code, out)
			}
			if !strings.Contains(string(out), "Usage") {
				t.Errorf("discover %v printed no usage\n%s", tc.args, out)
			}
		})
	}
}
