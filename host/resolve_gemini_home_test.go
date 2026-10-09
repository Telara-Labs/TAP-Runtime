package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Gemini CLI reads its user settings from GEMINI_CLI_HOME when set, and a
// system settings file from GEMINI_CLI_SYSTEM_SETTINGS_PATH. The runner
// reads the servers Gemini has from the same files Gemini does.
func TestGeminiSettingsFilesFollowGeminisOwnLocations(t *testing.T) {
	wd := t.TempDir()
	t.Run("default home", func(t *testing.T) {
		t.Setenv("GEMINI_CLI_HOME", "")
		t.Setenv("GEMINI_CLI_SYSTEM_SETTINGS_PATH", "")
		home, _ := os.UserHomeDir()
		got := geminiSettingsFiles(wd)
		want := []string{filepath.Join(home, ".gemini", "settings.json"), filepath.Join(wd, ".gemini", "settings.json")}
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("files = %v, want %v", got, want)
		}
	})
	t.Run("relocated home and system settings", func(t *testing.T) {
		gh := t.TempDir()
		sys := filepath.Join(t.TempDir(), "system.json")
		t.Setenv("GEMINI_CLI_HOME", gh)
		t.Setenv("GEMINI_CLI_SYSTEM_SETTINGS_PATH", sys)
		got := geminiSettingsFiles(wd)
		want := map[string]bool{filepath.Join(gh, ".gemini", "settings.json"): true, sys: true, filepath.Join(wd, ".gemini", "settings.json"): true}
		if len(got) != 3 {
			t.Fatalf("files = %v, want the relocated home, the system file and the project", got)
		}
		for _, f := range got {
			if !want[f] {
				t.Fatalf("files = %v: %s is not one Gemini reads", got, f)
			}
		}
	})
}
