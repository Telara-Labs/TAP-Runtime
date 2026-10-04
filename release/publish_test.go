package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeHosts stands in for GitHub (gh) and npm. Git is real: two bare
// repositories play GitLab and GitHub.
type fakeHosts struct {
	releases map[string]int // tag -> asset count
	npm      map[string]bool
	runs     int
	calls    []string
	failRun  bool
}

func (f *fakeHosts) run(dir string) func(name string, args ...string) (string, error) {
	return func(name string, args ...string) (string, error) {
		switch name {
		case "git":
			cmd := exec.Command("git", args...)
			cmd.Dir = dir
			out, err := cmd.Output()
			if err != nil {
				if ee, ok := err.(*exec.ExitError); ok {
					return string(out), fmt.Errorf("git %s: %v %s", strings.Join(args, " "), err, ee.Stderr)
				}
			}
			return string(out), err
		case "npm": // npm view <pkg>@<v> version
			v := args[1][strings.LastIndex(args[1], "@")+1:]
			if f.npm[v] {
				return v + "\n", nil
			}
			return "", fmt.Errorf("E404")
		case "gh":
			f.calls = append(f.calls, strings.Join(args[:2], " "))
			switch strings.Join(args[:2], " ") {
			case "release view":
				n, ok := f.releases[args[2]]
				if !ok {
					return "", fmt.Errorf("release not found")
				}
				return fmt.Sprint(n), nil
			case "release create", "release upload":
				f.releases[args[2]] = 12
				return "", nil
			case "run list":
				return fmt.Sprint(f.runs), nil
			case "workflow run":
				f.runs++
				return "", nil
			case "run watch":
				if f.failRun {
					return "", fmt.Errorf("exit status 1")
				}
				// The workflow publishes the tag it was given.
				for tag := range f.releases {
					f.npm[strings.TrimPrefix(tag, "v")] = true
				}
				return "", nil
			}
		}
		return "", fmt.Errorf("unexpected %s %v", name, args)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// releaseRepo is a checkout on main with origin and github remotes, both at the
// first commit.
func releaseRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "work")
	for _, r := range []string{"origin.git", "github.git"} {
		git(t, root, "init", "--quiet", "--bare", "-b", "main", r)
	}
	git(t, root, "init", "--quiet", "-b", "main", dir)
	git(t, dir, "config", "user.name", "t")
	git(t, dir, "config", "user.email", "t@t")
	os.MkdirAll(filepath.Join(dir, "npm"), 0o755)
	os.WriteFile(filepath.Join(dir, "npm", "package.json"), []byte("{\n  \"name\": \"@telaralabs/tap\",\n  \"version\": \"0.1.3\",\n  \"bin\": {\"tap\": \"bin/tap.cjs\"}\n}\n"), 0o644)
	git(t, dir, "add", ".")
	git(t, dir, "commit", "--quiet", "-m", "start")
	for _, r := range []string{"origin", "github"} {
		git(t, dir, "remote", "add", r, filepath.Join(root, r+".git"))
		git(t, dir, "push", "--quiet", r, "main")
	}
	return dir
}

func publisher(dir string, f *fakeHosts, version string, plan bool) *Publisher {
	return &Publisher{Dir: dir, Version: version, GitHubRepo: "o/r", Origin: "origin", GitHub: "github",
		Package: "@telaralabs/tap", Workflow: "release.yml", Plan: plan, Run: f.run(dir), Wait: time.Second,
		Build: func(tag, out string) error {
			for i := 0; i < 12; i++ {
				os.WriteFile(filepath.Join(out, fmt.Sprintf("f%d", i)), []byte(tag), 0o644)
			}
			return nil
		}}
}

func statuses(r Report) string {
	var b []string
	for _, s := range r.Steps {
		b = append(b, s.Name+"="+s.Status)
	}
	return strings.Join(b, " ")
}

// One command releases: the version commit holds npm/package.json only,
// the tag and main reach both remotes, the GitHub release gets the signed
// files, and the npm workflow publishes the version.
func TestPublishReleasesEverywhere(t *testing.T) {
	dir := releaseRepo(t)
	// Another session's work in the checkout must not ride along.
	os.WriteFile(filepath.Join(dir, "other.txt"), []byte("in flight"), 0o644)
	git(t, dir, "add", "other.txt")
	f := &fakeHosts{releases: map[string]int{}, npm: map[string]bool{"0.1.3": true}}
	r := publisher(dir, f, "0.1.4", false).Publish()
	if r.Status != "ok" {
		t.Fatalf("%+v", r)
	}
	if got := statuses(r); got != "preflight=done version=done tag=done push origin=done push github=done github release=done npm=done" {
		t.Fatalf("steps %s", got)
	}
	if files := git(t, dir, "show", "--name-only", "--format=", "v0.1.4"); files != "npm/package.json" {
		t.Fatalf("release commit holds %q", files)
	}
	if msg := git(t, dir, "log", "-1", "--format=%s", "v0.1.4"); msg != "chore(tap): release v0.1.4" {
		t.Fatalf("message %q", msg)
	}
	if st := git(t, dir, "diff", "--cached", "--name-only"); st != "other.txt" {
		t.Fatalf("staged work changed: %q", st)
	}
	for _, remote := range []string{"origin", "github"} {
		if git(t, dir, "ls-remote", remote, "refs/tags/v0.1.4") == "" || git(t, dir, "rev-parse", remote+"/main") == "" {
			t.Fatalf("%s lacks the release", remote)
		}
		if a, b := git(t, dir, "ls-remote", remote, "refs/heads/main"), git(t, dir, "rev-parse", "v0.1.4"); !strings.HasPrefix(a, b) {
			t.Fatalf("%s main %s, tag %s", remote, a, b)
		}
	}
	if f.releases["v0.1.4"] != 12 || !f.npm["0.1.4"] {
		t.Fatalf("release %v npm %v", f.releases, f.npm)
	}

	// A checkout that never saw the tag (another machine) resumes from the
	// remotes: delete the local tag and run again.
	git(t, dir, "tag", "-d", "v0.1.4")

	// Run again: everything is already done, nothing changes.
	head := git(t, dir, "rev-parse", "HEAD")
	r = publisher(dir, f, "0.1.4", false).Publish()
	if got := statuses(r); r.Status != "ok" || got != "preflight=done version=skipped push origin=skipped push github=skipped github release=skipped npm=skipped" {
		t.Fatalf("rerun %s %+v", got, r)
	}
	if git(t, dir, "rev-parse", "HEAD") != head || f.runs != 1 {
		t.Fatal("a rerun changed something")
	}
}

// --plan changes nothing and names the commits each push would publish.
func TestPublishPlanChangesNothing(t *testing.T) {
	dir := releaseRepo(t)
	os.WriteFile(filepath.Join(dir, "x.txt"), []byte("x"), 0o644)
	git(t, dir, "add", "x.txt")
	git(t, dir, "commit", "--quiet", "-m", "unpushed work")
	head := git(t, dir, "rev-parse", "HEAD")
	f := &fakeHosts{releases: map[string]int{}, npm: map[string]bool{}}
	r := publisher(dir, f, "0.1.4", true).Publish()
	if r.Status != "planned" {
		t.Fatalf("%+v", r)
	}
	if git(t, dir, "rev-parse", "HEAD") != head || git(t, dir, "tag") != "" || f.runs != 0 || len(f.releases) != 0 {
		t.Fatal("--plan changed something")
	}
	for _, s := range r.Steps {
		if s.Name == "push github" && !strings.Contains(s.Detail, "unpushed work") {
			t.Fatalf("the plan hides what the push publishes: %q", s.Detail)
		}
	}
}

// A failure stops the run and says where; the same command then resumes.
func TestPublishResumesAfterAFailedWorkflow(t *testing.T) {
	dir := releaseRepo(t)
	f := &fakeHosts{releases: map[string]int{}, npm: map[string]bool{}, failRun: true}
	r := publisher(dir, f, "0.1.4", false).Publish()
	if r.Status != "failed" || !strings.HasPrefix(r.Reason, "npm: workflow run") {
		t.Fatalf("%+v", r)
	}
	f.failRun = false
	r = publisher(dir, f, "0.1.4", false).Publish()
	if got := statuses(r); r.Status != "ok" || got != "preflight=done version=skipped push origin=skipped push github=skipped github release=skipped npm=done" {
		t.Fatalf("resume %s %+v", got, r)
	}
}

func TestPublishRefusals(t *testing.T) {
	t.Run("a remote has commits main lacks", func(t *testing.T) {
		dir := releaseRepo(t)
		other := filepath.Join(t.TempDir(), "other")
		git(t, filepath.Dir(other), "clone", "--quiet", filepath.Join(filepath.Dir(dir), "github.git"), other)
		git(t, other, "config", "user.name", "t")
		git(t, other, "config", "user.email", "t@t")
		os.WriteFile(filepath.Join(other, "y"), []byte("y"), 0o644)
		git(t, other, "add", "y")
		git(t, other, "commit", "--quiet", "-m", "only on github")
		git(t, other, "push", "--quiet", "origin", "main")
		r := publisher(dir, &fakeHosts{releases: map[string]int{}, npm: map[string]bool{}}, "0.1.4", false).Publish()
		if r.Status != "failed" || !strings.Contains(r.Reason, "does not contain github/main") {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("the tag points at another version", func(t *testing.T) {
		dir := releaseRepo(t)
		git(t, dir, "tag", "v0.1.4")
		r := publisher(dir, &fakeHosts{releases: map[string]int{}, npm: map[string]bool{}}, "0.1.4", false).Publish()
		if r.Status != "failed" || !strings.Contains(r.Reason, "already exists") {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("the tag exists only on github, at a commit of another version", func(t *testing.T) {
		// What happened to v0.1.4 on 2026-10-03: tagged on GitHub from a
		// commit whose package.json still said 0.1.3.
		dir := releaseRepo(t)
		git(t, dir, "tag", "v0.1.4")
		git(t, dir, "push", "--quiet", "github", "v0.1.4")
		git(t, dir, "tag", "-d", "v0.1.4")
		r := publisher(dir, &fakeHosts{releases: map[string]int{}, npm: map[string]bool{}}, "0.1.4", false).Publish()
		if r.Status != "failed" || !strings.Contains(r.Reason, "is not version 0.1.4") {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("the remotes disagree on the tag", func(t *testing.T) {
		dir := releaseRepo(t)
		git(t, dir, "tag", "v0.1.4")
		git(t, dir, "push", "--quiet", "github", "v0.1.4")
		git(t, dir, "commit", "--quiet", "--allow-empty", "-m", "later")
		git(t, dir, "tag", "-f", "v0.1.4")
		git(t, dir, "push", "--quiet", "origin", "v0.1.4")
		r := publisher(dir, &fakeHosts{releases: map[string]int{}, npm: map[string]bool{}}, "0.1.4", false).Publish()
		if r.Status != "failed" || !strings.Contains(r.Reason, "different commits") {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("not a version", func(t *testing.T) {
		r := publisher(t.TempDir(), &fakeHosts{}, "latest", false).Publish()
		if r.Status != "failed" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("package.json edited", func(t *testing.T) {
		dir := releaseRepo(t)
		os.WriteFile(filepath.Join(dir, "npm", "package.json"), []byte("{}"), 0o644)
		r := publisher(dir, &fakeHosts{releases: map[string]int{}, npm: map[string]bool{}}, "0.1.4", false).Publish()
		if r.Status != "failed" || !strings.Contains(r.Reason, "uncommitted") {
			t.Fatalf("%+v", r)
		}
	})
}

func TestSetPackageVersionKeepsTheRestOfTheFile(t *testing.T) {
	in := "{\n  \"name\": \"x\",\n  \"version\": \"0.1.3\",\n  \"dependencies\": {\"y\": {\"version\": \"9\"}}\n}\n"
	out, ok := setPackageVersion(in, "0.1.4")
	if !ok || out != strings.Replace(in, "0.1.3", "0.1.4", 1) || packageVersion(out) != "0.1.4" {
		t.Fatalf("%v %q", ok, out)
	}
}
