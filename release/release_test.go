package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const repo = ".."

var host = runtime.GOOS + "/" + runtime.GOARCH

func build(t *testing.T, key string) string {
	t.Helper()
	out := t.TempDir()
	if err := Build(repo, out, "0.0.0-test", []string{host}, key, ""); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAReleaseIsReproducible(t *testing.T) {
	a, b := build(t, ""), build(t, "")
	x, _ := os.ReadFile(filepath.Join(a, "SHA256SUMS"))
	y, _ := os.ReadFile(filepath.Join(b, "SHA256SUMS"))
	if string(x) != string(y) || len(x) == 0 {
		t.Fatalf("two builds of the same source differ:\n%s\n%s", x, y)
	}
	for _, want := range []string{"tap-runtime-0.0.0-test-", "sh-0.0.0-test.wasm", "THIRD_PARTY_NOTICES.txt"} {
		if !strings.Contains(string(x), want) {
			t.Errorf("the checksums do not list %s:\n%s", want, x)
		}
	}
}

func TestVerify(t *testing.T) {
	keys := filepath.Join(t.TempDir(), "k")
	if err := Keygen(keys); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "other")
	Keygen(other)

	out := build(t, keys+".key")
	if err := Verify(out, keys+".pub"); err != nil {
		t.Fatalf("a good release did not verify: %v", err)
	}
	if err := Verify(out, other+".pub"); err == nil {
		t.Error("a release verified against a key that did not sign it")
	}

	entries, _ := os.ReadDir(out)
	var binary string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "tap-runtime-") {
			binary = filepath.Join(out, e.Name())
		}
	}
	raw, _ := os.ReadFile(binary)
	os.WriteFile(binary, append(raw, 0), 0o755)
	if err := Verify(out, keys+".pub"); err == nil || !strings.Contains(err.Error(), "does not match its checksum") {
		t.Errorf("an altered binary verified: %v", err)
	}
	os.WriteFile(binary, raw, 0o755)

	// Altering the checksums to match an altered file breaks the signature.
	sums, _ := os.ReadFile(filepath.Join(out, "SHA256SUMS"))
	os.WriteFile(filepath.Join(out, "SHA256SUMS"), append(sums, []byte("0000  extra\n")...), 0o644)
	if err := Verify(out, keys+".pub"); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Errorf("altered checksums verified: %v", err)
	}
	os.WriteFile(filepath.Join(out, "SHA256SUMS"), sums, 0o644)

	os.WriteFile(filepath.Join(out, "planted"), []byte("x"), 0o644)
	if err := Verify(out, keys+".pub"); err == nil || !strings.Contains(err.Error(), "not in its checksums") {
		t.Errorf("a file nobody listed was accepted: %v", err)
	}
	os.Remove(filepath.Join(out, "planted"))

	os.Remove(filepath.Join(out, "SHA256SUMS.sig"))
	if err := Verify(out, keys+".pub"); err == nil {
		t.Error("an unsigned release verified when a signature was required")
	}
	if err := Verify(out, ""); err != nil {
		t.Errorf("checksums alone did not verify: %v", err)
	}
}

// The notices are derived from what is compiled in. Anything compiled in
// without a licence file stops the release.
func TestNoticesCoverWhatIsCompiledIn(t *testing.T) {
	n, err := Notices(repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"github.com/tetratelabs/wazero", "github.com/itchyny/gojq", "github.com/evanw/esbuild",
		"github.com/santhosh-tekuri/jsonschema", "gopkg.in/yaml.v3", "mvdan.cc/sh/v3 v3.14.1, modified"} {
		if !strings.Contains(string(n), want) {
			t.Errorf("the notices do not cover %s", want)
		}
	}
	for _, never := range []string{"GNU GENERAL PUBLIC LICENSE", "GNU AFFERO", "GNU LESSER"} {
		if strings.Contains(string(n), never) {
			t.Errorf("something compiled in is under the %s", never)
		}
	}
}

// serve builds a release that knows it is served from a local address, and
// serves it. The address is known before the build, because the build writes
// it into the runner and the install script.
func serve(t *testing.T) (dir, base string) {
	t.Helper()
	dir = t.TempDir()
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	t.Cleanup(srv.Close)
	if err := Build(repo, dir, "0.0.0-test", []string{host}, "", srv.URL); err != nil {
		t.Fatal(err)
	}
	if err := Verify(dir, ""); err != nil {
		t.Fatalf("a release with install scripts did not verify: %v", err)
	}
	return dir, srv.URL
}

func install(t *testing.T, dir, into string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command("sh", filepath.Join(dir, "install.sh"), "--client", "none", "--dir", into)
	return cmd.CombinedOutput()
}

// The whole path a person on a new machine takes: the install script fetches
// the runner and checks it, and the runner fetches its own bash-compatible
// interpreter and checks that, and a primitive written in bash runs.
func TestInstallScriptThenABashPrimitive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is for macOS and Linux; Windows has install.ps1")
	}
	dir, _ := serve(t)
	into := t.TempDir()
	if out, err := install(t, dir, into); err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	bin := filepath.Join(into, "tap-runtime")
	out, err := exec.Command(bin, "version").Output()
	if err != nil || strings.TrimSpace(string(out)) != "tap-runtime 0.0.0-test" {
		t.Fatalf("the installed program says %q, %v", out, err)
	}

	pkg := t.TempDir()
	os.WriteFile(filepath.Join(pkg, "primitive.yaml"), []byte("apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.example, name: hello, version: 0.1.0}\nexecution: {entrypoint: main.sh}\n"), 0o644)
	os.WriteFile(filepath.Join(pkg, "main.sh"), []byte("echo hello from a released runner\n"), 0o644)
	store := t.TempDir()
	cmd := exec.Command(bin, "--interpreters", store, "--runs", t.TempDir(), "--cache", t.TempDir(), pkg)
	cmd.Dir = pkg
	got, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(got), "hello from a released runner") {
		t.Fatalf("the primitive did not run: %v\n%s", err, got)
	}
	if !strings.Contains(string(got), "fetching") {
		t.Errorf("the interpreter was not fetched from the release:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(store, "sh-0.0.0-test.wasm")); err != nil {
		t.Errorf("the interpreter is not in the store: %v", err)
	}
}

// A runner that was altered where it is served is not installed.
func TestInstallScriptRefusesAnAlteredRunner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is for macOS and Linux; Windows has install.ps1")
	}
	dir, _ := serve(t)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "tap-runtime-") {
			raw, _ := os.ReadFile(filepath.Join(dir, e.Name()))
			os.WriteFile(filepath.Join(dir, e.Name()), append(raw, 0), 0o755)
		}
	}
	into := t.TempDir()
	out, err := install(t, dir, into)
	if err == nil || !strings.Contains(string(out), "nothing was installed") {
		t.Fatalf("an altered runner was installed: %v\n%s", err, out)
	}
	left, _ := os.ReadDir(into)
	if len(left) != 0 {
		t.Errorf("the refused download was left behind: %v", left)
	}
}

// Both scripts carry the digest of every runner they can install.
func TestInstallersPinEveryRunner(t *testing.T) {
	dir := t.TempDir()
	if err := Build(repo, dir, "0.0.0-test", []string{"linux/amd64", "windows/amd64"}, "", "https://example.com/r"); err != nil {
		t.Fatal(err)
	}
	sums, _ := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	for script, runner := range map[string]string{"install.sh": "tap-runtime-0.0.0-test-linux-amd64", "install.ps1": "tap-runtime-0.0.0-test-windows-amd64.exe"} {
		text, err := os.ReadFile(filepath.Join(dir, script))
		if err != nil {
			t.Fatal(err)
		}
		var want string
		for _, line := range strings.Split(string(sums), "\n") {
			if strings.HasSuffix(line, "  "+runner) {
				want = strings.Fields(line)[0]
			}
		}
		if want == "" || !strings.Contains(string(text), want) {
			t.Errorf("%s does not carry the digest of %s", script, runner)
		}
		if strings.Contains(string(text), "@") && strings.Contains(string(text), "@VERSION@") {
			t.Errorf("%s was not filled in", script)
		}
	}
}

func TestBuildRefusesWhatItCannotWriteSafely(t *testing.T) {
	for _, bad := range [][2]string{{"0.1", ""}, {"0.1.0 -X main.x=y", ""}, {"0.1.0", "http://example.com/r"}, {"0.1.0", "https://example.com/$(id)"}} {
		if err := Build(repo, t.TempDir(), bad[0], []string{host}, "", bad[1]); err == nil {
			t.Errorf("version %q with address %q was built", bad[0], bad[1])
		}
	}
}
