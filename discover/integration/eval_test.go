package integration

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover"
	"gitlab.com/telara-labs/tap-runtime/discover/internal/testkit"

	"gitlab.com/telara-labs/tap-runtime/discover/eval"

	"gitlab.com/telara-labs/tap-runtime/discover/model"

	"gitlab.com/telara-labs/tap-runtime/discover/history"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

func TestFrozenReaderRefusesAChangedCorpus(t *testing.T) {
	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	a := trace.Session{Client: "fake", ID: "a", Start: t0, Calls: []trace.Call{{Tool: "shell", Command: "ls", Time: t0}}}
	b := trace.Session{Client: "fake", ID: "b", Start: t0, Calls: []trace.Call{{Tool: "shell", Command: "pwd", Time: t0.Add(48 * time.Hour)}}}
	m := history.BuildManifest([]trace.Session{a, b}, t0.Add(24*time.Hour))
	if len(m.Sessions) != 1 || m.Sessions[0].ID != "a" {
		t.Fatalf("a session still active at the cutoff must not be frozen: %+v", m.Sessions)
	}
	ss, err := history.FrozenReader{Inner: testkit.FakeReader{Sessions: []trace.Session{a, b}}, Manifest: m}.Read(time.Time{})
	if err != nil || len(ss) != 1 {
		t.Fatalf("read %d, %v", len(ss), err)
	}
	a2 := a
	a2.Calls = append(append([]trace.Call(nil), a.Calls...), trace.Call{Tool: "shell", Command: "date"})
	if _, err := (history.FrozenReader{Inner: testkit.FakeReader{Sessions: []trace.Session{a2}}, Manifest: m}).Read(time.Time{}); !errors.Is(err, history.ErrCorpusChanged) {
		t.Fatalf("a changed session must stop the run: %v", err)
	}
	if _, err := (history.FrozenReader{Inner: testkit.FakeReader{Sessions: nil}, Manifest: m}).Read(time.Time{}); !errors.Is(err, history.ErrCorpusChanged) {
		t.Fatalf("a missing session must stop the run: %v", err)
	}
	dup := m
	dup.Sessions = append(append([]history.ManifestEntry(nil), m.Sessions...), m.Sessions[0])
	if _, err := (history.FrozenReader{Inner: testkit.FakeReader{Sessions: []trace.Session{a}}, Manifest: dup}).Read(time.Time{}); !errors.Is(err, history.ErrCorpusChanged) {
		t.Fatalf("a manifest naming one session twice must stop the run: %v", err)
	}
}

func TestLineageJoinsCopiesAndTemplatedPrompts(t *testing.T) {
	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	orig := trace.Session{Client: "claude-code", ID: "orig", Start: t0, Requests: []string{"fix the flaky gateway test"}, Calls: []trace.Call{{ID: "toolu_1", Tool: "shell", Command: "go test ./..."}}}
	resumed := trace.Session{Client: "claude-code", ID: "resumed", Start: t0, Requests: []string{"something else entirely here"}, Calls: []trace.Call{{ID: "toolu_1", Tool: "shell", Command: "go test ./..."}, {ID: "toolu_2", Tool: "shell", Command: "git diff"}}}
	auto1 := trace.Session{Client: "codex", ID: "x", Start: t0, Requests: []string{"Automation: hourly monitor run for the linkedin queue"}, Calls: []trace.Call{{Tool: "shell", Command: "ls"}}}
	auto2 := trace.Session{Client: "codex", ID: "y", Start: t0, Requests: []string{"automation:  hourly monitor run for the  linkedin queue"}, Calls: []trace.Call{{Tool: "shell", Command: "ls"}}}
	// Shared follow-ups and injected wrappers do not tie unrelated tasks.
	other := trace.Session{Client: "codex", ID: "z", Start: t0, Requests: []string{"# AGENTS.md instructions for /repo <instructions> rules", "rename the billing dashboard tiles to match the design", "do you still have more to do? please continue"}, Calls: []trace.Call{{Tool: "shell", Command: "ls"}}}
	other2 := trace.Session{Client: "codex", ID: "w", Start: t0, Requests: []string{"# AGENTS.md instructions for /repo <instructions> rules", "investigate why the export indexer retries forever", "do you still have more to do? please continue"}, Calls: []trace.Call{{Tool: "shell", Command: "ls"}}}
	c := eval.NewCorpus([]trace.Session{orig, resumed, auto1, auto2, other, other2})
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
	ss := append(testkit.RequestCorpus(), testkit.CredCorpus()...)
	o := discover.DefaultOptions()
	o.Readers = []trace.Reader{testkit.FakeReader{Sessions: ss}}
	rep, err := discover.Run(o)
	if err != nil {
		t.Fatal(err)
	}
	c := eval.NewCorpus(ss)
	a := eval.SampleEpisodes(c, rep, eval.SampleOptions{Seed: 7, PerReady: 2, PerGroup: 20, Uncovered: 6})
	b := eval.SampleEpisodes(c, rep, eval.SampleOptions{Seed: 7, PerReady: 2, PerGroup: 20, Uncovered: 6})
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

func TestSourceDigestSurvivesParserChanges(t *testing.T) {
	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	a := trace.Session{Client: "fake", ID: "a", Start: t0, SourceDigest: "abc", Calls: []trace.Call{{Tool: "shell", Command: "ls", Time: t0}}}
	m := history.BuildManifest([]trace.Session{a}, t0.Add(time.Hour))
	if m.Sessions[0].Digest != "abc" {
		t.Fatalf("digest %q", m.Sessions[0].Digest)
	}
	// A newer parser reads more out of the same bytes: same input.
	b := a
	b.Calls = []trace.Call{{Tool: "shell", Command: "ls", Time: t0, Output: "x", OutPaths: []string{".id"}}}
	if _, err := (history.FrozenReader{Inner: testkit.FakeReader{Sessions: []trace.Session{b}}, Manifest: m}).Read(time.Time{}); err != nil {
		t.Fatalf("a parser change is not an input change: %v", err)
	}
	b.SourceDigest = "def"
	if _, err := (history.FrozenReader{Inner: testkit.FakeReader{Sessions: []trace.Session{b}}, Manifest: m}).Read(time.Time{}); !errors.Is(err, history.ErrCorpusChanged) {
		t.Fatalf("changed source bytes must stop the run: %v", err)
	}
}

func TestSingleEpisodeContract(t *testing.T) {
	// A request that names both revisions and runs two diffs: every value
	// has a source, so the episode alone is a specified procedure.
	good := testkit.Episodes("g", 1, func(int) string { return "compare a1b2c3d and d4e5f6a for services and tests" }, func(int) []trace.Call {
		return []trace.Call{testkit.ShellCall("git diff --name-only a1b2c3d d4e5f6a -- services"), testkit.ShellCall("git diff --name-only a1b2c3d d4e5f6a -- tests")}
	})
	// A request whose reads target files nobody named: chosen during the run.
	bad := testkit.Episodes("b", 1, func(int) string { return "why is the gateway slow?" }, func(int) []trace.Call {
		return []trace.Call{{Tool: "Read", Args: map[string]string{"file_path": "src/x/pool.go"}}, testkit.ShellCall("rg -n timeout internal/y"), {Tool: "Read", Args: map[string]string{"file_path": "src/z/conn.go"}}}
	})
	c := eval.NewCorpus(append(good, bad...))
	cl := c.AssessEpisodes(c.Episodes())
	got := map[string]string{}
	for _, x := range cl {
		got[x.Session] = x.Suitability
	}
	if got["g00"] != model.SuitUseful || got["b00"] == model.SuitUseful {
		t.Fatalf("claims %v", got)
	}
}
