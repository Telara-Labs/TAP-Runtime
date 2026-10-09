package author_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/discover/author"
)

// Agents whose run mode may only touch their workspace (Kilo refused a
// draft folder in the home directory and the save never happened) draft in
// the workspace's .tap/drafts. The brief goes there by default, nothing
// under it reaches git, and saving still lands the package in the TAP
// collection, outside the workspace, written by the runner.
func TestDraftsLiveInTheWorkspaceOutOfGitAndSaveToTheCollection(t *testing.T) {
	home := homeWithClaudeSession(t)
	cfg := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", cfg)
	ws := t.TempDir()
	t.Chdir(ws)
	git, gitErr := exec.LookPath("git")
	if gitErr == nil {
		if out, err := exec.Command(git, "init", "-q", ws).CombinedOutput(); err != nil {
			t.Fatalf("git init: %v %s", err, out)
		}
	}

	var so, se bytes.Buffer
	if code := author.BriefCommand([]string{"--task", "claude-code/s1/0"}, home, &so, &se); code != 0 {
		t.Fatalf("brief without --out: exit %d: %s", code, se.String())
	}
	briefs, _ := filepath.Glob(filepath.Join(ws, ".tap", "drafts", "brief-*", "BRIEF.md"))
	if len(briefs) != 1 {
		t.Fatalf("brief not written under the workspace's drafts folder: %v (%s)", briefs, so.String())
	}
	md, _ := os.ReadFile(briefs[0])
	if !strings.Contains(string(md), author.DraftsDir+"/<name>/") {
		t.Error("BRIEF.md does not say to write the package in the workspace's drafts folder")
	}
	for _, f := range []string{filepath.Join(ws, ".tap", "drafts", ".gitignore"), filepath.Join(filepath.Dir(briefs[0]), ".gitignore")} {
		if b, err := os.ReadFile(f); err != nil || !strings.Contains(string(b), "\n*\n") {
			t.Errorf("%s does not ignore everything: %q %v", f, b, err)
		}
	}

	// The agent writes its package beside the brief and saves it.
	src := authoredPackage(t)
	pkg := filepath.Join(ws, ".tap", "drafts", "repo-changes")
	if err := os.CopyFS(pkg, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(ws, ".tap", "drafts", "cases.json"), []byte("{}\n"), 0o600)
	so.Reset()
	se.Reset()
	if code := author.SaveCommand([]string{".tap/drafts/repo-changes", "--client", "none"}, home, &so, &se); code != 0 {
		t.Fatalf("save from the workspace drafts folder: exit %d: %s", code, se.String())
	}
	coll, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := filepath.Glob(filepath.Join(coll, "tap", "primitives", "repo-changes*", "*", "primitive.yaml"))
	saved2, _ := filepath.Glob(filepath.Join(coll, "tap", "primitives", "repo-changes*", "primitive.yaml"))
	if len(saved)+len(saved2) == 0 {
		t.Fatalf("the package did not land in the TAP collection under %s: %s", coll, so.String())
	}
	if strings.HasPrefix(strings.Fields(so.String())[1], ws) {
		t.Errorf("saved inside the workspace: %s", so.String())
	}

	if gitErr != nil {
		t.Log("git not installed: the ignore files were checked, git status was not")
		return
	}
	out, err := exec.Command(git, "-C", ws, "status", "--porcelain", "--untracked-files=all").CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v %s", err, out)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("drafts would be committed:\n%s", out)
	}
}

// A package the agent started before running the brief still ends up
// ignored: validate and save add the drafts folder's ignore file too.
func TestIgnoreDraftsCoversAPackageWrittenFirst(t *testing.T) {
	ws := t.TempDir()
	pkg := filepath.Join(ws, ".tap", "drafts", "x")
	os.MkdirAll(pkg, 0o700)
	if err := author.IgnoreDrafts(pkg, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, ".tap", "drafts", ".gitignore")); err != nil {
		t.Errorf("drafts folder not ignored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pkg, ".gitignore")); err == nil {
		t.Error("an ignore file was written into the package, changing its contents")
	}
	other := filepath.Join(ws, "elsewhere")
	os.MkdirAll(other, 0o700)
	if err := author.IgnoreDrafts(other, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(other, ".gitignore")); err == nil {
		t.Error("a package outside the drafts folder got an ignore file")
	}
}

// An agent wrote its brief to a folder in the workspace root and moved the
// files into .tap/drafts/<name> with a shell glob, which leaves dot files
// behind; the package then showed up in git status. The brief command
// ignores the workspace's drafts folder wherever the brief itself goes.
func TestBriefIgnoresTheWorkspaceDraftsFolderWhereverItIsWritten(t *testing.T) {
	home := homeWithClaudeSession(t)
	ws := t.TempDir()
	t.Chdir(ws)
	var so, se bytes.Buffer
	if code := author.BriefCommand([]string{"--task", "claude-code/s1/0", "--out", "./my-brief"}, home, &so, &se); code != 0 {
		t.Fatalf("brief: exit %d: %s", code, se.String())
	}
	if _, err := os.Stat(filepath.Join(ws, ".tap", "drafts", ".gitignore")); err != nil {
		t.Errorf("the workspace drafts folder is not ignored after a brief written elsewhere: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "my-brief", ".gitignore")); err != nil {
		t.Errorf("the brief folder is not ignored: %v", err)
	}
}
