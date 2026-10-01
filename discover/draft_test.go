package discover

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/pack"

	"gitlab.com/telara-labs/tap-runtime/discover/model"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"

	"gitlab.com/telara-labs/tap-runtime/contract/manifest"
)

func runAndFind(t *testing.T, ss []trace.Session, labels string) (*model.Report, int) {
	t.Helper()
	o := DefaultOptions()
	o.Patterns = true
	o.Readers = []trace.Reader{fakeReader{sessions: ss}}
	rep, err := Run(o)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range rep.Candidates {
		if c.Qualified && model.LabelsOf(c) == labels {
			return rep, i
		}
	}
	t.Fatalf("no qualified candidate %q", labels)
	return nil, 0
}

func TestDraftReplaysThePlantedProcedure(t *testing.T) {
	rep, i := runAndFind(t, plantedCorpus(), "sh:make build → sh:go test → sh:git push")
	d, err := ReportDraft(rep, i, model.DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sh := string(d.Files["main.sh"])
	for _, want := range []string{"make build\n", "go test ./... -count=1\n", `git push origin "${1}"` + "\n"} {
		if !strings.Contains(sh, want) {
			t.Errorf("main.sh lacks %q:\n%s", want, sh)
		}
	}
	if len(d.Inputs) != 1 || d.Inputs[0].Type != trace.SlotWord || !strings.HasPrefix(d.Inputs[0].Example, "feature-") {
		t.Fatalf("inputs = %+v: the branch is the only thing that varied", d.Inputs)
	}
	m, err := manifest.Parse(d.Files["primitive.yaml"])
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range m.Commands {
		got[c.Command] = strings.Join(c.Args, " ") + " / " + c.Effect
	}
	want := map[string]string{"make": "build * / write", "go": "test * / write", "git": "push * / write"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("command %s = %q, want %q", k, got[k], v)
		}
	}
	if len(d.Problems) != 0 {
		t.Fatalf("a commands-only draft should pass the publish checks: %v", d.Problems)
	}
}

func TestDraftReadOnlyIsTheUsersCall(t *testing.T) {
	rep, i := runAndFind(t, plantedCorpus(), "sh:make build → sh:go test → sh:git push")
	d, err := ReportDraft(rep, i, model.DraftOptions{ReadOnly: map[int]bool{2: true}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Steps[0].Effect != "write" || d.Steps[1].Effect != "read" || d.Steps[2].Effect != "write" {
		t.Fatalf("effects = %s %s %s", d.Steps[0].Effect, d.Steps[1].Effect, d.Steps[2].Effect)
	}
}

// toolCorpus: 20 sessions pick up a ticket through two MCP tools, passing the
// same ticket id to both, among random noise.
func toolCorpus() []trace.Session {
	var out []trace.Session
	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	noise := []string{"ls", "pwd", "date", "whoami", "uptime", "df -h", "id", "hostname"}
	for i := 0; i < 60; i++ {
		s := trace.Session{Client: "fake", ID: fmt.Sprintf("t%02d", i), Start: t0.AddDate(0, 0, 7*(i%20))}
		for j := 0; j < 20; j++ {
			s.Calls = append(s.Calls, trace.Call{Tool: "shell", Command: noise[(i*7+j*3)%len(noise)] + fmt.Sprintf(" %d", j)})
			if i < 20 && j == 8 {
				id := fmt.Sprintf("TENG-%d", 3000+i)
				s.Calls = append(s.Calls,
					trace.Call{Tool: "mcp:telara_task_create", Args: map[string]string{"goal": fmt.Sprintf("work on %s part %d", id, i), "ticket": id}},
					trace.Call{Tool: "mcp:telara_jira_transition_issue", Args: map[string]string{"issue_key": id, "transition_id": "11"}})
			}
		}
		out = append(out, s)
	}
	return out
}

func TestDraftMCPToolsCarryContractsThatPassPublishChecks(t *testing.T) {
	rep, i := runAndFind(t, toolCorpus(), "mcp:telara_task_create → mcp:telara_jira_transition_issue")
	d, err := ReportDraft(rep, i, model.DraftOptions{Publisher: "dev.example"})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Problems) != 0 {
		t.Fatalf("publish problems: %v\n%s", d.Problems, d.Files["primitive.yaml"])
	}
	// The ticket id went to both tools: one input, not two.
	var names []string
	for _, in := range d.Inputs {
		names = append(names, in.Name)
	}
	if len(d.Inputs) != 2 {
		t.Fatalf("inputs = %v, want the goal and one shared ticket id", names)
	}
	sh := string(d.Files["main.sh"])
	if !strings.Contains(sh, `"transition_id":"11"`) {
		t.Errorf("a value that never varied must be written in:\n%s", sh)
	}
	if strings.Count(sh, "tap call ") != 2 {
		t.Errorf("want two tool calls:\n%s", sh)
	}
	m, _ := manifest.Parse(d.Files["primitive.yaml"])
	if len(m.Tools) != 2 || m.Tools[0].Capability != "dev.example/telara.task.create@1" {
		t.Fatalf("tools = %+v", m.Tools)
	}
}

func TestDraftKeepsRecordedArgumentTypes(t *testing.T) {
	ss := toolCorpus()
	for i := range ss {
		for j := range ss[i].Calls {
			if ss[i].Calls[j].Tool == "mcp:telara_jira_transition_issue" {
				// Recorded as {"issue_key": "...", "transition_id": "11", "notify": false}
				ss[i].Calls[j].Args["notify"] = "false"
				ss[i].Calls[j].RawArgs = map[string]bool{"notify": true}
			}
		}
	}
	rep, i := runAndFind(t, ss, "mcp:telara_task_create → mcp:telara_jira_transition_issue")
	d, err := ReportDraft(rep, i, model.DraftOptions{Publisher: "dev.example"})
	if err != nil {
		t.Fatal(err)
	}
	sh := string(d.Files["main.sh"])
	if !strings.Contains(sh, `"transition_id":"11"`) || !strings.Contains(sh, `"notify":false`) {
		t.Fatalf("a string must stay a string and a boolean a boolean:\n%s", sh)
	}
	if len(d.Problems) != 0 {
		t.Fatalf("problems: %v", d.Problems)
	}
}

func TestDraftPatchIsAHumanStepNotAGuess(t *testing.T) {
	// Random noise, so sessions differ (identical step sequences count once).
	rng := rand.New(rand.NewSource(11))
	noise := []string{"ls", "pwd", "date", "id", "uptime", "hostname", "df", "whoami"}
	var ss []trace.Session
	for i := 0; i < 40; i++ {
		s := trace.Session{Client: "fake", ID: fmt.Sprintf("p%02d", i)}
		for j := 0; j < 15; j++ {
			s.Calls = append(s.Calls, trace.Call{Tool: "shell", Command: noise[rng.Intn(len(noise))] + fmt.Sprint(" ", j)})
			if i < 15 && j == 5 {
				s.Calls = append(s.Calls,
					trace.Call{Tool: "shell", Command: "tail -n 20 activity.log"},
					trace.Call{Tool: "apply_patch", Args: map[string]string{"input": fmt.Sprintf("*** Begin Patch\n*** Update File: notes/activity.log\n@@\n+entry %d\n*** End Patch", i)}},
					trace.Call{Tool: "shell", Command: "git commit -m 'log " + fmt.Sprint(i) + "'"})
			}
		}
		ss = append(ss, s)
	}
	rep, i := runAndFind(t, ss, "sh:tail → patch:update → sh:git commit")
	d, err := ReportDraft(rep, i, model.DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if d.HumanSteps != 1 || d.Steps[1].Kind != KindHuman {
		t.Fatalf("steps = %+v", d.Steps)
	}
	m, _ := manifest.Parse(d.Files["primitive.yaml"])
	if len(m.Files) != 1 || m.Files[0].Path != "notes/activity.log" || m.Files[0].Access != "write" {
		t.Fatalf("files = %+v: the file every run edited must be declared", m.Files)
	}
	if strings.Contains(string(d.Files["main.sh"]), "entry ") {
		t.Fatal("the patch body differed every run and must not be replayed")
	}
}

func TestDraftBrowserKeepsObjectsAndDropsVariableNames(t *testing.T) {
	var ss []trace.Session
	rng := rand.New(rand.NewSource(3))
	noise := []string{"ls", "pwd", "date", "id", "uptime", "hostname"}
	for i := 0; i < 40; i++ {
		s := trace.Session{Client: "fake", ID: fmt.Sprintf("b%02d", i)}
		for j := 0; j < 15; j++ {
			s.Calls = append(s.Calls, trace.Call{Tool: "shell", Command: noise[rng.Intn(len(noise))] + fmt.Sprint(" ", j)})
			if i < 15 && j == 4 {
				tab := fmt.Sprintf("tab%d", i) // a different variable name every run
				code := fmt.Sprintf("await chrome.nameSession('Weekly check'); await %s.goto(\"https://partner.example.com/p/%d\"); await %s.playwright.evaluate(() => document.querySelector('h1').innerText);", tab, i, tab)
				s.Calls = append(s.Calls, trace.Call{Tool: "mcp:js", Args: map[string]string{"code": code}})
			}
		}
		ss = append(ss, s)
	}
	rep, i := runAndFind(t, ss, "js:nameSession → js:goto → js:playwright.evaluate")
	d, err := ReportDraft(rep, i, model.DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sh := string(d.Files["main.sh"])
	for _, want := range []string{"await chrome.nameSession(", "await tab.goto(", "() => document.querySelector(", `'\''h1'\''`} {
		if !strings.Contains(sh, want) {
			t.Errorf("main.sh lacks %q:\n%s", want, sh)
		}
	}
	if len(d.Inputs) != 1 || d.Inputs[0].Type != trace.SlotURL {
		t.Fatalf("inputs = %+v: only the page URL varied (the tab's variable name is not an input)", d.Inputs)
	}
	if len(d.Problems) != 0 {
		t.Fatalf("problems: %v", d.Problems)
	}
}

func TestDraftArgumentOrderDoesNotMakeFlagsInputs(t *testing.T) {
	var ss []trace.Session
	rng := rand.New(rand.NewSource(8))
	noise := []string{"ls", "pwd", "date", "id", "uptime", "hostname"}
	for i := 0; i < 40; i++ {
		s := trace.Session{Client: "fake", ID: fmt.Sprintf("g%02d", i)}
		for j := 0; j < 15; j++ {
			s.Calls = append(s.Calls, trace.Call{Tool: "shell", Command: noise[rng.Intn(len(noise))] + fmt.Sprint(" ", j)})
			if i < 16 && j == 6 {
				pkg := fmt.Sprintf("./internal/p%d/...", i)
				cmd := "go test -count=1 -run TestX " + pkg
				if i%2 == 1 {
					cmd = "go test -run TestX -count=1 " + pkg // same command, other order
				}
				s.Calls = append(s.Calls, trace.Call{Tool: "shell", Command: "gofmt -w " + pkg}, trace.Call{Tool: "shell", Command: cmd})
			}
		}
		ss = append(ss, s)
	}
	rep, i := runAndFind(t, ss, "sh:gofmt → sh:go test")
	d, err := ReportDraft(rep, i, model.DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Inputs) != 1 || d.Inputs[0].Type != trace.SlotPath {
		t.Fatalf("inputs = %+v: only the package varied, and gofmt and go test share it", d.Inputs)
	}
	sh := string(d.Files["main.sh"])
	if !strings.Contains(sh, `gofmt -w "${1}"`) || !strings.Contains(sh, `-count=1`) || !strings.Contains(sh, `-run TestX`) || !strings.Contains(sh, `"${1}"`) {
		t.Fatalf("main.sh:\n%s", sh)
	}
}

func TestRoutinesAreOnePerTask(t *testing.T) {
	o := DefaultOptions()
	o.Patterns = true
	o.Readers = []trace.Reader{fakeReader{sessions: plantedCorpus()}}
	rep, err := Run(o)
	if err != nil {
		t.Fatal(err)
	}
	rs := pack.ReportPatternRoutines(rep)
	if len(rs) != 1 || model.LabelsOf(rep.Candidates[rs[0]]) != "sh:make build → sh:go test → sh:git push" {
		var got []string
		for _, i := range rs {
			got = append(got, model.LabelsOf(rep.Candidates[i]))
		}
		t.Fatalf("routines = %q: one task, represented by its full procedure", got)
	}
}
