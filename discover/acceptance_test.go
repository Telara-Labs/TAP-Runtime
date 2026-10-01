package discover

// Acceptance cases C01-C24 of the discover execution plan
// (tap-discover-review-2026-09-29/CLAUDE-EXECUTION-PLAN.md, section 6).
// Each case builds a sanitized synthetic history, runs discovery, and
// asserts an observable result: a report field, a draft's text, or what the
// drafted program actually does when run against a stand-in for tap.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/history"

	"gitlab.com/telara-labs/tap-runtime/discover/redact"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// eps builds n sessions named prefix00.., each one request, four days apart.
// Calls without a recorded outcome are marked ok.
func eps(prefix string, n int, text func(i int) string, calls func(i int) []trace.Call) []trace.Session {
	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	var out []trace.Session
	for i := 0; i < n; i++ {
		s := trace.Session{Client: "fake", ID: fmt.Sprintf("%s%02d", prefix, i), Start: t0.AddDate(0, 0, 4*i)}
		s.AddRequest(text(i))
		for _, c := range calls(i) {
			c.Request, c.Time = 0, s.Start
			if c.Outcome == trace.OutcomeUnknown {
				c.Outcome = trace.OutcomeOK
			}
			s.Calls = append(s.Calls, c)
		}
		out = append(out, s)
	}
	return out
}

func runOn(t *testing.T, ss ...[]trace.Session) *Report {
	t.Helper()
	var all []trace.Session
	for _, s := range ss {
		all = append(all, s...)
	}
	o := DefaultOptions()
	o.Readers = []trace.Reader{fakeReader{sessions: all}}
	rep, err := Run(o)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// sessionsOf is the set of sessions a routine was found in.
func sessionsOf(r Routine) map[string]bool {
	out := map[string]bool{}
	for _, s := range r.Sources {
		out[s.Session] = true
	}
	return out
}

func prefixes(r Routine) map[string]bool {
	out := map[string]bool{}
	for s := range sessionsOf(r) {
		out[strings.TrimRight(s, "0123456789")] = true
	}
	return out
}

func hasStep(r Routine, label string) bool {
	for _, s := range r.Steps {
		if s.Label == label {
			return true
		}
	}
	return false
}

func stepIndex(r Routine, label string) int {
	for i, s := range r.Steps {
		if s.Label == label {
			return i
		}
	}
	return -1
}

func inputBySuffix(r Routine, suffix string) *ContractInput {
	for i := range r.Contract.Inputs {
		if strings.HasSuffix(r.Contract.Inputs[i].Name, suffix) {
			return &r.Contract.Inputs[i]
		}
	}
	return nil
}

func dump(rep *Report) string {
	var b strings.Builder
	for _, r := range rep.Routines {
		var srcs []string
		for s := range sessionsOf(r) {
			srcs = append(srcs, s)
		}
		sort.Strings(srcs)
		fmt.Fprintf(&b, "  %s role=%s suit=%s draft=%s blockers=%v merged=%q steps=%s sources=%v inputs=%+v\n",
			r.ID, r.SourceRole, r.Suitability, r.DraftStatus, r.Blockers, r.MergedInto, labelsOf(r.Candidate), srcs, r.Contract.Inputs)
	}
	return b.String()
}

// runDraft runs a draft's main.sh with bash, with a stand-in tap that logs
// each call and prints the canned output for its alias. It returns the
// combined output, each tool call made (alias + JSON), and the exit error.
func runDraft(t *testing.T, d *Draft, outputs map[string]string, args ...string) (string, []string, error) {
	t.Helper()
	_, out, calls, err := runDraftDir(t, d, outputs, args...)
	return out, calls, err
}

// runDraftDir is runDraft that also returns the directory the program ran in.
func runDraftDir(t *testing.T, d *Draft, outputs map[string]string, args ...string) (string, string, []string, error) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	logf := filepath.Join(dir, "calls.log")
	// tap: logs each call; "tap call" takes one argument, a JSON object,
	// as the guest's does (guest-sh/main.go), and fails otherwise.
	fake := "#!/bin/bash\n[ \"$1\" = call ] || exit 2\nif [ $# -ge 3 ]; then printf '%s' \"$3\" | " + realJQ(t) + " -e 'type == \"object\"' >/dev/null 2>&1 || { echo 'tap call: arguments are not a JSON object' >&2; exit 2; }; fi\nprintf '%s %s\\n' \"$2\" \"$3\" >> " + logf + "\nf=" + dir + "/out.$2\n[ -f \"$f\" ] && cat \"$f\"\nexit 0\n"
	os.WriteFile(filepath.Join(bin, "tap"), []byte(fake), 0o755)
	// jq: the guest's jq accepts only -r and one filter, reading stdin
	// (guest-sh/main.go runJQ). A draft must not rely on more.
	shim := "#!/bin/bash\nfilter=.\nraw=\nfor a in \"$@\"; do case \"$a\" in -r) raw=-r;; -*) echo \"jq: unsupported flag $a\" >&2; exit 2;; *) filter=\"$a\";; esac; done\nexec " + realJQ(t) + " -c $raw \"$filter\"\n"
	os.WriteFile(filepath.Join(bin, "jq"), []byte(shim), 0o755)
	for alias, out := range outputs {
		os.WriteFile(filepath.Join(dir, "out."+alias), []byte(out), 0o644)
	}
	script := filepath.Join(dir, "main.sh")
	os.WriteFile(script, d.Files["main.sh"], 0o755)
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	logb, _ := os.ReadFile(logf)
	var calls []string
	for _, l := range strings.Split(strings.TrimSpace(string(logb)), "\n") {
		if l != "" {
			calls = append(calls, l)
		}
	}
	return dir, string(out), calls, err
}

// C01: the same tool labels asking for different outcomes are different
// task contracts, never one routine with the operation as an input.
func TestC01SameToolsDifferentOutcomesAreDistinct(t *testing.T) {
	create := eps("create", 6, func(i int) string { return fmt.Sprintf("open a merge request for branch feat-%d", i) }, func(i int) []trace.Call {
		return []trace.Call{
			{Tool: "mcp:gitlab_list_branches", Args: map[string]string{"project": "telara/gateway"}},
			{Tool: "mcp:gitlab_merge_request", Args: map[string]string{"action": "create", "source_branch": fmt.Sprintf("feat-%d", i), "target_branch": "main"}},
		}
	})
	list := eps("list", 6, func(i int) string { return fmt.Sprintf("list the open merge requests for branch feat-%d", i) }, func(i int) []trace.Call {
		return []trace.Call{
			{Tool: "mcp:gitlab_list_branches", Args: map[string]string{"project": "telara/gateway"}},
			{Tool: "mcp:gitlab_merge_request", Args: map[string]string{"action": "list", "source_branch": fmt.Sprintf("feat-%d", i), "state": "opened"}},
		}
	})
	rep := runOn(t, create, list)
	found := false
	for _, r := range rep.Routines {
		p := prefixes(r)
		if p["create"] && p["list"] {
			t.Fatalf("one routine holds both outcomes:\n%s", dump(rep))
		}
		if in := inputBySuffix(r, "_action"); in != nil && in.Source == InputCaller {
			t.Fatalf("the operation became a caller input:\n%s", dump(rep))
		}
		if p["create"] && len(sessionsOf(r)) >= 3 {
			found = true
		}
	}
	if !found {
		t.Fatalf("the create-merge-request task was not found at all:\n%s", dump(rep))
	}
}

// C02: the same procedure on different resources is one parameterized
// routine, with the resource as the caller's input.
func TestC02DifferentResourceIDsAreOneParameterizedRoutine(t *testing.T) {
	ss := eps("pipe", 6, func(i int) string { return fmt.Sprintf("show me the jobs of pipeline %d", 100200+i) }, func(i int) []trace.Call {
		id := fmt.Sprint(100200 + i)
		return []trace.Call{
			{Tool: "mcp:gitlab_get_pipeline", Args: map[string]string{"pipeline_id": id}},
			{Tool: "mcp:gitlab_list_jobs", Args: map[string]string{"pipeline_id": id}},
		}
	})
	rep := runOn(t, ss)
	if len(rep.Routines) != 1 || len(sessionsOf(rep.Routines[0])) != 6 {
		t.Fatalf("want one routine over all six runs:\n%s", dump(rep))
	}
	r := rep.Routines[0]
	if r.Suitability != SuitUseful {
		t.Fatalf("suitability %q (%v):\n%s", r.Suitability, r.Reasons, dump(rep))
	}
	in := inputBySuffix(r, "pipeline_id")
	if in == nil || in.Source != InputCaller {
		t.Fatalf("pipeline_id must be the caller's input, given in the request: %+v", r.Contract.Inputs)
	}
	if strings.Contains(string(RoutineDraft(&r).Files["main.sh"]), "100200") {
		t.Fatal("a recorded resource id was written into the program")
	}
}

// C03: a staging write and a production write are different authority
// scopes: no routine spans both, and each names its scope.
func TestC03StagingAndProductionWritesDoNotShareAScope(t *testing.T) {
	roll := func(prefix, env, ctx string) []trace.Session {
		return eps(prefix, 6, func(i int) string { return fmt.Sprintf("roll the gateway to image v1.%d in %s", i, env) }, func(i int) []trace.Call {
			return []trace.Call{
				sh(fmt.Sprintf("kubectl --context %s -n gateway set image deploy/gateway gateway=registry.example/gw:v1.%d", ctx, i)),
				sh(fmt.Sprintf("kubectl --context %s -n gateway rollout status deploy/gateway", ctx)),
			}
		})
	}
	rep := runOn(t, roll("stg", "staging", "gke-staging"), roll("prd", "production", "gke-prod"))
	scopes := map[string]bool{}
	for _, r := range rep.Routines {
		p := prefixes(r)
		if p["stg"] && p["prd"] {
			t.Fatalf("one routine spans staging and production:\n%s", dump(rep))
		}
		if in := inputBySuffix(r, "_context"); in != nil {
			t.Fatalf("the kube context became an input (%+v): a caller could aim it anywhere:\n%s", in, dump(rep))
		}
		for _, s := range r.Contract.Scope {
			scopes[s] = true
		}
	}
	if !scopes["kubectl --context=gke-staging"] || !scopes["kubectl --context=gke-prod"] {
		t.Fatalf("each routine must name its scope; scopes = %v\n%s", scopes, dump(rep))
	}
}

// C04: two providers for a similar goal may share a family but are never
// merged into one implementation.
func TestC04DifferentProvidersAreNotInterchangeable(t *testing.T) {
	gl := eps("gl", 6, func(i int) string { return fmt.Sprintf("show the failing job log for pipeline %d", 300+i) }, func(i int) []trace.Call {
		return []trace.Call{
			{Tool: "mcp:gitlab_get_pipeline", Args: map[string]string{"pipeline_id": fmt.Sprint(300 + i)}},
			{Tool: "mcp:gitlab_get_job_log", Args: map[string]string{"pipeline_id": fmt.Sprint(300 + i), "status": "failed"}},
		}
	})
	gh := eps("gh", 6, func(i int) string { return fmt.Sprintf("show the failing job log for run %d", 900+i) }, func(i int) []trace.Call {
		return []trace.Call{sh(fmt.Sprintf("gh run view %d --json jobs", 900+i)), sh(fmt.Sprintf("gh run view %d --log-failed", 900+i))}
	})
	rep := runOn(t, gl, gh)
	for _, r := range rep.Routines {
		p := prefixes(r)
		if p["gl"] && p["gh"] {
			t.Fatalf("two providers were merged into one implementation:\n%s", dump(rep))
		}
		if r.MergedInto != "" {
			t.Fatalf("%s was merged into %s:\n%s", r.ID, r.MergedInto, dump(rep))
		}
	}
}

// C05: a bounded read-only check is useful; lacking a write is no reason
// to reject it.
func TestC05BoundedReadOnlyDiffCheckIsUseful(t *testing.T) {
	ss := eps("diff", 6, func(i int) string {
		return fmt.Sprintf("did we add tests without service changes between %07x and %07x?", 0xa1b2c00+i, 0xd4e5f00+i)
	}, func(i int) []trace.Call {
		a, b := fmt.Sprintf("%07x", 0xa1b2c00+i), fmt.Sprintf("%07x", 0xd4e5f00+i)
		return []trace.Call{sh("git diff --name-only " + a + " " + b + " -- services"), sh("git diff --name-only " + a + " " + b + " -- tests")}
	})
	rep := runOn(t, ss)
	if len(rep.Routines) != 1 {
		t.Fatalf("want one routine:\n%s", dump(rep))
	}
	r := rep.Routines[0]
	if r.Suitability != SuitUseful || r.DraftStatus != DraftComplete || r.Contract.Effect != EffectReadOnly {
		t.Fatalf("suit %q draft %q effect %q reasons %v:\n%s", r.Suitability, r.DraftStatus, r.Contract.Effect, r.Reasons, dump(rep))
	}
	callers := 0
	for _, in := range r.Contract.Inputs {
		if in.Source == InputCaller {
			callers++
		}
	}
	if callers != 2 {
		t.Fatalf("both revisions come from the request: %+v", r.Contract.Inputs)
	}
}

// C06: a declared list processed item by item is a useful procedure with a
// list input; the drafted loop handles any number of items, including none.
func TestC06ExplicitListLoopIsAUsefulProcedure(t *testing.T) {
	ss := eps("sum", 9, func(i int) string {
		var fs []string
		for f := 0; f < 2+i%3; f++ {
			fs = append(fs, fmt.Sprintf("docs/r%d-%d.txt", i, f))
		}
		return "compute the sha256 of " + strings.Join(fs, " ")
	}, func(i int) []trace.Call {
		var cs []trace.Call
		for f := 0; f < 2+i%3; f++ {
			cs = append(cs, sh(fmt.Sprintf("shasum -a 256 docs/r%d-%d.txt", i, f)))
		}
		return cs
	})
	rep := runOn(t, ss)
	if len(rep.Routines) != 1 {
		t.Fatalf("want one routine:\n%s", dump(rep))
	}
	r := rep.Routines[0]
	if r.Suitability != SuitUseful || r.DraftStatus != DraftComplete {
		t.Fatalf("suit %q draft %q blockers %v:\n%s", r.Suitability, r.DraftStatus, r.Blockers, dump(rep))
	}
	var list *ContractInput
	for i := range r.Contract.Inputs {
		if r.Contract.Inputs[i].Type == "list" {
			list = &r.Contract.Inputs[i]
		}
	}
	if list == nil || list.Source != InputCaller {
		t.Fatalf("the files are a caller-given list: %+v", r.Contract.Inputs)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "x.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "y z.txt"), []byte("y"), 0o644)
	script := filepath.Join(dir, "main.sh")
	os.WriteFile(script, RoutineDraft(&r).Files["main.sh"], 0o755)
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "jq"), []byte("#!/bin/bash\nfilter=.\nraw=\nfor a in \"$@\"; do case \"$a\" in -r) raw=-r;; -*) echo \"jq: unsupported flag $a\" >&2; exit 2;; *) filter=\"$a\";; esac; done\nexec "+realJQ(t)+" -c $raw \"$filter\"\n"), 0o755)
	run := func(arg string) (string, error) {
		cmd := exec.Command("bash", script, arg)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
		b, err := cmd.CombinedOutput()
		return string(b), err
	}
	out, err := run(`["x.txt","y z.txt"]`)
	if err != nil || strings.Count(out, "x.txt") != 1 || strings.Count(out, "y z.txt") != 1 {
		t.Fatalf("two files, including one with a space: %v\n%s\n%s", err, out, RoutineDraft(&r).Files["main.sh"])
	}
	if out, err := run(`[]`); err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("an empty list runs nothing and succeeds: %v %q", err, out)
	}
}

// explore is an open-ended investigation: each run reads and searches
// places the agent chose as it went.
func explore(prefix string, n int) []trace.Session {
	svc := []string{"billing", "gateway", "search", "tenants", "storage", "knowledge", "scheduler", "vault", "agents"}
	return eps(prefix, n, func(i int) string { return fmt.Sprintf("why does the %s service fail on startup?", svc[i%len(svc)]) }, func(i int) []trace.Call {
		return []trace.Call{
			{Tool: "Read", Args: map[string]string{"file_path": fmt.Sprintf("src/mod%d/init_%d.go", i*7, i)}},
			sh(fmt.Sprintf("rg -n handler%d src/mod%d", i*3, i*5)),
			{Tool: "Read", Args: map[string]string{"file_path": fmt.Sprintf("src/mod%d/config_%d.go", i*11, i)}},
			sh(fmt.Sprintf("rg -n retry%d internal/x%d", i*13, i)),
		}
	})
}

// C07: open-ended exploration is not a useful procedure.
func TestC07OpenEndedExplorationIsNotUseful(t *testing.T) {
	rep := runOn(t, explore("ex", 9))
	for _, r := range rep.Routines {
		if r.Suitability == SuitUseful {
			t.Fatalf("exploration claimed as a useful procedure:\n%s", dump(rep))
		}
	}
}

// C08: a bounded log collection inside different investigations is a
// useful subprocedure; the investigation around it is not claimed.
func TestC08BoundedLogCollectionInsideInvestigation(t *testing.T) {
	ex := explore("x", 6)
	for i := range ex {
		ns := fmt.Sprintf("team-%d", i)
		pod := fmt.Sprintf("api-7d9f8b6c%d-x2x9k", i)
		ex[i].Requests[0] = fmt.Sprintf("the api is crashlooping in namespace %s, find out why", ns)
		get := sh("kubectl --context minikube -n " + ns + " get pods")
		get.Outcome, get.Output = trace.OutcomeOK, "NAME READY STATUS RESTARTS AGE\n"+pod+" 0/1 CrashLoopBackOff 7 3m"
		get.OutIDs, get.OutCtx, get.OutPaths = trace.OutputRefsPaths(get.Output)
		logs := sh("kubectl --context minikube -n " + ns + " logs " + pod + " --tail=200")
		logs.Outcome = trace.OutcomeOK
		get.Time, logs.Time = ex[i].Start, ex[i].Start
		ex[i].Calls = append([]trace.Call{get, logs}, ex[i].Calls...)
	}
	rep := runOn(t, ex)
	var sub *Routine
	for i, r := range rep.Routines {
		if hasStep(r, "sh:kubectl get") && hasStep(r, "sh:kubectl logs") && r.Suitability == SuitUseful {
			sub = &rep.Routines[i]
		}
	}
	if sub == nil {
		t.Fatalf("the bounded log collection was not found as useful:\n%s", dump(rep))
	}
	if hasStep(*sub, "Read") || hasStep(*sub, "sh:rg") {
		t.Fatalf("the investigation's reads were claimed as part of the procedure:\n%s", dump(rep))
	}
	pod := inputBySuffix(*sub, "_arg1")
	for i := range sub.Contract.Inputs {
		if sub.Contract.Inputs[i].Source == InputPriorOutput {
			pod = &sub.Contract.Inputs[i]
		}
	}
	if pod == nil || pod.Source != InputPriorOutput || pod.From != 1 {
		t.Fatalf("the pod came from step 1's output: %+v", sub.Contract.Inputs)
	}
}

// C09: a value composed from an earlier read (a comment written from the
// issue) is neither a caller input nor a reason to drop the read.
func TestC09ReadResultsFeedingAWriteArePreserved(t *testing.T) {
	ss := eps("sum", 6, func(i int) string { return fmt.Sprintf("summarize the status of TENG-%d on the ticket", 4100+i) }, func(i int) []trace.Call {
		key := fmt.Sprintf("TENG-%d", 4100+i)
		summaries := []string{"Gateway pods restart during node upgrade", "Billing export misses the last day of the month", "Search index lags behind writes by an hour", "Vault token renewal fails after rotation", "Scheduler double-fires the nightly backfill", "Tenant provisioning stalls on quota check"}
		get := trace.Call{Tool: "mcp:telara_jira_get_issue", Args: map[string]string{"issue_key": key},
			Output: fmt.Sprintf(`{"key":"%s","summary":"%s","status":"In Progress"}`, key, summaries[i])}
		get.OutIDs, get.OutCtx, get.OutPaths = trace.OutputRefsPaths(get.Output)
		add := trace.Call{Tool: "mcp:telara_jira_add_comment", Args: map[string]string{"issue_key": key,
			"body": fmt.Sprintf("Status: In Progress. %s; next step is to confirm the fix in staging.", summaries[i])}}
		return []trace.Call{get, add}
	})
	rep := runOn(t, ss)
	seen := false
	for _, r := range rep.Routines {
		if !hasStep(r, "mcp:telara_jira_add_comment") {
			continue
		}
		seen = true
		if gi, ai := stepIndex(r, "mcp:telara_jira_get_issue"), stepIndex(r, "mcp:telara_jira_add_comment"); gi < 0 || gi > ai {
			t.Fatalf("the read that supplied the comment was dropped or moved:\n%s", dump(rep))
		}
		if in := inputBySuffix(r, "_body"); in == nil || in.Source == InputCaller {
			t.Fatalf("the comment body was composed from the issue, not given by the caller: %+v", r.Contract.Inputs)
		}
		if r.DraftStatus == DraftComplete {
			t.Fatalf("a draft that asks the caller for a composed comment is not complete:\n%s", dump(rep))
		}
	}
	if !seen {
		t.Fatalf("no routine found:\n%s", dump(rep))
	}
}

// C10: a read that checks state before a write stays before it, and a
// failing check stops the program.
func TestC10PreWriteCheckIsPreservedAndStopsOnFailure(t *testing.T) {
	ss := eps("roll", 6, func(i int) string { return fmt.Sprintf("roll the gateway to v2.%d", i) }, func(i int) []trace.Call {
		return []trace.Call{
			sh("kubectl --context minikube -n gateway get deploy gateway -o jsonpath={..image}"),
			sh(fmt.Sprintf("kubectl --context minikube -n gateway set image deploy/gateway gateway=registry.example/gw:v2.%d", i)),
			sh("kubectl --context minikube -n gateway rollout status deploy/gateway"),
		}
	})
	rep := runOn(t, ss)
	if len(rep.Routines) != 1 {
		t.Fatalf("want one routine:\n%s", dump(rep))
	}
	r := rep.Routines[0]
	if r.Contract.Effect != EffectWrites {
		t.Fatalf("set image writes: effect %q", r.Contract.Effect)
	}
	main := string(RoutineDraft(&r).Files["main.sh"])
	if g, s := strings.Index(main, "get deploy"), strings.Index(main, "set image"); g < 0 || s < 0 || g > s {
		t.Fatalf("the check must run before the write:\n%s", main)
	}
	if !strings.Contains(main, "set -eo pipefail") {
		t.Fatalf("a failing check (even inside a pipeline) must stop the program:\n%s", main)
	}
}

// C11: task bookkeeping around real work is not a procedure: a program that
// creates, checkpoints and completes a task without the work would claim
// work that never happened.
func TestC11BookkeepingIsNotCollapsedIntoAFakeCompletion(t *testing.T) {
	ss := eps("bk", 8, func(i int) string { return fmt.Sprintf("fix the flaky test in package p%d", i) }, func(i int) []trace.Call {
		create := trace.Call{Tool: "mcp:telara_task_create", Args: map[string]string{"name": fmt.Sprintf("fix flaky p%d", i)}}
		id := fmt.Sprintf("%08x-de01-4847-a933-187b18ef29%02d", 0x90991e90+i, i)
		create.Output = "Task created successfully.\n\n- **Task ID:** `" + id + "`"
		create.OutIDs, create.OutCtx, create.OutPaths = trace.OutputRefsPaths(create.Output)
		return []trace.Call{
			{Tool: "mcp:telara_task_list"},
			create,
			sh(fmt.Sprintf("go test ./p%d/... -run TestFlaky%d -count=5", i, i)),
			{Tool: "Edit", Args: map[string]string{"file_path": fmt.Sprintf("p%d/x_test.go", i), "old_string": "a", "new_string": fmt.Sprint(i)}},
			{Tool: "mcp:telara_task_checkpoint", Args: map[string]string{"task_id": id, "milestone": fmt.Sprintf("fixed p%d", i)}},
			{Tool: "mcp:telara_task_complete", Args: map[string]string{"task_id": id, "summary": fmt.Sprintf("p%d fixed", i)}},
		}
	})
	rep := runOn(t, ss)
	for _, r := range rep.Routines {
		if hasStep(r, "mcp:telara_task_complete") && r.Suitability == SuitUseful {
			t.Fatalf("a routine that completes a task was claimed useful without the work:\n%s", dump(rep))
		}
		only := true
		for _, s := range r.Steps {
			if !strings.HasPrefix(s.Label, "mcp:telara_task_") {
				only = false
			}
		}
		if only && r.SourceRole != RoleInfrastructure {
			t.Fatalf("task bookkeeping is infrastructure, got %q:\n%s", r.SourceRole, dump(rep))
		}
	}
}

// C12: tool search, describe and execute with different business actions
// are different outcomes; the recurring discovery calls alone are not one.
func TestC12DiscoveryThenDifferentActionsAreDistinct(t *testing.T) {
	actions := []string{"jira_get_issue", "gitlab_list_pipelines", "slack_send_message"}
	ss := eps("act", 12, func(i int) string { return fmt.Sprintf("use telara to run %s for item %d", actions[i%3], 500+i) }, func(i int) []trace.Call {
		a := actions[i%3]
		return []trace.Call{
			{Tool: "mcp:telara_tool_search", Args: map[string]string{"query": strings.ReplaceAll(a, "_", " ")}},
			{Tool: "mcp:telara_tool_describe", Args: map[string]string{"name": a}},
			{Tool: "mcp:telara_execute_action", Args: map[string]string{"action": a, "params": fmt.Sprintf(`{"id":"%d"}`, 500+i)}, RawArgs: map[string]bool{"params": true}},
		}
	})
	rep := runOn(t, ss)
	for _, r := range rep.Routines {
		if !hasStep(r, "mcp:telara_execute_action") {
			if r.Suitability == SuitUseful {
				t.Fatalf("tool discovery alone claimed as a useful procedure:\n%s", dump(rep))
			}
			continue
		}
		if in := inputBySuffix(r, "_action"); in != nil {
			t.Fatalf("the business action became an input (%+v):\n%s", in, dump(rep))
		}
		acts := map[int]bool{}
		for s := range sessionsOf(r) {
			var n int
			fmt.Sscanf(strings.TrimPrefix(s, "act"), "%d", &n)
			acts[n%3] = true
		}
		if len(acts) > 1 {
			t.Fatalf("one routine runs different business actions:\n%s", dump(rep))
		}
	}
}

// C13: routines with the same labels in a different order or multiplicity
// are not duplicates.
func TestC13OrderAndMultiplicityAreNotDeduplicatedAway(t *testing.T) {
	mk := func(id string, labels ...string) Routine {
		r := Routine{ID: id, Kind: "user", Family: "fam_1", Decision: "primitive", Suitability: SuitUseful}
		for _, l := range labels {
			r.Steps = append(r.Steps, StepTemplate{Label: l})
		}
		return r
	}
	rs := []Routine{
		mk("a", "sh:git status", "sh:git push"),
		mk("b", "sh:git push", "sh:git status"),
		mk("c", "sh:git status", "sh:git push", "sh:git push"),
		mk("d", "sh:git status", "sh:git push"),
	}
	n := mergeDuplicates(rs)
	if rs[1].MergedInto != "" || rs[2].MergedInto != "" {
		t.Fatalf("a different order or multiplicity was merged: %+v", rs)
	}
	if n != 1 || rs[3].MergedInto != "a" {
		t.Fatalf("an identical routine is a duplicate: merged %d, %+v", n, rs)
	}
}

// C14: a recorded compound line (conditional, redirect, pipeline, cd) is
// replayed as that line, with only the varying word as an input.
func TestC14ConditionalsAndRedirectsAreReplayedFaithfully(t *testing.T) {
	ss := eps("cond", 6, func(i int) string { return fmt.Sprintf("run the tests for svc%d", i) }, func(i int) []trace.Call {
		return []trace.Call{
			sh(fmt.Sprintf("cd services/svc%d && if [ -f go.mod ]; then go test ./... 2>&1 | tail -5; else echo no-module; fi > /dev/null || echo failed", i)),
			sh("git status --short"),
		}
	})
	rep := runOn(t, ss)
	main := string(RoutineDraft(&rep.Routines[0]).Files["main.sh"])
	if !strings.Contains(main, `cd "${1}" && if [ -f go.mod ]; then go test ./... 2>&1 | tail -5; else echo no-module; fi > /dev/null || echo failed`) {
		t.Fatalf("the recorded structure was not preserved:\n%s", main)
	}
}

// C15: a resumed copy is not a second task; an approval given between steps
// is recorded and does not carry over to a new run.
func TestC15ResumedCopiesAndApprovals(t *testing.T) {
	ss := eps("br", 6, func(i int) string { return fmt.Sprintf("delete the merged branches in repo%d", i) }, func(i int) []trace.Call { return nil })
	for i := range ss {
		s := &ss[i]
		list := trace.Call{ID: fmt.Sprintf("toolu_%d_a", i), Tool: "shell", Command: fmt.Sprintf("git -C repo%d branch --merged main", i), Time: s.Start, Outcome: trace.OutcomeOK}
		s.Calls = append(s.Calls, list)
		s.AddRequest("yes") // the user approves the deletion shown by the list
		del := trace.Call{ID: fmt.Sprintf("toolu_%d_b", i), Tool: "shell", Command: fmt.Sprintf("git -C repo%d branch -d feat-%d", i, i), Time: s.Start, Outcome: trace.OutcomeOK, Request: s.Request()}
		s.Calls = append(s.Calls, del)
	}
	resumed := ss[0]
	resumed.ID = "brcopy"
	resumed.Calls = append([]trace.Call(nil), ss[0].Calls...)
	rep := runOn(t, append(ss, resumed))
	if len(rep.Routines) != 1 {
		t.Fatalf("want one routine:\n%s", dump(rep))
	}
	r := rep.Routines[0]
	if sessionsOf(r)["brcopy"] || r.Sessions != 6 {
		t.Fatalf("the resumed copy counted as another task: sessions %d %v", r.Sessions, sessionsOf(r))
	}
	if r.Contract.Approvals == 0 {
		t.Fatalf("the approval between listing and deleting was not recorded: %+v", r.Contract)
	}
	if r.DraftStatus == DraftComplete {
		t.Fatalf("a recorded approval cannot be replayed by a program: draft %q blockers %v", r.DraftStatus, r.Blockers)
	}
}

// C16: text injected by a harness or editor is not a person's task.
func TestC16HarnessPromptsAreNotTasks(t *testing.T) {
	// An in-app browser context block with no request in it (the Codex
	// wrapper carries the user's words under "## My request for Codex:";
	// without that section it is context only). A push-script prompt that
	// asks for real work is NOT used here: blind labels treat it as a
	// machine-sent task (scheduled), not harness text.
	commit := eps("cm", 6, func(i int) string {
		return "# In app browser:\n- The user has the in-app browser open.\n- Current URL: http://localhost:3010/preview\n- Open tabs: 2"
	}, func(i int) []trace.Call {
		return []trace.Call{sh("git diff --stat"), sh(fmt.Sprintf("git log --oneline -%d", 3+i))}
	})
	agents := eps("ag", 6, func(i int) string {
		return "# AGENTS.md instructions for /Users/dev/repo\n\n<INSTRUCTIONS>\n# Rules\n- never push to main\n</INSTRUCTIONS>"
	}, func(i int) []trace.Call {
		return []trace.Call{sh("git status --short"), sh(fmt.Sprintf("git log --oneline -%d", 2+i))}
	})
	rep := runOn(t, commit, agents)
	if len(rep.Routines) == 0 {
		t.Fatal("no routine at all: the harness requests should still be reported, as harness")
	}
	for _, r := range rep.Routines {
		if r.SourceRole != RoleHarness || r.Suitability != SuitInvalid {
			t.Fatalf("role %q suit %q:\n%s", r.SourceRole, r.Suitability, dump(rep))
		}
	}
}

// C17: work already scheduled or covered by a skill names that baseline;
// it is neither rejected for it nor counted as new user automation.
func TestC17ScheduledAndSkillBaselines(t *testing.T) {
	sched := eps("sc", 6, func(i int) string { return "Automation: nightly repo health\nAutomation ID: auto-1\nCheck the repo." }, func(i int) []trace.Call {
		return []trace.Call{sh("git fetch --all"), sh(fmt.Sprintf("git log --oneline -%d", 5+i))}
	})
	skill := eps("sk", 6, func(i int) string { return fmt.Sprintf("release version 1.%d of the cli", i) }, func(i int) []trace.Call {
		return []trace.Call{
			{Tool: "Skill", Args: map[string]string{"skill": "cli-release"}},
			sh(fmt.Sprintf("git tag v1.%d", i)),
			sh(fmt.Sprintf("git push origin v1.%d", i)),
		}
	})
	rep := runOn(t, sched, skill)
	var gotSched, gotSkill bool
	for _, r := range rep.Routines {
		p := prefixes(r)
		if p["sc"] {
			gotSched = true
			if r.SourceRole != RoleScheduled || r.Baseline != "scheduled_automation" {
				t.Fatalf("scheduled work: role %q baseline %q", r.SourceRole, r.Baseline)
			}
			for _, why := range r.Reasons {
				if strings.Contains(why, "scheduled") || strings.Contains(why, "skill") || strings.Contains(why, "baseline") {
					t.Fatalf("scheduled work is a baseline, never itself a reason: %v", r.Reasons)
				}
			}
		}
		if p["sk"] {
			gotSkill = true
			if r.Baseline != "skill:cli-release" {
				t.Fatalf("skill baseline %q", r.Baseline)
			}
		}
	}
	if !gotSched || !gotSkill {
		t.Fatalf("routines missing:\n%s", dump(rep))
	}
	if n := rep.Funnel.ByRole[RoleUser][SuitUseful]; n > 1 {
		t.Fatalf("scheduled work counted among new user procedures: %v", rep.Funnel.ByRole)
	}
}

// C18: no recorded result, or only a tool's own success, is never verified.
func TestC18OutcomesAreNotOverstated(t *testing.T) {
	unknown := eps("un", 6, func(i int) string { return fmt.Sprintf("show pipeline %d", 700+i) }, func(i int) []trace.Call {
		return []trace.Call{
			{Tool: "mcp:gitlab_get_pipeline", Args: map[string]string{"pipeline_id": fmt.Sprint(700 + i)}, Outcome: trace.OutcomeUnknown},
			{Tool: "mcp:gitlab_list_jobs", Args: map[string]string{"pipeline_id": fmt.Sprint(700 + i)}, Outcome: trace.OutcomeUnknown},
		}
	})
	for i := range unknown {
		for j := range unknown[i].Calls {
			unknown[i].Calls[j].Outcome = trace.OutcomeUnknown
		}
	}
	rep := runOn(t, unknown)
	if len(rep.Routines) != 1 || rep.Routines[0].OutcomeEvidence != OutcomeEvUnknown {
		t.Fatalf("no recorded result must stay unknown:\n%s", dump(rep))
	}
	ok := runOn(t, eps("ok", 6, func(i int) string { return fmt.Sprintf("show pipeline %d", 800+i) }, func(i int) []trace.Call {
		return []trace.Call{
			{Tool: "mcp:gitlab_get_pipeline", Args: map[string]string{"pipeline_id": fmt.Sprint(800 + i)}},
			{Tool: "mcp:gitlab_list_jobs", Args: map[string]string{"pipeline_id": fmt.Sprint(800 + i)}},
		}
	}))
	if ok.Routines[0].OutcomeEvidence != OutcomeToolOK {
		t.Fatalf("tool success is observed, not verified: %q", ok.Routines[0].OutcomeEvidence)
	}
}

// C19: an id taken from a JSON result is bound by its path, and the
// program fails, calling nothing further, when it is missing.
func TestC19JSONDerivedIDIsATypedBinding(t *testing.T) {
	ss := eps("js", 8, func(i int) string { return "reply to the newest intro email" }, func(i int) []trace.Call {
		thread := fmt.Sprintf("18c%013x", 0xabc0+i)
		other := fmt.Sprintf("18d%013x", 0xabc0+i)
		search := trace.Call{Tool: "mcp:gmail_search_emails", Args: map[string]string{"query": "intro"},
			Output: fmt.Sprintf(`{"threads":[{"id":"%s","subject":"a"},{"id":"%s","subject":"b"}]}`, thread, other)}
		search.OutIDs, search.OutCtx, search.OutPaths = trace.OutputRefsPaths(search.Output)
		return []trace.Call{search, {Tool: "mcp:gmail_read_email_thread", Args: map[string]string{"thread_id": thread}}}
	})
	rep := runOn(t, ss)
	d := RoutineDraft(&rep.Routines[0])
	main := string(d.Files["main.sh"])
	if !strings.Contains(main, ".threads[0].id") {
		t.Fatalf("a JSON result is read by its path, not by text around the value:\n%s", main)
	}
	out, calls, err := runDraft(t, d, map[string]string{"gmail_search_emails": `{"threads":[{"id":"18cfeed","subject":"x"}]}`})
	if err != nil || len(calls) != 2 || !strings.Contains(calls[1], `"18cfeed"`) {
		t.Fatalf("normal run: %v calls %v\n%s", err, calls, out)
	}
	_, calls, err = runDraft(t, d, map[string]string{"gmail_search_emails": `{"threads":[]}`})
	if err == nil || len(calls) != 1 {
		t.Fatalf("a missing id must fail before the next call: err %v calls %v", err, calls)
	}
}

// C19b: when runs picked different results, the choice was the agent's: no
// path is invented and the value is not bound.
func TestC19VaryingSelectionIsNotBound(t *testing.T) {
	ss := eps("sel", 8, func(i int) string { return "reply to the right intro email" }, func(i int) []trace.Call {
		a, b := fmt.Sprintf("18c%013x", 0xabc0+i), fmt.Sprintf("18d%013x", 0xabc0+i)
		search := trace.Call{Tool: "mcp:gmail_search_emails", Args: map[string]string{"query": "intro"},
			Output: fmt.Sprintf(`{"threads":[{"id":"%s"},{"id":"%s"}]}`, a, b)}
		search.OutIDs, search.OutCtx, search.OutPaths = trace.OutputRefsPaths(search.Output)
		pick := a
		if i%2 == 1 {
			pick = b
		}
		return []trace.Call{search, {Tool: "mcp:gmail_read_email_thread", Args: map[string]string{"thread_id": pick}}}
	})
	rep := runOn(t, ss)
	r := rep.Routines[0]
	if strings.Contains(string(RoutineDraft(&r).Files["main.sh"]), "threads[") || r.DraftStatus == DraftComplete {
		t.Fatalf("a selection that varied was bound to one path: %q\n%s", r.DraftStatus, RoutineDraft(&r).Files["main.sh"])
	}
}

// C20: an id taken from text fails closed when its anchor is missing,
// repeated, or appears inside quoted text, and never runs what it extracts.
func TestC20TextDerivedIDFailsClosed(t *testing.T) {
	ss := eps("tx", 8, func(i int) string { return fmt.Sprintf("track the rollout of release %d", i) }, func(i int) []trace.Call {
		id := fmt.Sprintf("%08x-de01-4847-a933-187b18ef2985", 0x90991e90+i)
		create := trace.Call{Tool: "mcp:records_create", Args: map[string]string{"name": fmt.Sprintf("release %d", i)},
			Output: "Created successfully.\n\n- **Record ID:** `" + id + "`\n- **Name:** release"}
		create.OutIDs, create.OutCtx, create.OutPaths = trace.OutputRefsPaths(create.Output)
		return []trace.Call{create, {Tool: "mcp:records_watch", Args: map[string]string{"record_id": id}}}
	})
	rep := runOn(t, ss)
	d := RoutineDraft(&rep.Routines[0])
	good := "Created.\n\n- **Record ID:** `11111111-de01-4847-a933-187b18ef2985`\n"
	if _, calls, err := runDraft(t, d, map[string]string{"records_create": good}, "release 9"); err != nil || len(calls) != 2 || !strings.Contains(calls[1], "11111111-de01") {
		t.Fatalf("normal run: %v %v\n%s", err, calls, d.Files["main.sh"])
	}
	for name, out := range map[string]string{
		"missing":   "Created.\n\n- **Name:** release\n",
		"duplicate": good + "- **Record ID:** `22222222-de01-4847-a933-187b18ef2985`\n",
		"quoted":    "Note: \"- **Record ID:** `33333333-de01-4847-a933-187b18ef2985`\" was the old one\n" + good,
		"injection": "- **Record ID:** `$(touch${IFS}pwned)`\n",
	} {
		dir, _, calls, err := runDraftDir(t, d, map[string]string{"records_create": out}, "release 9")
		if err == nil || len(calls) != 1 {
			t.Errorf("%s: must fail before the next call: err %v calls %v", name, err, calls)
		}
		if _, statErr := os.Stat(filepath.Join(dir, "pwned")); statErr == nil {
			t.Errorf("%s: the extracted text was executed", name)
		}
	}
}

// C21: credentials, fixed, varying or nested, never reach any artifact.
// Covered by TestDraftNeverWritesCredentials and TestLeftoverCredentialBlocksEverything;
// this adds a nested varying secret in a JSON argument of a routine that is
// otherwise complete.
func TestC21NestedVaryingCredentialsNeverLeak(t *testing.T) {
	ss := eps("cr", 6, func(i int) string { return fmt.Sprintf("store the config for app%d", i) }, func(i int) []trace.Call {
		return []trace.Call{
			{Tool: "mcp:vault_read", Args: map[string]string{"path": fmt.Sprintf("secret/app%d", i)}},
			{Tool: "mcp:vault_write", Args: map[string]string{"path": fmt.Sprintf("secret/app%d", i), "data": fmt.Sprintf(`{"db":{"password":"pw-%d-abcdefgh","user":"svc"}}`, i)}, RawArgs: map[string]bool{"data": true}},
		}
	})
	rep := runOn(t, ss)
	for _, r := range rep.Routines {
		for name, body := range RoutineDraft(&r).Files {
			if strings.Contains(string(body), "pw-") {
				t.Fatalf("%s leaks a recorded credential", name)
			}
		}
		b, _ := json.Marshal(r)
		if strings.Contains(string(b), "pw-") {
			t.Fatal("the report leaks a recorded credential")
		}
	}
}

// C22: the repaired reader and drafting behaviors stay repaired. These are
// asserted by TestCursorMCPEnvelopeIsUnwrapped, TestTenOrMoreArgumentsAreBraced,
// TestDashUIsACredentialOnlyForUserFlagPrograms and TestCopiedCallsAreReadOnce;
// this case checks they still hold together in one run.
func TestC22RepairedBehaviorsHoldTogether(t *testing.T) {
	c := history.CursorCall(history.CursorRow{Name: "mcp-telara-telara_task_create", Args: `{"name":"x","args":{"title":"t"},"toolCallId":"1"}`})
	if c.Args["title"] != "t" || len(c.Args) != 1 {
		t.Fatalf("cursor envelope: %v", c.Args)
	}
	if redact.SensitiveSlot("sh:date", trace.Slot{Key: "-u=", Value: "+%H:%M"}) {
		t.Fatal("date -u is not a credential")
	}
	ss := []trace.Session{{Client: "c", ID: "a", Calls: []trace.Call{{ID: "1"}}}, {Client: "c", ID: "b", Calls: []trace.Call{{ID: "1"}, {ID: "2"}}}}
	trace.DropCopiedCalls(ss)
	if len(ss[1].Calls) != 1 {
		t.Fatal("copied call read twice")
	}
}

// C23: the same frozen input read in a different order gives the same
// report: the same routines, ids, decisions and sources.
func TestC23ReportDoesNotDependOnReadOrder(t *testing.T) {
	var all []trace.Session
	all = append(all, requestCorpus()...)
	all = append(all, explore("ex", 9)...)
	all = append(all, eps("pipe", 6, func(i int) string { return fmt.Sprintf("show me the jobs of pipeline %d", 100200+i) }, func(i int) []trace.Call {
		return []trace.Call{{Tool: "mcp:gitlab_get_pipeline", Args: map[string]string{"pipeline_id": fmt.Sprint(100200 + i)}}, {Tool: "mcp:gitlab_list_jobs", Args: map[string]string{"pipeline_id": fmt.Sprint(100200 + i)}}}
	})...)
	summary := func(ss []trace.Session) string {
		rep := runOn(t, ss)
		var lines []string
		for _, r := range rep.Routines {
			var srcs []string
			for _, s := range r.Sources {
				srcs = append(srcs, fmt.Sprintf("%s/%d", s.Session, s.Request))
			}
			sort.Strings(srcs)
			lines = append(lines, fmt.Sprintf("%s %s %s %s %v", r.ID, r.Decision, r.Suitability, r.MergedInto, srcs))
		}
		sort.Strings(lines)
		return strings.Join(lines, "\n")
	}
	want := summary(all)
	rev := make([]trace.Session, len(all))
	for i := range all {
		rev[len(all)-1-i] = all[i]
	}
	if got := summary(rev); got != want {
		t.Fatalf("reversed input order changed the report:\nforward:\n%s\nreversed:\n%s", want, got)
	}
}

// C24: grouping and abstention must not hide a known useful procedure
// among unrelated work (a recall guard: abstaining on everything fails).
func TestC24KnownUsefulProcedureIsFoundAmongNoise(t *testing.T) {
	diff := eps("dc", 6, func(i int) string {
		return fmt.Sprintf("did we add tests without service changes between %07x and %07x?", 0xa1b2c00+i, 0xd4e5f00+i)
	}, func(i int) []trace.Call {
		a, b := fmt.Sprintf("%07x", 0xa1b2c00+i), fmt.Sprintf("%07x", 0xd4e5f00+i)
		return []trace.Call{sh("git diff --name-only " + a + " " + b + " -- services"), sh("git diff --name-only " + a + " " + b + " -- tests")}
	})
	rep := runOn(t, requestCorpus(), explore("ex", 9), diff)
	for _, r := range rep.Routines {
		if prefixes(r)["dc"] && r.Suitability == SuitUseful && r.MergedInto == "" {
			return
		}
	}
	t.Fatalf("the diff check was not recommended:\n%s", dump(rep))
}

// A useful bounded procedure is recommended ahead of an unsuitable pattern,
// and the unsuitable one is not recommended at all.
func TestProceduresRankAboveInvestigations(t *testing.T) {
	diff := eps("dc", 6, func(i int) string {
		return fmt.Sprintf("did we add tests without service changes between %07x and %07x?", 0xa1b2c00+i, 0xd4e5f00+i)
	}, func(i int) []trace.Call {
		a, b := fmt.Sprintf("%07x", 0xa1b2c00+i), fmt.Sprintf("%07x", 0xd4e5f00+i)
		return []trace.Call{sh("git diff --name-only " + a + " " + b + " -- services"), sh("git diff --name-only " + a + " " + b + " -- tests")}
	})
	rep := runOn(t, explore("ex", 9), diff)
	useful, investigation := -1, -1
	for i, r := range rep.Routines {
		switch {
		case prefixes(r)["dc"]:
			useful = i
			if r.Suitability != SuitUseful {
				t.Fatalf("the diff check is useful: %q\n%s", r.Suitability, dump(rep))
			}
		case prefixes(r)["ex"]:
			if investigation < 0 {
				investigation = i
			}
			if r.Suitability == SuitUseful {
				t.Fatalf("the exploration is not useful:\n%s", dump(rep))
			}
		}
	}
	if useful < 0 || (investigation >= 0 && investigation < useful) {
		t.Fatalf("useful at %d, investigation at %d:\n%s", useful, investigation, dump(rep))
	}
}

// realJQ is the host's jq, used by the stand-ins above.
func realJQ(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq not available")
	}
	return p
}
