package discover

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// requestCorpus: 30 sessions over 10 weeks. Each carries a few requests:
//   - "move TENG-<n> to done": transition then comment on that ticket (the
//     ticket id is in the request: a primitive)
//   - "deploy the gateway": set image to a tag the agent chose, then watch
//     the rollout (the tag is never in the request; the agent chooses it and a primitive takes it as an input)
//   - noise requests of random reads.
func requestCorpus() []Session {
	rng := rand.New(rand.NewSource(21))
	noise := []string{"ls", "pwd", "date", "uptime", "hostname", "id", "df -h", "du -sh ."}
	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	var out []Session
	for i := 0; i < 30; i++ {
		s := Session{Client: "fake", ID: fmt.Sprintf("r%02d", i), Start: t0.AddDate(0, 0, 2*i)}
		call := func(c Call) {
			c.Request = len(s.Requests) - 1
			c.Time = s.Start
			s.Calls = append(s.Calls, c)
		}
		s.addRequest("look around the repo")
		for j := 0; j < 4; j++ {
			call(Call{Tool: "shell", Command: noise[rng.Intn(len(noise))] + fmt.Sprint(" ", j)})
		}
		if i%3 != 2 {
			ticket := fmt.Sprintf("TENG-%d", 3100+i)
			s.addRequest("please move " + ticket + " to done and say it shipped")
			call(Call{Tool: "mcp:telara_jira_transition_issue", Args: map[string]string{"issue_key": ticket, "transition_id": "21"}})
			call(Call{Tool: "mcp:telara_jira_add_comment", Args: map[string]string{"issue_key": ticket, "body": "shipped"}})
		}
		if i%3 != 0 {
			s.addRequest("deploy the gateway to minikube")
			tag := fmt.Sprintf("teng%d-v%d", 3000+i, rng.Intn(9))
			call(Call{Tool: "shell", Command: "kubectl --context minikube -n telara-middleware set image deploy/gateway gateway=telara/gateway:" + tag})
			call(Call{Tool: "shell", Command: "kubectl --context minikube -n telara-middleware rollout status deploy/gateway"})
		}
		out = append(out, s)
	}
	return out
}

func TestRecurringRequestsBecomePrimitives(t *testing.T) {
	o := DefaultOptions()
	o.Readers = []Reader{fakeReader{sessions: requestCorpus()}}
	rep, err := Run(o)
	if err != nil {
		t.Fatal(err)
	}
	f := rep.Funnel
	if f.Sessions != 30 || f.Routines < 2 {
		t.Fatalf("funnel = %+v", f)
	}
	var jira, deploy *Routine
	for i := range rep.Routines {
		l := labelsOf(rep.Routines[i].Candidate)
		switch {
		case strings.Contains(l, "jira_transition_issue"):
			jira = &rep.Routines[i]
		case strings.Contains(l, "kubectl set"):
			deploy = &rep.Routines[i]
		}
	}
	if jira == nil || jira.Failed != "" {
		t.Fatalf("the ticket request must be a primitive: %+v", jira)
	}
	// The deploy's image tag was never in the request: the agent chose it,
	// and a primitive takes it as an input. That does not remove it.
	if deploy == nil || deploy.Failed != "" {
		t.Fatalf("the deploy request must be a primitive too: %+v", deploy)
	}
	if f.Primitives < 2 {
		t.Fatalf("funnel = %+v", f)
	}
	for _, in := range deploy.Inputs {
		if in.Type == SlotPath && in.Explained != 0 {
			t.Errorf("the image tag never appeared in a request, so explained must be 0: %+v", in)
		}
	}
	sh := string(jira.Draft().Files["main.sh"])
	if !strings.Contains(sh, `"${1}"`) || !strings.Contains(sh, `transition_id: "21"`) {
		t.Fatalf("draft:\n%s", sh)
	}
	var buf bytes.Buffer
	WriteFunnel(&buf, rep, 0, true)
	for _, want := range []string{"Reviewed 30 sessions", "Consolidated to"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("funnel output lacks %q:\n%s", want, buf.String())
		}
	}
}

func TestSaveInstallsOnceAndNeverReplacesAForeignFolder(t *testing.T) {
	o := DefaultOptions()
	o.Readers = []Reader{fakeReader{sessions: requestCorpus()}}
	rep, err := Run(o)
	if err != nil {
		t.Fatal(err)
	}
	d := rep.Primitives()[0].Draft()
	root := t.TempDir()
	path, unchanged, err := d.Save(root)
	if err != nil || unchanged {
		t.Fatalf("save: %v %v", err, unchanged)
	}
	for _, f := range []string{"primitive.yaml", "main.sh", "README.md", "SKILL.md", SavedMarker} {
		if _, err := os.Stat(filepath.Join(path, f)); err != nil {
			t.Errorf("saved folder lacks %s", f)
		}
	}
	if _, unchanged, err := d.Save(root); err != nil || !unchanged {
		t.Fatalf("saving the same draft again must change nothing: %v %v", err, unchanged)
	}
	foreign := filepath.Join(t.TempDir(), d.Name)
	os.MkdirAll(foreign, 0o755)
	if _, _, err := d.Save(filepath.Dir(foreign)); err == nil {
		t.Fatal("a folder that is not a saved primitive must not be replaced")
	}
}

func TestReviewWithoutARegistryOnlySaves(t *testing.T) {
	o := DefaultOptions()
	o.Readers = []Reader{fakeReader{sessions: requestCorpus()}}
	rep, _ := Run(o)
	var saved []string
	var out bytes.Buffer
	err := Review(strings.NewReader("all\n"), &out, rep, ReviewConfig{}, ReviewActions{
		Save: func(d *Draft) (string, error) { saved = append(saved, d.Name); return "/skills/" + d.Name, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != len(rep.Primitives()) || strings.Contains(out.String(), "Publish which") {
		t.Fatalf("saved %v\n%s", saved, out.String())
	}
}

func TestPick(t *testing.T) {
	cases := map[string][]int{"1 3": {0, 2}, "2-4": {1, 2, 3}, "all": {0, 1, 2, 3, 4}, "": nil, "9, 1, 1": {0}}
	for in, want := range cases {
		got := Pick(in, 5)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("Pick(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestCommandRunsWithNoHistory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var out, errOut bytes.Buffer
	if code := Command([]string{"--client", "claude-code,codex"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Reviewed 0 sessions") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestExplorationIsNotAPrimitive(t *testing.T) {
	// Every request greps, heads and seds different files for different
	// things: it recurs, but nothing in it is fixed.
	var ss []Session
	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 20; i++ {
		s := Session{Client: "fake", ID: fmt.Sprintf("x%02d", i), Start: t0.AddDate(0, 0, 3*i)}
		// Nothing is fixed in these requests; the fixed-step rule must
		// catch them.
		s.addRequest(fmt.Sprintf("why does src/pkg%d/file%d.go fail? look in src/pkg%d", i, i, i))
		for _, c := range []string{
			fmt.Sprintf("grep -rn 'needle%d' src/pkg%d", i, i),
			fmt.Sprintf("head -%d src/pkg%d/file%d.go", 20+i, i, i),
			fmt.Sprintf("sed -n '%d,%dp' src/pkg%d/file%d.go", i, i+40, i, i),
		} {
			s.Calls = append(s.Calls, Call{Tool: "shell", Command: c, Request: 0, Time: s.Start})
		}
		ss = append(ss, s)
	}
	o := DefaultOptions()
	o.Readers = []Reader{fakeReader{sessions: ss}}
	rep, err := Run(o)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Funnel.Routines != 1 || rep.Funnel.Primitives != 0 || rep.Routines[0].Failed != CheckReplays {
		t.Fatalf("funnel %+v; failed %q: %s", rep.Funnel, rep.Routines[0].Failed, rep.Routines[0].Why)
	}
}

func TestProceduresRankAboveInvestigations(t *testing.T) {
	o := DefaultOptions()
	o.Readers = []Reader{fakeReader{sessions: requestCorpus()}}
	rep, err := Run(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rep.Primitives() {
		if s := r.Draft().FixedShare; s <= 0 || s > 1 {
			t.Fatalf("fixed share %v out of range for %s", s, labelsOf(r.Candidate))
		}
	}
	// The ticket routine fixes its tools and transition and takes one id; it
	// is mostly fixed.
	for _, r := range rep.Primitives() {
		if strings.Contains(labelsOf(r.Candidate), "jira_transition_issue") && r.Draft().FixedShare < 0.6 {
			t.Fatalf("ticket routine fixed share = %v", r.Draft().FixedShare)
		}
	}
}
