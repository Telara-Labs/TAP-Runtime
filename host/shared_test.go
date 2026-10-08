package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// procClient is an agent session that starts tap serve the way an agent
// does: as a program, talking over its standard input and output.
type procClient struct {
	t      *testing.T
	name   string
	cmd    *exec.Cmd
	in     io.WriteCloser
	sc     *bufio.Scanner
	next   int
	asked  []string // the elicitations this session was shown
	errout strings.Builder
}

func startProcClient(t *testing.T, exe string, env []string, dir, name string, args ...string) *procClient {
	t.Helper()
	cmd := exec.Command(exe, append([]string{"serve"}, args...)...)
	cmd.Dir, cmd.Env = dir, env
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	c := &procClient{t: t, name: name, cmd: cmd, in: in, sc: bufio.NewScanner(out)}
	cmd.Stderr = &lockedBuilder{b: &c.errout}
	c.sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { in.Close(); cmd.Wait() })
	return c
}

type lockedBuilder struct {
	mu sync.Mutex
	b  *strings.Builder
}

func (l *lockedBuilder) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// call sends a request and answers every elicitation with yes until the
// request's own answer arrives. It returns an error message instead of
// failing, so goroutines can use it.
func (c *procClient) call(method string, params any) (map[string]any, error) {
	c.next++
	id := c.next
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	for c.sc.Scan() {
		var m map[string]any
		if json.Unmarshal(c.sc.Bytes(), &m) != nil {
			continue
		}
		if m["method"] == "elicitation/create" {
			p, _ := m["params"].(map[string]any)
			msg, _ := p["message"].(string)
			c.asked = append(c.asked, msg)
			a, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": map[string]any{"action": "accept", "content": map[string]any{"approve": true, "limit": 5}}})
			c.in.Write(append(a, '\n'))
			continue
		}
		if fmt.Sprint(m["id"]) != strconv.Itoa(id) {
			continue
		}
		if e, ok := m["error"].(map[string]any); ok {
			return nil, fmt.Errorf("%s: %v", method, e["message"])
		}
		r, _ := m["result"].(map[string]any)
		return r, nil
	}
	return nil, fmt.Errorf("%s: the server closed (%v); stderr: %s", method, c.sc.Err(), c.errout.String())
}

func (c *procClient) initialize() error {
	_, err := c.call("initialize", map[string]any{"protocolVersion": "2025-06-18",
		"capabilities": map[string]any{"elicitation": map[string]any{"form": map[string]any{}}},
		"clientInfo":   map[string]any{"name": c.name, "version": "1"}})
	return err
}

// buildTap builds the tap program into a directory of the test's own.
func buildTap(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "tap")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", exe, "./host")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building tap: %v\n%s", err, out)
	}
	return exe
}

// buildTapWithRelay builds tap as a release does on this platform: with the
// small relay built into it.
func buildTapWithRelay(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows builds carry no relay")
	}
	// As a release does: the relay goes in through an overlay, never into
	// the source, where builds running at the same time would meet.
	dir := t.TempDir()
	relay := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", filepath.Join(dir, "tap-relay"), "./cmd/tap-relay")
	relay.Dir = repoRoot
	relay.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0")
	if out, err := relay.CombinedOutput(); err != nil {
		t.Fatalf("building the relay: %v\n%s", err, out)
	}
	overlay, _ := json.Marshal(map[string]any{"Replace": map[string]string{filepath.Join(repoRoot, "host", "relaybin", "tap-relay"): filepath.Join(dir, "tap-relay")}})
	os.WriteFile(filepath.Join(dir, "overlay.json"), overlay, 0o600)
	exe := filepath.Join(t.TempDir(), "tap")
	cmd := exec.Command("go", "build", "-tags", "relayembed", "-overlay", filepath.Join(dir, "overlay.json"), "-o", exe, "./host")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building tap: %v\n%s", err, out)
	}
	return exe
}

// isolatedHome is an environment whose home, and so whose cache and
// configuration directories, belong to the test.
func isolatedHome(t *testing.T) (string, []string) {
	t.Helper()
	home, _ := filepath.EvalSymlinks(t.TempDir())
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "LOCALAPPDATA", "APPDATA", "USERPROFILE":
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "HOME="+home, "USERPROFILE="+home,
		"LOCALAPPDATA="+filepath.Join(home, "AppData", "Local"), "APPDATA="+filepath.Join(home, "AppData", "Roaming"))
	return home, env
}

// runnerPID is the shared runner's process, from the lock file it holds.
func runnerPID(t *testing.T, home string) int {
	t.Helper()
	dirs := []string{filepath.Join(home, "Library", "Caches"), filepath.Join(home, ".cache"), filepath.Join(home, "AppData", "Local")}
	for _, d := range dirs {
		locks, _ := filepath.Glob(filepath.Join(d, "tap-runtime", "shared", "r-*.lock"))
		for _, l := range locks {
			b, _ := os.ReadFile(l)
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				return pid
			}
		}
	}
	return 0
}

func killRunner(home string, t *testing.T) {
	if pid := runnerPID(t, home); pid > 0 {
		if p, err := os.FindProcess(pid); err == nil {
			p.Kill()
		}
	}
}

// countRunners counts the shared runners started from exe.
func countRunners(t *testing.T, exe string) int {
	t.Helper()
	out, _ := exec.Command("ps", "-Ao", "args").Output()
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, exe+" shared-runner") {
			n++
		}
	}
	return n
}

// Every agent session starts its own tap serve. Started together, twenty of
// them are answered by one runner, and each session's primitive runs in that
// session's own folder, with its approval asked of that session only.
func TestManySessionsShareOneRunner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("counts processes with ps")
	}
	exe := buildTap(t)
	home, env := isolatedHome(t)
	t.Cleanup(func() { killRunner(home, t) })
	store := interpreterStore(t)
	catalog := t.TempDir()
	pkg := writePackage(t, writeManifest, writeScript)
	for _, name := range []string{"primitive.yaml", "main.sh"} {
		b, _ := os.ReadFile(filepath.Join(pkg, name))
		os.MkdirAll(filepath.Join(catalog, "writer"), 0o700)
		os.WriteFile(filepath.Join(catalog, "writer", name), b, 0o600)
	}
	digest, m, err := packageDigest(filepath.Join(catalog, "writer"))
	if err != nil {
		t.Fatal(err)
	}
	identity := map[string]any{"ref": m.Metadata.Publisher + "/" + m.Metadata.Name + "@" + m.Metadata.Version, "digest": digest}

	names := []string{"claude-code", "codex-mcp-client", "gemini-cli-mcp-client", "goose-cli", "cursor", "opencode", "kilo", "crush", "qwen-code", "copilot-cli"}
	const sessions = 20
	clients := make([]*procClient, sessions)
	dirs := make([]string, sessions)
	for i := range clients {
		dirs[i], _ = filepath.EvalSymlinks(t.TempDir())
		os.MkdirAll(filepath.Join(dirs[i], "out"), 0o700)
		clients[i] = startProcClient(t, exe, env, dirs[i], names[i%len(names)], "--interpreters", store, "--catalog-root", catalog)
	}
	var wg sync.WaitGroup
	errs := make([]error, sessions)
	// Half the sessions run the primitive; the other half only search, and
	// must never be shown another session's approval.
	for i, c := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if errs[i] = c.initialize(); errs[i] != nil {
				return
			}
			if _, errs[i] = c.call("tools/call", map[string]any{"name": "tap_search", "arguments": map[string]any{"query": "writer"}}); errs[i] != nil {
				return
			}
			if i%2 == 0 {
				r, err := c.call("tools/call", map[string]any{"name": "tap_run", "arguments": identity})
				if err != nil {
					errs[i] = err
				} else if body := fmt.Sprint(r["content"]); !strings.Contains(body, "a written") {
					errs[i] = fmt.Errorf("the run did not write: %s", body)
				}
			}
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("session %d (%s): %v", i, clients[i].name, err)
		}
	}
	if n := countRunners(t, exe); n != 1 {
		t.Fatalf("%d shared runners are running, want 1", n)
	}
	for i, c := range clients {
		_, err := os.Stat(filepath.Join(dirs[i], "out", "a.txt"))
		if i%2 == 0 {
			if err != nil {
				t.Errorf("session %d ran in its folder but its file is not there: %v", i, err)
			}
			if len(c.asked) == 0 {
				t.Errorf("session %d was never asked to approve its own write", i)
			}
		} else {
			if err == nil {
				t.Errorf("session %d ran nothing, but a file was written in its folder", i)
			}
			if len(c.asked) != 0 {
				t.Errorf("session %d was shown another session's approval: %q", i, c.asked)
			}
		}
	}
}

// When the runner stops while sessions are open, each session's next call
// starts a new one and the session goes on.
func TestASessionSurvivesTheRunnerStopping(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("kills by process id")
	}
	exe := buildTap(t)
	home, env := isolatedHome(t)
	t.Cleanup(func() { killRunner(home, t) })
	dir := t.TempDir()
	c := startProcClient(t, exe, env, dir, "claude-code", "--interpreters", interpreterStore(t), "--catalog-root", t.TempDir())
	if err := c.initialize(); err != nil {
		t.Fatal(err)
	}
	first := runnerPID(t, home)
	if first == 0 {
		t.Fatalf("no runner; stderr: %s", c.errout.String())
	}
	if p, err := os.FindProcess(first); err == nil {
		p.Kill()
	}
	time.Sleep(300 * time.Millisecond)
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if _, err = c.call("tools/list", map[string]any{}); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("the session did not go on after the runner stopped: %v", err)
	}
	if second := runnerPID(t, home); second == 0 || second == first {
		t.Fatalf("no new runner: was %d, now %d", first, second)
	}
}

// A session started with flags that cannot be shared answers in its own
// process, as tap serve always has.
func TestSharedEligible(t *testing.T) {
	for args, want := range map[string]bool{
		"--interpreters x --name tap":   true,
		"--config-dir /x":               false,
		"-config-dir=/x":                false,
		"--http-listen 127.0.0.1:1":     false,
		"--own-process":                 false,
		"--catalog-root --own-process2": true,
	} {
		if got := sharedEligible(strings.Fields(args)); got != want {
			t.Errorf("%q: %v, want %v", args, got, want)
		}
	}
}

// A release's tap serve becomes the small relay: each session a client keeps
// open is a relay of a few megabytes, and the work is done by the runner.
func TestSessionsBecomeTheSmallRelay(t *testing.T) {
	exe := buildTapWithRelay(t)
	home, env := isolatedHome(t)
	t.Cleanup(func() { killRunner(home, t) })
	const sessions = 10
	clients := make([]*procClient, sessions)
	for i := range clients {
		clients[i] = startProcClient(t, exe, env, t.TempDir(), "claude-code", "--interpreters", interpreterStore(t), "--catalog-root", t.TempDir())
	}
	for i, c := range clients {
		if err := c.initialize(); err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
		if _, err := c.call("tools/call", map[string]any{"name": "tap_search", "arguments": map[string]any{"query": "anything"}}); err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
	}
	out, err := exec.Command("ps", "-Ao", "rss=,args=").Output()
	if err != nil {
		t.Fatal(err)
	}
	relays, full := 0, 0
	for _, line := range strings.Split(string(out), "\n") {
		rss, args, _ := strings.Cut(strings.TrimSpace(line), " ")
		args = strings.TrimSpace(args)
		switch {
		case strings.Contains(args, filepath.Join(home, "")) && strings.Contains(args, "/tap-relay-") && strings.Contains(args, "--runner "+exe):
			relays++
			if kb, _ := strconv.Atoi(rss); kb > 12*1024 {
				t.Errorf("a relay holds %d KB: %s", kb, args)
			}
		case strings.HasPrefix(args, exe+" serve"):
			full++
		}
	}
	if relays != sessions || full != 0 {
		t.Fatalf("%d small relays and %d full tap serve processes for %d sessions", relays, full, sessions)
	}
	if n := countRunners(t, exe); n != 1 {
		t.Fatalf("%d runners, want 1", n)
	}
}
