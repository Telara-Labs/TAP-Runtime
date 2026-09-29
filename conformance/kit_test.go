package conformance

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func buildInto(t *testing.T, dir, out, pkg string, env ...string) string {
	t.Helper()
	path := filepath.Join(dir, out)
	cmd := exec.Command("go", "build", "-o", path, pkg)
	cmd.Dir = ".."
	cmd.Env = append(os.Environ(), env...)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building %s: %v\n%s", pkg, err, b)
	}
	return path
}

func TestThisRunnerPassesEveryLane(t *testing.T) {
	dir := t.TempDir()
	runner := buildInto(t, dir, "tap", "./host")
	store := filepath.Join(dir, "store")
	os.MkdirAll(store, 0o755)
	buildInto(t, store, "sh.wasm", "./guest-sh", "GOOS=wasip1", "GOARCH=wasm")

	var log bytes.Buffer
	lanes := Run([]string{runner, "serve", "--interpreters", store, "--runs", filepath.Join(dir, "runs")}, &log)
	t.Logf("\n%s", log.String())
	if len(lanes) < 15 {
		t.Fatalf("only %d lanes ran", len(lanes))
	}
	for _, l := range lanes {
		if !l.Passed {
			t.Errorf("%s: %s", l.Name, l.Detail)
		}
	}
}

// A kit that cannot fail proves nothing. This runner has no sandbox, reads
// no manifest and asks nobody, and the kit must say so.
func TestARunnerThatDoesEverythingWrongFails(t *testing.T) {
	dir := t.TempDir()
	bad := buildInto(t, dir, "badrunner", "./conformance/testdata/badrunner")
	var log bytes.Buffer
	lanes := Run([]string{bad}, &log)
	t.Logf("\n%s", log.String())

	mustFail := map[string]bool{
		"a program cannot read a file it did not declare":             true,
		"a program cannot write a file it did not declare":            true,
		"a declared directory cannot be left through a symbolic link": true,
		"a program cannot run a command it did not declare":           true,
		"a program does not see the runner's environment":             true,
		"a change is made when a person says yes":                     true, // it changes things and asks nobody
		"no change is made when a person says no":                     true,
		"a number allowed is a number not exceeded":                   true,
		"a client that cannot show a prompt gets no changes":          true,
		"a manifest with a field the format does not have is refused": true,
		"an entrypoint outside the package is refused":                true,
		"a subprocess runtime is not run as if it were contained":     true,
		"a program can fetch only the origins it declared":            true,
		"a flag before a subcommand must be declared, with its value": true,
	}
	for _, l := range lanes {
		if mustFail[l.Name] && l.Passed {
			t.Errorf("the kit passed a runner that does not do this: %s", l.Name)
		}
	}
	seen := map[string]bool{}
	for _, l := range lanes {
		seen[l.Name] = true
	}
	for name := range mustFail {
		if !seen[name] {
			t.Errorf("lane %q did not run", name)
		}
	}
}
