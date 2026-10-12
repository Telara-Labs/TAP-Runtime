package main

import (
	agents "github.com/Telara-Labs/TAP-Runtime/discover/client"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// tapMainArg makes the test binary act as the runner.
const tapMainArg = "-tap-run-main"

// No test may read or write the real user's configuration: it holds the
// choices a person made (tap bind) and the packages they trust.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "tap-config-")
	if err != nil {
		panic(err)
	}
	userConfigDir = func() (string, error) { return dir, nil }
	// Nor may a test fill the person's cache with interpreters, compiled
	// modules or run records. The default roots resolve to a directory of the
	// test binary's own. A test that needs the real one asks for it with
	// useRealCache.
	cacheDir := filepath.Join(dir, "cache")
	cacheRoot = func() (string, error) { return cacheDir, nil }
	// Detection also counts an agent's program on PATH; tests say what is
	// installed, not the machine running them.
	agents.LookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	// tap trust asks the person at the terminal; tests answer yes for them,
	// except the test of the question itself.
	confirmTrust = func(string) (bool, error) { return true, nil }
	// A busy CI runner takes over 20 seconds for a run that compiles its
	// interpreter cold; tests read the result inline unless they test the
	// handoff (handoff_test.go sets its own deadline).
	handoffAfter = 10 * time.Minute
	// A test that needs the runner as a separate process runs this test
	// binary with tapMainArg first, rather than building the runner again.
	if len(os.Args) > 1 && os.Args[1] == tapMainArg {
		os.Args = append([]string{"tap"}, os.Args[2:]...)
		main()
		os.RemoveAll(dir)
		os.Exit(0)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
