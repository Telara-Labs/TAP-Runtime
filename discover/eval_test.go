package discover

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFrozenReaderRefusesAChangedCorpus(t *testing.T) {
	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	a := Session{Client: "fake", ID: "a", Start: t0, Calls: []Call{{Tool: "shell", Command: "ls", Time: t0}}}
	b := Session{Client: "fake", ID: "b", Start: t0, Calls: []Call{{Tool: "shell", Command: "pwd", Time: t0.Add(48 * time.Hour)}}}
	m := BuildManifest([]Session{a, b}, t0.Add(24*time.Hour))
	if len(m.Sessions) != 1 || m.Sessions[0].ID != "a" {
		t.Fatalf("a session still active at the cutoff must not be frozen: %+v", m.Sessions)
	}
	ss, err := FrozenReader{Inner: fakeReader{sessions: []Session{a, b}}, Manifest: m}.Read(time.Time{})
	if err != nil || len(ss) != 1 {
		t.Fatalf("read %d, %v", len(ss), err)
	}
	a2 := a
	a2.Calls = append(append([]Call(nil), a.Calls...), Call{Tool: "shell", Command: "date"})
	if _, err := (FrozenReader{Inner: fakeReader{sessions: []Session{a2}}, Manifest: m}).Read(time.Time{}); !errors.Is(err, ErrCorpusChanged) {
		t.Fatalf("a changed session must stop the run: %v", err)
	}
	if _, err := (FrozenReader{Inner: fakeReader{sessions: nil}, Manifest: m}).Read(time.Time{}); !errors.Is(err, ErrCorpusChanged) {
		t.Fatalf("a missing session must stop the run: %v", err)
	}
	dup := m
	dup.Sessions = append(append([]ManifestEntry(nil), m.Sessions...), m.Sessions[0])
	if _, err := (FrozenReader{Inner: fakeReader{sessions: []Session{a}}, Manifest: dup}).Read(time.Time{}); !errors.Is(err, ErrCorpusChanged) {
		t.Fatalf("a manifest naming one session twice must stop the run: %v", err)
	}
}

func TestLineageJoinsCopiesAndTemplatedPrompts(t *testing.T) {
	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	orig := Session{Client: "claude-code", ID: "orig", Start: t0, Requests: []string{"fix the flaky gateway test"}, Calls: []Call{{ID: "toolu_1", Tool: "shell", Command: "go test ./..."}}}
	resumed := Session{Client: "claude-code", ID: "resumed", Start: t0, Requests: []string{"something else entirely here"}, Calls: []Call{{ID: "toolu_1", Tool: "shell", Command: "go test ./..."}, {ID: "toolu_2", Tool: "shell", Command: "git diff"}}}
	auto1 := Session{Client: "codex", ID: "x", Start: t0, Requests: []string{"Automation: hourly monitor run"}, Calls: []Call{{Tool: "shell", Command: "ls"}}}
	auto2 := Session{Client: "codex", ID: "y", Start: t0, Requests: []string{"automation:  hourly monitor run"}, Calls: []Call{{Tool: "shell", Command: "ls"}}}
	other := Session{Client: "codex", ID: "z", Start: t0, Requests: []string{"yes"}, Calls: []Call{{Tool: "shell", Command: "ls"}}}
	other2 := Session{Client: "codex", ID: "w", Start: t0, Requests: []string{"yes"}, Calls: []Call{{Tool: "shell", Command: "ls"}}}
	c := NewCorpus([]Session{orig, resumed, auto1, auto2, other, other2})
	ln := map[string]string{}
	for _, e := range c.Episodes() {
		ln[e.Session] = e.Lineage
	}
	if ln["orig"] != ln["resumed"] || ln["x"] != ln["y"] || ln["z"] == ln["w"] || ln["orig"] == ln["x"] {
		t.Fatalf("lineages: %v", ln)
	}
	if s, _ := c.Session("claude-code", "resumed"); len(s.Calls) != 1 {
		t.Fatalf("the copied call must be dropped from the resumed session: %d calls", len(s.Calls))
	}
}

func TestSampleIsReproducibleAndCarriesNoDecision(t *testing.T) {
	ss := append(requestCorpus(), credCorpus()...)
	o := DefaultOptions()
	o.Readers = []Reader{fakeReader{sessions: ss}}
	rep, err := Run(o)
	if err != nil {
		t.Fatal(err)
	}
	c := NewCorpus(ss)
	a := SampleEpisodes(c, rep, SampleOptions{Seed: 7, PerReady: 2, PerGroup: 20, Uncovered: 6})
	b := SampleEpisodes(c, rep, SampleOptions{Seed: 7, PerReady: 2, PerGroup: 20, Uncovered: 6})
	if len(a) == 0 || !reflect.DeepEqual(a, b) {
		t.Fatalf("same seed, different sample: %d vs %d", len(a), len(b))
	}
	seen := map[string]bool{}
	for _, e := range a {
		k := e.Client + "/" + e.Session
		if seen[k] {
			t.Fatalf("two episodes from session %s", k)
		}
		seen[k] = true
		p := c.RenderEpisode(e)
		for _, leak := range []string{e.Stratum, "primitive", "needs_authoring", "removed", "abcDEF1234567890ghiJKL"} {
			if leak != "" && strings.Contains(p, leak) {
				t.Fatalf("packet %s shows %q:\n%s", e.ID, leak, p)
			}
		}
	}
}
