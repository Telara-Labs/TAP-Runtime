package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// OpenCode and Crush have no bridge, so TAP cannot borrow their connections,
// but they still search TAP and can save a recurring task. Without the skill
// they answered from memory and never called tap_search.
func TestSetupWritesTheAuthorSkillForAgentsWithoutABridge(t *testing.T) {
	interpreterStore(t)
	for client, dir := range map[string]string{
		"opencode": ".config/opencode/skills",
		"crush":    ".config/crush/skills",
	} {
		t.Run(client, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			var out, errOut bytes.Buffer
			if rc := installCommand([]string{"--client", client}, &out, &errOut); rc != 0 {
				t.Fatalf("install %d: %s", rc, errOut.String())
			}
			got, err := os.ReadFile(filepath.Join(home, dir, authorSkillName, "SKILL.md"))
			if err != nil || string(got) != authorSkill {
				t.Fatalf("skill not written: %v\n%s", err, out.String())
			}
		})
	}
}
