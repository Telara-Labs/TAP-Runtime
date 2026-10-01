package discover

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitlab.com/telara-labs/tap-runtime/discover/pyparse"
)

func fileReplaceSession(id, file, old, new string) Session {
	body := "path = '" + file + "'\ntext = open(path).read()\nchanged = text.replace('" + old + "', '" + new + "')\nopen(path, 'w').write(changed)\n"
	return selSession(id, "Replace text in a file", Call{Tool: "shell", Command: "cd project && python3 - <<'PY'\n" + body + "PY\necho done", Outcome: OutcomeOK})
}

func TestStrictInlineFileReplaceCompilesOnlyProvedSameFileShape(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("Python AST parser unavailable")
	}
	ss := []Session{fileReplaceSession("one", "a.txt", "old", "new"), fileReplaceSession("two", "b.txt", "before", "after")}
	spans := SelectSpanProposals(ss)
	if len(spans) != 2 || spans[0].CodeShape == "" || spans[0].CodeShape != spans[1].CodeShape {
		t.Fatalf("strict scripts did not produce one shape: %+v", spans)
	}
	candidates := GroupLogicCandidates(spans)
	if len(candidates) != 1 {
		t.Fatalf("strict scripts did not produce one retrieval family: %+v", candidates)
	}
	qualified, ok := generatedCandidateTaskEvidence(candidates[0], map[string]SpanProposal{spans[0].ID: spans[0], spans[1].ID: spans[1]})
	if !ok {
		t.Fatal("cross-session strict source was lost before parsing")
	}
	variants, err := GroupProgramVariants(qualified, spans, ss)
	if err != nil || len(variants) != 1 {
		t.Fatalf("code variant grouping: %+v %v", variants, err)
	}
	g, err := SynthesizeProgramGraph(variants[0], spans, ss)
	if err != nil || len(g.Problems) != 0 || g.InlineFileReplace == nil || !g.InlineFileReplace.Embedded {
		t.Fatalf("strict AST graph: %+v %v", g, err)
	}
	pkg, err := GenerateProgramPackage(g)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Manifest.Files) != 1 || pkg.Manifest.Files[0].Path != "." || pkg.Manifest.Files[0].Access != "write" ||
		!bytes.Contains(pkg.Files["main.py"], []byte("tap.read(inputs['file_path'])")) ||
		!bytes.Contains(pkg.Files["main.py"], []byte("tap.write(inputs['file_path'], after)")) ||
		!bytes.Contains(pkg.Files["README.md"], []byte("embedded in a larger shell")) {
		t.Fatalf("package hid reach or surrounding-shell exclusion: %+v", pkg.Manifest)
	}
	bad := strings.Replace(ss[1].Calls[0].Command, "open(path, 'w')", "open('other.txt', 'w')", 1)
	ss[1].Calls[0].Command = bad
	spans = SelectSpanProposals(ss)
	for _, c := range GroupLogicCandidates(spans) {
		g, err := SynthesizeProgramGraph(c, spans, ss)
		if err == nil && len(g.Problems) == 0 && g.InlineFileReplace != nil {
			t.Fatal("different write target compiled")
		}
	}
}

func TestStrictInlineFileReplaceRejectsAliasAndAdditionalEffects(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("Python AST parser unavailable")
	}
	for _, body := range []string{
		"path='a'\npath=open(path).read()\nchanged=path.replace('x','y')\nopen(path,'w').write(changed)",
		"path='a'\ndata=open(path).read()\npath=data.replace('x','y')\nopen(path,'w').write(path)",
		"path='a'\ndata=open(path).read()\nchanged=data.replace('x','y')\nopen('other','w').write(changed)",
		"path='a'\ndata=open(path).read()\nchanged=data.replace('x','y')\nprint('side effect')\nopen(path,'w').write(changed)",
	} {
		if pyparse.StrictInlineFileReplace(body) {
			t.Fatalf("unsafe body passed strict AST parser: %q", body)
		}
	}
}

func TestInlineFileReplaceRunsThroughHostAndRespectsFileReach(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs TAP host")
	}
	g := &ProgramGraph{CandidateID: "lc_filereplacetest", Executions: 2, Sessions: 2,
		InlineFileReplace: &InlineFileReplace{Embedded: true},
		Inputs:            []ProgramInput{{Name: "file_path", Type: "string"}, {Name: "old", Type: "string"}, {Name: "new", Type: "string"}},
		Steps:             []ProgramStep{{Role: "read file", Tool: "tap.read", Effect: "read"}, {Role: "replace text", Tool: "python.str.replace", Effect: "none"}, {Role: "write same file", Tool: "tap.write", Effect: "write"}}}
	pkg, err := GenerateProgramPackage(g)
	if err != nil {
		t.Fatal(err)
	}
	pkgDir := t.TempDir()
	for name, data := range pkg.Files {
		if err := os.WriteFile(filepath.Join(pkgDir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(t.TempDir(), "tap")
	build := exec.Command("go", "build", "-o", bin, "./host")
	build.Dir = ".."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build TAP host: %v\n%s", err, output)
	}
	work := t.TempDir()
	inside := filepath.Join(work, "inside.txt")
	if err := os.WriteFile(inside, []byte("old old"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(approve bool, input string) ([]byte, error) {
		args := []string{"--runs", t.TempDir()}
		if approve {
			args = append([]string{"--approve"}, args...)
		}
		cmd := exec.Command(bin, append(args, pkgDir, input)...)
		cmd.Dir = work
		return cmd.CombinedOutput()
	}
	if output, err := run(false, `{"file_path":"inside.txt","old":"old","new":"new"}`); err == nil || !bytes.Contains(output, []byte("write needs approval")) {
		t.Fatalf("write gate did not refuse: %v\n%s", err, output)
	}
	if got, _ := os.ReadFile(inside); string(got) != "old old" {
		t.Fatalf("unapproved write changed file: %q", got)
	}
	if output, err := run(true, `{"file_path":"inside.txt","old":"old","new":"new"}`); err != nil {
		t.Fatalf("approved fresh input failed: %v\n%s", err, output)
	}
	if got, _ := os.ReadFile(inside); string(got) != "new new" {
		t.Fatalf("fresh replacement did not run: %q", got)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if output, err := run(true, `{"file_path":"`+outside+`","old":"old","new":"new"}`); err == nil || !bytes.Contains(output, []byte("outside the files")) {
		t.Fatalf("out-of-scope path was not refused: %v\n%s", err, output)
	}
	if got, _ := os.ReadFile(outside); string(got) != "old" {
		t.Fatalf("outside file changed: %q", got)
	}
}
