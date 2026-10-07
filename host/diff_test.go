package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

const diffManifest = `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: example.test, name: greeting, version: 1.0.0}
interface:
  inputSchema: {type: object, properties: {name: {type: string}}, required: [name]}
  outputSchema: {type: object, properties: {message: {type: string}}, required: [message]}
execution: {entrypoint: main.sh}
`

func TestPackageDiffReview(t *testing.T) {
	old := writePackage(t, diffManifest, "echo old\n")
	for _, tc := range []struct {
		name, manifest, code, path, review string
		sameVersion, widening              bool
	}{
		{"version", strings.Replace(diffManifest, "1.0.0", "1.0.1", 1), "echo old\n", "metadata.version", "fresh review", false, false},
		{"same-version-code", diffManifest, "echo new\n", "entrypoint.sha256", "same version", true, false},
		{"input-required", strings.Replace(diffManifest, "required: [name]", "required: [name, tenant]", 1), "echo old\n", "interface.inputSchema.required", "callers may break", true, false},
		{"output-type", strings.Replace(diffManifest, "message: {type: string}", "message: {type: integer}", 1), "echo old\n", "interface.outputSchema.properties.message.type", "callers may break", true, false},
		{"permission", diffManifest + "files: [{path: notes/*, access: write}]\n", "echo old\n", "files", "files changed", true, true},
		{"limit", strings.Replace(diffManifest, "entrypoint: main.sh", "entrypoint: main.sh, timeoutSeconds: 1", 1), "echo old\n", "execution.timeoutSeconds", "execution changed", true, false},
		{"formatting", diffManifest + "# different bytes\n", "echo old\n", "", "without a parsed field change", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := writePackage(t, tc.manifest, tc.code)
			r, err := comparePackages(old, next)
			if err != nil {
				t.Fatal(err)
			}
			if r.Status != "review_required" || r.SameVersionChange != tc.sameVersion || (len(r.AuthorityWidening) > 0) != tc.widening {
				t.Fatalf("incorrect classification: %+v", r)
			}
			found := tc.path == ""
			for _, c := range r.Changes {
				if c.Path == tc.path {
					found = true
				}
			}
			if !found || !strings.Contains(strings.Join(r.Review, "\n"), tc.review) {
				t.Fatalf("missing change/review: %+v", r)
			}
			want, _, err := mf.RunDigest(next)
			if err != nil || r.After.Digest != want {
				t.Fatalf("diff digest %s != execution digest %s: %v", r.After.Digest, want, err)
			}
			var stdout, stderr bytes.Buffer
			if code := diffCommand([]string{"--json", old, next}, &stdout, &stderr); code != 1 || stderr.Len() != 0 {
				t.Fatalf("exit %d: %s", code, &stderr)
			}
			var decoded packageDiff
			if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil || decoded.Status != "review_required" {
				t.Fatalf("invalid JSON %s: %v", &stdout, err)
			}
		})
	}
}

func TestPackageDiffIdenticalAndReadOnly(t *testing.T) {
	pkg := writePackage(t, diffManifest, "echo must-not-execute\n")
	before, _ := os.ReadDir(pkg)
	var stdout, stderr bytes.Buffer
	if code := diffCommand([]string{"--json", pkg, pkg}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, &stderr)
	}
	var r packageDiff
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil || r.Status != "identical" || len(r.Changes) != 0 || r.SameVersionChange {
		t.Fatalf("incorrect identical diff: %s: %v", &stdout, err)
	}
	after, _ := os.ReadDir(pkg)
	if len(before) != len(after) || strings.Contains(stdout.String(), "must-not-execute") {
		t.Fatal("comparison executed or modified the package")
	}
}

func TestPackageDiffInvalidCannotLookCompatible(t *testing.T) {
	old := writePackage(t, diffManifest, "echo old\n")
	for _, tc := range []struct{ name, manifest string }{
		{"wrong-identity", strings.Replace(diffManifest, "name: greeting", "name: unrelated", 1)},
		{"old-format", strings.Replace(diffManifest, "dev/v3", "dev/v1", 1)},
		{"escape", strings.Replace(diffManifest, "entrypoint: main.sh", "entrypoint: ../main.sh", 1)},
		{"unknown-bound", diffManifest + "timeot: 4\n"},
		{"no-version", strings.Replace(diffManifest, "version: 1.0.0", "version: ''", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := writePackage(t, tc.manifest, "echo next\n")
			var stdout, stderr bytes.Buffer
			if code := diffCommand([]string{"--json", old, next}, &stdout, &stderr); code != 2 || stdout.Len() != 0 {
				t.Fatalf("invalid looks comparable: exit %d %s %s", code, &stdout, &stderr)
			}
		})
	}
	for _, args := range [][]string{nil, {old}, {old, old, old}, {"--bad", old, old}} {
		var out, err bytes.Buffer
		if code := diffCommand(args, &out, &err); code != 2 {
			t.Fatalf("args %v: exit %d", args, code)
		}
	}
}

func TestPackageDiffRefusesSymlinkedProgram(t *testing.T) {
	pkg := writePackage(t, diffManifest, "echo old\n")
	linked := writePackage(t, diffManifest, "echo next\n")
	path := filepath.Join(linked, "main.sh")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(pkg, "main.sh"), path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := comparePackages(pkg, linked); err == nil || !strings.Contains(err.Error(), "symlinked") {
		t.Fatalf("symlink accepted: %v", err)
	}
}

func TestPackageDiffDeclarationsCannotHideChanges(t *testing.T) {
	// The authority helper recognizes higher effect ranks. Removing a pin or
	// switching between equally ranked effects still changes the exact diff.
	base := diffManifest + "tools: [{alias: lookup, capability: notes.search, effect: destructive, pin: {server: Notes, tool: search}}]\n"
	old := writePackage(t, base, "echo old\n")
	next := writePackage(t, strings.Replace(strings.Replace(base, "effect: destructive", "effect: financial", 1), ", pin: {server: Notes, tool: search}", "", 1), "echo old\n")
	r, err := comparePackages(old, next)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "review_required" || !strings.Contains(strings.Join(r.Review, "\n"), "tools changed") {
		t.Fatalf("change hidden: %+v", r)
	}
	for _, c := range r.Changes {
		if c.Path == "tools" {
			return
		}
	}
	t.Fatal("missing exact tool declaration diff")
}

func TestPackageDiffPreservesNullPresenceAndLargeBounds(t *testing.T) {
	for _, tc := range []struct{ name, old, next, path string }{
		{"null", diffManifest, strings.Replace(diffManifest, "name: {type: string}", "name: {type: string, default: null}", 1), "interface.inputSchema.properties.name.default"},
		{"large-bound", strings.Replace(diffManifest, "name: {type: string}", "name: {type: integer, maximum: 9007199254740992}", 1), strings.Replace(diffManifest, "name: {type: string}", "name: {type: integer, maximum: 9007199254740993}", 1), "interface.inputSchema.properties.name.maximum"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := comparePackages(writePackage(t, tc.old, "echo old\n"), writePackage(t, tc.next, "echo old\n"))
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range r.Changes {
				if c.Path != tc.path {
					continue
				}
				if tc.name == "null" && (c.BeforePresent || !c.AfterPresent) {
					t.Fatalf("presence lost: %+v", c)
				}
				if tc.name == "large-bound" && c.Before == c.After {
					t.Fatalf("precision lost: %+v", c)
				}
				return
			}
			t.Fatalf("contract change hidden: %+v", r)
		})
	}
}

func TestPackageDiffTextDistinguishesAbsentFromNull(t *testing.T) {
	old := writePackage(t, diffManifest, "echo old\n")
	next := writePackage(t, strings.Replace(diffManifest, "name: {type: string}", "name: {type: string, default: null}", 1), "echo old\n")
	var stdout, stderr bytes.Buffer
	if code := diffCommand([]string{old, next}, &stdout, &stderr); code != 1 || !strings.Contains(stdout.String(), "name.default: <absent> -> null") {
		t.Fatalf("addition hidden: exit %d %s %s", code, &stdout, &stderr)
	}
	stdout.Reset()
	if code := diffCommand([]string{next, old}, &stdout, &stderr); code != 1 || !strings.Contains(stdout.String(), "name.default: null -> <absent>") {
		t.Fatalf("removal hidden: exit %d %s %s", code, &stdout, &stderr)
	}
}

func TestPackageDiffCLI(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	old := writePackage(t, diffManifest, "echo old\n")
	next := writePackage(t, strings.Replace(diffManifest, "1.0.0", "2.0.0", 1), "echo new\n")
	for _, tc := range []struct {
		next string
		exit int
	}{{old, 0}, {next, 1}} {
		cmd := exec.Command(self, tapMainArg, "diff", "--json", old, tc.next)
		out, _ := cmd.CombinedOutput()
		if cmd.ProcessState.ExitCode() != tc.exit {
			t.Fatalf("exit %d: %s", cmd.ProcessState.ExitCode(), out)
		}
		var r packageDiff
		if err := json.Unmarshal(out, &r); err != nil || r.Before.Ref != "example.test/greeting@1.0.0" {
			t.Fatalf("CLI did not route to diff: %s, %v", out, err)
		}
	}
}

func TestPackageVersionsRunIndependentlyAndRejectChangedDigest(t *testing.T) {
	// These calls cross the real MCP wire and execute real shell programs in
	// the runner's WebAssembly sandbox. No external service is involved.
	c := startServer(t, true, accept)
	old := writePackage(t, diffManifest, `echo '{"message":"hello"}'`+"\n")
	newManifest := strings.Replace(strings.Replace(diffManifest, "1.0.0", "2.0.0", 1), "message", "greeting", -1)
	next := writePackage(t, newManifest, `echo '{"greeting":"hello"}'`+"\n")
	r, err := comparePackages(old, next)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "review_required" || !strings.Contains(strings.Join(r.Review, "\n"), "callers may break") {
		t.Fatalf("changed contract hidden: %+v", r)
	}
	for _, tc := range []struct{ pkg, want string }{{old, `"message":"hello"`}, {next, `"greeting":"hello"`}} {
		identity := c.stagePackage(tc.pkg)
		args := map[string]any{"ref": identity["ref"], "digest": identity["digest"], "args": []string{`{"name":"reader"}`}}
		result := c.call("tools/call", map[string]any{"name": "tap_run", "arguments": args})
		if result["isError"] == true || !strings.Contains(toolText(t, result), tc.want) {
			t.Fatalf("version %s did not run: %#v", identity["ref"], result)
		}
		if tc.pkg == next {
			continue
		}
		// Keep the old ref and version but mutate its installed code. The old
		// digest must fail rather than execute the new bytes.
		sum := sha256.Sum256([]byte(tc.pkg))
		path := filepath.Join(c.catalogRoot, hex.EncodeToString(sum[:8]), "main.sh")
		if err := os.WriteFile(path, []byte("echo changed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		refused := c.call("tools/call", map[string]any{"name": "tap_run", "arguments": args})
		if refused["isError"] != true {
			t.Fatalf("stale digest ran: %#v", refused)
		}
	}
}
