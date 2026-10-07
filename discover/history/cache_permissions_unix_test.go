//go:build !windows

package history

import (
	"os"
	"testing"
)

func assertPrivateCacheFile(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("cache file mode: %v %v, want private mode 0600", info, err)
	}
}
