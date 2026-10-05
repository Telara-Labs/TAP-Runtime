package integration

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/discover/pipeline"

	"github.com/Telara-Labs/TAP-Runtime/discover"
	"github.com/Telara-Labs/TAP-Runtime/discover/internal/testkit"

	"github.com/Telara-Labs/TAP-Runtime/discover/routine"

	"github.com/Telara-Labs/TAP-Runtime/discover/pack"

	"github.com/Telara-Labs/TAP-Runtime/discover/model"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

func TestRecurringRequestsBecomePrimitives(t *testing.T) {
	o := pipeline.DefaultOptions()
	o.Readers = []trace.Reader{testkit.FakeReader{Sessions: testkit.RequestCorpus()}}
	rep, err := pipeline.Run(o)
	if err != nil {
		t.Fatal(err)
	}
	f := rep.Funnel
	if f.Sessions != 30 || f.Routines < 2 {
		t.Fatalf("funnel = %+v", f)
	}
	var jira, deploy *model.Routine
	for i := range rep.Routines {
		l := model.LabelsOf(rep.Routines[i].Candidate)
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
		if in.Type == trace.SlotPath && in.Explained != 0 {
			t.Errorf("the image tag never appeared in a request, so explained must be 0: %+v", in)
		}
	}
	sh := string(routine.RoutineDraft(jira).Files["main.sh"])
	if !strings.Contains(sh, `"${1}"`) || !strings.Contains(sh, `"transition_id":"21"`) {
		t.Fatalf("draft:\n%s", sh)
	}
	var buf bytes.Buffer
	routine.WriteFunnel(&buf, rep, 0, true)
	for _, want := range []string{"Reviewed 30 sessions", "Useful procedures for user tasks: ", "No draft has been executed"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("funnel output lacks %q:\n%s", want, buf.String())
		}
	}
}

func TestSaveInstallsOnceAndNeverReplacesAForeignFolder(t *testing.T) {
	o := pipeline.DefaultOptions()
	o.Readers = []trace.Reader{testkit.FakeReader{Sessions: testkit.RequestCorpus()}}
	rep, err := pipeline.Run(o)
	if err != nil {
		t.Fatal(err)
	}
	d := routine.RoutineDraft(routine.ReportPrimitives(rep)[0])
	root := t.TempDir()
	path, unchanged, err := pack.SaveDraft(d, root)
	if err != nil || unchanged {
		t.Fatalf("save: %v %v", err, unchanged)
	}
	for _, f := range []string{"primitive.yaml", "main.sh", "README.md", "SKILL.md", pack.SavedMarker} {
		if _, err := os.Stat(filepath.Join(path, f)); err != nil {
			t.Errorf("saved folder lacks %s", f)
		}
	}
	if _, unchanged, err := pack.SaveDraft(d, root); err != nil || !unchanged {
		t.Fatalf("saving the same draft again must change nothing: %v %v", err, unchanged)
	}
	foreign := filepath.Join(t.TempDir(), d.Name)
	os.MkdirAll(foreign, 0o755)
	if _, _, err := pack.SaveDraft(d, filepath.Dir(foreign)); err == nil {
		t.Fatal("a folder that is not a saved primitive must not be replaced")
	}
}

func TestReviewWithoutARegistryOnlySaves(t *testing.T) {
	o := pipeline.DefaultOptions()
	o.Readers = []trace.Reader{testkit.FakeReader{Sessions: testkit.RequestCorpus()}}
	rep, _ := pipeline.Run(o)
	var saved []string
	var out bytes.Buffer
	err := routine.Review(strings.NewReader("all\n"), &out, rep, routine.ReviewConfig{}, routine.ReviewActions{
		Save: func(d *model.Draft) (string, error) { saved = append(saved, d.Name); return "/skills/" + d.Name, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != len(routine.ReportPrimitives(rep)) || strings.Contains(out.String(), "Publish which") {
		t.Fatalf("saved %v\n%s", saved, out.String())
	}
}

func TestPick(t *testing.T) {
	cases := map[string][]int{"1 3": {0, 2}, "2-4": {1, 2, 3}, "all": {0, 1, 2, 3, 4}, "": nil, "9, 1, 1": {0}}
	for in, want := range cases {
		got := routine.Pick(in, 5)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("Pick(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestCommandRunsWithNoHistory(t *testing.T) {
	setHome(t, t.TempDir())
	var out, errOut bytes.Buffer
	// With no subcommand, discover ends in the primitive menu.
	if code := discover.Command([]string{"--client", "claude-code,codex"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Summary") || !strings.Contains(out.String(), "Sessions read") {
		t.Fatalf("output:\n%s", out.String())
	}
	out.Reset()
	if code := discover.Command([]string{"report", "--client", "claude-code,codex"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("report exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Reviewed 0 sessions") {
		t.Fatalf("report output:\n%s", out.String())
	}
}

func TestExplorationIsNotAPrimitive(t *testing.T) {
	// Every request greps, heads and seds different files for different
	// things: it recurs, but nothing in it is fixed.
	var ss []trace.Session
	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 20; i++ {
		s := trace.Session{Client: "fake", ID: fmt.Sprintf("x%02d", i), Start: t0.AddDate(0, 0, 3*i)}
		// Nothing is fixed in these requests; the fixed-step rule must
		// catch them.
		s.AddRequest(fmt.Sprintf("why does src/pkg%d/file%d.go fail? look in src/pkg%d", i, i, i))
		for _, c := range []string{
			fmt.Sprintf("grep -rn 'needle%d' src/pkg%d", i, i),
			fmt.Sprintf("head -%d src/pkg%d/file%d.go", 20+i, i, i),
			fmt.Sprintf("sed -n '%d,%dp' src/pkg%d/file%d.go", i, i+40, i, i),
		} {
			s.Calls = append(s.Calls, trace.Call{Tool: "shell", Command: c, Request: 0, Time: s.Start})
		}
		ss = append(ss, s)
	}
	o := pipeline.DefaultOptions()
	o.Readers = []trace.Reader{testkit.FakeReader{Sessions: ss}}
	rep, err := pipeline.Run(o)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Funnel.Routines != 1 || rep.Funnel.Primitives != 0 || rep.Routines[0].Suitability == model.SuitUseful {
		t.Fatalf("funnel %+v; failed %q: %s", rep.Funnel, rep.Routines[0].Failed, rep.Routines[0].Why)
	}
}

func TestFixedShareIsAFractionOfTheRoutine(t *testing.T) {
	o := pipeline.DefaultOptions()
	o.Readers = []trace.Reader{testkit.FakeReader{Sessions: testkit.RequestCorpus()}}
	rep, err := pipeline.Run(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range routine.ReportPrimitives(rep) {
		if s := routine.RoutineDraft(r).FixedShare; s <= 0 || s > 1 {
			t.Fatalf("fixed share %v out of range for %s", s, model.LabelsOf(r.Candidate))
		}
	}
	// The ticket routine fixes its tools and transition and takes one id; it
	// is mostly fixed.
	for _, r := range routine.ReportPrimitives(rep) {
		if strings.Contains(model.LabelsOf(r.Candidate), "jira_transition_issue") && routine.RoutineDraft(r).FixedShare < 0.6 {
			t.Fatalf("ticket routine fixed share = %v", routine.RoutineDraft(r).FixedShare)
		}
	}
}
