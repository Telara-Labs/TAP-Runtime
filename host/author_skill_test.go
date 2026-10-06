package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A fresh machine has no primitive to find. Setup gives the agent the skill
// that says when a procedure is worth saving and how; remove takes it away.
func TestSetupWritesRefreshesAndRemovesTheAuthorSkill(t *testing.T) {
	interpreterStore(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	skill := filepath.Join(home, ".config/goose/skills/tap-author/SKILL.md")
	var out, errOut bytes.Buffer
	if rc := installCommand([]string{"--client", "goose", "--print"}, &out, &errOut); rc != 0 {
		t.Fatalf("print %d: %s", rc, errOut.String())
	}
	if _, err := os.Stat(skill); !os.IsNotExist(err) || !strings.Contains(out.String(), "would write the authoring skill") {
		t.Fatalf("--print wrote the skill or did not say it would: %v\n%s", err, out.String())
	}
	if rc := installCommand([]string{"--client", "goose"}, &out, &errOut); rc != 0 {
		t.Fatalf("install %d: %s", rc, errOut.String())
	}
	got, err := os.ReadFile(skill)
	if err != nil || string(got) != authorSkill {
		t.Fatalf("skill not written as embedded: %v", err)
	}
	// A stale copy TAP wrote is refreshed.
	if err := os.WriteFile(skill, []byte("---\n"+authorSkillMarker+"\n---\nold\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rc := installCommand([]string{"--client", "goose"}, &out, &errOut); rc != 0 {
		t.Fatalf("repeat %d: %s", rc, errOut.String())
	}
	if got, _ := os.ReadFile(skill); string(got) != authorSkill {
		t.Fatal("stale TAP-written skill was not refreshed")
	}
	if rc := installCommand([]string{"--client", "goose", "--remove"}, &out, &errOut); rc != 0 {
		t.Fatalf("remove %d: %s", rc, errOut.String())
	}
	if _, err := os.Stat(filepath.Dir(skill)); !os.IsNotExist(err) {
		t.Fatalf("remove left the skill: %v", err)
	}
}

func TestSetupKeepsAForeignSkillNamedTapAuthor(t *testing.T) {
	interpreterStore(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	skill := filepath.Join(home, ".config/goose/skills/tap-author/SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skill), 0o700); err != nil {
		t.Fatal(err)
	}
	mine := []byte("---\nname: tap-author\n---\nthe person's own\n")
	if err := os.WriteFile(skill, mine, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	for _, args := range [][]string{{"--client", "goose"}, {"--client", "goose", "--remove"}} {
		if rc := installCommand(args, &out, &errOut); rc != 0 {
			t.Fatalf("%v: %d %s", args, rc, errOut.String())
		}
		if got, _ := os.ReadFile(skill); !bytes.Equal(got, mine) {
			t.Fatalf("%v changed a skill TAP did not write", args)
		}
	}
	if !strings.Contains(out.String(), "which TAP did not write") {
		t.Fatalf("foreign skill not reported:\n%s", out.String())
	}
}

func TestEmptySearchSaysWhetherTheTaskRecurs(t *testing.T) {
	// The history read starts when the agent connects; an earlier session
	// asked for the same kind of task with other values.
	stubHistory(t, []pastRequest{
		req("earlier", 2, "Can you check whether GitLab Runner commit 3c39fceb is ready to release after v19.4.0?"),
		req("other", 1, "Summarize the open Jira tickets for the billing team"),
	})
	c := startServer(t, true, accept)
	search := toolObject(t, c.call("tools/call", map[string]any{"name": "tap_search", "arguments": map[string]any{"query": "gitlab runner commit release readiness"}}))
	if m, _ := search["matches"].([]any); len(m) != 0 {
		t.Fatalf("matches = %#v", m)
	}
	if note, _ := search["note"].(string); !strings.Contains(note, "1 earlier session") || !strings.Contains(note, "tap-author") || !strings.Contains(note, "discover brief --task claude-code/earlier/0") || !strings.Contains(note, runnerCommand()) {
		t.Fatalf("recurring task note: %#v", search)
	}
	search = toolObject(t, c.call("tools/call", map[string]any{"name": "tap_search", "arguments": map[string]any{"query": "draft a launch email"}}))
	if note, _ := search["note"].(string); !strings.Contains(note, "Do the task as usual") || strings.Contains(note, "tap-author") {
		t.Fatalf("first-time task offered for saving: %#v", search)
	}
	pkg := writePackage(t, `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: example.test, name: greeting, version: 1.0.0, description: Greet a person}
execution: {entrypoint: main.sh}
`, "echo hi\n")
	c.stagePackage(pkg)
	search = toolObject(t, c.call("tools/call", map[string]any{"name": "tap_search", "arguments": map[string]any{"query": "greet"}}))
	if _, ok := search["note"]; ok {
		t.Fatalf("a search with matches carries a note: %#v", search)
	}
}
