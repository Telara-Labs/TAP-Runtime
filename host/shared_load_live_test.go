package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/bridge"
)

// These measure what a burst of agent sessions costs the machine: how many
// tap processes there are and how much memory they hold, sampled while the
// sessions start, search, and run a primitive. They read the person's real
// agent history, so they run only when asked.
var sharedLoad = flag.Bool("shared-load", false, "measure a burst of tap serve sessions against this machine's real agent history")
var sharedLoadBaseline = flag.String("shared-load-baseline", "", "a tap program to measure the same burst with, for comparison")
var sharedLoadSessions = flag.Int("shared-load-sessions", 40, "sessions started at once")
var sharedLoadBaselineSessions = flag.Int("shared-load-baseline-sessions", 10, "sessions started at once with the baseline program")
var sharedLoadOut = flag.String("shared-load-out", "", "directory for the samples (CSV) and the summary")
var sharedLoadAgents = flag.Int("shared-load-agents", 0, "also start this many real Claude Code and this many real Codex sessions at once, each running the primitive through its own MCP connection")
var sharedLoadLinger = flag.Duration("shared-load-linger", 5*time.Second, "keep sampling this long after every session answered, while the sessions stay open")
var sharedLoadCold = flag.Bool("shared-load-cold", true, "set the agent history caches aside first, as on a first start, and put them back after")

const loadManifest = `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.test, name: load-check, version: 0.1.0}
execution: {entrypoint: main.sh}
`

// loadSample is the tap processes of one program at one moment.
type loadSample struct {
	at       time.Duration
	procs    int
	runners  int
	rssMB    float64
	swapMB   float64
	answered int
	runnerMB float64
}

type loadResult struct {
	label      string
	sessions   int
	samples    []loadSample
	peakProcs  int
	peakRSS    float64
	peakRunner float64
	peakSwap   float64
	baseSwap   float64
	allDone    time.Duration
	failures   []string
	runnerSeen int
}

// sampleTap reads every process started from exe.
func sampleTap(exe string) (procs, runners int, rssMB, runnerMB float64) {
	out, err := exec.Command("ps", "-Ao", "rss=,args=").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		rss, args, ok := strings.Cut(line, " ")
		if !ok || !strings.HasPrefix(strings.TrimSpace(args), exe+" ") {
			continue
		}
		kb, _ := strconv.ParseFloat(rss, 64)
		procs++
		rssMB += kb / 1024
		if strings.HasPrefix(strings.TrimSpace(args), exe+" shared-runner") {
			runners++
			runnerMB += kb / 1024
		}
	}
	return
}

func swapUsedMB() float64 {
	out, err := exec.Command("sysctl", "-n", "vm.swapusage").Output()
	if err != nil {
		return 0
	}
	// total = 40960.00M  used = 33711.50M  free = 7248.50M  (encrypted)
	f := strings.Fields(string(out))
	for i := 0; i+2 < len(f); i++ {
		if f[i] == "used" {
			v, _ := strconv.ParseFloat(strings.TrimSuffix(f[i+2], "M"), 64)
			return v
		}
	}
	return 0
}

// setCachesAside moves the request caches out of the way so every session
// reads its agent's history from the start, and returns how to restore them.
func setCachesAside(t *testing.T) func() {
	t.Helper()
	base, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "tap-runtime")
	files, _ := filepath.Glob(filepath.Join(dir, "requests-*.json"))
	aside := t.TempDir()
	var moved [][2]string
	for _, f := range files {
		to := filepath.Join(aside, filepath.Base(f))
		if err := os.Rename(f, to); err != nil {
			t.Fatal(err)
		}
		moved = append(moved, [2]string{to, f})
	}
	return func() {
		// What the burst wrote is a fresh cache; the person's own goes back.
		for _, m := range moved {
			os.Rename(m[0], m[1])
		}
	}
}

// burst starts sessions sessions of exe at once, the way an agent that opens
// many sessions does, and samples until every one has answered a search and
// a run, or two minutes pass.
func burst(t *testing.T, label, exe string, sessions int, catalog string, identity map[string]any) loadResult {
	t.Helper()
	res := loadResult{label: label, sessions: sessions, baseSwap: swapUsedMB()}
	names := []string{"codex-mcp-client", "claude-code", "gemini-cli-mcp-client", "goose-cli", "cursor", "opencode", "kilo", "crush", "qwen-code", "copilot-cli",
		"windsurf", "cline", "roo", "zed", "continue", "amp", "aider", "antigravity", "cursor-cli", "vscode-copilot"}
	env := os.Environ()
	store := interpreterStore(t)
	var answered sync.WaitGroup
	var mu sync.Mutex
	done := 0
	start := time.Now()
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		tick := time.NewTicker(200 * time.Millisecond)
		defer tick.Stop()
		for {
			p, r, rss, rmb := sampleTap(exe)
			mu.Lock()
			s := loadSample{at: time.Since(start), procs: p, runners: r, rssMB: rss, swapMB: swapUsedMB(), answered: done, runnerMB: rmb}
			mu.Unlock()
			res.samples = append(res.samples, s)
			select {
			case <-stop:
				return
			case <-tick.C:
			}
		}
	}()
	clients := make([]*procClient, sessions)
	for i := range clients {
		dir := t.TempDir()
		clients[i] = startProcClient(t, exe, env, dir, names[i%len(names)], "--interpreters", store, "--catalog-root", catalog)
	}
	for i, c := range clients {
		answered.Add(1)
		go func() {
			defer answered.Done()
			fail := func(err error) {
				mu.Lock()
				res.failures = append(res.failures, fmt.Sprintf("session %d (%s): %v", i, c.name, err))
				mu.Unlock()
			}
			if err := c.initialize(); err != nil {
				fail(err)
				return
			}
			if _, err := c.call("tools/call", map[string]any{"name": "tap_search", "arguments": map[string]any{"query": "load check"}}); err != nil {
				fail(err)
				return
			}
			r, err := c.call("tools/call", map[string]any{"name": "tap_run", "arguments": identity})
			// A run that outlasts the call is collected with tap_result, as
			// a client does.
			for err == nil {
				run := heldRunID(fmt.Sprint(r["content"]))
				if run == "" {
					break
				}
				r, err = c.call("tools/call", map[string]any{"name": "tap_result", "arguments": map[string]any{"run": run}})
			}
			if err != nil {
				fail(err)
				return
			}
			if body := fmt.Sprint(r["content"]); !strings.Contains(body, "load-check-ok") {
				fail(fmt.Errorf("the run did not answer: %s", body))
				return
			}
			mu.Lock()
			done++
			mu.Unlock()
		}()
	}
	finished := make(chan struct{})
	go func() { answered.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(2 * time.Minute):
		res.failures = append(res.failures, "not every session answered within two minutes")
	}
	res.allDone = time.Since(start)
	// Keep sampling after: the cost of sessions that sit open.
	time.Sleep(*sharedLoadLinger)
	close(stop)
	<-sampled
	for _, s := range res.samples {
		res.peakProcs = max(res.peakProcs, s.procs)
		res.peakRSS = max(res.peakRSS, s.rssMB)
		res.peakRunner = max(res.peakRunner, s.runnerMB)
		res.peakSwap = max(res.peakSwap, s.swapMB)
		res.runnerSeen = max(res.runnerSeen, s.runners)
	}
	for _, c := range clients {
		c.in.Close()
	}
	return res
}

// agentBurst starts n real Claude Code and n real Codex sessions at once,
// each with exe as its MCP server the way tap install configures it, and has
// each call tap_search and tap_run through the client's own MCP connection
// (Claude Code's control channel, Codex's app-server). No model is called.
func agentBurst(t *testing.T, label, exe string, n int, catalog string, identity map[string]any) loadResult {
	t.Helper()
	claude, err := bridge.ClaudeExecutable()
	if err != nil {
		t.Fatalf("Claude Code: %v", err)
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Fatalf("Codex: %v", err)
	}
	res := loadResult{label: label, sessions: 2 * n, baseSwap: swapUsedMB()}
	var mu sync.Mutex
	done := 0
	start := time.Now()
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		tick := time.NewTicker(200 * time.Millisecond)
		defer tick.Stop()
		for {
			p, r, rss, rmb := sampleTap(exe)
			mu.Lock()
			res.samples = append(res.samples, loadSample{at: time.Since(start), procs: p, runners: r, rssMB: rss, swapMB: swapUsedMB(), answered: done, runnerMB: rmb})
			mu.Unlock()
			select {
			case <-stop:
				return
			case <-tick.C:
			}
		}
	}()
	serverArgs := []string{"serve", "--interpreters", interpreterStore(t), "--catalog-root", catalog}
	var wg sync.WaitGroup
	var callers []liveAcceptanceCaller
	for i := 0; i < 2*n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			kind := "claude"
			if i%2 == 1 {
				kind = "codex"
			}
			fail := func(err error) {
				mu.Lock()
				res.failures = append(res.failures, fmt.Sprintf("%s %d: %v", kind, i, err))
				mu.Unlock()
			}
			work, _ := filepath.EvalSymlinks(t.TempDir())
			var c liveAcceptanceCaller
			var err error
			if kind == "claude" {
				c, err = startClaudeAcceptance(t, claude, exe, serverArgs, work, io.Discard, "tap", "", "")
			} else {
				c, err = startCodexAcceptance(t, exe, serverArgs, work, io.Discard, "tap", false)
			}
			if err != nil {
				fail(err)
				return
			}
			mu.Lock()
			callers = append(callers, c)
			mu.Unlock()
			if _, err := c.Call("tap_search", map[string]any{"query": "load check"}); err != nil {
				fail(fmt.Errorf("tap_search: %w", err))
				return
			}
			out, err := c.Call("tap_run", identity)
			for err == nil && heldRunID(out) != "" {
				out, err = c.Call("tap_result", map[string]any{"run": heldRunID(out)})
			}
			if err != nil {
				fail(fmt.Errorf("tap_run: %w", err))
				return
			}
			if !strings.Contains(out, "load-check-ok") {
				fail(fmt.Errorf("the run did not answer: %s", out))
				return
			}
			mu.Lock()
			done++
			mu.Unlock()
		}()
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(4 * time.Minute):
		res.failures = append(res.failures, "not every session answered within four minutes")
	}
	res.allDone = time.Since(start)
	time.Sleep(5 * time.Second)
	close(stop)
	<-sampled
	for _, s := range res.samples {
		res.peakProcs = max(res.peakProcs, s.procs)
		res.peakRSS = max(res.peakRSS, s.rssMB)
		res.peakRunner = max(res.peakRunner, s.runnerMB)
		res.peakSwap = max(res.peakSwap, s.swapMB)
		res.runnerSeen = max(res.runnerSeen, s.runners)
	}
	for _, c := range callers {
		c.Close()
	}
	return res
}

var heldRunPattern = regexp.MustCompile(`Call tap_result with run "([^"]+)"`)

// heldRunID is the run tap_run handed back while it was still running.
func heldRunID(text string) string {
	if m := heldRunPattern.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return ""
}

func writeLoad(t *testing.T, dir string, results []loadResult) {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	os.MkdirAll(dir, 0o700)
	var summary strings.Builder
	fmt.Fprintf(&summary, "%-16s %8s %10s %12s %16s %14s %14s %8s %s\n", "program", "sessions", "peak procs", "peak RSS MB", "runner peak MB", "swap growth MB", "all answered", "runners", "failures")
	for _, r := range results {
		f, err := os.Create(filepath.Join(dir, "samples-"+r.label+".csv"))
		if err != nil {
			t.Fatal(err)
		}
		w := csv.NewWriter(f)
		w.Write([]string{"ms", "tap_processes", "shared_runners", "tap_rss_mb", "runner_rss_mb", "swap_used_mb", "sessions_answered"})
		for _, s := range r.samples {
			w.Write([]string{strconv.FormatInt(s.at.Milliseconds(), 10), strconv.Itoa(s.procs), strconv.Itoa(s.runners),
				strconv.FormatFloat(s.rssMB, 'f', 1, 64), strconv.FormatFloat(s.runnerMB, 'f', 1, 64), strconv.FormatFloat(s.swapMB, 'f', 1, 64), strconv.Itoa(s.answered)})
		}
		w.Flush()
		f.Close()
		fmt.Fprintf(&summary, "%-16s %8d %10d %12.0f %16.0f %14.0f %14s %8d %d\n", r.label, r.sessions, r.peakProcs, r.peakRSS, r.peakRunner, r.peakSwap-r.baseSwap, r.allDone.Round(100*time.Millisecond), r.runnerSeen, len(r.failures))
		sort.Strings(r.failures)
		for _, f := range r.failures {
			fmt.Fprintf(&summary, "  %s\n", f)
		}
	}
	os.WriteFile(filepath.Join(dir, "summary.txt"), []byte(summary.String()), 0o600)
	t.Logf("samples in %s\n%s", dir, summary.String())
}

func TestSharedRunnerLoad(t *testing.T) {
	if !*sharedLoad {
		t.Skip("pass -shared-load to measure a burst of sessions against this machine's agent history")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("samples processes with ps")
	}
	exe := buildTap(t)
	catalog := t.TempDir()
	os.MkdirAll(filepath.Join(catalog, "load-check"), 0o700)
	os.WriteFile(filepath.Join(catalog, "load-check", "primitive.yaml"), []byte(loadManifest), 0o600)
	os.WriteFile(filepath.Join(catalog, "load-check", "main.sh"), []byte("echo load-check-ok\n"), 0o600)
	digest, m, err := packageDigest(filepath.Join(catalog, "load-check"))
	if err != nil {
		t.Fatal(err)
	}
	identity := map[string]any{"ref": m.Metadata.Publisher + "/" + m.Metadata.Name + "@" + m.Metadata.Version, "digest": digest}
	// The person's own trust store is used, as an agent's session would. The
	// package is the test's own and only prints a line; it is trusted for
	// this test and the record removed after.
	if err := newTrustStore().add(digest, m.Metadata.Name, filepath.Join(catalog, "load-check")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { untrustDigest(t, digest) })

	var results []loadResult
	if *sharedLoadBaseline != "" {
		restore := func() {}
		if *sharedLoadCold {
			restore = setCachesAside(t)
		}
		results = append(results, burst(t, "baseline", *sharedLoadBaseline, *sharedLoadBaselineSessions, catalog, identity))
		restore()
		time.Sleep(10 * time.Second) // let the machine settle between bursts
	}
	restore := func() {}
	if *sharedLoadCold {
		restore = setCachesAside(t)
	}
	results = append(results, burst(t, "shared", exe, *sharedLoadSessions, catalog, identity))
	restore()
	if *sharedLoadAgents > 0 {
		if *sharedLoadBaseline != "" {
			time.Sleep(10 * time.Second)
			results = append(results, agentBurst(t, "agents-baseline", *sharedLoadBaseline, *sharedLoadAgents, catalog, identity))
		}
		time.Sleep(10 * time.Second)
		results = append(results, agentBurst(t, "agents-shared", exe, *sharedLoadAgents, catalog, identity))
	}
	if pid := runnerPIDReal(); pid > 0 {
		if p, err := os.FindProcess(pid); err == nil {
			p.Kill()
		}
	}
	writeLoad(t, *sharedLoadOut, results)
	for _, r := range results {
		if len(r.failures) > 0 {
			t.Errorf("%s: %d sessions failed", r.label, len(r.failures))
		}
	}
	for _, r := range results {
		if strings.HasSuffix(r.label, "shared") && r.runnerSeen != 1 {
			t.Errorf("%s: %d shared runners seen, want 1", r.label, r.runnerSeen)
		}
	}
}

// runnerPIDReal is the test-built runner in the person's own cache.
func runnerPIDReal() int {
	base, err := os.UserCacheDir()
	if err != nil {
		return 0
	}
	locks, _ := filepath.Glob(filepath.Join(base, "tap-runtime", "shared", "r-*.lock"))
	newest, pid := time.Time{}, 0
	for _, l := range locks {
		info, err := os.Stat(l)
		if err != nil || info.ModTime().Before(newest) {
			continue
		}
		b, _ := os.ReadFile(l)
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && n > 0 {
			newest, pid = info.ModTime(), n
		}
	}
	return pid
}

// untrustDigest removes the test package's record from the trust store.
func untrustDigest(t *testing.T, digest string) {
	t.Helper()
	store := newTrustStore()
	store.mu.Lock()
	defer store.mu.Unlock()
	all := store.load()
	if _, ok := all[digest]; !ok {
		return
	}
	delete(all, digest)
	b, _ := json.MarshalIndent(all, "", "  ")
	if err := os.WriteFile(store.path, append(b, '\n'), 0o600); err != nil {
		t.Logf("could not remove the test package's trust record %s: %v", digest, err)
	}
}
