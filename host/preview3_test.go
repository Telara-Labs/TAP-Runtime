package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/internal/component"
)

// The Preview 3 tests build real components with Rust's wasm32-wasip3 target
// (the beta toolchain) and run them in the real, pinned wasmtime, obtained
// the way a person's runner obtains it: downloaded once from the official
// release and checked against the pinned digests. The download goes into the
// test binary's own cache (TestMain), never the person's.

const preview3Toolchain = "beta"

func rustcP3(t *testing.T, mainRS string) []byte {
	t.Helper()
	b := rustcWith(t, preview3Toolchain, mainRS, "wasm32-wasip3")
	p3, why, err := component.Preview3(b)
	if err != nil || !p3 {
		t.Fatalf("the wasip3 build is not recognised as Preview 3: %v %v", p3, err)
	}
	t.Logf("recognised as Preview 3: %s", why)
	return b
}

var sharedWasmtime struct {
	once sync.Once
	path string
	err  error
}

// needWasmtime obtains the pinned wasmtime into the test cache, once for the
// test binary, and returns its path. A machine without network skips.
func needWasmtime(t *testing.T) string {
	t.Helper()
	if _, ok := wasmtimeBuilds[runtime.GOOS+"/"+runtime.GOARCH]; !ok {
		t.Skipf("no official wasmtime build is pinned for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	sharedWasmtime.once.Do(func() {
		store, err := storeDir("")
		if err != nil {
			sharedWasmtime.err = err
			return
		}
		sharedWasmtime.path, sharedWasmtime.err = obtainWasmtime(store)
	})
	if err := sharedWasmtime.err; err != nil {
		if strings.Contains(err.Error(), "refused") {
			t.Fatal(err)
		}
		t.Skipf("wasmtime %s could not be obtained (no network?): %v", wasmtimeVersion, err)
	}
	return sharedWasmtime.path
}

// runPackage runs main.wasm under manifest with one argument list.
func runPackage(t *testing.T, wasm []byte, manifest string, cacheDir, interpDir string, args ...string) (*Result, error, []map[string]any) {
	t.Helper()
	pkg := t.TempDir()
	os.WriteFile(filepath.Join(pkg, "primitive.yaml"), []byte(manifest), 0o600)
	os.WriteFile(filepath.Join(pkg, "main.wasm"), wasm, 0o600)
	var j bytes.Buffer
	res, err := Run(context.Background(), Options{Package: pkg, Args: args, Journal: &j, CacheDir: cacheDir, InterpDir: interpDir, RunsDir: t.TempDir()})
	return res, err, journalRequests(t, j.Bytes())
}

// The Rust example's source is built three times: as a WASI Preview 1
// module, a Preview 2 component and a Preview 3 component. The first runs in
// wazero, the second in the runner's own component loader, the third in
// wasmtime. All three return the same output, make the same broker
// requests, and are refused the same undeclared read.
func TestTheSamePrimitiveBuiltForPreview1And2And3BehavesTheSame(t *testing.T) {
	example, _ := filepath.Abs(filepath.Join("..", "examples", "languages", "rust"))
	src := filepath.Join(example, "src", "main.rs")
	builds := []struct {
		name string
		wasm []byte
	}{
		{"preview 1", rustcFor(t, src, "wasm32-wasip1")},
		{"preview 2", rustcFor(t, src, "wasm32-wasip2")},
		{"preview 3", rustcP3(t, src)},
	}
	needWasmtime(t)
	manifest, err := os.ReadFile(filepath.Join(example, "primitive.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(filepath.Join(example, "..", "fixtures", "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	dir := inDir(t)
	os.MkdirAll(filepath.Join(dir, "examples", "languages", "fixtures"), 0o755)
	os.WriteFile(filepath.Join(dir, "examples", "languages", "fixtures", "tasks.json"), fixture, 0o600)
	cache := t.TempDir()

	type outcome struct {
		res     *Result
		err     error
		journal []map[string]any
	}
	runAll := func(t *testing.T, manifest, input string) []outcome {
		var out []outcome
		for _, b := range builds {
			res, err, j := runPackage(t, b.wasm, manifest, cache, "", input)
			if err != nil {
				t.Fatalf("%s: %v", b.name, err)
			}
			out = append(out, outcome{res, err, j})
		}
		for i := 1; i < len(out); i++ {
			a, b := out[0], out[i]
			if a.res.Stdout != b.res.Stdout || a.res.Stderr != b.res.Stderr || a.res.Exit != b.res.Exit || a.res.Ran != b.res.Ran || a.res.Refused != b.res.Refused {
				t.Fatalf("results differ:\n%s %+v\n%s %+v", builds[0].name, a.res, builds[i].name, b.res)
			}
			if !reflect.DeepEqual(a.journal, b.journal) {
				t.Fatalf("broker requests differ:\n%s %v\n%s %v", builds[0].name, a.journal, builds[i].name, b.journal)
			}
		}
		return out
	}

	for _, c := range []struct{ input, want string }{
		{`{"status":"open"}`, `{"status":"open","count":2,"items":[{"id":"TAP-1","title":"Record terminal evidence"},{"id":"TAP-3","title":"Review extraction output"}]}`},
		{`{"status":"done"}`, `{"status":"done","count":1,"items":[{"id":"TAP-2","title":"Verify a declared file read"}]}`},
	} {
		t.Run(c.input, func(t *testing.T) {
			out := runAll(t, string(manifest), c.input)
			p3 := out[2]
			t.Logf("broker requests of all three builds: %v", p3.journal)
			if p3.res.Stdout != c.want || p3.res.Exit != 0 || p3.res.Ran != 1 || p3.res.Refused != 0 || len(p3.journal) == 0 {
				t.Fatalf("%+v %v", p3.res, p3.journal)
			}
		})
	}
	t.Run("an undeclared read is refused for all three", func(t *testing.T) {
		undeclared := strings.Replace(string(manifest), "  - {path: examples/languages/fixtures/tasks.json, access: read}\n", "  - {path: elsewhere.json, access: read}\n", 1)
		if undeclared == string(manifest) {
			t.Fatal("the example's file declaration was not found")
		}
		out := runAll(t, undeclared, `{"status":"open"}`)
		p3 := out[2]
		if p3.res.Exit == 0 || p3.res.Ran != 0 || p3.res.Refused != 1 || !strings.Contains(p3.res.Stderr, "refused") {
			t.Fatalf("%+v", p3.res)
		}
		refusedInJournal := false
		for _, e := range p3.journal {
			if o, _ := e["outcome"].(string); strings.HasPrefix(o, "refused") {
				refusedInJournal = true
			}
		}
		if !refusedInJournal {
			t.Fatalf("the refusal is not in the journal: %v", p3.journal)
		}
	})
}

// A Preview 3 component asks for the host's files, the package directory,
// a TCP connection to a listener that is really there, a UDP socket, a
// process and an environment variable the runner has. Every attempt fails
// inside the guest; the listener is never reached and nothing is written.
const preview3Probe = `
use std::io::Write;
fn kind<T>(r: std::io::Result<T>) -> String { match r { Ok(_) => "allowed".into(), Err(e) => format!("{:?}", e.kind()) } }
fn main() {
    let a: Vec<String> = std::env::args().collect();
    let report = format!(
        "{{\"argv0\":\"{}\",\"file\":\"{}\",\"hosts\":\"{}\",\"pkg\":\"{}\",\"dir\":\"{}\",\"write\":\"{}\",\"tcp\":\"{}\",\"udp\":\"{}\",\"process\":\"{}\",\"var\":\"{}\",\"env\":{}}}",
        a[0],
        kind(std::fs::read(&a[1])),
        kind(std::fs::read("/etc/hosts")),
        kind(std::fs::read_dir(&a[3])),
        kind(std::fs::read_dir("/")),
        kind(std::fs::write(format!("{}.new", a[1]), b"x")),
        kind(std::net::TcpStream::connect(a[2].as_str())),
        kind(std::net::UdpSocket::bind("127.0.0.1:0")),
        kind(std::process::Command::new("/bin/sh").arg("-c").arg("echo outside").output()),
        match std::env::var("TAP_PREVIEW3_PROBE") { Ok(_) => "allowed".to_string(), Err(e) => format!("{:?}", e) },
        std::env::vars().count(),
    );
    let mut out = std::io::stdout().lock();
    let frame = format!("{{\"method\":\"return\",\"stdout\":{:?},\"stderr\":\"\",\"exit\":0}}\n", report);
    out.write_all(frame.as_bytes()).unwrap();
    out.flush().unwrap();
}
`

func TestAPreview3ComponentHasNoDirectFilesSocketsEnvironmentOrProcesses(t *testing.T) {
	src := filepath.Join(t.TempDir(), "main.rs")
	os.WriteFile(src, []byte(preview3Probe), 0o600)
	wasm := rustcP3(t, src)
	needWasmtime(t)
	secret := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(secret, []byte("host data"), 0o600)
	t.Setenv("TAP_PREVIEW3_PROBE", "should not be visible")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Close()
			accepted <- struct{}{}
		}
	}()
	pkgDir, _ := filepath.Abs(".")
	manifest := "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.test, name: preview3-probe, version: 0.1.0}\nexecution: {entrypoint: main.wasm, timeoutSeconds: 30}\n"
	res, err, journal := runPackage(t, wasm, manifest, t.TempDir(), "", secret, ln.Addr().String(), pkgDir)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(res.Stdout), &got); err != nil {
		t.Fatalf("%q: %v", res.Stdout, err)
	}
	t.Logf("the component's own attempts: %v", got)
	if got["argv0"] != "primitive" {
		t.Errorf("argv[0] is %v, not primitive", got["argv0"])
	}
	for _, k := range []string{"file", "hosts", "pkg", "dir", "write", "tcp", "udp", "process", "var"} {
		if got[k] == "allowed" {
			t.Errorf("%s was allowed: %v", k, got)
		}
	}
	if got["env"] != float64(0) || got["var"] != "NotPresent" {
		t.Errorf("the environment must be empty: %v", got)
	}
	select {
	case <-accepted:
		t.Error("the component reached a TCP listener on the host")
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := os.Stat(secret + ".new"); !os.IsNotExist(err) {
		t.Error("the component wrote a host file")
	}
	if res.Ran != 0 || res.Refused != 0 || len(journal) != 0 {
		t.Fatalf("the probe reached the broker: %+v %v", res, journal)
	}
}

// A Preview 3 component that imports wasi:http is refused before anything
// starts, as a Preview 2 one is.
func TestAPreview3ComponentThatImportsHTTPIsRefusedBeforeItRuns(t *testing.T) {
	src := filepath.Join(t.TempDir(), "main.rs")
	os.WriteFile(src, []byte(preview3Probe), 0o600)
	wasm := rustcP3(t, src)
	// Rename an interface the probe imports (same length) into wasi:http.
	renamed := bytes.ReplaceAll(wasm, []byte("wasi:cli/terminal-stdin@0.3"), []byte("wasi:http/terminal-stdi@0.3"))
	if bytes.Equal(renamed, wasm) {
		t.Fatal("the probe does not import wasi:cli/terminal-stdin")
	}
	started := false
	onWasmtimeStart = func(int) { started = true }
	t.Cleanup(func() { onWasmtimeStart = nil })
	manifest := "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.test, name: preview3-http, version: 0.1.0}\nexecution: {entrypoint: main.wasm}\n"
	_, err, _ := runPackage(t, renamed, manifest, t.TempDir(), t.TempDir(), "x")
	if err == nil || !strings.Contains(err.Error(), "no direct network access") {
		t.Fatalf("a wasi:http import was not refused: %v", err)
	}
	if started {
		t.Fatal("wasmtime was started for a refused component")
	}
}

// processGone reports whether the process with this ID has ended and been
// collected (Unix).
func processGone(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return true
	}
	return p.Signal(syscall.Signal(0)) != nil
}

// The run's time limit stops the wasmtime process, not only the runner's
// side of the pipes: afterwards no process with its ID is left, and the run
// is recorded as timed out.
func TestAPreview3ComponentIsKilledAtItsDeadline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the process check uses a Unix signal")
	}
	src := filepath.Join(t.TempDir(), "main.rs")
	os.WriteFile(src, []byte(`fn main() { loop { std::hint::black_box(0); } }`), 0o600)
	wasm := rustcP3(t, src)
	needWasmtime(t)
	var mu sync.Mutex
	var pids []int
	onWasmtimeStart = func(pid int) { mu.Lock(); pids = append(pids, pid); mu.Unlock() }
	t.Cleanup(func() { onWasmtimeStart = nil })
	manifest := "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.test, name: preview3-spin, version: 0.1.0}\nexecution: {entrypoint: main.wasm, timeoutSeconds: 1}\n"
	start := time.Now()
	_, err, journal := runPackage(t, wasm, manifest, t.TempDir(), "")
	if err == nil || !strings.Contains(err.Error(), "ran past its time limit") {
		t.Fatalf("the spinning component was not stopped by its deadline: %v", err)
	}
	if took := time.Since(start); took > 20*time.Second {
		t.Fatalf("stopping took %s", took)
	}
	timedOut := false
	for _, e := range journal {
		if e["outcome"] == "timed_out" {
			timedOut = true
		}
	}
	if !timedOut {
		t.Fatalf("the journal does not record the time-out: %v", journal)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(pids) != 1 {
		t.Fatalf("wasmtime processes started: %v", pids)
	}
	if !processGone(pids[0]) {
		t.Fatalf("wasmtime process %d is still running after the run timed out", pids[0])
	}
}

// The guest memory ceiling holds in wasmtime: a component that allocates
// more than it ends with the same "ran out of memory" a Preview 1 or 2 guest
// gets, and one that stays under it runs.
func TestAPreview3ComponentIsHeldToTheGuestMemoryLimit(t *testing.T) {
	src := filepath.Join(t.TempDir(), "main.rs")
	os.WriteFile(src, []byte(`
use std::io::Write;
fn main() {
    let n: usize = std::env::args().nth(1).unwrap().parse().unwrap();
    let v = vec![7u8; n];
    let frame = format!("{{\"method\":\"return\",\"stdout\":\"{}\",\"stderr\":\"\",\"exit\":0}}\n", v[n - 1]);
    std::io::stdout().write_all(frame.as_bytes()).unwrap();
}
`), 0o600)
	wasm := rustcP3(t, src)
	needWasmtime(t)
	manifest := "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.test, name: preview3-memory, version: 0.1.0}\nexecution: {entrypoint: main.wasm, timeoutSeconds: 60}\n"
	cache := t.TempDir()
	res, err, _ := runPackage(t, wasm, manifest, cache, "", "100000000")
	if err != nil || res.Stdout != "7" {
		t.Fatalf("100 MB under the limit: %+v %v", res, err)
	}
	over := int64(guestMemoryPages)*65536 + 64<<20
	_, err, _ = runPackage(t, wasm, manifest, cache, "", strconv.FormatInt(over, 10))
	if err == nil || !strings.Contains(err.Error(), "ran out of memory") {
		t.Fatalf("an allocation over the limit ended with: %v", err)
	}
}

// copyFile copies src to dst with mode.
func copyFile(t *testing.T, src, dst string, mode os.FileMode) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	os.MkdirAll(filepath.Dir(dst), 0o700)
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// pinnedURLUnreachable makes any download of wasmtime fail for one test, so
// that a test can show nothing was downloaded.
func pinnedURLUnreachable(t *testing.T) {
	t.Helper()
	key := runtime.GOOS + "/" + runtime.GOARCH
	was := wasmtimeBuilds[key]
	b := was
	b.URL = "http://127.0.0.1:1/wasmtime-must-not-be-downloaded.tar.xz"
	wasmtimeBuilds[key] = b
	t.Cleanup(func() { wasmtimeBuilds[key] = was })
}

// A wasmtime already on PATH is used when it is the official build of the
// pinned version: the same SHA-256. Nothing is downloaded.
func TestAnOfficialWasmtimeOnPATHIsReused(t *testing.T) {
	official := needWasmtime(t)
	bin := t.TempDir()
	name := "wasmtime"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	copyFile(t, official, filepath.Join(bin, name), 0o755)
	t.Setenv("PATH", bin)
	pinnedURLUnreachable(t)
	store := t.TempDir()
	got, err := obtainWasmtime(store)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(bin, name) {
		t.Fatalf("used %s, not the official copy on PATH", got)
	}
	if exists(filepath.Join(store, wasmtimeFile())) {
		t.Fatal("wasmtime was put in the store although an official copy was on PATH")
	}
}

// A wasmtime on PATH that says it is the pinned version but is a different
// program (built from source, or by a package manager) is not used, and is
// never run: the official archive is downloaded, checked and used instead.
// The second run
// of the same component starts from the compiled file.
func TestAWasmtimeWithTheRightVersionButAnotherDigestIsIgnored(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in wasmtime is a shell script")
	}
	if _, ok := wasmtimeBuilds[runtime.GOOS+"/"+runtime.GOARCH]; !ok {
		t.Skipf("no official wasmtime build is pinned for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	example, _ := filepath.Abs(filepath.Join("..", "examples", "languages", "rust"))
	wasm := rustcP3(t, filepath.Join(example, "src", "main.rs"))
	bin := t.TempDir()
	ran := filepath.Join(t.TempDir(), "ran")
	writeFile(t, filepath.Join(bin, "wasmtime"), "#!/bin/sh\n: > '"+ran+"'\necho 'wasmtime "+wasmtimeVersion+" (built elsewhere)'\n")
	os.Chmod(filepath.Join(bin, "wasmtime"), 0o755)
	t.Setenv("PATH", bin)
	if why := localWasmtimeProblem(filepath.Join(bin, "wasmtime"), wasmtimeBuilds[runtime.GOOS+"/"+runtime.GOARCH].ExeSHA256); !strings.Contains(why, "sha256") {
		t.Fatalf("the stand-in was not refused for its digest: %q", why)
	}

	store, cache := t.TempDir(), t.TempDir()
	manifest, err := os.ReadFile(filepath.Join(example, "primitive.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	inDir(t)
	start := time.Now()
	res, err, _ := runPackage(t, wasm, string(manifest), cache, store, `{"status":"x"}`)
	first := time.Since(start)
	if err != nil {
		if strings.Contains(err.Error(), "refused") || !strings.Contains(err.Error(), "source:") {
			t.Fatal(err)
		}
		t.Skipf("wasmtime could not be downloaded (no network?): %v", err)
	}
	if res.Stderr != "expected status open or done" || res.Exit != 1 {
		t.Fatalf("%+v", res)
	}
	if exists(ran) {
		t.Fatal("the stand-in wasmtime on PATH was executed")
	}
	exe := filepath.Join(store, wasmtimeFile())
	if ok, got, err := isPinnedWasmtime(exe, wasmtimeBuilds[runtime.GOOS+"/"+runtime.GOARCH].ExeSHA256); !ok || err != nil {
		t.Fatalf("the store does not hold the official program: %s %v", got, err)
	}
	cwasm := filepath.Join(cache, digest(wasm), wasmtimeCacheDirName(), "main.cwasm")
	if !exists(cwasm) {
		t.Fatalf("no compiled file at %s", cwasm)
	}

	pinnedURLUnreachable(t)
	start = time.Now()
	res, err, _ = runPackage(t, wasm, string(manifest), cache, store, `{"status":"x"}`)
	warm := time.Since(start)
	if err != nil || res.Exit != 1 {
		t.Fatalf("second run: %+v %v", res, err)
	}
	t.Logf("first run (download, check, compile, run): %s; second run: %s", first.Round(time.Millisecond), warm.Round(time.Millisecond))
}

// A copy in the store that is not the pinned program is replaced, not run.
func TestAnAlteredWasmtimeInTheStoreIsReplaced(t *testing.T) {
	official := needWasmtime(t)
	store := t.TempDir()
	writeFile(t, filepath.Join(store, wasmtimeFile()), "not wasmtime")
	// The replacement comes from the official archive; to keep this test
	// off the network, the shared download is offered as the "PATH" copy.
	bin := t.TempDir()
	name := "wasmtime"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	copyFile(t, official, filepath.Join(bin, name), 0o755)
	t.Setenv("PATH", bin)
	pinnedURLUnreachable(t)
	got, err := obtainWasmtime(store)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(bin, name) || exists(filepath.Join(store, wasmtimeFile())) {
		t.Fatalf("the altered copy was not set aside: used %s", got)
	}
}

// Someone who only runs Preview 1 modules and Preview 2 components never
// starts or downloads wasmtime.
func TestPreview1And2RunsNeverObtainWasmtime(t *testing.T) {
	example, _ := filepath.Abs(filepath.Join("..", "examples", "languages", "rust"))
	src := filepath.Join(example, "src", "main.rs")
	p1, p2 := rustcFor(t, src, "wasm32-wasip1"), rustcFor(t, src, "wasm32-wasip2")
	for _, b := range [][]byte{p1, p2} {
		if p3, _, err := component.Preview3(b); p3 || err != nil {
			t.Fatalf("a Preview 1 or 2 build was taken for Preview 3: %v %v", p3, err)
		}
	}
	pinnedURLUnreachable(t)
	t.Setenv("PATH", t.TempDir())
	started := false
	onWasmtimeStart = func(int) { started = true }
	t.Cleanup(func() { onWasmtimeStart = nil })
	manifest, err := os.ReadFile(filepath.Join(example, "primitive.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	store, cache := t.TempDir(), t.TempDir()
	inDir(t)
	for _, b := range [][]byte{p1, p2} {
		res, err, _ := runPackage(t, b, string(manifest), cache, store, `{"status":"x"}`)
		if err != nil || res.Exit != 1 {
			t.Fatalf("%+v %v", res, err)
		}
	}
	if started {
		t.Fatal("wasmtime was started")
	}
	err = filepath.WalkDir(store, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.Contains(d.Name(), "wasmtime") {
			return errors.New("found " + p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(cache, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), "wasmtime-") {
			return errors.New("found " + p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The clean-up keeps the pinned wasmtime and the current wasmtime's compiled
// files, and removes other versions of both.
func TestPruneKeepsOneWasmtimeVersion(t *testing.T) {
	cfg := t.TempDir()
	was := userConfigDir
	userConfigDir = func() (string, error) { return cfg, nil }
	t.Cleanup(func() { userConfigDir = was })
	t.Setenv("HOME", t.TempDir())

	store, compiled := t.TempDir(), t.TempDir()
	current := filepath.Join(store, wasmtimeFile())
	old := filepath.Join(store, "wasmtime-v48.0.0")
	writeFile(t, current, "the pinned wasmtime")
	writeFile(t, old, "an older wasmtime")
	sum := strings.Repeat("c", 64)
	keep := filepath.Join(compiled, sum, wasmtimeCacheDirName(), "main.cwasm")
	stale := filepath.Join(compiled, sum, "wasmtime-v48.0.0-"+runtime.GOARCH+"-"+runtime.GOOS, "main.cwasm")
	writeFile(t, keep, "compiled by the pinned wasmtime")
	writeFile(t, stale, "compiled by an older one")

	rep := pruneCaches(store, compiled, map[string]bool{sum: true})
	if len(rep.Errors) > 0 {
		t.Fatal(rep.Errors)
	}
	if !exists(current) || !exists(keep) {
		t.Fatal("the current wasmtime or its compiled file was removed")
	}
	if exists(old) || exists(filepath.Dir(stale)) {
		t.Fatalf("an older wasmtime was kept: %v", rep.Removed)
	}
}

// On a platform with no pinned build, a Preview 3 component is refused with
// a message that says so; nothing is looked for or downloaded.
func TestAPlatformWithoutAPinnedWasmtimeRefusesPreview3(t *testing.T) {
	key := runtime.GOOS + "/" + runtime.GOARCH
	was, had := wasmtimeBuilds[key]
	delete(wasmtimeBuilds, key)
	t.Cleanup(func() {
		if had {
			wasmtimeBuilds[key] = was
		}
	})
	store := t.TempDir()
	_, err := obtainWasmtime(store)
	if err == nil || !strings.Contains(err.Error(), "cannot run here") || !strings.Contains(err.Error(), "Preview 2 components can") {
		t.Fatalf("%v", err)
	}
	if entries, _ := os.ReadDir(store); len(entries) != 0 {
		t.Fatalf("the store was written: %v", entries)
	}
}
