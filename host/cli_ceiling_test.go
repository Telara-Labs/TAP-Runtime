package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise the real CLI callback: a fake callback that declines its second
// ask misses a CLI that accidentally renews the same finite consent forever.
func TestCLIApprovalCeilingStopsOwnedWrites(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	bin := filepath.Join(t.TempDir(), "tap")
	build := exec.CommandContext(ctx, "go", "build", "-o", bin, "./host")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build runner: %v\n%s", err, out)
	}
	store := interpreterStore(t)
	cache := t.TempDir()
	for _, tc := range []struct {
		name         string
		flags        []string
		want         int
		separateKind bool
	}{
		{"one", []string{"--approve", "--limit", "1"}, 1, false},
		{"two", []string{"--approve", "--limit", "2"}, 2, false},
		{"zero is unlimited", []string{"--approve", "--limit", "0"}, 3, false},
		{"omitted limit is unlimited", []string{"--approve"}, 3, false},
		{"no approval", nil, 0, false},
		{"limit does not approve", []string{"--limit", "1"}, 0, false},
		{"independent kinds", []string{"--approve", "--limit", "1"}, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest, script := appendManifest, appendScript
			if tc.separateKind {
				manifest = strings.Replace(manifest, "  - {path: in, access: read}", "  - {path: in, access: read}\n  - {path: out, access: write}", 1)
				script += "echo first > out/result.txt\necho second >> out/result.txt\necho complete\n"
			}
			pkg := writePackage(t, manifest, script)
			if tc.separateKind {
				if err := os.Mkdir(filepath.Join(pkg, "out"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			args := append([]string{"--interpreters", store, "--cache", cache, "--runs", t.TempDir()}, tc.flags...)
			cmd := exec.CommandContext(ctx, bin, append(args, pkg)...)
			cmd.Dir = pkg
			// Reuse standard OS configuration variables only, isolating the
			// real CLI from the person's configuration and trusted packages.
			home := t.TempDir()
			cmd.Env = append(os.Environ(), "HOME="+home, "USERPROFILE="+home, "APPDATA="+home, "LOCALAPPDATA="+home)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("run CLI: %v\n%s", err, out)
			}
			data, err := os.ReadFile(filepath.Join(pkg, "counter.txt"))
			if tc.want == 0 && os.IsNotExist(err) {
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := len(strings.Fields(string(data))); got != tc.want {
				t.Fatalf("CLI allowed %d writes, want %d; file=%q\n%s", got, tc.want, data, out)
			}
			if tc.separateKind {
				second, err := os.ReadFile(filepath.Join(pkg, "out", "result.txt"))
				if err != nil || string(second) != "first\n" {
					t.Fatalf("independent file-write kind: %q, %v\n%s", second, err, out)
				}
			}
		})
	}
}
