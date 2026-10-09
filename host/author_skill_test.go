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

// The skill is loaded into every agent, so it carries the three rules for
// automating any kind of work in a few lines and sends the author to the
// guide, which holds the general section and the browser and desktop cases.
func TestAuthorSkillAndGuideTeachDesignAnyClientAndTesting(t *testing.T) {
	for _, want := range []string{
		"#automating-work-that-agents-do", "desktop apps", "MCP connectors", "HTTP APIs",
		"needs_person", "never zero or a pass", "`optional`", "`tap.tools()`", "`blocked`",
		"private objects", "slow or partial loading", "no backend", "fixture results apart from live results",
		"tap discover validate", "`tap_run`",
	} {
		if !strings.Contains(authorSkill, want) {
			t.Errorf("skill lacks %q", want)
		}
	}
	guide, err := os.ReadFile(filepath.Join("..", "docs", "writing-a-primitive.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## Automating work that agents do", "### Design: what the program does and what it hands back",
		"### Run in any client", "### Test before saving", "### Browsers",
		"### Desktop applications and computer use", "Unsupported client, no backend",
		"Slow or partial loading", "Claude Code** lends no desktop tool",
		"(#browsers)", "(#desktop-applications-and-computer-use)", "(#describe-observable-states-and-completion)",
	} {
		if !strings.Contains(string(guide), want) {
			t.Errorf("guide lacks %q", want)
		}
	}
}

// A primitive is as fast as its author makes it, so the skill and the guide
// say how to make it fast and that a primitive is measured and improved with
// use, never at the cost of correctness.
func TestAuthorSkillAndGuideTeachSpeedAndImprovement(t *testing.T) {
	for _, want := range []string{
		"## Make it fast, then keep improving it", "#make-it-fast", "#measure-and-improve",
		"Count round trips", "Index once per run", "Batch", "Loop inside", "No fixed waits",
		"budget", "tool calls, duration, unresolved items", "identical inputs", "`[stats: ...]`",
		"`tap_evidence`", "improved version", "semantic version", "Never trade correctness for speed",
	} {
		if !strings.Contains(authorSkill, want) {
			t.Errorf("skill lacks %q", want)
		}
	}
	guide, err := os.ReadFile(filepath.Join("..", "docs", "writing-a-primitive.md"))
	if err != nil {
		t.Fatal(err)
	}
	g := string(guide)
	for _, want := range []string{
		"### Make it fast", "### Measure and improve",
		"**Count round trips.**", "0.7 s on Claude in Chrome", "1.4 s on Playwright",
		"**Index once per run, then match every input against the index.**", "382-thread inbox", "150 to 270 calls",
		"**Batch.**", "**Loop inside the backend when it allows.**", "about 3 seconds", "Backends\ndiffer",
		"**No fixed waits beyond what loading needs.**", "**Declare a budget and report it.**",
		"**Measure every validation case.**", "`host_log`", "`[stats: ...]`",
		"**Compare versions on identical inputs.**", "**Read the run record after real use.**",
		"advance the semantic version", "`CHANGELOG.md`",
		"**Never trade correctness for speed.**", "stays `unresolved`",
	} {
		if !strings.Contains(g, want) {
			t.Errorf("guide lacks %q", want)
		}
	}
	// The new parts sit inside the automation section, before the worked
	// browser case, so a reader reaches them before any one backend.
	fast, measure, browsers := strings.Index(g, "### Make it fast"), strings.Index(g, "### Measure and improve"), strings.Index(g, "### Browsers")
	if !(strings.Index(g, "## Automating work that agents do") < fast && fast < measure && measure < browsers) {
		t.Errorf("section order: fast=%d measure=%d browsers=%d", fast, measure, browsers)
	}
}

// Agents whose run mode may only touch their workspace refused a draft
// folder in the home directory, so nothing was saved. The skill drafts in
// the workspace's git-ignored .tap/drafts and never names a folder outside it.
func TestAuthorSkillDraftsInsideTheWorkspace(t *testing.T) {
	for _, want := range []string{
		"`.tap/drafts/<name>/`", "--out .tap/drafts/<name>-brief", "out of git", "TAP writes the collection",
		"declare a web read\n  under `fetch:`",
	} {
		if !strings.Contains(authorSkill, want) {
			t.Errorf("skill lacks %q", want)
		}
	}
	for _, outside := range []string{"~/tap-drafts", "outside the person's project"} {
		if strings.Contains(authorSkill, outside) {
			t.Errorf("skill still drafts outside the workspace: %q", outside)
		}
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
	search := toolObject(t, c.callDetail("tools/call", map[string]any{"name": "tap_search", "arguments": map[string]any{"query": "gitlab runner commit release readiness"}}))
	if m, _ := search["matches"].([]any); len(m) != 0 {
		t.Fatalf("matches = %#v", m)
	}
	if note, _ := search["note"].(string); !strings.Contains(note, "1 earlier session") || !strings.Contains(note, "tap-author") || !strings.Contains(note, "discover brief --task claude-code/earlier/0") || !strings.Contains(note, runnerCommand()) {
		t.Fatalf("recurring task note: %#v", search)
	}
	search = toolObject(t, c.callDetail("tools/call", map[string]any{"name": "tap_search", "arguments": map[string]any{"query": "draft a launch email"}}))
	if note, _ := search["note"].(string); !strings.Contains(note, "Do the task as usual") || strings.Contains(note, "tap-author") {
		t.Fatalf("first-time task offered for saving: %#v", search)
	}
	pkg := writePackage(t, `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: example.test, name: greeting, version: 1.0.0, description: Greet a person}
execution: {entrypoint: main.sh}
`, "echo hi\n")
	c.stagePackage(pkg)
	search = toolObject(t, c.callDetail("tools/call", map[string]any{"name": "tap_search", "arguments": map[string]any{"query": "greet"}}))
	if _, ok := search["note"]; ok {
		t.Fatalf("a search with matches carries a note: %#v", search)
	}
}
