package discover

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The author path (TENG-2936): brief, validate, save.

func homeWithClaudeSession(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "projects", "proj")
	os.MkdirAll(dir, 0o755)
	b, err := os.ReadFile("testdata/claude/proj/s1.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "s1.jsonl"), b, 0o644)
	return home
}

func TestBriefFromASelectedTaskEstablishesNothing(t *testing.T) {
	s, err := FindSession("claude-code", "s1", homeWithClaudeSession(t))
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewBrief(s, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != BriefStatus || b.Selection != SelectedTask {
		t.Errorf("status %q selection %q: a brief is an unassessed proposal", b.Status, b.Selection)
	}
	if len(b.Missing) != len(contractFields) {
		t.Errorf("a selected task has every contract field missing, got %v", b.Missing)
	}
	if b.Evidence.Request != "check the repos" || len(b.Evidence.Steps) == 0 || !strings.Contains(b.Evidence.Steps[0].Command, "git status") {
		t.Errorf("evidence is not the request and its calls: %+v", b.Evidence)
	}
	if !strings.HasPrefix(b.Ref, "src_") || strings.Contains(b.Ref, "s1") || OpaqueRef(b.Source) != b.Ref {
		t.Errorf("ref %q must be opaque and stable", b.Ref)
	}
	dir := filepath.Join(t.TempDir(), "brief")
	digest, err := b.Write(dir)
	if err != nil || !strings.HasPrefix(digest, "sha256:") {
		t.Fatal(digest, err)
	}
	md, _ := os.ReadFile(filepath.Join(dir, "BRIEF.md"))
	if !strings.Contains(string(md), "Private:") || !strings.Contains(string(md), "unassessed_proposal") {
		t.Errorf("BRIEF.md must say it is private and unassessed:\n%s", md)
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(filepath.Join(dir, "brief.json")); info.Mode().Perm() != 0o600 {
			t.Errorf("brief.json mode %v, want 0600", info.Mode().Perm())
		}
	}
	if _, err := NewBrief(s, 7, nil); err == nil {
		t.Error("a request the session does not have must be refused")
	}
}

func TestBriefRedactsCredentials(t *testing.T) {
	s := Session{Client: "codex", ID: "x", Requests: []string{"deploy with Bearer abcdefghijklmnopqrstu"},
		Calls: []Call{{Tool: "shell", Command: "curl -H 'Authorization: Bearer abcdefghijklmnopqrstu' https://h", Output: "token glpat-abcdefghijklmnopqrstuv"}}}
	b, err := NewBrief(s, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(b)
	if strings.Contains(string(raw), "abcdefghijklmnopqrstu") {
		t.Errorf("a credential reached the brief: %s", raw)
	}
}

// A candidate Discover rejected can still be briefed: a zero-recommendation
// report must not hide a task the user selected. What Discover inferred is
// carried as an unverified proposal, never as established.
func TestBriefFromARejectedCandidate(t *testing.T) {
	home := homeWithClaudeSession(t)
	report := filepath.Join(t.TempDir(), "report.json")
	rep := map[string]any{"routines": []map[string]any{{
		"id": "r1", "decision": "removed", "failed": "inconsistent_order", "why": "only 1 of 19",
		"source_role": "scheduled", "suitability": SuitInsufficient,
		"contract": map[string]any{"goal": "unknown", "output": "report", "effect": "read_only",
			"inputs": []map[string]any{{"name": "repo", "type": "path", "source": "caller"}}},
		"sources": []map[string]any{{"client": "claude-code", "session": "s1", "request": 0}},
	}}}
	b, _ := json.Marshal(rep)
	os.WriteFile(report, b, 0o644)

	out := filepath.Join(t.TempDir(), "brief")
	var so, se bytes.Buffer
	if code := briefCommand([]string{"--candidate", "r1", "--report", report, "--out", out}, home, &so, &se); code != 0 {
		t.Fatalf("exit %d: %s", code, se.String())
	}
	var got Brief
	raw, _ := os.ReadFile(filepath.Join(out, "brief.json"))
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Selection != DiscoverCandidate || got.Status != BriefStatus || got.Candidate == nil || got.Candidate.Decision != "removed" {
		t.Errorf("brief does not carry the rejected candidate as a proposal: %+v", got)
	}
	if !strings.Contains(got.Contract.Output.EstablishedBy, "unverified") {
		t.Errorf("discover's output label must be marked unverified, got %q", got.Contract.Output.EstablishedBy)
	}
	for _, f := range []string{"goal", "procedure", "oracle", "failures"} {
		if !contains(got.Missing, f) {
			t.Errorf("%s was not established by anything and must be missing: %v", f, got.Missing)
		}
	}
	if contains(got.Missing, "output") {
		t.Errorf("output was proposed by discover and is not missing: %v", got.Missing)
	}
	if code := briefCommand([]string{"--candidate", "nope", "--report", report, "--out", out}, home, &so, &se); code == 0 {
		t.Error("an unknown routine must be refused")
	}
	if code := briefCommand([]string{"--task", "claude-code/s1/0", "--candidate", "r1", "--report", report, "--out", out}, home, &so, &se); code != 2 {
		t.Error("--task and --candidate together must be refused")
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPackageDirDigestAndPlaceholders(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"primitive.yaml": "a: 1\n", "main.py": "print(1)\n"})
	_, d1, err := PackageDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, d2, _ := PackageDir(dir)
	if d1 != d2 {
		t.Error("the same bytes must have the same digest")
	}
	writeFiles(t, dir, map[string]string{"main.py": "print(2)\n"})
	if _, d3, _ := PackageDir(dir); d3 == d1 {
		t.Error("a changed byte must change the digest")
	}
	if marks, _ := FindPlaceholders(dir); len(marks) != 0 {
		t.Errorf("no placeholders yet, got %v", marks)
	}
	writeFiles(t, dir, map[string]string{"main.py": "# TODO: finish\n"})
	if marks, _ := FindPlaceholders(dir); len(marks) != 1 {
		t.Errorf("a TODO: marker must be found, got %v", marks)
	}
}

// stubRunner stands in for tap in these unit tests only; the real runner is
// exercised by the root module's test (host/authored_test.go). The package's
// "mode" file picks its behaviour.
const stubRunner = `#!/bin/sh
if [ "$1" = manifest ]; then echo "ok"; exit 0; fi
journal=""
while [ $# -gt 0 ]; do case "$1" in -journal) journal="$2"; shift 2;; -*) shift;; *) break;; esac; done
pkg="$1"; shift
[ -n "$journal" ] && echo '{"command":"stub","args":"'"$*"'"}' >> "$journal"
case "$(cat "$pkg/mode")" in
  noresult) echo "admission refused" >&2; exit 2;;
  write) echo x > created.txt;;
esac
echo "RESULT (exit 0)"
cat "$pkg/canned.json"
`

func validateFixture(t *testing.T, mode, canned string) (pkg, cases, runner string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stub runner is a POSIX shell script")
	}
	root := t.TempDir()
	pkg = filepath.Join(root, "pkg")
	writeFiles(t, pkg, map[string]string{"mode": mode, "canned.json": canned, "primitive.yaml": "x: 1\n"})
	runner = filepath.Join(root, "tap-stub")
	os.WriteFile(runner, []byte(stubRunner), 0o755)
	writeFiles(t, root, map[string]string{"oracle.sh": `echo '{"n": 1, "args": "'"$*"'"}'` + "\n"})
	cf := CaseFile{Package: "p", Oracle: []string{"sh", "oracle.sh"}, Cases: []Case{{
		ID: "c1", Kind: "normal", Args: []string{"a"},
		Setup: []SetupStep{{Write: "in/x.txt", Text: "x\n"}, {Run: []string{"git", "init", "-q", "repo"}}},
	}}}
	b, _ := json.Marshal(cf)
	cases = filepath.Join(root, "cases.json")
	os.WriteFile(cases, b, 0o644)
	return pkg, cases, runner
}

func TestValidatePassesOnlyWhatMatchesTheOracleWithoutEffects(t *testing.T) {
	for _, tc := range []struct {
		name, mode, canned string
		pass               bool
		failure            string
	}{
		{"matches", "ok", `{"args":"a","n":1}`, true, ""},
		{"wrong result", "ok", `{"args":"a","n":2}`, false, "result differs"},
		{"writes into the fixture", "write", `{"args":"a","n":1}`, false, "created work/created.txt"},
		{"never ran", "noresult", `{}`, false, "no RESULT line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pkg, cases, runner := validateFixture(t, tc.mode, tc.canned)
			rec, err := Validate(ValidateOptions{Package: pkg, Cases: cases, Runner: runner})
			if err != nil {
				t.Fatal(err)
			}
			_, digest, _ := PackageDir(pkg)
			if rec.PackageDigest != digest || rec.RunnerSHA256 == "" || rec.CasesSHA256 == "" || rec.OracleSHA256["oracle.sh"] == "" {
				t.Errorf("receipts must name the exact package, runner, cases and oracle: %+v", rec)
			}
			c := rec.Cases[0]
			if c.Pass != tc.pass || rec.AllPassed != tc.pass {
				t.Fatalf("pass %v, want %v: %v", c.Pass, tc.pass, c.Failures)
			}
			if tc.failure != "" && !strings.Contains(strings.Join(c.Failures, "; "), tc.failure) {
				t.Errorf("failures %v lack %q", c.Failures, tc.failure)
			}
			if tc.mode != "noresult" && len(c.Actions) != 1 {
				t.Errorf("the runner's journal must be read back into the receipt, got %v", c.Actions)
			}
		})
	}
}

func TestFreezeAndOracleDrift(t *testing.T) {
	pkg, cases, runner := validateFixture(t, "ok", `{"args":"a","n":1}`)
	if n, err := Freeze(cases); err != nil || n != 1 {
		t.Fatalf("freeze: %d %v", n, err)
	}
	if n, err := Freeze(cases); err != nil || n != 0 {
		t.Fatalf("a frozen case must be left as it is: %d %v", n, err)
	}
	cf, _, _ := LoadCases(cases)
	if cf.Cases[0].Expect == nil || cf.Cases[0].Expect.Exit != 0 {
		t.Fatalf("expectation not recorded: %+v", cf.Cases[0])
	}
	// The oracle changes after freezing: the case fails even though the
	// package agrees with the new oracle.
	os.WriteFile(filepath.Join(filepath.Dir(cases), "oracle.sh"), []byte(`echo '{"n": 2}'`+"\n"), 0o644)
	os.WriteFile(filepath.Join(pkg, "canned.json"), []byte(`{"n":2}`), 0o644)
	rec, err := Validate(ValidateOptions{Package: pkg, Cases: cases, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	if rec.AllPassed || !strings.Contains(strings.Join(rec.Cases[0].Failures, ";"), "oracle drift") {
		t.Errorf("oracle drift must fail the case: %+v", rec.Cases[0])
	}
}

func TestValidateRefusesUnfinishedPackagesAndUnsafeSetup(t *testing.T) {
	pkg, cases, runner := validateFixture(t, "ok", `{}`)
	os.WriteFile(filepath.Join(pkg, "main.py"), []byte("x = 'REPLACE_ME'\n"), 0o644)
	if _, err := Validate(ValidateOptions{Package: pkg, Cases: cases, Runner: runner}); err == nil || !strings.Contains(err.Error(), "not finished") {
		t.Errorf("a package with a placeholder must not be validated: %v", err)
	}
	for _, st := range []SetupStep{{Run: []string{"rm", "-rf", "x"}}, {Write: "../../escape", Text: "x"}, {Write: "/abs", Text: "x"}, {Write: "a", Mkdir: "b"}} {
		if f, err := newFixture(Case{ID: "s", Setup: []SetupStep{st}}); err == nil {
			f.remove()
			t.Errorf("setup step %+v must be refused", st)
		}
	}
}

func authoredPackage(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pkg")
	contract := map[string]BriefField{}
	for _, f := range contractFields {
		contract[f] = BriefField{Value: "the " + f, EstablishedBy: "source request text and its git calls"}
	}
	a := Authoring{Kind: "tap.authoring/v1", Name: "repo-changes", Publisher: "dev.local", Author: "host-agent",
		Agent: "claude-code", Selection: SelectedTask, Sources: []string{"src_0123456789ab"}, BriefDigest: "sha256:00",
		Contract: contract, Interface: json.RawMessage(`{"args":[{"name":"script","type":"path"}]}`)}
	b, _ := json.MarshalIndent(a, "", "  ")
	writeFiles(t, dir, map[string]string{"AUTHORING.json": string(b), "primitive.yaml": "x: 1\n", "main.py": "print(1)\n"})
	return dir
}

func TestSavePackageBindsValidationToTheDigest(t *testing.T) {
	pkg := authoredPackage(t)
	_, digest, _ := PackageDir(pkg)
	root := t.TempDir()

	path, v, unchanged, err := SavePackage(pkg, root, nil, "")
	if err != nil || v != ValidationNotRun || unchanged {
		t.Fatalf("save without receipts: %q %v %v", v, unchanged, err)
	}
	if _, _, _, err := SavePackage(pkg, root, &Receipts{PackageDigest: "sha256:other", AllPassed: true, Cases: []CaseReceipt{{}}}, "r"); err == nil {
		t.Error("receipts for another digest must be refused")
	}
	pass := &Receipts{PackageDigest: digest, AllPassed: true, Cases: make([]CaseReceipt, 5)}
	if _, v, unchanged, err = SavePackage(pkg, root, pass, "sha256:r"); err != nil || v != ValidationPassed || unchanged {
		t.Fatalf("passing receipts must mark it passed and replace the not_run save: %q %v %v", v, unchanged, err)
	}
	var m savedMarker
	b, _ := os.ReadFile(filepath.Join(path, SavedMarker))
	json.Unmarshal(b, &m)
	if m.Origin != OriginAgentAuthored || m.Digest != digest || m.Validation != ValidationPassed || m.Cases != 5 || m.Receipts != "sha256:r" {
		t.Errorf("marker %+v", m)
	}
	skill, _ := os.ReadFile(filepath.Join(path, "SKILL.md"))
	if !strings.Contains(string(skill), "not a\nrecommendation made by `tap discover`") || !strings.Contains(string(skill), "**passed**") {
		t.Errorf("SKILL.md must say who wrote it and its status:\n%s", skill)
	}
	if _, _, unchanged, _ := SavePackage(pkg, root, pass, "sha256:r"); !unchanged {
		t.Error("saving the same package with the same receipts must change nothing")
	}
	fail := &Receipts{PackageDigest: digest, AllPassed: false, Cases: make([]CaseReceipt, 5)}
	if _, v, _, _ := SavePackage(pkg, root, fail, "sha256:f"); v != ValidationFailed {
		t.Errorf("failing receipts must mark it failed, got %q", v)
	}
}

func TestReadAuthoringRefusesAnIncompleteLineage(t *testing.T) {
	for name, edit := range map[string]func(a map[string]any){
		"unestablished field": func(a map[string]any) {
			a["contract"].(map[string]any)["oracle"] = map[string]any{"value": "x", "established_by": ""}
		},
		"session location":   func(a map[string]any) { a["sources"] = []string{"claude-code/6bd4cdcf/0"} },
		"not agent-authored": func(a map[string]any) { a["author"] = "discover" },
		"candidate unnamed":  func(a map[string]any) { a["selection"] = DiscoverCandidate },
	} {
		t.Run(name, func(t *testing.T) {
			pkg := authoredPackage(t)
			var a map[string]any
			b, _ := os.ReadFile(filepath.Join(pkg, "AUTHORING.json"))
			json.Unmarshal(b, &a)
			edit(a)
			b, _ = json.Marshal(a)
			os.WriteFile(filepath.Join(pkg, "AUTHORING.json"), b, 0o644)
			if _, err := ReadAuthoring(pkg); err == nil {
				t.Error("must be refused")
			}
		})
	}
}

func TestSavedDraftMarkerIsUnchanged(t *testing.T) {
	b, _ := json.Marshal(savedMarker{Name: "a/b", Digest: "sha256:x", Validation: ValidationNotRun})
	if string(b) != `{"name":"a/b","digest":"sha256:x","validation":"not_run"}` {
		t.Errorf("a saved draft's marker changed shape: %s", b)
	}
}

// The oracle runs inside each fixture: a case file named by a relative path
// must still find it, and an oracle that could not run must not be taken for
// an expected failure.
func TestOracleIsFoundFromARelativeCasesPathAndMustReport(t *testing.T) {
	_, cases, _ := validateFixture(t, "ok", `{}`)
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(filepath.Dir(filepath.Dir(cases)))
	rel := filepath.Join(filepath.Base(filepath.Dir(cases)), "cases.json")
	if n, err := Freeze(rel); err != nil || n != 1 {
		t.Fatalf("freeze from a relative path: %d %v", n, err)
	}
	cf, _, _ := LoadCases(rel)
	if cf.Cases[0].Expect.Exit != 0 || cf.Cases[0].Expect.Output == nil {
		t.Errorf("the oracle did not run: %+v", cf.Cases[0].Expect)
	}
	cf.Cases[0].Expect = nil
	cf.Oracle = []string{"sh", "no-such-oracle.sh"}
	b, _ := json.Marshal(cf)
	os.WriteFile(rel, b, 0o644)
	if _, err := Freeze(rel); err == nil || !strings.Contains(err.Error(), "printed nothing") {
		t.Errorf("an oracle that never ran must be an error, got %v", err)
	}
}

// An opportunity the selection pass surfaced reaches the author path: the
// brief carries the pass's route and reasons as its claim, and every
// contract field stays for the agent to establish.
func TestBriefFromASurfacedOpportunity(t *testing.T) {
	home := homeWithClaudeSession(t)
	report := filepath.Join(t.TempDir(), "report.json")
	op := Opportunity{ID: EpisodeID("claude-code", "s1", 0), Client: "claude-code", Session: "s1", Request: 0,
		Recommended: true, Route: RouteParamLoop, Reasons: []string{"loop:sh:git log:3"}, Task: "claude-code/s1/0"}
	b, _ := json.Marshal(map[string]any{"opportunities": []Opportunity{op}})
	os.WriteFile(report, b, 0o644)
	out := filepath.Join(t.TempDir(), "brief")
	var so, se bytes.Buffer
	if code := briefCommand([]string{"--opportunity", op.ID, "--report", report, "--out", out}, home, &so, &se); code != 0 {
		t.Fatalf("exit %d: %s", code, se.String())
	}
	var got Brief
	raw, _ := os.ReadFile(filepath.Join(out, "brief.json"))
	json.Unmarshal(raw, &got)
	if got.Selection != DiscoverOpportunity || got.Status != BriefStatus || got.Opportunity == nil || got.Opportunity.Route != RouteParamLoop {
		t.Errorf("brief does not carry the opportunity as a proposal: %+v", got)
	}
	if len(got.Missing) != len(contractFields) {
		t.Errorf("an opportunity establishes no contract field, missing %v", got.Missing)
	}
	if code := briefCommand([]string{"--opportunity", "ep_nope", "--report", report, "--out", out}, home, &so, &se); code == 0 {
		t.Error("an unknown opportunity must be refused")
	}
}
