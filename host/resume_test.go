package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.com/telara-labs/tap-runtime/bind"
)

const appendManifest = `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.test, name: appender, version: 0.1.0}
execution: {entrypoint: main.sh}
files:
  - {path: in, access: read}
commands:
  - {command: tee, globals: ["-a"], effect: write}
`

// Each line is one real change to a real file, made by a real program.
const appendScript = `echo one | tee -a counter.txt
echo two | tee -a counter.txt
echo three | tee -a counter.txt
echo finished
`

var yes = func(Ask) bool { return true }

func lines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Fields(string(b))
}

func opts(t *testing.T, pkg, runs string) Options {
	return Options{Package: pkg, Approve: yes, Journal: io.Discard, InterpDir: interpreterStore(t), RunsDir: runs}
}

func onlyRun(t *testing.T, runs string) string {
	t.Helper()
	entries, err := os.ReadDir(runs)
	if err != nil || len(entries) != 1 {
		t.Fatalf("want one run recorded, have %d (%v)", len(entries), err)
	}
	return entries[0].Name()
}

func TestResumeDoesNothingTwice(t *testing.T) {
	dir := inDir(t)
	runs := t.TempDir()
	pkg := writePackage(t, appendManifest, appendScript)

	o := opts(t, pkg, runs)
	o.stopAfter = 2
	if _, err := Run(context.Background(), o); !errors.Is(err, errInterrupted) {
		t.Fatalf("the run was not interrupted: %v", err)
	}
	if got := lines(t, filepath.Join(dir, "counter.txt")); strings.Join(got, ",") != "one,two" {
		t.Fatalf("before the interruption the file holds %v", got)
	}

	o = opts(t, pkg, runs)
	o.Resume = onlyRun(t, runs)
	res, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if got := lines(t, filepath.Join(dir, "counter.txt")); strings.Join(got, ",") != "one,two,three" {
		t.Fatalf("after resuming the file holds %v; a change was made twice or not at all", got)
	}
	if res.Replayed != 2 || res.Ran != 1 {
		t.Fatalf("replayed %d and ran %d, want 2 and 1", res.Replayed, res.Ran)
	}
	if !strings.Contains(res.Stdout, "finished") || !strings.Contains(res.Stdout, "one") {
		t.Fatalf("the resumed program did not see the answers it had before:\n%s", res.Stdout)
	}

	// A finished run is finished.
	o = opts(t, pkg, runs)
	o.Resume = res.RunID
	if _, err := Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "finished") {
		t.Fatalf("a finished run was resumed: %v", err)
	}
	if got := lines(t, filepath.Join(dir, "counter.txt")); len(got) != 3 {
		t.Fatalf("the file now holds %v", got)
	}
}

// The run stops after a change is recorded as begun and before anyone knows
// whether it happened. Doing it again could do it twice.
func TestAChangeInterruptedMidwayIsNotRepeated(t *testing.T) {
	dir := inDir(t)
	runs := t.TempDir()
	pkg := writePackage(t, appendManifest, appendScript)

	o := opts(t, pkg, runs)
	o.stopDuring = "r2"
	if _, err := Run(context.Background(), o); !errors.Is(err, errInterrupted) {
		t.Fatalf("the run was not interrupted: %v", err)
	}
	o = opts(t, pkg, runs)
	o.Resume = onlyRun(t, runs)
	res, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if got := lines(t, filepath.Join(dir, "counter.txt")); strings.Join(got, ",") != "one,three" {
		t.Fatalf("the file holds %v; the interrupted change must be neither repeated nor block what follows", got)
	}
	if res.Unknown != 1 {
		t.Fatalf("%d outcomes reported unknown, want 1", res.Unknown)
	}
	if !strings.Contains(res.Stderr, "unknown") {
		t.Fatalf("the program was not told:\n%s", res.Stderr)
	}
}

// A read changes nothing, so an interrupted one is simply asked again.
func TestAReadInterruptedMidwayIsAskedAgain(t *testing.T) {
	inDir(t)
	os.MkdirAll("in", 0o755)
	os.WriteFile("in/x.txt", []byte("content\n"), 0o644)
	runs := t.TempDir()
	pkg := writePackage(t, appendManifest, "cat in/x.txt\necho after\n")

	o := opts(t, pkg, runs)
	o.stopDuring = "r1"
	if _, err := Run(context.Background(), o); !errors.Is(err, errInterrupted) {
		t.Fatalf("the run was not interrupted: %v", err)
	}
	o = opts(t, pkg, runs)
	o.Resume = onlyRun(t, runs)
	res, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Unknown != 0 || !strings.Contains(res.Stdout, "content") || !strings.Contains(res.Stdout, "after") {
		t.Fatalf("unknown=%d, output:\n%s", res.Unknown, res.Stdout)
	}
}

// What a resumed program is told comes from the record, not from the world
// as it is now. Otherwise it would take a path it did not take before.
func TestAResumedProgramSeesWhatItSawBefore(t *testing.T) {
	dir := inDir(t)
	os.MkdirAll("in", 0o755)
	os.WriteFile("in/name.txt", []byte("first"), 0o644)
	runs := t.TempDir()
	pkg := writePackage(t, appendManifest, "name=$(cat in/name.txt)\necho $name | tee -a counter.txt\necho again | tee -a counter.txt\n")

	o := opts(t, pkg, runs)
	o.stopAfter = 2
	if _, err := Run(context.Background(), o); !errors.Is(err, errInterrupted) {
		t.Fatal(err)
	}
	os.WriteFile("in/name.txt", []byte("changed-while-stopped"), 0o644)

	o = opts(t, pkg, runs)
	o.Resume = onlyRun(t, runs)
	if _, err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if got := lines(t, filepath.Join(dir, "counter.txt")); strings.Join(got, ",") != "first,again" {
		t.Fatalf("the file holds %v", got)
	}
}

func TestResumeRefusesAChangedPackage(t *testing.T) {
	inDir(t)
	runs := t.TempDir()
	pkg := writePackage(t, appendManifest, appendScript)
	o := opts(t, pkg, runs)
	o.stopAfter = 1
	if _, err := Run(context.Background(), o); !errors.Is(err, errInterrupted) {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(pkg, "main.sh"), []byte(appendScript+"echo extra | tee -a counter.txt\n"), 0o644)
	o = opts(t, pkg, runs)
	o.Resume = onlyRun(t, runs)
	if _, err := Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "different version") {
		t.Fatalf("a run was continued with a changed package: %v", err)
	}
}

func TestTheRunsClockAndRandomBytesAreItsOwn(t *testing.T) {
	a := &stream{seed: [32]byte{1}}
	b := &stream{seed: [32]byte{1}}
	c := &stream{seed: [32]byte{2}}
	x, y, z := make([]byte, 100), make([]byte, 100), make([]byte, 100)
	a.Read(x)
	b.Read(y[:37])
	b.Read(y[37:])
	c.Read(z)
	if string(x) != string(y) {
		t.Fatal("the same run gave different random bytes")
	}
	if string(x) == string(z) {
		t.Fatal("two runs gave the same random bytes")
	}
}

// slowBridge answers after a delay and remembers how many calls overlapped.
type slowBridge struct {
	fakeBridge
	mu       sync.Mutex
	inFlight int
	most     int
	order    []string
}

func (s *slowBridge) Call(t bind.Tool, args map[string]any) (string, error) {
	s.mu.Lock()
	s.inFlight++
	if s.inFlight > s.most {
		s.most = s.inFlight
	}
	s.mu.Unlock()
	// The first call given is the slowest, so answers arrive in reverse.
	n, _ := args["n"].(float64)
	time.Sleep(time.Duration(400-int(n)*80) * time.Millisecond)
	s.mu.Lock()
	s.inFlight--
	s.order = append(s.order, t.Name)
	s.mu.Unlock()
	return `{"n":` + strings.TrimSuffix(strings.TrimSuffix(jsonNumber(n), ".0"), ".") + `}`, nil
}

func jsonNumber(f float64) string {
	return strings.TrimRight(strings.TrimRight(formatFloat(f), "0"), ".")
}

func TestPipelinedCallsOverlapAndAnswerInTheOrderGiven(t *testing.T) {
	store := interpreterStore(t)
	if _, _, _, err := obtain(store, "main.js"); err != nil {
		t.Skipf("the JavaScript interpreter could not be obtained: %v", err)
	}
	inDir(t)
	runs := t.TempDir()
	pkg := t.TempDir()
	os.WriteFile(filepath.Join(pkg, "primitive.yaml"), []byte(`apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.test, name: fan-out, version: 0.1.0}
execution: {entrypoint: main.js}
tools:
  - {alias: search, capability: gmail.threads.search, effect: read}
`), 0o644)
	os.WriteFile(filepath.Join(pkg, "main.js"), []byte(`
const r = tap.callMany([["search", {n: 0}], ["search", {n: 1}], ["search", {n: 2}], ["search", {n: 3}]]);
print(r.map((x) => x.n).join(","));
`), 0o644)

	b := &slowBridge{fakeBridge: *gmail()}
	o := Options{Package: pkg, Approve: yes, Journal: io.Discard, InterpDir: store, RunsDir: runs, Bridge: b}
	t0 := time.Now()
	res, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	took := time.Since(t0)
	if strings.TrimSpace(res.Stdout) != "0,1,2,3" {
		t.Fatalf("answers reached the program as %q", strings.TrimSpace(res.Stdout))
	}
	if b.most < 2 {
		t.Fatalf("at most %d call was in flight at once; nothing overlapped", b.most)
	}
	t.Logf("4 calls of 400, 320, 240 and 160 ms took %s with up to %d in flight; one after another they take 1.12 s", took.Round(time.Millisecond), b.most)
}

func formatFloat(f float64) string {
	s := ""
	if f == float64(int(f)) {
		for _, d := range []int{int(f)} {
			s = itoa(d) + ".0"
		}
		return s
	}
	return "0"
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
