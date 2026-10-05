package history

import (
	"path/filepath"
	"testing"
)

// TENG-3171: Antigravity names a saved large output with a file:// link;
// on Windows it writes the drive and backslashes straight after file://.
func TestAntigravityFilePath(t *testing.T) {
	for link, want := range map[string]string{
		"file:///Users/a/conv/.system_generated/steps/2/output.txt":        "/Users/a/conv/.system_generated/steps/2/output.txt",
		`file://C:\Users\runner\conv\.system_generated\steps\2\output.txt`: "C:/Users/runner/conv/.system_generated/steps/2/output.txt",
		"file:///C:/Users/runner/conv/out%20put.txt":                       "C:/Users/runner/conv/out put.txt",
		"file://localhost/home/u/x.txt":                                    "/home/u/x.txt",
	} {
		got, ok := antigravityFilePath(link)
		if !ok || got != filepath.Clean(filepath.FromSlash(want)) {
			t.Errorf("%s -> %q %v, want %q", link, got, ok, want)
		}
	}
	for _, link := range []string{"https://x/y", "file://relative/path", "file://"} {
		if p, ok := antigravityFilePath(link); ok {
			t.Errorf("%s read as %q", link, p)
		}
	}
}
