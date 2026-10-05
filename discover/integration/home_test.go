package integration

import (
	"path/filepath"
	"testing"
)

// setHome points every place the program finds the person's home and
// application folders at home: HOME on Unix, USERPROFILE, APPDATA and
// LOCALAPPDATA on Windows, and the XDG folders on Linux. Setting HOME
// alone left Windows reading the real profile.
func setHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
}
