// Package launch checks a published TAP release the way a stranger meets it:
// installed from npm, the release install scripts and the Go module proxy,
// on a machine with no agents and no history, following the README. Nothing
// here builds the runner from this checkout; the checkout supplies only the
// examples, the history fixtures and the trusted release key.
//
//	go test ./acceptance/launch -count=1 -v -launch-version 0.1.11
//
// Without -launch-version every test skips.
package launch

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

var launchVersion = flag.String("launch-version", "", "published TAP version to check, e.g. 0.1.11; the tests skip without it")

const (
	repo = "https://github.com/Telara-Labs/TAP-Runtime"
	pkg  = "@telaralabs/tap"
)

// root is the repository checkout (examples, fixtures, release key).
var root, _ = filepath.Abs("../..")

func version(t *testing.T) string {
	t.Helper()
	if *launchVersion == "" {
		t.Skip("pass -launch-version to check a published release")
	}
	return strings.TrimPrefix(*launchVersion, "v")
}

// machine is a fresh user: an empty home, no agents, a PATH with only the
// system's tools and whatever was installed into it.
type machine struct {
	t    *testing.T
	home string
	bin  []string
}

func newMachine(t *testing.T) *machine {
	return &machine{t: t, home: t.TempDir()}
}

func (m *machine) env() []string {
	path := append(append([]string{}, m.bin...), systemPath()...)
	env := []string{
		"HOME=" + m.home, "USERPROFILE=" + m.home,
		"APPDATA=" + filepath.Join(m.home, "AppData", "Roaming"),
		"LOCALAPPDATA=" + filepath.Join(m.home, "AppData", "Local"),
		"XDG_CONFIG_HOME=" + filepath.Join(m.home, ".config"),
		"XDG_CACHE_HOME=" + filepath.Join(m.home, ".cache"),
		"PATH=" + strings.Join(path, string(os.PathListSeparator)),
		"TERM=dumb",
	}
	// What the OS itself needs to start programs, and npm and Go to reach
	// their registries.
	for _, k := range []string{"SystemRoot", "SYSTEMROOT", "ComSpec", "PATHEXT", "TEMP", "TMP", "TMPDIR", "WINDIR", "GOPATH", "GOMODCACHE", "GOCACHE", "GOROOT"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// systemPath is the directories of the tools a fresh machine has: node,
// npm, go, curl, the shell. It excludes nothing tap-related because the
// runner machine has none installed.
func systemPath() []string {
	var out []string
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if _, err := os.Stat(filepath.Join(d, exe("tap"))); err == nil {
			continue // a tap from somewhere else would hide the one under test
		}
		out = append(out, d)
	}
	return out
}

func exe(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

type result struct {
	out  string
	code int
}

func (m *machine) run(dir, name string, args ...string) result {
	m.t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = m.env()
	cmd.Dir = dir
	if dir == "" {
		cmd.Dir = m.home
	}
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	cmd.Stdin = strings.NewReader("")
	err := cmd.Run()
	code := 0
	if err != nil {
		code = -1
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		if code == -1 {
			m.t.Fatalf("%s %v: %v\n%s", name, args, err, b.String())
		}
	}
	m.t.Logf("$ %s %s  [exit %d]\n%s", filepath.Base(name), strings.Join(args, " "), code, indent(b.String()))
	return result{b.String(), code}
}

func indent(s string) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	return "    " + strings.ReplaceAll(s, "\n", "\n    ")
}

func want(t *testing.T, r result, code int, contains ...string) {
	t.Helper()
	if r.code != code {
		t.Errorf("exit %d, want %d\n%s", r.code, code, r.out)
	}
	for _, c := range contains {
		if !strings.Contains(r.out, c) {
			t.Errorf("output lacks %q\n%s", c, r.out)
		}
	}
	for _, bad := range []string{"panic:", "goroutine ", "host  fatal"} {
		if strings.Contains(r.out, bad) {
			t.Errorf("output shows %q\n%s", bad, r.out)
		}
	}
}

// npmTap installs the release from npm, as the README's first line says,
// and returns the tap command it put on PATH.
func npmTap(t *testing.T, m *machine, v string) string {
	t.Helper()
	npm, err := exec.LookPath("npm")
	if err != nil {
		t.Fatal("npm is not on PATH")
	}
	prefix := filepath.Join(m.home, "npm-global")
	r := m.run("", npm, "install", "-g", "--prefix", prefix, pkg+"@"+v)
	// npm runs the package's setup step but hides its output, so the README
	// tells people to run tap setup themselves; that is checked below.
	want(t, r, 0)
	bin := filepath.Join(prefix, "bin")
	tap := filepath.Join(bin, "tap")
	if runtime.GOOS == "windows" {
		bin, tap = prefix, filepath.Join(prefix, "tap.cmd")
	}
	m.bin = append(m.bin, bin)
	return tap
}

// The README, top to bottom, with the npm package.
func TestNpmInstallAndReadme(t *testing.T) {
	v := version(t)
	m := newMachine(t)
	tap := npmTap(t, m, v)

	want(t, m.run("", tap, "version"), 0, "tap "+v)
	want(t, m.run("", tap, "--help"), 0, "usage: tap", "tap discover", "tap setup")
	want(t, m.run("", tap), 2, "usage: tap")
	want(t, m.run("", tap, "setup"), 0, "No agent TAP can connect to is installed here.", "then run: tap setup")
	want(t, m.run("", tap, "install", "--client", "all", "--print"), 0)
	want(t, m.run("", tap, "not-a-primitive"), 2, "is not a primitive folder")

	// discover with no history says so and what to do.
	want(t, m.run("", tap, "discover"), 0, "No agent history found on this machine.")
	want(t, m.run("", tap, "discover", "--client", "codex", "--days", "30"), 0, "No sessions from the last 30 days", "codex")

	// discover on real Codex history (three recorded runs of one task)
	// finds the repeated procedure.
	copyTree(t, filepath.Join(root, "discover", "history", "testdata", "scripted3", "codex", "home", ".codex"), filepath.Join(m.home, ".codex"))
	r := m.run("", tap, "discover", "--client", "codex", "--json")
	want(t, r, 0)
	var res struct {
		Summary struct {
			Sessions int `json:"sessions"`
		} `json:"summary"`
		Primitives []json.RawMessage `json:"primitives"`
	}
	if err := json.Unmarshal([]byte(jsonPart(r.out)), &res); err != nil {
		t.Fatalf("discover --json is not JSON: %v\n%s", err, r.out)
	}
	if res.Summary.Sessions != 3 || len(res.Primitives) == 0 {
		t.Errorf("discover read %d sessions and found %d primitives, want 3 and at least 1", res.Summary.Sessions, len(res.Primitives))
	}
	runExamples(t, m, tap)
}

// runExamples runs the three hello primitives (the runner downloads each
// interpreter from the release on first use and checks its pinned sha256)
// and the approval gate on a write.
func runExamples(t *testing.T, m *machine, tap string) {
	t.Helper()
	work := t.TempDir()
	for name, out := range map[string]string{"hello-sh": "hello from bash", "hello-py": "hello", "hello-ts": "hello"} {
		dir := filepath.Join(work, name)
		copyTree(t, filepath.Join(root, "examples", name), dir)
		want(t, m.run(work, tap, name), 0, "RESULT (exit 0)", out)
	}

	// A write is refused unless approved, and nothing is written.
	gate := filepath.Join(work, "write-gate")
	os.MkdirAll(gate, 0o755)
	os.WriteFile(filepath.Join(gate, "primitive.yaml"), []byte("apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.example, name: write-gate, version: 0.1.0}\nexecution: {entrypoint: main.py}\nfiles:\n  - {path: out, access: write}\n"), 0o644)
	os.WriteFile(filepath.Join(gate, "main.py"), []byte("tap.write(\"out/note.txt\", \"written\")\nprint(\"wrote\")\n"), 0o644)
	os.MkdirAll(filepath.Join(work, "out"), 0o755)
	note := filepath.Join(work, "out", "note.txt")
	refused := m.run(work, tap, "write-gate")
	if refused.code == 0 && strings.Contains(refused.out, "wrote") {
		t.Errorf("an unapproved write ran\n%s", refused.out)
	}
	if _, err := os.Stat(note); err == nil {
		t.Fatalf("an unapproved write changed the file")
	}
	want(t, m.run(work, tap, "--approve", "write-gate"), 0, "RESULT (exit 0)", "wrote")
	if b, err := os.ReadFile(note); err != nil || string(b) != "written" {
		t.Errorf("an approved write did not land: %q %v", b, err)
	}
}

// The release install script, as docs/install.md gives it.
func TestInstallScript(t *testing.T) {
	v := version(t)
	m := newMachine(t)
	dir := filepath.Join(m.home, "bin")
	base := fmt.Sprintf("%s/releases/download/v%s", repo, v)
	if runtime.GOOS == "windows" {
		ps := fmt.Sprintf("& ([scriptblock]::Create((Invoke-RestMethod '%s/install.ps1'))) -Client none -Dir '%s'", base, dir)
		want(t, m.run("", "powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", ps), 0)
	} else {
		want(t, m.run("", "sh", "-c", fmt.Sprintf("curl -fsSL %s/install.sh | sh -s -- --client none --dir '%s'", base, dir)), 0)
	}
	m.bin = append(m.bin, dir)
	tap := filepath.Join(dir, exe("tap"))
	want(t, m.run("", tap, "version"), 0, "tap "+v)
	want(t, m.run("", tap, "discover"), 0, "No agent history found on this machine.")
}

// The release's SHA256SUMS is signed by the key this repository publishes,
// and every runner and interpreter in it matches its sum.
func TestReleaseSignature(t *testing.T) {
	v := version(t)
	if runtime.GOOS == "windows" {
		t.Skip("checked once, on Linux and macOS")
	}
	dir := t.TempDir()
	var rel struct {
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	getJSON(t, fmt.Sprintf("https://api.github.com/repos/Telara-Labs/TAP-Runtime/releases/tags/v%s", v), &rel)
	if len(rel.Assets) == 0 {
		t.Fatal("the release has no assets")
	}
	for _, a := range rel.Assets {
		download(t, a.URL, filepath.Join(dir, a.Name))
	}
	cmd := exec.Command("go", "run", "./release", "verify", "--dir", dir, "--pub", "release/release.pub")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	t.Logf("release verify:\n%s", indent(string(out)))
	if err != nil {
		t.Fatalf("release verify failed: %v", err)
	}
}

// go install, as the README's build-from-source section gives it.
func TestGoInstall(t *testing.T) {
	v := version(t)
	m := newMachine(t)
	gobin := filepath.Join(m.home, "gobin")
	cmd := exec.Command("go", "install", "github.com/Telara-Labs/TAP-Runtime/host@v"+v)
	cmd.Env = append(os.Environ(), "GOBIN="+gobin, "GOWORK=off", "GOFLAGS=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go install: %v\n%s", err, out)
	}
	want(t, m.run("", filepath.Join(gobin, exe("host")), "--help"), 0, "usage: tap")
}

// discover reads local files only and sends nothing: run it on recorded
// history with the network taken away (macOS) or every connect recorded
// (Linux), and require the same result.
func TestDiscoverMakesNoNetworkConnections(t *testing.T) {
	v := version(t)
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("checked on macOS and Linux")
	}
	m := newMachine(t)
	tap := npmTap(t, m, v)
	copyTree(t, filepath.Join(root, "discover", "history", "testdata", "scripted3", "codex", "home", ".codex"), filepath.Join(m.home, ".codex"))
	open := m.run("", tap, "discover", "--client", "codex", "--json")
	want(t, open, 0)
	// npm's tap.cmd/tap.cjs is node; the runner itself is what discover runs in.
	runner := nativeRunner(t, m, tap)
	switch runtime.GOOS {
	case "darwin":
		closed := m.run("", "sandbox-exec", "-p", "(version 1)(allow default)(deny network*)", runner, "discover", "--client", "codex", "--json")
		want(t, closed, 0)
		if jsonPart(closed.out) != jsonPart(open.out) {
			t.Errorf("with the network denied, discover gave a different result")
		}
	case "linux":
		if _, err := exec.LookPath("strace"); err != nil {
			t.Fatal("strace is needed to record connections")
		}
		trace := filepath.Join(t.TempDir(), "strace.log")
		traced := m.run("", "strace", "-f", "-qq", "-e", "trace=connect,sendto,sendmsg", "-e", "signal=none", "-o", trace, runner, "discover", "--client", "codex", "--json")
		want(t, traced, 0)
		log, _ := os.ReadFile(trace)
		inet := regexp.MustCompile(`AF_INET6?`)
		if hits := inet.FindAllString(string(log), -1); len(hits) > 0 {
			t.Errorf("discover made %d internet connection attempts:\n%s", len(hits), log)
		}
		t.Logf("network syscalls recorded:\n%s", indent(string(log)))
		if jsonPart(traced.out) != jsonPart(open.out) {
			t.Errorf("under strace, discover gave a different result")
		}
	}
}

// nativeRunner is the platform binary the npm package runs, checked by the
// package against its sha256 before every run.
func nativeRunner(t *testing.T, m *machine, tap string) string {
	t.Helper()
	assets, _ := filepath.Glob(filepath.Join(filepath.Dir(filepath.Dir(tap)), "lib", "node_modules", "@telaralabs", "tap", "assets", "tap-*"))
	for _, a := range assets {
		if strings.Contains(a, runtime.GOOS) && strings.Contains(a, goarch()) {
			return a
		}
	}
	t.Fatalf("no runner for %s/%s in %v", runtime.GOOS, goarch(), assets)
	return ""
}

func goarch() string { return runtime.GOARCH }

func jsonPart(s string) string {
	if i := strings.Index(s, "{"); i >= 0 {
		return strings.TrimSpace(s[i:])
	}
	return s
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.Walk(from, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		if fi.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

var client = &http.Client{Timeout: 2 * time.Minute}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s: %s\n%s", url, resp.Status, b)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

func download(t *testing.T, url, to string) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("%s: %s", url, resp.Status)
	}
	f, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		t.Fatal(err)
	}
}
