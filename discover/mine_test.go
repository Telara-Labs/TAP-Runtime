package discover

import (
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMinePatternsRespectsWindow(t *testing.T) {
	seqs := [][]int{
		{1, 2, 3},
		{1, 9, 2, 9, 3},
		{1, 9, 9, 9, 2, 3}, // 2 is 4 steps after 1: outside a window of 3
	}
	ps, _, _ := minePatterns(seqs, mineLimits{window: 3, minSupport: 2, maxLen: 3, maxOut: 1000})
	got := map[string]int{}
	for _, p := range ps {
		got[p.key()] = len(p.sessions)
	}
	if got["1,2,3"] != 2 {
		t.Fatalf("1,2,3 support = %d, want 2 (third session breaks the window); all: %v", got["1,2,3"], got)
	}
}

func TestClosedOnlyDropsSubsumedPatterns(t *testing.T) {
	ps := []pattern{
		{items: []int{1, 2}, sessions: []int{0, 1, 2}},
		{items: []int{1, 2, 3}, sessions: []int{0, 1, 2}}, // same support: 1,2 is not closed
		{items: []int{1, 3}, sessions: []int{0, 1, 2, 3}}, // more support than 1,2,3: stays
	}
	var keys []string
	for _, p := range closedOnly(ps) {
		keys = append(keys, p.key())
	}
	if strings.Join(keys, " ") != "1,2,3 1,3" {
		t.Fatalf("closed = %v", keys)
	}
}

func TestPoissonUpper(t *testing.T) {
	// P(X >= 3 | lambda = 1) = 1 - e^-1 (1 + 1 + 1/2) = 0.080301...
	if got := poissonUpper(3, 1); math.Abs(got-0.0803014) > 1e-6 {
		t.Fatalf("poissonUpper(3,1) = %v", got)
	}
	if got := poissonUpper(0, 5); got != 1 {
		t.Fatalf("poissonUpper(0,5) = %v", got)
	}
	// The true value (~1e-1260) is below float64's range: 0 is correct, and
	// the old 1-cdf form wrongly returned 2.2e-16 here.
	if got := poissonUpper(400, 0.1); got < 0 || got > 1e-300 {
		t.Fatalf("deep tail = %v", got)
	}
	// P(X >= 60 | lambda = 20) = 4.2333e-13, from a direct summation of the
	// pmf over 60..399 in Python, independent of this code.
	if got := poissonUpper(60, 20); math.Abs(got-4.2333e-13)/4.2333e-13 > 1e-4 {
		t.Fatalf("poissonUpper(60,20) = %v", got)
	}
}

func TestBinomialUpper(t *testing.T) {
	// P(X >= 8 | n = 10, p = 0.5) = (45 + 10 + 1) / 1024.
	if got := binomialUpper(8, 10, 0.5); math.Abs(got-56.0/1024) > 1e-12 {
		t.Fatalf("binomialUpper(8,10,.5) = %v", got)
	}
	if binomialUpper(0, 10, 0.3) != 1 || binomialUpper(11, 10, 0.3) != 0 {
		t.Fatal("edge cases")
	}
}

func TestBenjaminiHochberg(t *testing.T) {
	// Worked example: p = .01 .04 .03 .20, m = 4.
	q := benjaminiHochberg([]float64{0.01, 0.04, 0.03, 0.20})
	want := []float64{0.04, 0.04 * 4 / 3, 0.04 * 4 / 3, 0.20}
	for i := range want {
		if math.Abs(q[i]-want[i]) > 1e-12 {
			t.Fatalf("q = %v, want %v", q, want)
		}
	}
}

// fakeReader serves synthetic sessions as if they were a client's history.
type fakeReader struct {
	sessions []Session
	name     string
}

func (f fakeReader) Client() string {
	if f.name == "" {
		return "fake"
	}
	return f.name
}

func (f fakeReader) Read(time.Time) ([]Session, error) { return f.sessions, nil }

// plantedCorpus has 60 sessions of random tool calls. Twenty of them also
// run one procedure (build, test, push with a changing branch name) and load
// the "ship" skill. A procedure that is really there must qualify; random
// co-occurrence must not.
func plantedCorpus() []Session {
	rng := rand.New(rand.NewSource(42))
	noise := []string{"ls", "cat a", "grep x y", "head -3", "wc -l", "tail -5", "sed -n 1p", "find .", "du -sh", "pwd", "whoami", "date"}
	var out []Session
	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 60; i++ {
		s := Session{Client: "fake", ID: fmt.Sprintf("s%02d", i), Start: t0.AddDate(0, 0, 7*(i%20))}
		call := func(cmd string) {
			s.Calls = append(s.Calls, Call{Client: "fake", Session: s.ID, Tool: "shell", Command: cmd, Time: s.Start})
		}
		for j := 0; j < 25; j++ {
			call(noise[rng.Intn(len(noise))] + fmt.Sprintf(" %d", rng.Intn(1000)))
			if i < 20 && j == 10 {
				s.Calls = append(s.Calls, Call{Client: "fake", Session: s.ID, Tool: "Skill", Args: map[string]string{"skill": "ship"}})
				call("make build")
				call("go test ./... -count=1")
				call(fmt.Sprintf("git push origin feature-%d", i))
			}
		}
		out = append(out, s)
	}
	return out
}

func TestRunFindsPlantedProcedureAndNotNoise(t *testing.T) {
	o := DefaultOptions()
	o.Patterns = true
	o.Readers = []Reader{fakeReader{sessions: plantedCorpus()}}
	o.Permutations = 30
	rep, err := Run(o)
	if err != nil {
		t.Fatal(err)
	}
	var planted *Candidate
	for i, c := range rep.Candidates {
		if labelsOf(c) == "sh:make build → sh:go test → sh:git push" {
			planted = &rep.Candidates[i]
		}
	}
	if planted == nil || !planted.Qualified {
		t.Fatalf("planted procedure not qualified: %+v", planted)
	}
	if planted.Sessions != 20 || planted.Weeks != 20 {
		t.Fatalf("sessions %d weeks %d, want 20 and 20", planted.Sessions, planted.Weeks)
	}
	push := planted.Steps[2]
	if push.Template != "sh:git push origin <word>" || len(push.Params) != 1 {
		t.Fatalf("push template = %q params %v: the branch must be the only parameter", push.Template, push.Params)
	}
	for _, c := range rep.Candidates {
		if !c.Qualified {
			continue
		}
		for _, s := range c.Steps {
			switch s.Label {
			case "sh:make build", "sh:go test", "sh:git push":
			default:
				t.Errorf("a qualified candidate contains noise step %q: %s", s.Label, labelsOf(c))
			}
		}
	}
	if len(rep.Recall) != 1 || rep.Recall[0].Skill != "ship" || rep.Recall[0].BestQualified == nil || rep.Recall[0].BestQualified.F1 != 1 {
		t.Fatalf("recall = %+v", rep.Recall)
	}
}

func TestDuplicateSessionsCountOnce(t *testing.T) {
	s := Session{Client: "fake", ID: "a", Calls: []Call{{Tool: "shell", Command: "make"}, {Tool: "shell", Command: "go test"}}}
	d := s
	d.ID = "b"
	o := DefaultOptions()
	o.Patterns = true
	o.Readers = []Reader{fakeReader{sessions: []Session{s, d}}}
	rep, err := Run(o)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Clients[0].Sessions != 1 || rep.Clients[0].DuplicateSessions != 1 {
		t.Fatalf("stats = %+v", rep.Clients[0])
	}
}

// TestPackageCannotReachTheNetwork enforces the design boundary (doc 24
// section 6): discovery reads local history and sends nothing. No file of
// this package may import a networking package or the CLI's API client.
func TestPackageCannotReachTheNetwork(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		af, err := parser.ParseFile(fset, f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range af.Imports {
			p := strings.Trim(im.Path.Value, `"`)
			// net/url only parses URL strings; it opens no connection.
			if p == "net" || (strings.HasPrefix(p, "net/") && p != "net/url") || strings.Contains(p, "/internal/api") || strings.Contains(p, "/internal/auth") || strings.Contains(p, "/internal/relay") {
				t.Errorf("%s imports %s", f, p)
			}
		}
	}
}

// TestPruningDropsNothingThatCouldQualify checks the necessity bound used to
// cut the search: every pattern the unpruned search finds whose items all
// satisfy f^k <= alpha must still be found with pruning on.
func TestPruningDropsNothingThatCouldQualify(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	seqs := make([][]int, 80)
	for i := range seqs {
		for j := 0; j < 30; j++ {
			// Label 0 is in nearly every session; 1..9 are rarer.
			x := 0
			if rng.Intn(3) > 0 {
				x = 1 + rng.Intn(9)
			}
			seqs[i] = append(seqs[i], x)
		}
	}
	df := make([]int, 10)
	for _, s := range seqs {
		seen := map[int]bool{}
		for _, x := range s {
			if !seen[x] {
				seen[x] = true
				df[x]++
			}
		}
	}
	can := func(items []int, k int) bool {
		for _, x := range items {
			if math.Pow(float64(df[x])/float64(len(seqs)), float64(k)) > 0.05 {
				return false
			}
		}
		return true
	}
	lim := mineLimits{window: 4, minSupport: 3, maxLen: 3, maxOut: 1 << 30}
	all, _, _ := minePatterns(seqs, lim)
	lim.canQualify = can
	pruned, _, _ := minePatterns(seqs, lim)
	have := map[string]bool{}
	for _, p := range pruned {
		have[p.key()] = true
	}
	admissible := 0
	for _, p := range all {
		if can(p.items, len(p.sessions)) {
			admissible++
			if !have[p.key()] {
				t.Errorf("pruning dropped %s (support %d)", p.key(), len(p.sessions))
			}
		}
	}
	if admissible == 0 || len(pruned) >= len(all) {
		t.Fatalf("test is vacuous: %d admissible, %d pruned of %d", admissible, len(pruned), len(all))
	}
}

// TestClientVocabulariesAreNotRecurrence: two clients whose tools have
// different names, each calling its tools at random. Nothing recurs, so
// nothing may qualify. Before the between-session shuffle was stratified by
// client, pairs of one client's tools qualified merely for sharing a client.
func TestClientVocabulariesAreNotRecurrence(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	var ss []Session
	for i := 0; i < 80; i++ {
		client, tools := "alpha", []string{"a_read", "a_grep", "a_edit", "a_list", "a_run"}
		if i%2 == 1 {
			client, tools = "beta", []string{"b_read", "b_grep", "b_edit", "b_list", "b_run"}
		}
		s := Session{Client: client, ID: fmt.Sprintf("%s%d", client, i)}
		for j := 0; j < 30; j++ {
			s.Calls = append(s.Calls, Call{Tool: tools[rng.Intn(len(tools))], Args: map[string]string{"n": fmt.Sprint(rng.Intn(1000))}})
		}
		ss = append(ss, s)
	}
	var alpha, beta []Session
	for _, s := range ss {
		if s.Client == "alpha" {
			alpha = append(alpha, s)
		} else {
			beta = append(beta, s)
		}
	}
	o := DefaultOptions()
	o.Patterns = true
	o.Readers = []Reader{fakeReader{alpha, "alpha"}, fakeReader{beta, "beta"}}
	rep, err := Run(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range rep.Candidates {
		if c.Qualified {
			t.Errorf("random calls qualified: %s (q %.2g, order q %.2g, necessity q %.2g)", labelsOf(c), c.Q, c.OrderQ, c.NecessityQ)
		}
	}
}

func TestTemplateCollapsesVariadicParameters(t *testing.T) {
	occ := [][]Step{}
	for _, files := range [][]string{{"a.go", "b.go", "c.go"}, {"x.go", "y.go", "z.go"}} {
		st := Step{Label: "sh:git add", Skeleton: "sh:git add "}
		for i, f := range files {
			st.Slots = append(st.Slots, Slot{Key: fmt.Sprint(i), Type: SlotPath, Value: f})
		}
		occ = append(occ, []Step{st})
	}
	if got := templateOf("sh:git add", occ, 0).Template; got != "sh:git add <path>…" {
		t.Fatalf("template = %q", got)
	}
}

func TestHypergeomUpper(t *testing.T) {
	// 10 sessions, 4 contain the pattern, the skill has 3: P(all 3 hold it)
	// = C(4,3)/C(10,3) = 4/120.
	if got := hypergeomUpper(3, 4, 3, 10); math.Abs(got-4.0/120) > 1e-12 {
		t.Fatalf("hypergeomUpper(3,4,3,10) = %v", got)
	}
	if hypergeomUpper(0, 4, 3, 10) < 0.999999 || hypergeomUpper(4, 4, 3, 10) != 0 {
		t.Fatal("edge cases")
	}
}

func TestSkillComparisonFindsThePlantedProcedure(t *testing.T) {
	o := DefaultOptions()
	o.Patterns = true
	o.Readers = []Reader{fakeReader{sessions: plantedCorpus()}}
	rep, err := Run(o)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Skills) != 1 || rep.Skills[0].Skill != "ship" {
		t.Fatalf("skills = %+v", rep.Skills)
	}
	// The whole report must encode: a procedure that never occurs outside
	// its skill once made the lift +Inf, which JSON cannot represent.
	if _, err := json.Marshal(rep); err != nil {
		t.Fatalf("report does not encode: %v", err)
	}
	top := rep.Skills[0].Procedures[0]
	if !top.OnlyInSkill {
		t.Errorf("planted procedure occurs only in its skill; OnlyInSkill = false")
	}
	if labelsOf(top.Candidate) != "sh:make build → sh:go test → sh:git push" || top.InSkill != 20 || top.Outside != 0 {
		t.Fatalf("top procedure = %s in %d outside %d", labelsOf(top.Candidate), top.InSkill, top.Outside)
	}
}
