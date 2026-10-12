package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/bind"
)

// stepper is a real compiled guest that sends the requests its first argument
// lists, and returns what the runner answered. It lets a test choose exactly
// what a primitive asks for, in what order, and whether the requests are in
// flight together.
const stepperSource = `package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

type script struct {
	Steps  []map[string]any ` + "`json:\"steps\"`" + `
	Seq    bool             ` + "`json:\"seq\"`" + `
	Print  *string          ` + "`json:\"print\"`" + `
	Exit   int              ` + "`json:\"exit\"`" + `
	Stderr string           ` + "`json:\"stderr\"`" + `
}

func main() {
	var sc script
	if len(os.Args) > 1 {
		json.Unmarshal([]byte(os.Args[1]), &sc)
	}
	enc := json.NewEncoder(os.Stdout)
	rd := bufio.NewReaderSize(os.Stdin, 1<<20)
	read := func() map[string]any {
		line, err := rd.ReadBytes('\n')
		if err != nil && len(line) == 0 {
			os.Exit(3)
		}
		var r map[string]any
		json.Unmarshal(line, &r)
		return r
	}
	var steps []map[string]any
	for _, s := range sc.Steps {
		n := 1
		if r, ok := s["repeat"].(float64); ok {
			n = int(r)
			delete(s, "repeat")
		}
		for i := 0; i < n; i++ {
			c := map[string]any{}
			for k, v := range s {
				c[k] = v
			}
			steps = append(steps, c)
		}
	}
	replies := make([]map[string]any, len(steps))
	index := map[string]int{}
	for i, s := range steps {
		id := fmt.Sprintf("s%d", i)
		s["id"] = id
		index[id] = i
		enc.Encode(s)
		if sc.Seq {
			r := read()
			replies[index[r["id"].(string)]] = r
		}
	}
	if !sc.Seq {
		for range steps {
			r := read()
			replies[index[r["id"].(string)]] = r
		}
	}
	out := ""
	if sc.Print != nil {
		out = *sc.Print
	} else {
		b, _ := json.Marshal(map[string]any{"replies": replies})
		out = string(b)
	}
	enc.Encode(map[string]any{"method": "return", "stdout": out + "\n", "stderr": sc.Stderr, "exit": sc.Exit})
}
`

var (
	stepperOnce sync.Once
	stepperWasm []byte
	stepperErr  error
)

func stepperModule(t *testing.T) []byte {
	t.Helper()
	stepperOnce.Do(func() {
		work, err := os.MkdirTemp("", "tap-stepper-")
		if err != nil {
			stepperErr = err
			return
		}
		src := filepath.Join(work, "main.go")
		if stepperErr = os.WriteFile(src, []byte(stepperSource), 0o600); stepperErr != nil {
			return
		}
		bin := filepath.Join(work, "main.wasm")
		cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", bin, src)
		cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
		if out, err := cmd.CombinedOutput(); err != nil {
			stepperErr = fmt.Errorf("%v\n%s", err, out)
			return
		}
		stepperWasm, stepperErr = os.ReadFile(bin)
	})
	if stepperErr != nil {
		t.Fatalf("building the test guest: %v", stepperErr)
	}
	return stepperWasm
}

// pkgSpec is a package for these tests: a name, how it runs, and the rest of
// its manifest.
type pkgSpec struct {
	name string
	exec string // more of the execution block, such as ", timeoutSeconds: 2"
	rest string // top-level manifest lines
}

func (p pkgSpec) write(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	manifest := fmt.Sprintf("apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.test, name: %s, version: 0.1.0}\nexecution: {entrypoint: main.wasm%s}\n%s", p.name, p.exec, p.rest)
	if err := os.WriteFile(filepath.Join(dir, "primitive.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.wasm"), stepperModule(t), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// use is the manifest line that pins the package in dir under an alias.
func use(t *testing.T, alias, dir, effect string) string {
	t.Helper()
	digest, m, err := packageDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("  - {alias: %s, ref: %s/%s, version: %s, digest: %s, effect: %s}\n", alias, m.Metadata.Publisher, m.Metadata.Name, m.Metadata.Version, digest, effect)
}

func catalogOf(t *testing.T, dirs ...string) func() ([]catalogEntry, error) {
	t.Helper()
	var entries []catalogEntry
	for _, d := range dirs {
		e, err := readCatalogEntry(d, "test")
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, e)
	}
	return func() ([]catalogEntry, error) { return entries, nil }
}

// lockedBridge is the client double, safe for the runs of a tree to share.
type lockedBridge struct {
	*fakeBridge
	mu sync.Mutex
}

func (l *lockedBridge) Call(t bind.Tool, args map[string]any) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fakeBridge.Call(t, args)
}

func (l *lockedBridge) made(tool string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, c := range l.calls {
		if strings.HasSuffix(c, "/"+tool) {
			n++
		}
	}
	return n
}

func gmailBridge() *lockedBridge { return &lockedBridge{fakeBridge: gmail()} }

const (
	usesSearch = "  - {alias: search, capability: gmail.threads.search, effect: read}\n"
	usesDraft  = "  - {alias: draft, capability: gmail.drafts.create, effect: write}\n"
)

// script encodes a stepper's input.
func script(t *testing.T, v map[string]any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func steps(s ...map[string]any) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func runStep(alias string, input map[string]any) map[string]any {
	return map[string]any{"method": "run", "alias": alias, "input": input}
}

type useRun struct {
	pkg, runs string
	catalog   func() ([]catalogEntry, error)
	bridge    *lockedBridge
	approve   Approver
	args      []string
}

func (u useRun) options(t *testing.T) Options {
	o := Options{Package: u.pkg, Args: u.args, Journal: io.Discard, CacheDir: sharedCompileCache(t), RunsDir: u.runs, catalog: u.catalog, Approve: u.approve}
	if u.bridge != nil {
		o.Bridge = u.bridge
	}
	return o
}

var compileCache = struct {
	sync.Mutex
	dir string
}{}

func sharedCompileCache(t *testing.T) string {
	compileCache.Lock()
	defer compileCache.Unlock()
	if compileCache.dir == "" {
		d, err := os.MkdirTemp("", "tap-cache-")
		if err != nil {
			t.Fatal(err)
		}
		compileCache.dir = d
	}
	return compileCache.dir
}

// replies are what the stepper reported the runner answered.
func repliesOf(t *testing.T, res *Result) []map[string]any {
	t.Helper()
	var out struct {
		Replies []map[string]any `json:"replies"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &out); err != nil {
		t.Fatalf("the guest returned %q (stderr %q): %v", res.Stdout, res.Stderr, err)
	}
	return out.Replies
}

func str(v any) string { s, _ := v.(string); return s }

func approveAll(t *testing.T) Approver {
	return func(Ask) Grant { return Grant{OK: true, Limit: Unlimited} }
}

// journalLines reads the index of a run.
func journalLines(t *testing.T, runs, id string) []map[string]any {
	t.Helper()
	f, err := os.Open(filepath.Join(runs, id, "index.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func headerOf(t *testing.T, runs, id string) map[string]any {
	t.Helper()
	for _, l := range journalLines(t, runs, id) {
		if l["phase"] == "header" {
			h, _ := l["header"].(map[string]any)
			return h
		}
	}
	t.Fatalf("run %s has no header", id)
	return nil
}

// runDirs lists the runs recorded, by id.
func runDirs(t *testing.T, runs string) []string {
	t.Helper()
	entries, _ := os.ReadDir(runs)
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// rootRun is the one run nobody else started.
func rootRun(t *testing.T, runs string) string {
	t.Helper()
	var roots []string
	for _, id := range runDirs(t, runs) {
		if str(headerOf(t, runs, id)["parent_run_id"]) == "" {
			roots = append(roots, id)
		}
	}
	if len(roots) != 1 {
		t.Fatalf("want one root run, have %v", roots)
	}
	return roots[0]
}

// 1
func TestAPrimitiveRunsAPinnedPrimitiveAndGetsItsOutput(t *testing.T) {
	inDir(t)
	kid := pkgSpec{name: "kid"}.write(t)
	parent := pkgSpec{name: "boss", rest: "uses:\n" + use(t, "kid", kid, "read")}.write(t)
	runs := t.TempDir()
	in := script(t, map[string]any{"steps": steps(runStep("kid", map[string]any{"print": `{"answer":42}`}))})
	res, err := Run(context.Background(), useRun{pkg: parent, runs: runs, catalog: catalogOf(t, kid), args: []string{in}}.options(t))
	if err != nil {
		t.Fatal(err)
	}
	rp := repliesOf(t, res)
	if len(rp) != 1 || str(rp[0]["result"]) != `{"answer":42}` || rp[0]["exit"].(float64) != 0 || str(rp[0]["refused"]) != "" {
		t.Fatalf("the parent was told %v", rp)
	}
	if len(res.Children) != 1 || res.Children[0].Alias != "kid" || res.Children[0].RunID == "" {
		t.Fatalf("the result does not show the child: %+v", res.Children)
	}
	if res.Calls != 0 {
		t.Fatalf("the child's calls were counted as the parent's: %d", res.Calls)
	}
	var link string
	for _, l := range journalLines(t, runs, res.RunID) {
		if l["phase"] == "begin" && l["method"] == "run" {
			link = str(l["child_run_id"])
		}
	}
	if link == "" || link != res.Children[0].RunID {
		t.Fatalf("the parent's run record does not name the child: %q", link)
	}
	h := headerOf(t, runs, link)
	if str(h["parent_run_id"]) != res.RunID || str(h["parent_request_id"]) != "s0" {
		t.Fatalf("the child's header does not name its parent: %v", h)
	}
}

// The package under test declares a loopback origin; this serves it and
// counts what reaches it.
func countingServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// 2
func TestAChildCannotUseItsParentsAuthority(t *testing.T) {
	inDir(t)
	srv, hits := countingServer(t)
	bridge := gmailBridge()
	kid := pkgSpec{name: "kid"}.write(t) // declares nothing
	parent := pkgSpec{name: "boss", rest: "tools:\n" + usesDraft + "fetch:\n  - {origin: \"" + srv.URL + "\", methods: [GET, POST]}\nuses:\n" + use(t, "kid", kid, "read")}.write(t)
	in := script(t, map[string]any{"steps": steps(runStep("kid", map[string]any{"steps": steps(
		map[string]any{"method": "call", "alias": "draft", "arguments": map[string]any{}},
		map[string]any{"method": "fetch", "url": srv.URL + "/x", "http_method": "GET"},
	), "seq": true}))})
	res, err := Run(context.Background(), useRun{pkg: parent, runs: t.TempDir(), catalog: catalogOf(t, kid), bridge: bridge, approve: approveAll(t), args: []string{in}}.options(t))
	if err != nil {
		t.Fatal(err)
	}
	outer := repliesOf(t, res)
	var inner struct {
		Replies []map[string]any `json:"replies"`
	}
	if err := json.Unmarshal([]byte(str(outer[0]["result"])), &inner); err != nil || len(inner.Replies) != 2 {
		t.Fatalf("the child's output was %q: %v", outer[0]["result"], err)
	}
	for i, r := range inner.Replies {
		if str(r["refused"]) == "" {
			t.Errorf("request %d of the child was not refused: %v", i, r)
		}
	}
	if hits.Load() != 0 || bridge.made("create_draft") != 0 {
		t.Fatalf("the parent's authority reached the child: %d fetches, %d writes", hits.Load(), bridge.made("create_draft"))
	}
}

// 3
func TestAParentCannotUseAChildsAuthority(t *testing.T) {
	inDir(t)
	srv, hits := countingServer(t)
	kid := pkgSpec{name: "kid", rest: "fetch:\n  - {origin: \"" + srv.URL + "\"}\n"}.write(t)
	parent := pkgSpec{name: "boss", rest: "uses:\n" + use(t, "kid", kid, "read")}.write(t)
	fetch := map[string]any{"method": "fetch", "url": srv.URL + "/x", "http_method": "GET"}
	in := script(t, map[string]any{"seq": true, "steps": steps(
		runStep("kid", map[string]any{"steps": steps(fetch)}),
		fetch,
	)})
	res, err := Run(context.Background(), useRun{pkg: parent, runs: t.TempDir(), catalog: catalogOf(t, kid), approve: approveAll(t), args: []string{in}}.options(t))
	if err != nil {
		t.Fatal(err)
	}
	rp := repliesOf(t, res)
	if str(rp[0]["refused"]) != "" || hits.Load() != 1 {
		t.Fatalf("the child could not use what it declares: %v, %d fetches", rp[0], hits.Load())
	}
	if str(rp[1]["refused"]) == "" || hits.Load() != 1 {
		t.Fatalf("the parent used an origin only its child declares: %v, %d fetches", rp[1], hits.Load())
	}
}

// 4
func TestAnUnderDeclaredUseEffectIsRefusedAtAdmission(t *testing.T) {
	inDir(t)
	bridge := gmailBridge()
	kid := pkgSpec{name: "kid", rest: "tools:\n" + usesDraft}.write(t)
	parent := pkgSpec{name: "boss", rest: "uses:\n" + use(t, "kid", kid, "read")}.write(t)
	runs := t.TempDir()
	_, err := Run(context.Background(), useRun{pkg: parent, runs: runs, catalog: catalogOf(t, kid), bridge: bridge, args: []string{"{}"}}.options(t))
	if err == nil || !strings.Contains(err.Error(), "kid is declared read but dev.test/kid can write (tool gmail.drafts.create)") || !strings.Contains(err.Error(), "Declare effect: write.") {
		t.Fatalf("an under-declared effect was not refused as the spec says: %v", err)
	}
	if len(runDirs(t, runs)) != 0 || len(bridge.calls) != 0 {
		t.Fatalf("something started before the refusal: runs %v, calls %v", runDirs(t, runs), bridge.calls)
	}
	// A child that uses a primitive that can write is as much a writer.
	inner := pkgSpec{name: "inner", rest: "tools:\n" + usesDraft}.write(t)
	middle := pkgSpec{name: "middle", rest: "uses:\n" + use(t, "inner", inner, "write")}.write(t)
	top := pkgSpec{name: "top", rest: "uses:\n" + use(t, "middle", middle, "read")}.write(t)
	_, err = Run(context.Background(), useRun{pkg: top, runs: runs, catalog: catalogOf(t, inner, middle), bridge: bridge, args: []string{"{}"}}.options(t))
	if err == nil || !strings.Contains(err.Error(), "middle is declared read but dev.test/middle can write (use inner)") {
		t.Fatalf("an effect reached through a use was not counted: %v", err)
	}
}

// 5
func TestAMissingPinnedChildIsRefusedBeforeAnythingRuns(t *testing.T) {
	inDir(t)
	runs := t.TempDir()
	kid := pkgSpec{name: "kid"}.write(t)
	parent := pkgSpec{name: "boss", rest: "uses:\n" + use(t, "kid", kid, "read")}.write(t)
	_, err := Run(context.Background(), useRun{pkg: parent, runs: runs, catalog: catalogOf(t), args: []string{"{}"}}.options(t))
	if err == nil || !strings.Contains(err.Error(), "kid needs dev.test/kid@0.1.0") || !strings.Contains(err.Error(), "which is not installed here. Save or pull that version first.") {
		t.Fatalf("a missing child was not refused as the spec says: %v", err)
	}
	// The child is installed; the one beneath it is not.
	grand := pkgSpec{name: "grand"}.write(t)
	mid := pkgSpec{name: "mid", rest: "uses:\n" + use(t, "grand", grand, "read")}.write(t)
	top := pkgSpec{name: "top", rest: "uses:\n" + use(t, "mid", mid, "read")}.write(t)
	_, err = Run(context.Background(), useRun{pkg: top, runs: runs, catalog: catalogOf(t, mid), args: []string{"{}"}}.options(t))
	if err == nil || !strings.Contains(err.Error(), "grand needs dev.test/grand") {
		t.Fatalf("a missing grandchild was not refused: %v", err)
	}
	if len(runDirs(t, runs)) != 0 {
		t.Fatalf("a run started before the refusal: %v", runDirs(t, runs))
	}
}

// 6
func TestAChangedChildDigestIsRefused(t *testing.T) {
	inDir(t)
	kid := pkgSpec{name: "kid"}.write(t)
	parent := pkgSpec{name: "boss", rest: "uses:\n" + use(t, "kid", kid, "read")}.write(t)
	// The same ref, different bytes: a newer build installed under it.
	changed := pkgSpec{name: "kid", rest: "files:\n  - {path: out, access: write}\n"}.write(t)
	_, err := Run(context.Background(), useRun{pkg: parent, runs: t.TempDir(), catalog: catalogOf(t, changed), args: []string{"{}"}}.options(t))
	if err == nil || !strings.Contains(err.Error(), "different one") {
		t.Fatalf("a re-pinned child was not refused: %v", err)
	}
	// And a package that is installed under the digest but is another one by name.
	other := pkgSpec{name: "other"}.write(t)
	entries, _ := catalogOf(t, other)()
	d, _, _ := packageDigest(kid)
	entries[0].Digest = d
	_, err = Run(context.Background(), useRun{pkg: parent, runs: t.TempDir(), catalog: func() ([]catalogEntry, error) { return entries, nil }, args: []string{"{}"}}.options(t))
	if err == nil || !strings.Contains(err.Error(), "not installed here") {
		t.Fatalf("a package of another name answered for the pin: %v", err)
	}
}

// 7
func TestCyclesAndDepthAreRefused(t *testing.T) {
	inDir(t)
	// A digest pins its content, so two packages cannot honestly pin each
	// other. The catalog is made to say so: a (whose entry claims a forged
	// digest) is pinned by b, and b by a.
	forged := strings.Repeat("ab", 32)
	b := pkgSpec{name: "bee", rest: fmt.Sprintf("uses:\n  - {alias: a, ref: dev.test/ay, version: 0.1.0, digest: %s, effect: read}\n", forged)}.write(t)
	a := pkgSpec{name: "ay", rest: "uses:\n" + use(t, "b", b, "read")}.write(t)
	entries, _ := catalogOf(t, a, b)()
	entries[0].Digest = forged
	cat := func() ([]catalogEntry, error) { return entries, nil }
	_, err := Run(context.Background(), useRun{pkg: a, runs: t.TempDir(), catalog: cat, args: []string{"{}"}}.options(t))
	if err == nil || !strings.Contains(err.Error(), "uses itself") {
		t.Fatalf("a cycle was not refused when run: %v", err)
	}
	if why := usesCheckDir(a, cat); !strings.Contains(why, "uses itself") {
		t.Fatalf("a cycle was not refused when saved: %q", why)
	}

	// Depth: four levels below the one asked for run; a fifth is refused.
	chain := func(levels int) (top string, cat func() ([]catalogEntry, error)) {
		var dirs []string
		prev := pkgSpec{name: "leaf"}.write(t)
		dirs = append(dirs, prev)
		for i := 1; i <= levels; i++ {
			prev = pkgSpec{name: fmt.Sprintf("level%d", i), rest: "uses:\n" + use(t, "next", prev, "read")}.write(t)
			dirs = append(dirs, prev)
		}
		return prev, catalogOf(t, dirs[:len(dirs)-1]...)
	}
	// root + 4 levels: root uses level... counted from the root, depth 4 is allowed.
	top4, cat4 := chain(4)
	if _, err := Run(context.Background(), useRun{pkg: top4, runs: t.TempDir(), catalog: cat4, args: []string{"{}"}}.options(t)); err != nil {
		t.Fatalf("four levels below the root were refused: %v", err)
	}
	top5, cat5 := chain(5)
	_, err = Run(context.Background(), useRun{pkg: top5, runs: t.TempDir(), catalog: cat5, args: []string{"{}"}}.options(t))
	if err == nil || !strings.Contains(err.Error(), "levels down") {
		t.Fatalf("five levels were not refused: %v", err)
	}
	if why := usesCheckDir(top5, cat5); !strings.Contains(why, "levels down") {
		t.Fatalf("depth was not refused when saved: %q", why)
	}
}

// 8
func TestTheTreeSharesOneDeadlineAndDispatchBudget(t *testing.T) {
	inDir(t)
	bridge := gmailBridge()
	kid := pkgSpec{name: "kid", rest: "tools:\n" + usesSearch}.write(t)
	boss := pkgSpec{name: "boss", rest: "uses:\n" + use(t, "kid", kid, "read")}.write(t)
	// limits belong to execution: rewrite the manifest with the root's limit.
	setExecution(t, boss, ", limits: {max_dispatches: 5}")
	loop := runStep("kid", map[string]any{"steps": steps(map[string]any{"method": "call", "alias": "search", "arguments": map[string]any{}, "repeat": 20}), "seq": true})
	res, err := Run(context.Background(), useRun{pkg: boss, runs: t.TempDir(), catalog: catalogOf(t, kid), bridge: bridge, args: []string{script(t, map[string]any{"steps": steps(loop)})}}.options(t))
	if err != nil {
		t.Fatal(err)
	}
	if n := bridge.made("search_threads"); n != 4 {
		t.Fatalf("the child made %d calls; the root allows 5 requests in all, one being the run", n)
	}
	if !strings.Contains(repliesOf(t, res)[0]["result"].(string), "max_dispatches") {
		t.Fatalf("the child was not told why it was stopped: %v", repliesOf(t, res)[0])
	}

	// A child cannot outlast the root's clock.
	sleeper := pkgSpec{name: "sleeper", rest: "commands:\n  - {command: sleep, args: [\"*\"], effect: read}\n"}.write(t)
	slow := pkgSpec{name: "slowboss", exec: ", timeoutSeconds: 1", rest: "uses:\n" + use(t, "sleeper", sleeper, "read")}.write(t)
	started := time.Now()
	_, err = Run(context.Background(), useRun{pkg: slow, runs: t.TempDir(), catalog: catalogOf(t, sleeper), args: []string{script(t, map[string]any{"steps": steps(runStep("sleeper", map[string]any{"steps": steps(map[string]any{"method": "exec", "command": "sleep", "args": []string{"30"}})}))})}}.options(t))
	if err == nil || !strings.Contains(err.Error(), "time limit") {
		t.Fatalf("the slow child outlasted the root's deadline: %v", err)
	}
	if took := time.Since(started); took > 10*time.Second {
		t.Fatalf("the run took %s to stop", took)
	}
}

// setExecution adds to the execution block of the manifest in dir.
func setExecution(t *testing.T, dir, more string) {
	t.Helper()
	p := filepath.Join(dir, "primitive.yaml")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(b), "entrypoint: main.wasm", "entrypoint: main.wasm"+more, 1)
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// 9
func TestAChildsApprovalNamesTheCallPathAndPausesTheRootBudget(t *testing.T) {
	inDir(t)
	bridge := gmailBridge()
	kid := pkgSpec{name: "kid", rest: "tools:\n" + usesDraft}.write(t)
	boss := pkgSpec{name: "boss", exec: ", timeoutSeconds: 2", rest: "uses:\n" + use(t, "kid", kid, "write")}.write(t)
	var mu sync.Mutex
	var asks []Ask
	approve := func(a Ask) Grant {
		mu.Lock()
		asks = append(asks, a)
		mu.Unlock()
		time.Sleep(3 * time.Second) // longer than the root's whole allowance
		return Grant{OK: true, Limit: Unlimited}
	}
	in := script(t, map[string]any{"steps": steps(runStep("kid", map[string]any{"steps": steps(map[string]any{"method": "call", "alias": "draft", "arguments": map[string]any{}})}))})
	res, err := Run(context.Background(), useRun{pkg: boss, runs: t.TempDir(), catalog: catalogOf(t, kid), bridge: bridge, approve: approve, args: []string{in}}.options(t))
	if err != nil {
		t.Fatalf("the root's clock ran while a person was being asked: %v", err)
	}
	if len(asks) != 1 || asks[0].Primitive != "boss › kid" || asks[0].Effect != "write" {
		t.Fatalf("the person was asked %+v", asks)
	}
	if bridge.made("create_draft") != 1 || len(res.Children) != 1 {
		t.Fatalf("the approved change was not made: %d, %+v", bridge.made("create_draft"), res.Children)
	}
}

// 10
func TestConcurrentChildrenThatBothAskDoNotDeadlock(t *testing.T) {
	inDir(t)
	bridge := gmailBridge()
	kid := pkgSpec{name: "kid", rest: "tools:\n" + usesDraft}.write(t)
	boss := pkgSpec{name: "boss", rest: "uses:\n" + use(t, "one", kid, "write") + use(t, "two", kid, "write")}.write(t)
	var inFlight, most atomic.Int32
	approve := func(Ask) Grant {
		n := inFlight.Add(1)
		for {
			m := most.Load()
			if n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(150 * time.Millisecond)
		inFlight.Add(-1)
		return Grant{OK: true, Limit: Unlimited}
	}
	draft := map[string]any{"steps": steps(map[string]any{"method": "call", "alias": "draft", "arguments": map[string]any{}})}
	in := script(t, map[string]any{"steps": steps(runStep("one", draft), runStep("two", draft))})
	done := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), useRun{pkg: boss, runs: t.TempDir(), catalog: catalogOf(t, kid), bridge: bridge, approve: approve, args: []string{in}}.options(t))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("two children that both asked a person deadlocked")
	}
	if bridge.made("create_draft") != 2 {
		t.Fatalf("both changes should have been approved and made: %d", bridge.made("create_draft"))
	}
	if most.Load() != 1 {
		t.Fatalf("the approver was asked %d questions at once", most.Load())
	}
}

// 11
func TestResumeReplaysAFinishedChildWithoutRunningIt(t *testing.T) {
	inDir(t)
	bridge := gmailBridge()
	kid := pkgSpec{name: "kid", rest: "tools:\n" + usesDraft}.write(t)
	boss := pkgSpec{name: "boss", rest: "uses:\n" + use(t, "kid", kid, "write")}.write(t)
	runs := t.TempDir()
	in := script(t, map[string]any{"seq": true, "steps": steps(
		runStep("kid", map[string]any{"steps": steps(map[string]any{"method": "call", "alias": "draft", "arguments": map[string]any{}})}),
		runStep("kid", map[string]any{"print": `{"second":true}`}),
	)})
	u := useRun{pkg: boss, runs: runs, catalog: catalogOf(t, kid), bridge: bridge, approve: approveAll(t), args: []string{in}}
	o := u.options(t)
	o.stopAfter = 1
	if _, err := Run(context.Background(), o); !errors.Is(err, errInterrupted) {
		t.Fatalf("the run was not interrupted: %v", err)
	}
	if bridge.made("create_draft") != 1 {
		t.Fatalf("the child's change was not made once: %d", bridge.made("create_draft"))
	}
	o = u.options(t)
	o.Resume = rootRun(t, runs)
	res, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if bridge.made("create_draft") != 1 {
		t.Fatalf("the finished child ran again: %d changes", bridge.made("create_draft"))
	}
	if res.Replayed != 1 {
		t.Fatalf("replayed %d requests, want 1", res.Replayed)
	}
	rp := repliesOf(t, res)
	if str(rp[1]["result"]) != `{"second":true}` {
		t.Fatalf("the second run did not happen after the resume: %v", rp)
	}
}

// A parent that stops after its child finished but before it heard has the
// child's output in the child's own record.
func TestResumeReadsAChildThatFinishedBeforeItsParentHeard(t *testing.T) {
	inDir(t)
	bridge := gmailBridge()
	kid := pkgSpec{name: "kid", rest: "tools:\n" + usesDraft}.write(t)
	boss := pkgSpec{name: "boss", rest: "uses:\n" + use(t, "kid", kid, "write")}.write(t)
	runs := t.TempDir()
	in := script(t, map[string]any{"steps": steps(runStep("kid", map[string]any{
		"print": `{"kept":"yes"}`,
		"steps": steps(map[string]any{"method": "call", "alias": "draft", "arguments": map[string]any{}}),
	}))})
	u := useRun{pkg: boss, runs: runs, catalog: catalogOf(t, kid), bridge: bridge, approve: approveAll(t), args: []string{in}}
	o := u.options(t)
	o.stopAfterChild = true
	if _, err := Run(context.Background(), o); !errors.Is(err, errInterrupted) {
		t.Fatalf("the run was not interrupted: %v", err)
	}
	o = u.options(t)
	o.Resume = rootRun(t, runs)
	res, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if bridge.made("create_draft") != 1 {
		t.Fatalf("the child ran again: %d changes", bridge.made("create_draft"))
	}
	if got := str(repliesOf(t, res)[0]["result"]); got != `{"kept":"yes"}` {
		t.Fatalf("the parent was not given the child's output: %q", got)
	}
}

// 12
func TestResumeContinuesAnInterruptedChildAndReportsItsUnknownWrite(t *testing.T) {
	inDir(t)
	bridge := gmailBridge()
	kid := pkgSpec{name: "kid", rest: "tools:\n" + usesDraft}.write(t)
	boss := pkgSpec{name: "boss", rest: "uses:\n" + use(t, "kid", kid, "write")}.write(t)
	runs := t.TempDir()
	in := script(t, map[string]any{"seq": true, "steps": steps(runStep("kid", map[string]any{"steps": steps(map[string]any{"method": "call", "alias": "draft", "arguments": map[string]any{}})}))})
	u := useRun{pkg: boss, runs: runs, catalog: catalogOf(t, kid), bridge: bridge, approve: approveAll(t), args: []string{in}}
	o := u.options(t)
	o.childHook = func(_ string, co *Options) { co.stopDuring = "s0" }
	if _, err := Run(context.Background(), o); !errors.Is(err, errInterrupted) {
		t.Fatalf("the run was not interrupted: %v", err)
	}
	o = u.options(t)
	o.Resume = rootRun(t, runs)
	res, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Unknown != 1 {
		t.Fatalf("the parent reports %d unknown changes, want 1", res.Unknown)
	}
	rp := repliesOf(t, res)
	if rp[0]["unknown"] != true {
		t.Fatalf("the parent was not told the outcome is unknown: %v", rp[0])
	}
	if bridge.made("create_draft") != 0 {
		t.Fatalf("an interrupted change was made again: %d", bridge.made("create_draft"))
	}
	if len(runDirs(t, runs)) != 2 {
		t.Fatalf("the child was not resumed under its own record: %v", runDirs(t, runs))
	}
}

// 13
func TestChildInputAndOutputAreCheckedAgainstTheirSchemas(t *testing.T) {
	inDir(t)
	kid := pkgSpec{name: "kid", rest: `interface:
  inputSchema: {type: object, properties: {name: {type: string}}, required: [name]}
  outputSchema: {type: object, properties: {ok: {type: boolean}}, required: [ok]}
`}.write(t)
	boss := pkgSpec{name: "boss", rest: "uses:\n" + use(t, "kid", kid, "read")}.write(t)
	in := script(t, map[string]any{"seq": true, "steps": steps(
		runStep("kid", map[string]any{"print": `{"ok":true}`}),                         // name is missing
		runStep("kid", map[string]any{"name": 5, "print": `{"ok":true}`}),              // name is the wrong type
		runStep("kid", map[string]any{"name": "x", "print": `{"ok":true}`}),            // fine
		runStep("kid", map[string]any{"name": "x", "print": `{"ok":"yes"}`}),           // output has the wrong type
		runStep("kid", map[string]any{"name": "x", "print": `{}`}),                     // output lacks a required key
		runStep("kid", map[string]any{"name": "x", "print": `not json`}),               // output is not an object
		runStep("kid", map[string]any{"name": "x", "print": `{"ok":true}`, "exit": 3}), // the child failed
	)})
	res, err := Run(context.Background(), useRun{pkg: boss, runs: t.TempDir(), catalog: catalogOf(t, kid), args: []string{in}}.options(t))
	if err != nil {
		t.Fatal(err)
	}
	rp := repliesOf(t, res)
	if !strings.Contains(str(rp[0]["refused"]), `"name"`) {
		t.Errorf("a missing field was not refused by name: %v", rp[0])
	}
	if !strings.Contains(str(rp[1]["refused"]), `"name" is integer, not string`) {
		t.Errorf("a wrong type was not refused: %v", rp[1])
	}
	if str(rp[2]["refused"]) != "" || str(rp[2]["result"]) != `{"ok":true}` || rp[2]["exit"].(float64) != 0 {
		t.Errorf("a good run was not answered: %v", rp[2])
	}
	for i := 3; i <= 5; i++ {
		if rp[i]["exit"].(float64) != 70 || !strings.Contains(str(rp[i]["stderr"]), "output did not match the declared outputSchema") {
			t.Errorf("output %d was not refused as the spec says: %v", i, rp[i])
		}
	}
	if rp[6]["exit"].(float64) != 3 || str(rp[6]["refused"]) != "" {
		t.Errorf("a failed child is a result to inspect: %v", rp[6])
	}
}

// 14
func TestFirstRunTrustShowsTheWholeTreeAndTrustsEachPinnedChild(t *testing.T) {
	inDir(t)
	kid := pkgSpec{name: "kid", rest: "tools:\n" + usesSearch + "files:\n  - {path: out, access: write}\n"}.write(t)
	mid := pkgSpec{name: "mid", rest: "uses:\n" + use(t, "kid", kid, "write")}.write(t)
	boss := pkgSpec{name: "boss", rest: "uses:\n" + use(t, "mid", mid, "write")}.write(t)
	entries, _ := catalogOf(t, kid, mid)()
	self, m, err := packageDigest(boss)
	if err != nil {
		t.Fatal(err)
	}
	kids, err := resolveUses(entries, m, self)
	if err != nil {
		t.Fatal(err)
	}
	store := &trustStore{path: filepath.Join(t.TempDir(), "trusted.json")}
	var declared []string
	ask := func(name, publisher, version, path, digest, decl string) bool {
		declared = append(declared, decl)
		return true
	}
	if why := admitPackageTree(store, ask, boss, kids); why != "" {
		t.Fatal(why)
	}
	if len(declared) != 1 {
		t.Fatalf("the person was asked %d times, want once", len(declared))
	}
	for _, want := range []string{"uses mid (dev.test/mid@0.1.0)", "uses mid/kid (dev.test/kid@0.1.0)", "files: out (write)", "tool: gmail.threads.search (search, read)"} {
		if !strings.Contains(declared[0], want) {
			t.Errorf("the question does not show %q:\n%s", want, declared[0])
		}
	}
	all := store.load()
	kidDigest, _, _ := packageDigest(kid)
	midDigest, _, _ := packageDigest(mid)
	if !store.has(self) || !store.has(kidDigest) || !store.has(midDigest) {
		t.Fatalf("not every digest in the tree was kept: %v", all)
	}
	if all[kidDigest].Name != "boss/mid/kid" || all[midDigest].Name != "boss/mid" {
		t.Fatalf("the children are not named by their path: %q, %q", all[midDigest].Name, all[kidDigest].Name)
	}
	if why := admitPackageTree(store, ask, boss, kids); why != "" || len(declared) != 1 {
		t.Fatalf("a trusted tree asked again: %q, %d questions", why, len(declared))
	}
	// A no leaves nothing behind.
	fresh := &trustStore{path: filepath.Join(t.TempDir(), "trusted.json")}
	if why := admitPackageTree(fresh, func(string, string, string, string, string, string) bool { return false }, boss, kids); why == "" || fresh.has(kidDigest) || fresh.has(self) {
		t.Fatalf("a refusal kept something: %q %v", why, fresh.load())
	}
	// A tree whose parent is trusted but whose child is not asks again.
	partial := &trustStore{path: filepath.Join(t.TempDir(), "trusted.json")}
	partial.add(self, "boss", boss)
	if why := admitPackageTree(partial, ask, boss, kids); why != "" || len(declared) != 2 || !partial.has(kidDigest) {
		t.Fatalf("an untrusted child was run unasked: %q, %d questions", why, len(declared))
	}
}

// 16
func TestWebBuildRefusesUses(t *testing.T) {
	kid := pkgSpec{name: "kid"}.write(t)
	dir := t.TempDir()
	manifest := "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.test, name: page, version: 0.1.0}\nexecution: {entrypoint: main.js}\nuses:\n" + use(t, "kid", kid, "read")
	os.WriteFile(filepath.Join(dir, "primitive.yaml"), []byte(manifest), 0o600)
	os.WriteFile(filepath.Join(dir, "main.js"), []byte("print('x')\n"), 0o600)
	_, _, err := buildWebPage(t.TempDir(), []string{dir})
	if err == nil || !strings.Contains(err.Error(), "uses other primitives") {
		t.Fatalf("a web page was built for a primitive that uses others: %v", err)
	}
}

// The relay runs one program at a time; uses is refused there with the client named.
func TestUsesAreRefusedUnderTheRelay(t *testing.T) {
	inDir(t)
	kid := pkgSpec{name: "kid"}.write(t)
	boss := pkgSpec{name: "boss", rest: "uses:\n" + use(t, "kid", kid, "read")}.write(t)
	o := useRun{pkg: boss, runs: t.TempDir(), catalog: catalogOf(t, kid), args: []string{"{}"}}.options(t)
	o.relay, o.relayClient = &relayRun{}, "gemini-cli"
	_, err := Run(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "gemini-cli") || !strings.Contains(err.Error(), "uses other primitives") {
		t.Fatalf("uses under the relay was not refused with the client named: %v", err)
	}
}

// tools lists the aliases of what a primitive uses.
func TestToolsListsTheUses(t *testing.T) {
	inDir(t)
	kid := pkgSpec{name: "kid"}.write(t)
	boss := pkgSpec{name: "boss", rest: "uses:\n" + use(t, "kid", kid, "read")}.write(t)
	in := script(t, map[string]any{"steps": steps(map[string]any{"method": "tools"})})
	res, err := Run(context.Background(), useRun{pkg: boss, runs: t.TempDir(), catalog: catalogOf(t, kid), args: []string{in}}.options(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := repliesOf(t, res)[0]["uses"]; fmt.Sprint(got) != "[kid]" {
		t.Fatalf("tools said %v", got)
	}
}

// A run request for something not under uses is refused.
func TestARunRequestForAnUnlistedPrimitiveIsRefused(t *testing.T) {
	inDir(t)
	kid := pkgSpec{name: "kid"}.write(t)
	boss := pkgSpec{name: "boss", rest: "uses:\n" + use(t, "kid", kid, "read")}.write(t)
	in := script(t, map[string]any{"steps": steps(runStep("nobody", map[string]any{}))})
	res, err := Run(context.Background(), useRun{pkg: boss, runs: t.TempDir(), catalog: catalogOf(t, kid), args: []string{in}}.options(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := str(repliesOf(t, res)[0]["refused"]); !strings.Contains(got, "not one of the primitives this one uses") {
		t.Fatalf("an unlisted primitive was run: %q", got)
	}
}

// The package for a run request carries its authority only through its own digest:
// the fetch grants kept for it, and none of its parent's.
func TestAChildGetsOnlyWhatWasKeptForItsOwnDigest(t *testing.T) {
	inDir(t)
	srv, hits := countingServer(t)
	kid := pkgSpec{name: "kid", rest: "fetch:\n  - {origin: \"" + srv.URL + "\"}\n"}.write(t)
	boss := pkgSpec{name: "boss", rest: "fetch:\n  - {origin: \"" + srv.URL + "\"}\nuses:\n" + use(t, "kid", kid, "read")}.write(t)
	kidDigest, _, _ := packageDigest(kid)
	bossDigest, _, _ := packageDigest(boss)
	fetch := map[string]any{"method": "fetch", "url": srv.URL + "/x", "http_method": "GET"}
	in := script(t, map[string]any{"seq": true, "steps": steps(runStep("kid", map[string]any{"steps": steps(fetch)}))})
	var asked atomic.Int32
	refuse := func(Ask) Grant { asked.Add(1); return Grant{} }
	o := useRun{pkg: boss, runs: t.TempDir(), catalog: catalogOf(t, kid), approve: refuse, args: []string{in}}.options(t)
	// Origins are kept for the parent only.
	o.FetchOrigins = []string{srv.URL}
	o.authFor = func(d string) childAuth {
		if d == bossDigest {
			t.Errorf("the parent's grants were asked for the child")
		}
		return childAuth{}
	}
	res, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 0 || asked.Load() != 1 {
		t.Fatalf("the parent's granted origin reached the child: %d fetches, %d questions", hits.Load(), asked.Load())
	}
	_ = res
	// Kept for the child's own digest, it runs unasked.
	o = useRun{pkg: boss, runs: t.TempDir(), catalog: catalogOf(t, kid), approve: refuse, args: []string{in}}.options(t)
	o.authFor = func(d string) childAuth {
		if d == kidDigest {
			return childAuth{Origins: []string{srv.URL}}
		}
		return childAuth{}
	}
	if _, err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 || asked.Load() != 1 {
		t.Fatalf("what was kept for the child's digest was not used: %d fetches, %d questions", hits.Load(), asked.Load())
	}
}
