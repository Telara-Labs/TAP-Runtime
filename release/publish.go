package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// publish cuts a TAP runner release with one command, the way
// clipush does for the Telara CLI:
//
//	go run ./release publish --version 0.1.4 [--plan]
//
// It commits the version to npm/package.json (that file only), tags the
// commit, builds and signs the release from a clean export of it, pushes
// main and the tag to GitLab (origin) and GitHub, creates the GitHub release
// with the signed files, runs the npm workflow and waits for npm to report
// the version. Every step first checks whether it is already done, so after
// a failure the same command picks up where it stopped. --plan changes
// nothing and says what would happen, including which commits each push
// would publish. The result is one JSON report on standard output.

// Publisher is everything publish needs from the machine; tests replace the
// parts that would reach GitHub or npm.
type Publisher struct {
	Dir        string // the tap-runtime checkout
	Version    string // X.Y.Z
	Key        string // release signing key
	GitHubRepo string // owner/name on GitHub
	Origin     string // GitLab remote name
	GitHub     string // GitHub remote name
	Package    string // npm package name
	Workflow   string // GitHub Actions workflow that publishes to npm
	Plan       bool
	// Run runs a program in Dir and returns its standard output.
	Run func(name string, args ...string) (string, error)
	// Build builds and verifies the signed release of tag into out.
	Build func(tag, out string) error
	// Wait is how long to wait for npm to show a published version. npm can
	// take several minutes to process a package after publish accepts it.
	Wait time.Duration
}

// Step is one line of the report.
type Step struct {
	Name   string `json:"name"`
	Status string `json:"status"` // done, skipped (already done), planned, failed
	Detail string `json:"detail,omitempty"`
}

// Report is what publish prints.
type Report struct {
	Status  string `json:"status"` // ok, planned, failed
	Version string `json:"version"`
	Tag     string `json:"tag"`
	Steps   []Step `json:"steps"`
	Reason  string `json:"reason,omitempty"`
}

var semver = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.-]+)?$`)

func (p *Publisher) git(args ...string) (string, error) {
	out, err := p.Run("git", args...)
	return strings.TrimSpace(out), err
}

// Publish runs every step and returns the report.
func (p *Publisher) Publish() Report {
	r := Report{Version: p.Version, Tag: "v" + p.Version, Status: "ok"}
	if p.Plan {
		r.Status = "planned"
	}
	fail := func(step, why string) Report {
		r.Steps = append(r.Steps, Step{Name: step, Status: "failed", Detail: why})
		r.Status, r.Reason = "failed", step+": "+why
		return r
	}
	add := func(name, status, detail string) {
		if p.Plan && status == "done" {
			status = "planned"
		}
		r.Steps = append(r.Steps, Step{Name: name, Status: status, Detail: detail})
	}
	tag := r.Tag
	if !semver.MatchString(p.Version) {
		return fail("preflight", fmt.Sprintf("version %q is not X.Y.Z", p.Version))
	}

	// Preflight: on main, holding everything both remotes have.
	if b, err := p.git("branch", "--show-current"); err != nil || b != "main" {
		return fail("preflight", "the checkout must be on main, not "+b)
	}
	for _, remote := range []string{p.Origin, p.GitHub} {
		// main only: the remotes' older tags need not agree (v0.1.0 does not).
		if _, err := p.git("fetch", "--quiet", remote, "main"); err != nil {
			return fail("preflight", fmt.Sprintf("fetch %s: %v", remote, err))
		}
		if _, err := p.git("merge-base", "--is-ancestor", remote+"/main", "HEAD"); err != nil {
			return fail("preflight", fmt.Sprintf("local main does not contain %s/main: bring its commits in first (git log HEAD..%s/main)", remote, remote))
		}
	}
	if st, _ := p.git("status", "--porcelain", "--", "npm/package.json"); st != "" {
		return fail("preflight", "npm/package.json has uncommitted changes")
	}
	published := p.npmHas()
	// Preflight ran for real, plan or not.
	r.Steps = append(r.Steps, Step{Name: "preflight", Status: "done", Detail: "on main; contains " + p.Origin + "/main and " + p.GitHub + "/main"})

	// Version commit and tag. A tag on either remote counts, and the two
	// remotes and the checkout must agree on where it points.
	local, _ := p.git("rev-list", "-n", "1", tag)
	seen := map[string]string{}
	if local != "" {
		seen["local"] = local
	}
	for _, remote := range []string{p.Origin, p.GitHub} {
		if out, _ := p.git("ls-remote", "--tags", remote, "refs/tags/"+tag, "refs/tags/"+tag+"^{}"); out != "" {
			var sha string
			for _, line := range strings.Split(out, "\n") {
				if f := strings.Fields(line); len(f) == 2 && (sha == "" || strings.HasSuffix(f[1], "^{}")) {
					sha = f[0]
				}
			}
			seen[remote] = sha
		}
	}
	var target string
	for _, sha := range seen {
		if target != "" && sha != target {
			return fail("version", fmt.Sprintf("%s points at different commits: %v", tag, seen))
		}
		target = sha
	}
	if target != "" && local == "" {
		if _, err := p.git("fetch", "--quiet", p.GitHub, "refs/tags/"+tag+":refs/tags/"+tag); err != nil {
			if _, err := p.git("fetch", "--quiet", p.Origin, "refs/tags/"+tag+":refs/tags/"+tag); err != nil {
				return fail("version", "fetch "+tag+": "+err.Error())
			}
		}
	}
	var tagErr error
	if target == "" {
		tagErr = fmt.Errorf("no tag")
	}
	switch {
	case tagErr == nil:
		v, err := p.git("show", target+":npm/package.json")
		if err != nil || packageVersion(v) != p.Version {
			return fail("version", fmt.Sprintf("%s already exists at %s, whose npm/package.json is not version %s", tag, short(target), p.Version))
		}
		add("version", "skipped", tag+" already exists at "+short(target))
	default:
		head, _ := p.git("rev-parse", "HEAD")
		cur, _ := p.git("show", "HEAD:npm/package.json")
		if packageVersion(cur) == p.Version {
			add("version", "skipped", "npm/package.json is already "+p.Version)
		} else if p.Plan {
			add("version", "planned", fmt.Sprintf("commit npm/package.json %s -> %s as chore(tap): release %s", packageVersion(cur), p.Version, tag))
		} else {
			path := filepath.Join(p.Dir, "npm", "package.json")
			raw, err := os.ReadFile(path)
			if err != nil {
				return fail("version", err.Error())
			}
			bumped, ok := setPackageVersion(string(raw), p.Version)
			if !ok {
				return fail("version", "npm/package.json has no version field")
			}
			if err := os.WriteFile(path, []byte(bumped), 0o644); err != nil {
				return fail("version", err.Error())
			}
			// A path-limited commit: whatever else is staged stays staged.
			if _, err := p.git("commit", "--quiet", "-m", "chore(tap): release "+tag, "--", "npm/package.json"); err != nil {
				return fail("version", err.Error())
			}
			add("version", "done", fmt.Sprintf("committed npm/package.json %s -> %s on %s", packageVersion(cur), p.Version, short(head)))
		}
		if p.Plan {
			add("tag", "planned", "tag "+tag+" on the release commit")
		} else {
			if _, err := p.git("tag", tag); err != nil {
				return fail("tag", err.Error())
			}
			target, _ = p.git("rev-parse", "HEAD")
			add("tag", "done", tag+" at "+short(target))
		}
	}

	// Push main and the tag to both remotes. Pushing main publishes every
	// local commit not yet on the remote; the report names them.
	for _, remote := range []string{p.Origin, p.GitHub} {
		ahead, _ := p.git("log", "--oneline", remote+"/main..HEAD")
		remoteTag, _ := p.Run("git", "ls-remote", "--tags", remote, "refs/tags/"+tag)
		if ahead == "" && strings.TrimSpace(remoteTag) != "" {
			add("push "+remote, "skipped", "main and "+tag+" already there")
			continue
		}
		detail := "main and " + tag
		if ahead != "" {
			detail += "; publishes:\n" + ahead
		}
		if p.Plan {
			add("push "+remote, "planned", detail)
			continue
		}
		args := []string{"push", "--quiet", remote, "main", tag}
		if remote == p.GitHub {
			args = append([]string{"-c", "http.version=HTTP/1.1", "-c", "http.postBuffer=524288000"}, args...)
		}
		if _, err := p.git(args...); err != nil {
			return fail("push "+remote, err.Error())
		}
		add("push "+remote, "done", detail)
	}

	// The GitHub release with the signed files.
	assets, relErr := p.Run("gh", "release", "view", tag, "--repo", p.GitHubRepo, "--json", "assets", "--jq", ".assets | length")
	n := strings.TrimSpace(assets)
	switch {
	case relErr == nil && n != "0" && n != "":
		add("github release", "skipped", tag+" exists with "+n+" files")
	case published:
		add("github release", "skipped", "npm already has "+p.Version)
	case p.Plan:
		add("github release", "planned", "build the versioned VSIX with pinned official vsce; build and sign "+tag+" from a clean export including the VSIX, verify it, create the release on "+p.GitHubRepo)
	default:
		out, err := os.MkdirTemp("", "tap-release-")
		if err != nil {
			return fail("github release", err.Error())
		}
		defer os.RemoveAll(out)
		if err := p.Build(tag, out); err != nil {
			return fail("github release", "build: "+err.Error())
		}
		files, _ := filepath.Glob(filepath.Join(out, "*"))
		if relErr == nil {
			args := append([]string{"release", "upload", tag, "--repo", p.GitHubRepo, "--clobber"}, files...)
			if _, err := p.Run("gh", args...); err != nil {
				return fail("github release", err.Error())
			}
		} else {
			args := append([]string{"release", "create", tag, "--repo", p.GitHubRepo, "--verify-tag", "--title", "TAP Runtime " + tag,
				"--notes", "Signed TAP runner release " + tag + ". Verify with: go run ./release verify --dir <downloaded release> --pub release/release.pub"}, files...)
			if _, err := p.Run("gh", args...); err != nil {
				return fail("github release", err.Error())
			}
		}
		add("github release", "done", fmt.Sprintf("%d signed files", len(files)))
	}

	// npm, through the workflow that checks the signed release and publishes
	// with npm Trusted Publishing.
	switch {
	case published:
		add("npm", "skipped", p.Package+"@"+p.Version+" already published")
	case p.Plan:
		add("npm", "planned", "run "+p.Workflow+" with tag "+tag+", wait for it, then confirm npm shows "+p.Version)
	default:
		before, _ := p.Run("gh", "run", "list", "--repo", p.GitHubRepo, "--workflow", p.Workflow, "--limit", "1", "--json", "databaseId", "--jq", ".[0].databaseId")
		if _, err := p.Run("gh", "workflow", "run", p.Workflow, "--repo", p.GitHubRepo, "--ref", "main", "-f", "tag="+tag); err != nil {
			return fail("npm", err.Error())
		}
		var id string
		for i := 0; i < 30; i++ {
			got, _ := p.Run("gh", "run", "list", "--repo", p.GitHubRepo, "--workflow", p.Workflow, "--limit", "1", "--json", "databaseId", "--jq", ".[0].databaseId")
			if got = strings.TrimSpace(got); got != "" && got != strings.TrimSpace(before) {
				id = got
				break
			}
			time.Sleep(2 * time.Second)
		}
		if id == "" {
			return fail("npm", "the workflow run did not appear")
		}
		if _, err := p.Run("gh", "run", "watch", id, "--repo", p.GitHubRepo, "--exit-status"); err != nil {
			return fail("npm", fmt.Sprintf("workflow run %s failed: %v (gh run view %s --repo %s --log-failed)", id, err, id, p.GitHubRepo))
		}
		deadline := time.Now().Add(p.Wait)
		for !p.npmHas() {
			if time.Now().After(deadline) {
				return fail("npm", fmt.Sprintf("workflow run %s passed but npm does not show %s yet", id, p.Version))
			}
			time.Sleep(5 * time.Second)
		}
		add("npm", "done", fmt.Sprintf("%s@%s published by workflow run %s", p.Package, p.Version, id))
	}
	return r
}

// npmHas reports whether npm serves this version.
func (p *Publisher) npmHas() bool {
	out, err := p.Run("npm", "view", p.Package+"@"+p.Version, "version")
	return err == nil && strings.TrimSpace(out) == p.Version
}

var versionField = regexp.MustCompile(`("version"\s*:\s*")([^"]*)(")`)

func packageVersion(doc string) string {
	var m struct {
		Version string `json:"version"`
	}
	json.Unmarshal([]byte(doc), &m)
	return m.Version
}

// setPackageVersion changes the top-level version and nothing else in the
// file, keeping its formatting.
func setPackageVersion(doc, v string) (string, bool) {
	loc := versionField.FindStringSubmatchIndex(doc)
	if loc == nil {
		return doc, false
	}
	return doc[:loc[4]] + v + doc[loc[5]:], true
}

func short(sha string) string {
	if len(sha) > 9 {
		return sha[:9]
	}
	return sha
}

// buildFromExport builds and verifies the signed release of tag from a
// clean export of that commit, so nothing uncommitted in the checkout can
// reach it.
func buildFromExport(dir, key, githubRepo string) func(tag, out string) error {
	return buildFromExportWithRunner(dir, key, githubRepo, run)
}

func buildFromExportWithRunner(dir, key, githubRepo string, command releaseCommand) func(tag, out string) error {
	return func(tag, out string) error {
		src, err := os.MkdirTemp("", "tap-release-src-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(src)
		// Complete the clean export before extraction. Piping two subprocesses
		// through Cmd.StdoutPipe can stall archive/untar teardown on macOS.
		exported, err := os.CreateTemp("", "tap-release-export-*.tar")
		if err != nil {
			return err
		}
		defer os.Remove(exported.Name())
		archive := exec.Command("git", "archive", "--format=tar", tag)
		archive.Dir = dir
		archive.Stdout = exported
		if err := archive.Run(); err != nil {
			exported.Close()
			return fmt.Errorf("git archive %s: %w", tag, err)
		}
		if err := exported.Close(); err != nil {
			return err
		}
		untar := exec.Command("tar", "-x", "-f", exported.Name(), "-C", src)
		if out, err := untar.CombinedOutput(); err != nil {
			return fmt.Errorf("extract clean export: %w: %s", err, out)
		}
		version := strings.TrimPrefix(tag, "v")
		base := "https://github.com/" + githubRepo + "/releases/download/" + tag
		vsix, err := packageVSIX(src, out, version, command)
		if err != nil {
			return fmt.Errorf("package VS Code extension: %w", err)
		}
		if _, err := command(src, []string{"GOWORK=off"}, "go", "run", "./release", "build", "--version", version, "--out", out, "--key", key, "--download-base", base, "--extra", vsix); err != nil {
			return err
		}
		_, err = command(src, []string{"GOWORK=off"}, "go", "run", "./release", "verify", "--dir", out, "--pub", "release/release.pub")
		return err
	}
}
