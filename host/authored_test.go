package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gitlab.com/telara-labs/tap-runtime/discover"
)

// TestAuthoredPackageValidatesThroughTheRealRunner is the author path's
// check (TENG-2936) against this runner, built from this tree: `tap discover
// validate` starts it as a separate process on fresh fixtures, compares each
// result with an oracle and inspects the fixture afterwards. A package that
// answers wrongly, or changes the fixture, must not pass.
func TestAuthoredPackageValidatesThroughTheRealRunner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the oracle is a POSIX shell script")
	}
	store := interpreterStore(t)
	bin := filepath.Join(t.TempDir(), "tap")
	build := exec.Command("go", "build", "-o", bin, "./host")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the runner: %v\n%s", err, out)
	}

	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "oracle.sh"), []byte(`printf '{"lines": %d}\n' "$(( $(wc -l < "$1") ))"`+"\n"), 0o644)
	cases := discover.CaseFile{Package: "line-count", Oracle: []string{"sh", "oracle.sh"}, Cases: []discover.Case{
		{ID: "normal", Kind: "normal", Setup: []discover.SetupStep{{Write: "in/a.txt", Text: "one\ntwo\nthree\n"}}, Args: []string{"in/a.txt"}},
		{ID: "empty", Kind: "empty", Setup: []discover.SetupStep{{Write: "in/a.txt", Text: ""}}, Args: []string{"in/a.txt"}},
	}}
	b, _ := json.Marshal(cases)
	casesPath := filepath.Join(root, "cases.json")
	os.WriteFile(casesPath, b, 0o644)
	if _, err := discover.Freeze(casesPath); err != nil {
		t.Fatal(err)
	}

	const readOnly = "apiVersion: primitives.telara.dev/v3\nkind: Primitive\n" +
		"metadata: {publisher: dev.test, name: line-count, version: 0.1.0}\nexecution: {entrypoint: main.sh}\n" +
		"files:\n  - {path: in, access: read}\n"
	const writes = readOnly + "  - {path: in/out.txt, access: write}\n"
	for _, c := range []struct {
		name, manifest, script string
		pass                   bool
		why                    string
	}{
		{"a correct package passes", readOnly, `n=$(cat "$1" | wc -l)` + "\n" + `echo "{\"lines\": $n}"` + "\n", true, ""},
		{"a wrong answer fails", readOnly, `n=$(cat "$1" | wc -l)` + "\n" + `echo "{\"lines\": $((n + 1))}"` + "\n", false, "result differs"},
		{"an effect fails", writes, `n=$(cat "$1" | wc -l)` + "\n" + `echo x > in/out.txt` + "\n" + `echo "{\"lines\": $n}"` + "\n", false, "changed the fixture"},
	} {
		t.Run(c.name, func(t *testing.T) {
			pkg := writePackage(t, c.manifest, c.script)
			rec, err := discover.Validate(discover.ValidateOptions{Package: pkg, Cases: casesPath, Runner: bin,
				RunnerFlags: []string{"-interpreters", store, "-approve"}})
			if err != nil {
				t.Fatal(err)
			}
			if rec.ManifestCheck.Exit != 0 {
				t.Fatalf("manifest check: %s", rec.ManifestCheck.Raw)
			}
			for _, cr := range rec.Cases {
				if len(cr.HostLog) == 0 || cr.Package.Exit < 0 {
					t.Errorf("%s: the runner did not run the package: %+v", cr.ID, cr)
				}
			}
			if rec.AllPassed != c.pass {
				t.Fatalf("all passed %v, want %v: %+v", rec.AllPassed, c.pass, rec.Cases)
			}
			if c.why != "" {
				var all []string
				for _, cr := range rec.Cases {
					all = append(all, cr.Failures...)
				}
				if !strings.Contains(strings.Join(all, "; "), c.why) {
					t.Errorf("failures %v lack %q", all, c.why)
				}
			}
		})
	}
}
