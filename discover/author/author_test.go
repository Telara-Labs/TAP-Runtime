package author_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/discover/internal/testkit"

	"github.com/Telara-Labs/TAP-Runtime/discover/author"

	"github.com/Telara-Labs/TAP-Runtime/discover/retrieval"

	"github.com/Telara-Labs/TAP-Runtime/discover/pack"

	"github.com/Telara-Labs/TAP-Runtime/discover/model"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

// The author path: brief, validate, save.

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
	s, err := author.FindSession("claude-code", "s1", homeWithClaudeSession(t))
	if err != nil {
		t.Fatal(err)
	}
	b, err := author.NewBrief(s, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != model.BriefStatus || b.Selection != author.SelectedTask {
		t.Errorf("status %q selection %q: a brief is an unassessed proposal", b.Status, b.Selection)
	}
	if len(b.Missing) != len(author.ContractFields) {
		t.Errorf("a selected task has every contract field missing, got %v", b.Missing)
	}
	if b.Evidence.Request != "check the repos" || len(b.Evidence.Steps) == 0 || !strings.Contains(b.Evidence.Steps[0].Command, "git status") {
		t.Errorf("evidence is not the request and its calls: %+v", b.Evidence)
	}
	if !strings.HasPrefix(b.Ref, "src_") || strings.Contains(b.Ref, "s1") || author.OpaqueRef(b.Source) != b.Ref {
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
	if _, err := author.NewBrief(s, 7, nil); err == nil {
		t.Error("a request the session does not have must be refused")
	}
}

func TestBriefRedactsCredentials(t *testing.T) {
	s := trace.Session{Client: "codex", ID: "x", Requests: []string{"deploy with Bearer abcdefghijklmnopqrstu"},
		Calls: []trace.Call{{Tool: "shell", Command: "curl -H 'Authorization: Bearer abcdefghijklmnopqrstu' https://h", Output: "token glpat-abcdefghijklmnopqrstuv"}}}
	b, err := author.NewBrief(s, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(b)
	if strings.Contains(string(raw), "abcdefghijklmnopqrstu") {
		t.Errorf("a credential reached the brief: %s", raw)
	}
}

func TestBriefFromRecurringLogicShowsDifferentExecutions(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "projects", "proj")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(session, request, pipeline string) {
		t.Helper()
		lines := []string{
			fmt.Sprintf(`{"type":"user","message":{"content":%q}}`, request),
			fmt.Sprintf(`{"type":"assistant","message":{"id":"%s-m1","content":[{"type":"tool_use","id":"%s-t1","name":"mcp__telara__telara_gitlab_list_pipelines","input":{}}]}}`, session, session),
			fmt.Sprintf(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"%s-t1","content":%q}]}}`, session, `{"id":"`+pipeline+`"}`),
			fmt.Sprintf(`{"type":"assistant","message":{"id":"%s-m2","content":[{"type":"tool_use","id":"%s-t2","name":"mcp__telara__telara_gitlab_list_jobs","input":{"pipeline_id":%q}}]}}`, session, session, pipeline),
			fmt.Sprintf(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"%s-t2","content":"jobs found"}]}}`, session),
		}
		if err := os.WriteFile(filepath.Join(dir, session+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a", "Find failed build jobs", "81234567")
	write("b", "Check the deploy pipeline", "91234567")
	a, err := author.FindSession("claude-code", "a", home)
	if err != nil {
		t.Fatal(err)
	}
	b, err := author.FindSession("claude-code", "b", home)
	if err != nil {
		t.Fatal(err)
	}
	spans := retrieval.SelectSpanProposals([]trace.Session{a, b})
	groups := retrieval.GroupLogicCandidates(spans)
	if len(groups) != 1 {
		t.Fatalf("want one parameterized flow, got %+v", groups)
	}
	report := filepath.Join(t.TempDir(), "report.json")
	raw, _ := json.Marshal(map[string]any{"span_proposals": spans, "logic_candidates": groups})
	if err := os.WriteFile(report, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "brief")
	var stdout, stderr bytes.Buffer
	if code := author.BriefCommand([]string{"--logic", groups[0].ID, "--report", report, "--out", out}, home, &stdout, &stderr); code != 0 {
		t.Fatalf("brief exit %d: %s", code, stderr.String())
	}
	var got author.Brief
	briefRaw, err := os.ReadFile(filepath.Join(out, "brief.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(briefRaw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Selection != author.DiscoverLogic || got.Logic == nil || len(got.LogicExamples) != 2 ||
		len(got.LogicExamples[0].Evidence.Steps) != 2 || len(got.LogicExamples[1].Evidence.Steps) != 2 {
		t.Fatalf("logic brief must compare two selected executions: %+v", got)
	}
	md, _ := os.ReadFile(filepath.Join(out, "BRIEF.md"))
	if !strings.Contains(string(md), "runtime arguments") || !strings.Contains(string(md), "81234567") || !strings.Contains(string(md), "91234567") {
		t.Fatalf("authoring brief did not show parameterized examples: %s", md)
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
		"source_role": "scheduled", "suitability": model.SuitInsufficient,
		"contract": map[string]any{"goal": "unknown", "output": "report", "effect": "read_only",
			"inputs": []map[string]any{{"name": "repo", "type": "path", "source": "caller"}}},
		"sources": []map[string]any{{"client": "claude-code", "session": "s1", "request": 0}},
	}}}
	b, _ := json.Marshal(rep)
	os.WriteFile(report, b, 0o644)

	out := filepath.Join(t.TempDir(), "brief")
	var so, se bytes.Buffer
	if code := author.BriefCommand([]string{"--candidate", "r1", "--report", report, "--out", out}, home, &so, &se); code != 0 {
		t.Fatalf("exit %d: %s", code, se.String())
	}
	var got author.Brief
	raw, _ := os.ReadFile(filepath.Join(out, "brief.json"))
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Selection != author.DiscoverCandidate || got.Status != model.BriefStatus || got.Candidate == nil || got.Candidate.Decision != "removed" {
		t.Errorf("brief does not carry the rejected candidate as a proposal: %+v", got)
	}
	if !strings.Contains(got.Contract.Output.EstablishedBy, "unverified") {
		t.Errorf("discover's output label must be marked unverified, got %q", got.Contract.Output.EstablishedBy)
	}
	for _, f := range []string{"goal", "procedure", "oracle", "failures"} {
		if !testkit.Contains(got.Missing, f) {
			t.Errorf("%s was not established by anything and must be missing: %v", f, got.Missing)
		}
	}
	if testkit.Contains(got.Missing, "output") {
		t.Errorf("output was proposed by discover and is not missing: %v", got.Missing)
	}
	if code := author.BriefCommand([]string{"--candidate", "nope", "--report", report, "--out", out}, home, &so, &se); code == 0 {
		t.Error("an unknown routine must be refused")
	}
	if code := author.BriefCommand([]string{"--task", "claude-code/s1/0", "--candidate", "r1", "--report", report, "--out", out}, home, &so, &se); code != 2 {
		t.Error("--task and --candidate together must be refused")
	}
}

func TestPackageDirDigestAndPlaceholders(t *testing.T) {
	dir := t.TempDir()
	testkit.WriteFiles(t, dir, map[string]string{"primitive.yaml": "a: 1\n", "main.py": "print(1)\n"})
	_, d1, err := author.PackageDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, d2, _ := author.PackageDir(dir)
	if d1 != d2 {
		t.Error("the same bytes must have the same digest")
	}
	testkit.WriteFiles(t, dir, map[string]string{"main.py": "print(2)\n"})
	if _, d3, _ := author.PackageDir(dir); d3 == d1 {
		t.Error("a changed byte must change the digest")
	}
	if marks, _ := author.FindPlaceholders(dir); len(marks) != 0 {
		t.Errorf("no placeholders yet, got %v", marks)
	}
	testkit.WriteFiles(t, dir, map[string]string{"main.py": "# TODO: finish\n"})
	if marks, _ := author.FindPlaceholders(dir); len(marks) != 1 {
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
	testkit.WriteFiles(t, pkg, map[string]string{"mode": mode, "canned.json": canned, "primitive.yaml": "x: 1\n"})
	runner = filepath.Join(root, "tap-stub")
	os.WriteFile(runner, []byte(stubRunner), 0o755)
	testkit.WriteFiles(t, root, map[string]string{"oracle.sh": `echo '{"n": 1, "args": "'"$*"'"}'` + "\n"})
	cf := author.CaseFile{Package: "p", Oracle: []string{"sh", "oracle.sh"}, Cases: []author.Case{{
		ID: "c1", Kind: "normal", Args: []string{"a"},
		Setup: []author.SetupStep{{Write: "in/x.txt", Text: "x\n"}, {Run: []string{"git", "init", "-q", "repo"}}},
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
			rec, err := author.Validate(author.ValidateOptions{Package: pkg, Cases: cases, Runner: runner})
			if err != nil {
				t.Fatal(err)
			}
			_, digest, _ := author.PackageDir(pkg)
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
	if n, err := author.Freeze(cases); err != nil || n != 1 {
		t.Fatalf("freeze: %d %v", n, err)
	}
	if n, err := author.Freeze(cases); err != nil || n != 0 {
		t.Fatalf("a frozen case must be left as it is: %d %v", n, err)
	}
	cf, _, _ := author.LoadCases(cases)
	if cf.Cases[0].Expect == nil || cf.Cases[0].Expect.Exit != 0 {
		t.Fatalf("expectation not recorded: %+v", cf.Cases[0])
	}
	// The oracle changes after freezing: the case fails even though the
	// package agrees with the new oracle.
	os.WriteFile(filepath.Join(filepath.Dir(cases), "oracle.sh"), []byte(`echo '{"n": 2}'`+"\n"), 0o644)
	os.WriteFile(filepath.Join(pkg, "canned.json"), []byte(`{"n":2}`), 0o644)
	rec, err := author.Validate(author.ValidateOptions{Package: pkg, Cases: cases, Runner: runner})
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
	if _, err := author.Validate(author.ValidateOptions{Package: pkg, Cases: cases, Runner: runner}); err == nil || !strings.Contains(err.Error(), "not finished") {
		t.Errorf("a package with a placeholder must not be validated: %v", err)
	}
	for _, st := range []author.SetupStep{{Run: []string{"rm", "-rf", "x"}}, {Write: "../../escape", Text: "x"}, {Write: "/abs", Text: "x"}, {Write: "a", Mkdir: "b"}} {
		if f, err := author.NewFixture(author.Case{ID: "s", Setup: []author.SetupStep{st}}); err == nil {
			f.Remove()
			t.Errorf("setup step %+v must be refused", st)
		}
	}
}

func authoredPackage(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pkg")
	contract := map[string]author.BriefField{}
	for _, f := range author.ContractFields {
		contract[f] = author.BriefField{Value: "the " + f, EstablishedBy: "source request text and its git calls"}
	}
	a := author.Authoring{Kind: "tap.authoring/v1", Name: "repo-changes", Publisher: "dev.local", Author: "host-agent",
		Agent: "claude-code", Selection: author.SelectedTask, Sources: []string{"src_0123456789ab"}, BriefDigest: "sha256:00",
		Contract: contract, Interface: json.RawMessage(`{"args":[{"name":"script","type":"path"}]}`)}
	b, _ := json.MarshalIndent(a, "", "  ")
	testkit.WriteFiles(t, dir, map[string]string{"AUTHORING.json": string(b), "primitive.yaml": "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.local, name: repo-changes, version: 0.1.0}\nexecution: {entrypoint: main.py}\n", "main.py": "print(1)\n", "CHANGELOG.md": "# Changelog\n\n## 0.1.0\n\n- Initial authored procedure.\n"})
	return dir
}

func TestSavePackageBindsValidationToTheDigest(t *testing.T) {
	pkg := authoredPackage(t)
	_, digest, _ := author.PackageDir(pkg)
	root := t.TempDir()

	path, v, unchanged, err := author.SavePackage(pkg, root, nil, "")
	if err != nil || v != model.ValidationNotRun || unchanged {
		t.Fatalf("save without receipts: %q %v %v", v, unchanged, err)
	}
	if _, _, _, err := author.SavePackage(pkg, root, &author.Receipts{PackageDigest: "sha256:other", AllPassed: true, Cases: []author.CaseReceipt{{}}}, "r"); err == nil {
		t.Error("receipts for another digest must be refused")
	}
	pass := &author.Receipts{PackageDigest: digest, AllPassed: true, Cases: make([]author.CaseReceipt, 5)}
	if _, v, unchanged, err = author.SavePackage(pkg, root, pass, "sha256:r"); err != nil || v != model.ValidationPassed || unchanged {
		t.Fatalf("passing receipts must mark it passed and replace the not_run save: %q %v %v", v, unchanged, err)
	}
	var m pack.Marker
	b, _ := os.ReadFile(filepath.Join(path, pack.SavedMarker))
	json.Unmarshal(b, &m)
	if m.Origin != author.OriginAgentAuthored || m.Digest != digest || m.Validation != model.ValidationPassed || m.Cases != 5 || m.Receipts != "sha256:r" {
		t.Errorf("marker %+v", m)
	}
	skill, _ := os.ReadFile(filepath.Join(path, "SKILL.md"))
	if !strings.Contains(string(skill), "not a\nrecommendation made by `tap discover`") || !strings.Contains(string(skill), "**passed**") {
		t.Errorf("SKILL.md must say who wrote it and its status:\n%s", skill)
	}
	if _, _, unchanged, _ := author.SavePackage(pkg, root, pass, "sha256:r"); !unchanged {
		t.Error("saving the same package with the same receipts must change nothing")
	}
	if _, v, unchanged, err = author.SavePackage(pkg, root, nil, ""); err != nil || v != model.ValidationPassed || !unchanged {
		t.Fatalf("identical save erased passing evidence: %q %v %v", v, unchanged, err)
	}
	kept, err := pack.ReadMarker(path)
	if err != nil || kept != m {
		t.Fatalf("receipt/validation metadata changed: %+v %v", kept, err)
	}
	keptSkill, _ := os.ReadFile(filepath.Join(path, "SKILL.md"))
	if string(keptSkill) != string(skill) {
		t.Fatal("identical save rewrote validation skill")
	}
	fail := &author.Receipts{PackageDigest: digest, AllPassed: false, Cases: make([]author.CaseReceipt, 5)}
	if _, v, _, _ := author.SavePackage(pkg, root, fail, "sha256:f"); v != model.ValidationFailed {
		t.Errorf("failing receipts must mark it failed, got %q", v)
	}
	if _, v, unchanged, err = author.SavePackage(pkg, root, nil, ""); err != nil || v != model.ValidationFailed || !unchanged {
		t.Fatalf("identical save erased failed evidence: %q %v %v", v, unchanged, err)
	}
}

func TestReadAuthoringRefusesAnIncompleteLineage(t *testing.T) {
	for name, edit := range map[string]func(a map[string]any){
		"unestablished field": func(a map[string]any) {
			a["contract"].(map[string]any)["oracle"] = map[string]any{"value": "x", "established_by": ""}
		},
		"session location":   func(a map[string]any) { a["sources"] = []string{"claude-code/6bd4cdcf/0"} },
		"not agent-authored": func(a map[string]any) { a["author"] = "discover" },
		"candidate unnamed":  func(a map[string]any) { a["selection"] = author.DiscoverCandidate },
	} {
		t.Run(name, func(t *testing.T) {
			pkg := authoredPackage(t)
			var a map[string]any
			b, _ := os.ReadFile(filepath.Join(pkg, "AUTHORING.json"))
			json.Unmarshal(b, &a)
			edit(a)
			b, _ = json.Marshal(a)
			os.WriteFile(filepath.Join(pkg, "AUTHORING.json"), b, 0o644)
			if _, err := author.ReadAuthoring(pkg); err == nil {
				t.Error("must be refused")
			}
		})
	}
}

func TestReadAuthoringAcceptsParameterizedLogicLineage(t *testing.T) {
	pkg := authoredPackage(t)
	path := filepath.Join(pkg, "AUTHORING.json")
	var a author.Authoring
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &a); err != nil {
		t.Fatal(err)
	}
	a.Selection, a.Candidate = author.DiscoverLogic, "lc_0123456789ab"
	a.Sources = []string{"src_0123456789ab", "src_abcdef012345"}
	b, _ = json.Marshal(a)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := author.ReadAuthoring(pkg); err != nil {
		t.Fatalf("validated logic package lineage rejected: %v", err)
	}
	a.Candidate = ""
	b, _ = json.Marshal(a)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := author.ReadAuthoring(pkg); err == nil {
		t.Fatal("logic selection without candidate id must be rejected")
	}
}

func TestSavedDraftMarkerIsUnchanged(t *testing.T) {
	b, _ := json.Marshal(pack.Marker{Name: "a/b", Digest: "sha256:x", Validation: model.ValidationNotRun})
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
	if n, err := author.Freeze(rel); err != nil || n != 1 {
		t.Fatalf("freeze from a relative path: %d %v", n, err)
	}
	cf, _, _ := author.LoadCases(rel)
	if cf.Cases[0].Expect.Exit != 0 || cf.Cases[0].Expect.Output == nil {
		t.Errorf("the oracle did not run: %+v", cf.Cases[0].Expect)
	}
	cf.Cases[0].Expect = nil
	cf.Oracle = []string{"sh", "no-such-oracle.sh"}
	b, _ := json.Marshal(cf)
	os.WriteFile(rel, b, 0o644)
	if _, err := author.Freeze(rel); err == nil || !strings.Contains(err.Error(), "printed nothing") {
		t.Errorf("an oracle that never ran must be an error, got %v", err)
	}
}

// An opportunity the selection pass surfaced reaches the author path: the
// brief carries the pass's route and reasons as its claim, and every
// contract field stays for the agent to establish.
func TestBriefFromASurfacedOpportunity(t *testing.T) {
	home := homeWithClaudeSession(t)
	report := filepath.Join(t.TempDir(), "report.json")
	op := model.Opportunity{ID: trace.EpisodeID("claude-code", "s1", 0), Client: "claude-code", Session: "s1", Request: 0,
		Recommended: true, Route: model.RouteParamLoop, Reasons: []string{"loop:sh:git log:3"}, Task: "claude-code/s1/0"}
	b, _ := json.Marshal(map[string]any{"opportunities": []model.Opportunity{op}})
	os.WriteFile(report, b, 0o644)
	out := filepath.Join(t.TempDir(), "brief")
	var so, se bytes.Buffer
	if code := author.BriefCommand([]string{"--opportunity", op.ID, "--report", report, "--out", out}, home, &so, &se); code != 0 {
		t.Fatalf("exit %d: %s", code, se.String())
	}
	var got author.Brief
	raw, _ := os.ReadFile(filepath.Join(out, "brief.json"))
	json.Unmarshal(raw, &got)
	if got.Selection != author.DiscoverOpportunity || got.Status != model.BriefStatus || got.Opportunity == nil || got.Opportunity.Route != model.RouteParamLoop {
		t.Errorf("brief does not carry the opportunity as a proposal: %+v", got)
	}
	if len(got.Missing) != len(author.ContractFields) {
		t.Errorf("an opportunity establishes no contract field, missing %v", got.Missing)
	}
	if code := author.BriefCommand([]string{"--opportunity", "ep_nope", "--report", report, "--out", out}, home, &so, &se); code == 0 {
		t.Error("an unknown opportunity must be refused")
	}
}
