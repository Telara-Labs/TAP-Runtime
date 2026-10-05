package bridge

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveClaudeExecutablePrefersNativeInstall(t *testing.T) {
	home := t.TempDir()
	native := filepath.Join(home, ".local", "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(native), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(native, []byte("native"), 0o700); err != nil {
		t.Fatal(err)
	}
	pathCalled := false
	got, err := resolveClaudeExecutable(home, "darwin", func(string) (string, error) {
		pathCalled = true
		return "/stale/path/claude", nil
	})
	if err != nil || got != native {
		t.Fatalf("resolved %q, %v; want native executable %q", got, err, native)
	}
	if pathCalled {
		t.Fatal("looked on PATH despite an executable native install")
	}
}

func TestResolveClaudeExecutableFallsBackToPath(t *testing.T) {
	for _, tc := range []struct {
		name       string
		makeNative bool
		mode       os.FileMode
	}{
		{name: "missing"},
		{name: "not executable", makeNative: true, mode: 0o600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			native := filepath.Join(home, ".local", "bin", "claude")
			if tc.makeNative {
				if err := os.MkdirAll(filepath.Dir(native), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(native, []byte("not executable"), tc.mode); err != nil {
					t.Fatal(err)
				}
			}
			pathExecutable := filepath.Join(t.TempDir(), "claude")
			if err := os.WriteFile(pathExecutable, []byte("path"), 0o700); err != nil {
				t.Fatal(err)
			}
			got, err := resolveClaudeExecutable(home, "darwin", func(name string) (string, error) {
				if name != "claude" {
					t.Fatalf("looked up %q on PATH, want claude", name)
				}
				return pathExecutable, nil
			})
			if err != nil || got != pathExecutable {
				t.Fatalf("resolved %q, %v; want PATH executable %q", got, err, pathExecutable)
			}
		})
	}
}

func TestResolveClaudeExecutableUsesExeNameForWindows(t *testing.T) {
	home := t.TempDir()
	native := filepath.Join(home, ".local", "bin", "claude.exe")
	if err := os.MkdirAll(filepath.Dir(native), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(native, []byte("native"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveClaudeExecutable(home, "windows", func(string) (string, error) {
		t.Fatal("Windows native install should be preferred")
		return "", nil
	})
	if err != nil || got != native {
		t.Fatalf("resolved %q, %v; want %q", got, err, native)
	}
}

func TestResolveClaudeExecutableReportsMissingInstall(t *testing.T) {
	wantErr := errors.New("missing")
	got, err := resolveClaudeExecutable(t.TempDir(), "darwin", func(name string) (string, error) {
		if name != "claude" {
			t.Fatalf("looked up %q on PATH, want claude", name)
		}
		return "", wantErr
	})
	if got != "" || !errors.Is(err, wantErr) {
		t.Fatalf("resolved %q, %v; want wrapped missing error", got, err)
	}
}
