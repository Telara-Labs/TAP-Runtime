package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

var updateKitGolden = flag.Bool("update-kit-golden", false, "rewrite the Kit export golden files")

func kitPkg(name string, fetch ...mf.Fetch) kitPackage {
	return kitPackage{Name: name, Manifest: &mf.Manifest{Fetch: fetch}}
}

func compareGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "kit", name)
	if *updateKitGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from its golden file; rerun with -update-kit-golden after reading the diff\n--- got\n%s", name, got)
	}
}

func TestKitGoldenFiles(t *testing.T) {
	pkgs := []kitPackage{
		kitPkg("dev.example/releases", mf.Fetch{Origin: "https://api.github.com", Methods: []string{"GET"}}),
		kitPkg("dev.example/issues",
			mf.Fetch{Origin: "https://api.github.com/", Methods: []string{"post", "GET"}},
			mf.Fetch{Origin: "https://example.atlassian.net:443"}),
	}
	desc, df, err := kitFiles("0.2.23", pkgs)
	if err != nil {
		t.Fatal(err)
	}
	compareGolden(t, "tap.yaml.golden", desc)
	compareGolden(t, "tap.dockerfile.golden", df)

	// No primitive declares a fetch: the Kit grants no network at all.
	desc, _, err = kitFiles("0.2.23", []kitPackage{kitPkg("dev.example/local")})
	if err != nil {
		t.Fatal(err)
	}
	compareGolden(t, "tap-no-fetch.yaml.golden", desc)
}

func TestKitDeclaresNoCredentials(t *testing.T) {
	desc, df, err := kitFiles("0.2.23", []kitPackage{kitPkg("a/b", mf.Fetch{Origin: "https://api.github.com"})})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"credential", "inject", "apiKey"} {
		if strings.Contains(string(desc), s) || strings.Contains(string(df), s) {
			t.Fatalf("Kit mentions %q", s)
		}
	}
}

func TestKitRulesAreTheExactUnion(t *testing.T) {
	a := []kitPackage{
		kitPkg("p/one",
			mf.Fetch{Origin: "https://b.example.com", Methods: []string{"POST", "GET"}},
			mf.Fetch{Origin: "https://a.example.com"}),
		kitPkg("p/two",
			mf.Fetch{Origin: "https://B.example.com/", Methods: []string{"get", "DELETE"}},
			mf.Fetch{Origin: "https://a.example.com", Methods: []string{"GET", "GET"}},
			mf.Fetch{Origin: "https://c.example.com:443", Methods: []string{"HEAD"}}),
	}
	want := []kitRule{
		{Host: "a.example.com", Methods: []string{"GET"}},
		{Host: "b.example.com", Methods: []string{"DELETE", "GET", "POST"}},
		{Host: "c.example.com", Methods: []string{"HEAD"}},
	}
	got, err := kitRules(a)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("union\n got %#v\nwant %#v", got, want)
	}
	// Package order does not change the answer.
	b := []kitPackage{a[1], a[0]}
	got2, err := kitRules(b)
	if err != nil || !reflect.DeepEqual(got2, want) {
		t.Fatalf("reordered union %#v, %v", got2, err)
	}
	for _, r := range got {
		if strings.Contains(r.Host, "*") {
			t.Fatalf("wildcard in %q", r.Host)
		}
	}
	// A host nobody declared is not in the union, and a method nobody
	// declared for a host is not either.
	for _, r := range got {
		if r.Host == "a.example.com" && len(r.Methods) != 1 {
			t.Fatalf("a.example.com gained methods: %v", r.Methods)
		}
	}
}

func TestKitRefusesWhatThePolicyCannotHold(t *testing.T) {
	cases := map[string]string{
		"http://insecure.example.com":  "http",
		"https://api.example.com:8443": "port 8443",
		"https://*.atlassian.net":      "wildcard",
		"not an origin":                "scheme and a host",
	}
	for origin, why := range cases {
		_, _, err := kitFiles("0.2.23", []kitPackage{
			kitPkg("p/fine", mf.Fetch{Origin: "https://ok.example.com"}),
			kitPkg("dev.example/bad", mf.Fetch{Origin: origin}),
		})
		if err == nil {
			t.Fatalf("%q exported", origin)
		}
		msg := err.Error()
		if !strings.Contains(msg, "dev.example/bad") || !strings.Contains(msg, origin) || !strings.Contains(msg, why) {
			t.Fatalf("%q: refusal %q should name the package, the origin and %q", origin, msg, why)
		}
		if strings.Contains(msg, "p/fine") {
			t.Fatalf("refusal blames a package that is fine: %s", msg)
		}
	}
	if _, _, err := kitFiles("dev", nil); err == nil {
		t.Fatal("exported from a non-release runner")
	}
}

func TestKitExportDefaultsToEveryInstalledPrimitive(t *testing.T) {
	home, cfg, work, out := t.TempDir(), t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "kit")
	t.Setenv("HOME", home)
	oldConfig := userConfigDir
	userConfigDir = func() (string, error) { return cfg, nil }
	t.Cleanup(func() { userConfigDir = oldConfig })
	t.Chdir(work)
	oldVersion := version
	version = "0.2.23"
	t.Cleanup(func() { version = oldVersion })

	root := filepath.Join(cfg, "tap", "primitives")
	install := func(name, fetch string) {
		dir := writePackage(t, `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: example.test, name: `+name+`, version: 1.0.0, description: d}
execution: {entrypoint: main.sh}
fetch: [`+fetch+`]
`, "echo ok\n")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(dir, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	install("one", `{origin: "https://one.example.com", methods: [GET]}`)
	install("two", `{origin: "https://two.example.com", methods: [GET, POST]}`)
	var stdout, stderr bytes.Buffer

	if code := kitCommand([]string{"export", "--out", out}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	desc, err := os.ReadFile(filepath.Join(out, "tap.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(desc)
	for _, h := range []string{"one.example.com", "two.example.com"} {
		if !strings.Contains(s, h) {
			t.Fatalf("descriptor lacks %s:\n%s", h, s)
		}
	}
	if strings.Contains(s, "old.example.com") {
		t.Fatalf("descriptor includes an origin from nothing installed:\n%s", s)
	}
	if _, err := os.Stat(filepath.Join(out, "tap.dockerfile")); err != nil {
		t.Fatal(err)
	}

	// Naming a package directory exports that package alone.
	one := filepath.Join(root, "one")
	out2 := filepath.Join(t.TempDir(), "kit")
	stdout.Reset()
	stderr.Reset()
	if code := kitCommand([]string{"export", "--out", out2, one}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	desc, _ = os.ReadFile(filepath.Join(out2, "tap.yaml"))
	if !strings.Contains(string(desc), "one.example.com") || strings.Contains(string(desc), "two.example.com") {
		t.Fatalf("explicit package export:\n%s", desc)
	}
}

func TestKitExportRefusalWritesNothing(t *testing.T) {
	dir := writePackage(t, `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: example.test, name: wild, version: 1.0.0, description: d}
execution: {entrypoint: main.sh}
fetch: [{origin: "https://*.atlassian.net", methods: [GET]}]
`, "echo ok\n")
	oldVersion := version
	version = "0.2.23"
	t.Cleanup(func() { version = oldVersion })
	out := filepath.Join(t.TempDir(), "kit")
	var stdout, stderr bytes.Buffer
	if code := kitCommand([]string{"export", "--out", out, dir}, &stdout, &stderr); code == 0 {
		t.Fatal("wildcard origin exported")
	}
	if !strings.Contains(stderr.String(), "example.test/wild") || !strings.Contains(stderr.String(), "https://*.atlassian.net") {
		t.Fatalf("refusal: %s", stderr.String())
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("refusal left %s behind", out)
	}
}

// TestKitConformance builds the exported Kit with the local Docker daemon and
// runs the specification's own conformance suite against the image. It needs
// docker, the sandbox-kit-spec `kit-tck` on PATH, and network access to the
// release the dockerfile downloads, so it skips when the first two are absent.
func TestKitConformance(t *testing.T) {
	tck, err := exec.LookPath("kit-tck")
	if err != nil {
		t.Skip("kit-tck is not on PATH")
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker is not on PATH")
	}
	if err := exec.Command(docker, "info").Run(); err != nil {
		t.Skip("no Docker daemon is reachable")
	}
	// A docker-container builder is what exports an OCI layout and loads the
	// sandbox-kit frontend; the default builder does neither. This test makes
	// its own and removes it.
	builder := fmt.Sprintf("tap-kit-conformance-%d", os.Getpid())
	if out, err := exec.Command(docker, "buildx", "create", "--driver", "docker-container", "--name", builder).CombinedOutput(); err != nil {
		t.Skipf("no docker-container builder could be created: %v\n%s", err, out)
	}
	t.Cleanup(func() { exec.Command(docker, "buildx", "rm", "--force", builder).Run() })

	// The runner under test is not a release, so the Kit is built for the
	// newest release the dockerfile can download.
	const release = "0.2.23"
	cases := map[string][]kitPackage{
		"with-origins": {
			kitPkg("dev.example/a", mf.Fetch{Origin: "https://api.github.com", Methods: []string{"GET", "POST"}}),
			kitPkg("dev.example/b", mf.Fetch{Origin: "https://example.atlassian.net"}),
		},
		"no-origins": {kitPkg("dev.example/c")},
	}
	for name, pkgs := range cases {
		t.Run(name, func(t *testing.T) {
			desc, df, err := kitFiles(release, pkgs)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "tap.yaml"), desc, 0o644)
			os.WriteFile(filepath.Join(dir, "tap.dockerfile"), df, 0o644)
			archive := filepath.Join(dir, "kit.tar")
			build := exec.Command(docker, "buildx", "--builder", builder, "build", "--platform", "linux/"+runtime.GOARCH,
				"-f", filepath.Join(dir, "tap.yaml"), "--output", "type=oci,dest="+archive, "--tag", "tap-kit:test", dir)
			if out, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build the Kit: %v\n%s", err, out)
			}
			layout := filepath.Join(dir, "layout")
			if err := os.MkdirAll(layout, 0o755); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command("tar", "-xf", archive, "-C", layout).CombinedOutput(); err != nil {
				t.Fatalf("unpack: %v\n%s", err, out)
			}
			// The OCI export names the image by the tag's last segment.
			out, err := exec.Command(tck, "validate", "--verbose", "--layout", layout, "test").CombinedOutput()
			if err != nil {
				t.Fatalf("kit-tck validate failed: %v\n%s", err, out)
			}
			t.Logf("kit-tck validate:\n%s", out)
		})
	}
}
