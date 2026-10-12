package codegen_test

import (
	"os"
	"path/filepath"
	"testing"
)

// hostCacheRoot is the cache the runners these tests build use. A runner
// built by a test would otherwise keep what it downloads and compiles in the
// cache of the person running the tests.
var hostCacheRoot string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "tap-codegen-cache-")
	if err != nil {
		panic(err)
	}
	hostCacheRoot = dir
	seedInterpreters(dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// seedInterpreters copies the interpreters the person has already downloaded
// into the test cache, so that the tests need no network. The runner checks
// each against its pinned digest; the person's own cache is only read.
func seedInterpreters(root string) {
	base, err := os.UserCacheDir()
	if err != nil {
		return
	}
	from := filepath.Join(base, "tap-runtime", "interpreters")
	to := filepath.Join(root, "interpreters")
	os.MkdirAll(to, 0o700)
	for _, pattern := range []string{"python-*.wasm", "qjs-wasi-*.wasm"} {
		files, _ := filepath.Glob(filepath.Join(from, pattern))
		for _, f := range files {
			if b, err := os.ReadFile(f); err == nil {
				os.WriteFile(filepath.Join(to, filepath.Base(f)), b, 0o600)
			}
		}
	}
}

// hostBuildArgs builds the runner into out with the test cache as its default.
func hostBuildArgs(t *testing.T, out string) []string {
	t.Helper()
	return []string{"build", "-ldflags", "-X main.testCacheRoot=" + hostCacheRoot, "-o", out, "./host"}
}
