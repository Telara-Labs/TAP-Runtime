package launch

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise npx itself with an empty user home and npm cache. A version check
// alone could accidentally exercise an existing tap command, so inspect the
// command npx puts first on PATH before running the harmless Bash example.
func TestNpxPublishedPackage(t *testing.T) {
	v := version(t)
	m := newMachine(t)
	npx, err := exec.LookPath("npx")
	if err != nil {
		t.Fatal("npx is not on PATH")
	}
	cache := filepath.Join(m.home, "npm-cache")
	work := filepath.Join(m.home, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	args := []string{"--yes", "--cache", cache, "--package", pkg + "@" + v, "--"}
	run := func(command ...string) result {
		t.Helper()
		return m.run(work, npx, append(append([]string{}, args...), command...)...)
	}
	want(t, run("tap", "version"), 0, "tap "+v)

	// npx prepends the selected package's node_modules/.bin. Verify both that
	// location and its package version; neither a global nor a project tap
	// installation can satisfy this check.
	probe := `const fs=require('node:fs'),p=require('node:path');
const name=process.platform==='win32'?'tap.cmd':'tap';
const command=process.env.PATH.split(p.delimiter).map(d=>p.join(d,name)).find(f=>fs.existsSync(f));
if(!command)throw new Error('npx did not provide tap');
const manifest=p.join(p.dirname(command),'..','@telaralabs','tap','package.json');
console.log(JSON.stringify({command,version:JSON.parse(fs.readFileSync(manifest,'utf8')).version}));`
	// npx.cmd forwards through cmd.exe on Windows, where multiline -e
	// arguments can stop after the first line. A fixture file preserves the
	// exact same probe on every platform.
	probePath := filepath.Join(work, "npx-resolution.cjs")
	if err := os.WriteFile(probePath, []byte(probe), 0o600); err != nil {
		t.Fatal(err)
	}
	r := run("node", probePath)
	want(t, r, 0)
	var resolved struct {
		Command string `json:"command"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(r.out)), &resolved); err != nil {
		t.Fatalf("npx command resolution is not JSON: %v\n%s", err, r.out)
	}
	rel, err := filepath.Rel(cache, resolved.Command)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		t.Fatalf("npx selected a command outside its owned cache: %q (%v)", resolved.Command, err)
	}
	if resolved.Version != v {
		t.Fatalf("npx selected package version %q, want %q", resolved.Version, v)
	}
	t.Logf("actual npx selected %s, package version %s", resolved.Command, resolved.Version)

	copyTree(t, filepath.Join(root, "examples", "hello-sh"), filepath.Join(work, "hello-sh"))
	want(t, run("tap", "hello-sh"), 0, "RESULT (exit 0)", "hello from bash")
}
