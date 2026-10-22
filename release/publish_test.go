package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	viewed   []string        // every npm view argument, in order
	missing  map[string]bool // pkg@version that npm lacks although the version is published
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
			f.viewed = append(f.viewed, args[1])
			v := args[1][strings.LastIndex(args[1], "@")+1:]
			if f.npm[v] && !f.missing[args[1]] {
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
				// A complete release: the signed sums and the npm sources,
				// padded with runner files up to n names.
				names := []string{"SHA256SUMS", "SHA256SUMS.sig", NpmSourcesName(strings.TrimPrefix(args[2], "v"))}
				for i := len(names); i < n; i++ {
					names = append(names, fmt.Sprintf("file-%d", i))
				}
				return strings.Join(names[:max(n, 0)], "\n") + "\n", nil
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
		Packages: npmPackageNames(), Workflow: "release.yml", Plan: plan, Run: f.run(dir), Wait: time.Second,
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

// The plan names all six npm packages in publish order, platform packages
// first and the main package last.
func TestPublishPlanListsEverySixNPMPackagesInOrder(t *testing.T) {
	dir := releaseRepo(t)
	f := &fakeHosts{releases: map[string]int{}, npm: map[string]bool{}}
	r := publisher(dir, f, "0.1.4", true).Publish()
	var detail string
	for _, s := range r.Steps {
		if s.Name == "npm" {
			detail = s.Detail
		}
	}
	last := -1
	for _, name := range []string{"tap-darwin-arm64", "tap-darwin-x64", "tap-linux-arm64", "tap-linux-x64", "tap-win32-x64", "@telaralabs/tap\n"} {
		i := strings.Index(detail+"\n", name)
		if i < 0 || i <= last {
			t.Fatalf("%s is missing or out of order in the plan:\n%s", name, detail)
		}
		last = i
	}
	if got := npmPackageNames(); len(got) != 6 || got[5] != "@telaralabs/tap" {
		t.Fatalf("the main package must be published last: %v", got)
	}
}

// A release counts as published only when npm serves every package; one
// missing package sends the workflow out again.
func TestPublishWaitsForAllSixPackages(t *testing.T) {
	dir := releaseRepo(t)
	f := &fakeHosts{releases: map[string]int{"v0.1.4": 12}, npm: map[string]bool{"0.1.4": true}, missing: map[string]bool{"@telaralabs/tap-linux-x64@0.1.4": true}}
	p := publisher(dir, f, "0.1.4", false)
	if p.npmHas() {
		t.Fatal("npm lacks a platform package yet the release counted as published")
	}
	if got := p.npmMissing(); len(got) != 1 || got[0] != "@telaralabs/tap-linux-x64" {
		t.Fatalf("missing %v", got)
	}
	delete(f.missing, "@telaralabs/tap-linux-x64@0.1.4")
	f.viewed = nil
	if !p.npmHas() || len(f.viewed) != 6 {
		t.Fatalf("every package must be checked: %v", f.viewed)
	}
}

// The staging step writes the six packages from one release: each platform
// package with its own os and cpu and only its runner, the main package at
// the same version with the five as optional dependencies, in publish order.
func TestStagedNPMPackagesMatchTheReleaseTool(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; the npm staging cannot run")
	}
	root := t.TempDir()
	rel := filepath.Join(root, "release")
	os.MkdirAll(rel, 0o755)
	var sums []string
	write := func(name, body string) {
		os.WriteFile(filepath.Join(rel, name), []byte(body), 0o755)
		sum := sha256.Sum256([]byte(body))
		sums = append(sums, hex.EncodeToString(sum[:])+"  "+name)
	}
	for _, f := range []string{"darwin-arm64", "darwin-amd64", "linux-arm64", "linux-amd64", "windows-amd64.exe"} {
		write("tap-0.1.4-"+f, "runner "+f)
	}
	write("THIRD_PARTY_NOTICES.txt", "notices")
	os.WriteFile(filepath.Join(rel, "SHA256SUMS"), []byte(strings.Join(sums, "\n")+"\n"), 0o644)
	out := filepath.Join(root, "packages")
	cmd := exec.Command(node, filepath.Join("..", "npm", "pack.cjs"), "--allow-unsigned-for-tests", rel, out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	var order struct {
		Version      string
		PublishOrder []struct{ Name, Dir string }
	}
	readJSON := func(path string, into any) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, into); err != nil {
			t.Fatal(err)
		}
	}
	readJSON(filepath.Join(out, "packages.json"), &order)
	want := NPMPackages()
	if order.Version != "0.1.4" || len(order.PublishOrder) != len(want) {
		t.Fatalf("%+v", order)
	}
	for i, w := range want {
		got := order.PublishOrder[i]
		if got.Name != w.Name {
			t.Fatalf("package %d is %s, the release tool expects %s", i, got.Name, w.Name)
		}
		var pkg struct {
			Name, Version        string
			OS, CPU              []string
			OptionalDependencies map[string]string
		}
		readJSON(filepath.Join(out, got.Dir, "package.json"), &pkg)
		if pkg.Version != "0.1.4" {
			t.Fatalf("%s is at %s", w.Name, pkg.Version)
		}
		if w.OS == "" { // the main package
			if len(pkg.OS) != 0 || len(pkg.OptionalDependencies) != 5 {
				t.Fatalf("main package %+v", pkg)
			}
			for _, dep := range want[:5] {
				if pkg.OptionalDependencies[dep.Name] != "0.1.4" {
					t.Fatalf("optional dependency %s: %v", dep.Name, pkg.OptionalDependencies)
				}
			}
			continue
		}
		if strings.Join(pkg.OS, ",") != w.OS || strings.Join(pkg.CPU, ",") != strings.Join(w.CPU, ",") {
			t.Fatalf("%s declares os %v cpu %v, want %s %v", w.Name, pkg.OS, pkg.CPU, w.OS, w.CPU)
		}
		var files []string
		filepath.WalkDir(filepath.Join(out, got.Dir), func(p string, d os.DirEntry, _ error) error {
			if !d.IsDir() {
				rel, _ := filepath.Rel(filepath.Join(out, got.Dir), p)
				files = append(files, filepath.ToSlash(rel))
			}
			return nil
		})
		bin := "bin/tap"
		if w.OS == "win32" {
			bin = "bin/tap.exe"
		}
		if strings.Join(files, " ") != strings.Join([]string{"LICENSE", "README.md", "THIRD_PARTY_NOTICES.txt", bin, "package.json"}, " ") {
			t.Fatalf("%s holds %v", w.Name, files)
		}
	}
}

func TestMissingAssetsNamesWhatAnIncompleteReleaseLacks(t *testing.T) {
	got := missingAssets([]string{"SHA256SUMS", "tap-1.2.3-linux-amd64"}, "SHA256SUMS", "SHA256SUMS.sig", NpmSourcesName("1.2.3"))
	if len(got) != 2 || got[0] != "SHA256SUMS.sig" || got[1] != "tap-npm-sources-1.2.3.tar.gz" {
		t.Fatalf("missingAssets = %v", got)
	}
	if got := missingAssets([]string{"a", "b"}, "a"); len(got) != 0 {
		t.Fatalf("missingAssets = %v", got)
	}
}
