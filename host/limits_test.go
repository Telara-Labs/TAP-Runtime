package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Execution.timeoutSeconds and limits were accepted and never
// enforced. These tests run the real runner against programs that break each
// bound.

func runLimited(t *testing.T, entry, manifestExtra, program string, o Options) (*Result, error) {
	t.Helper()
	store := interpreterStore(t)
	if _, _, _, err := obtain(store, entry); err != nil {
		t.Skipf("the interpreter could not be obtained: %v", err)
	}
	inDir(t)
	pkg := t.TempDir()
	manifest := "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.test, name: limits, version: 0.1.0}\nexecution:\n  entrypoint: " + entry + "\n" + manifestExtra
	os.WriteFile(filepath.Join(pkg, "primitive.yaml"), []byte(manifest), 0o644)
	os.WriteFile(filepath.Join(pkg, entry), []byte(program), 0o644)
	o.Package, o.Journal, o.InterpDir, o.RunsDir = pkg, io.Discard, store, t.TempDir()
	return Run(context.Background(), o)
}

func TestAProgramThatNeverEndsIsStoppedAtItsTimeLimit(t *testing.T) {
	start := time.Now()
	_, err := runLimited(t, "main.sh", "  timeoutSeconds: 1\n", "while true; do :; done\n", Options{})
	if err == nil || !strings.Contains(err.Error(), "time limit") {
		t.Fatalf("a program that never ends was not stopped at its limit: %v", err)
	}
	if took := time.Since(start); took > 20*time.Second {
		t.Fatalf("a 1 second limit took %s to apply", took)
	}
}

func TestTheRunTimeLimitStopsADispatchedHostProgram(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep is not installed")
	}
	cache := t.TempDir()
	if _, err := runLimited(t, "main.sh", "", "echo ready\n", Options{CacheDir: cache}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := runLimited(t, "main.sh", "  timeoutSeconds: 1\ncommands:\n  - {command: sleep, args: [\"3\"], effect: read}\n", "sleep 3\n", Options{CacheDir: cache})
	if err == nil || !strings.Contains(err.Error(), "time limit") {
		t.Fatalf("run did not report its time limit: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2500*time.Millisecond {
		t.Fatalf("run waited for the host program after the guest timed out: %s", elapsed)
	}
}

func TestTheTimeLimitDoesNotRunWhileAPersonIsBeingAsked(t *testing.T) {
	asked := false
	res, err := runLimited(t, "main.py", "  timeoutSeconds: 1\nfiles:\n  - {path: out, access: write}\n",
		"tap.write(\"out/a.txt\", \"hello\")\nprint(\"done\")\n", Options{
			Approve: func(Ask) Grant {
				asked = true
				time.Sleep(2500 * time.Millisecond) // longer than the limit
				return Grant{OK: true, Limit: Unlimited}
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	if !asked {
		t.Fatal("the program did not need approval, so it does not test the pause")
	}
	if !strings.Contains(res.Stdout, "done") {
		t.Fatalf("waiting for a person cost the run its time: %q", res.Stdout)
	}
}

func TestPythonCannotAllocateBeyondTheMemoryCeiling(t *testing.T) {
	res, err := runLimited(t, "main.py", "", `
try:
    block = bytearray(1 << 30)
    print("allocated")
except MemoryError:
    print("refused")
`, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Stdout, "allocated") || !strings.Contains(res.Stdout, "refused") {
		t.Fatalf("a 1 GiB allocation was allowed: %q (stderr %q)", res.Stdout, res.Stderr)
	}
}

func TestAPythonProgramPastItsDispatchLimitIsRefused(t *testing.T) {
	res, err := runLimited(t, "main.py", "  limits: {max_dispatches: 2}\ncommands:\n  - {command: git, args: [\"*\"], effect: read}\n", `
out = []
for i in range(4):
    try:
        tap.exec("git", ["--version"])
        out.append("ran")
    except PermissionError as e:
        out.append("refused")
print(",".join(out))
`, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "ran,ran,refused,refused" {
		t.Fatalf("limits.max_dispatches = 2 gave %q", got)
	}
}

func TestReadLineRefusesALineOverTheCap(t *testing.T) {
	long := strings.Repeat("x", 5000)
	rd := bufio.NewReaderSize(strings.NewReader(long+"\n"), 64)
	if _, err := readLine(rd, 1000); !errors.Is(err, errLineTooLong) {
		t.Fatalf("a 5000 byte line was accepted under a 1000 byte cap: %v", err)
	}
	rd = bufio.NewReaderSize(strings.NewReader(long+"\nnext\n"), 64)
	line, err := readLine(rd, 10000)
	if err != nil || string(line) != long+"\n" {
		t.Fatalf("a line under the cap was damaged: %d bytes, %v", len(line), err)
	}
	if line, _ = readLine(rd, 10000); string(line) != "next\n" {
		t.Fatalf("the line after it was lost: %q", line)
	}
}

func TestACappedBufferKeepsTheStartAndSaysItDroppedTheRest(t *testing.T) {
	c := &cappedBuffer{max: 10}
	n, err := c.Write([]byte(strings.Repeat("a", 25)))
	if n != 25 || err != nil {
		t.Fatalf("a write that was over the cap reported %d, %v; a program would see a failed write", n, err)
	}
	c.Write([]byte("more"))
	if c.String() != strings.Repeat("a", 10) || !c.truncated {
		t.Fatalf("got %q truncated=%v", c.String(), c.truncated)
	}
}

func TestGuestStderrIsBoundedAndStillRecognizesMemoryFailures(t *testing.T) {
	g := &guestStderr{cappedBuffer: cappedBuffer{max: 10}}
	for _, p := range []string{"first diagnostic\n", strings.Repeat("x", 4096), "Memory", "Error", "\n"} {
		n, err := g.Write([]byte(p))
		if err != nil || n != len(p) {
			t.Fatalf("stderr backpressure changed guest behavior: %d %v", n, err)
		}
	}
	if g.Len() != 10 || !strings.HasPrefix(g.String(), "first diag") || !strings.Contains(g.String(), "was dropped") || !g.memoryFailure {
		t.Fatalf("bounded diagnostics: retained=%d memory=%v output=%q", g.Len(), g.memoryFailure, g.String())
	}
	clean := &guestStderr{cappedBuffer: cappedBuffer{max: 100}}
	clean.Write([]byte("ordinary error\n"))
	if clean.String() != "ordinary error\n" || clean.memoryFailure || clean.truncated {
		t.Fatalf("ordinary diagnostics changed: %+v", clean)
	}
}

func TestTheBudgetStopsWhilePaused(t *testing.T) {
	fired := make(chan struct{}, 1)
	b := newBudget(150*time.Millisecond, func() { fired <- struct{}{} })
	defer b.stop()
	b.pause()
	select {
	case <-fired:
		t.Fatal("the clock ran while paused")
	case <-time.After(400 * time.Millisecond):
	}
	b.resume()
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("the clock did not run after resume")
	}
	if !b.expired() {
		t.Fatal("expired() did not report the expiry")
	}
}

func TestDefaultsApplyWhenTheManifestSaysNothing(t *testing.T) {
	if timeoutFor(0) != defaultTimeout || timeoutFor(7) != 7*time.Second {
		t.Fatal("timeoutFor")
	}
	if dispatchesFor(nil) != defaultDispatches || dispatchesFor(map[string]int{"max_dispatches": 3}) != 3 {
		t.Fatal("dispatchesFor")
	}
}

func TestAGuestThatRunsOutOfMemorySaysSoAndDoesNotFloodTheLog(t *testing.T) {
	_, err := runLimited(t, "main.sh", "", `x=a; i=0; while [ $i -lt 40 ]; do x="$x$x"; i=$((i+1)); done; echo built
`, Options{})
	if err == nil || !strings.Contains(err.Error(), "ran out of memory") {
		t.Fatalf("a guest that ran out of memory ended with: %v", err)
	}
	if got := clipLines(strings.Repeat("line\n", 100), 12); strings.Count(got, "\n") > 13 || !strings.Contains(got, "89 more lines") {
		t.Errorf("clipLines: %q", got)
	}
}
