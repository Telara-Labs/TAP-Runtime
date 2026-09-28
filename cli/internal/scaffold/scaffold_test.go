package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"telara.dev/tap/internal/model"
)

// TestScaffold_APIPackageMatchesSkillTemplate is the CHANGELOG.md v1 CLI fix
// item 5 regression: `tap init` must emit the tap-creator skill's own rich
// authoring templates (SKILL.md's claim, false before this fix -- STATUS.md
// G0 agenda item 5). This is an AUTHORING skeleton full of REPLACE_*
// placeholders for the author to fill in, so -- unlike the old minimal echo
// skeleton this replaces -- it does not validate/test clean unmodified;
// that invariant is retired along with the echo skeleton it belonged to.
func TestScaffold_APIPackageMatchesSkillTemplate(t *testing.T) {
	root := t.TempDir()
	dir, err := Scaffold(Options{Dir: filepath.Join(root, "pkg"), Name: "acme.example/scaffold-test"})
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}

	wantFiles := []string{
		"LICENSE", "README.md", "primitive.yaml", "requirements-checklist.json",
		"schemas/input.json", "schemas/output.json",
		"src/classify.star", "src/verdict.star",
		"tests/contract.test.yaml", "tests/fixtures/REPLACE.json",
		"workflow.yaml",
	}
	for _, rel := range wantFiles {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("expected scaffolded file %q, got: %v", rel, err)
		}
	}

	primitiveContent, err := os.ReadFile(filepath.Join(dir, "primitive.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(primitiveContent)
	if !strings.Contains(s, "name: scaffold-test") {
		t.Errorf("expected REPLACE_KEBAB_NAME to be substituted with the given name, got:\n%s", s)
	}
	if !strings.Contains(s, "publisher: acme.example") {
		t.Errorf("expected the given publisher to replace the template default, got:\n%s", s)
	}
	// Every other placeholder is left for the author -- this is the whole
	// point of copying the skill's authoring templates verbatim.
	if !strings.Contains(s, "REPLACE_HOST") || !strings.Contains(s, "REPLACE_SLOT") {
		t.Errorf("expected content-specific REPLACE_* placeholders to remain for the author to fill in, got:\n%s", s)
	}

	// The package still parses structurally (valid YAML/JSON, entrypoint
	// resolves) even though it is not validate-clean -- LoadPackage must not
	// choke on an authoring skeleton.
	pkg, err := model.LoadPackage(dir)
	if err != nil {
		t.Fatalf("LoadPackage: %v", err)
	}
	if pkg.WorkflowErr != nil {
		t.Fatalf("workflow.yaml should parse structurally even as a placeholder skeleton: %v", pkg.WorkflowErr)
	}
}

// TestScaffold_WebPackageMatchesSkillTemplate mirrors
// TestScaffold_APIPackageMatchesSkillTemplate for `tap init --web`.
func TestScaffold_WebPackageMatchesSkillTemplate(t *testing.T) {
	root := t.TempDir()
	dir, err := Scaffold(Options{Dir: filepath.Join(root, "pkg"), Name: "dev.telara/scaffold-web-test", Web: true})
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}

	wantFiles := []string{
		"LICENSE", "README.md", "primitive.yaml", "requirements-checklist.json",
		"schemas/input.json", "schemas/output.json",
		"src/extract_plan.yaml", "src/normalize.star",
		"tests/contract.test.yaml",
		"tests/fixtures/REPLACE_cached_extraction.json",
		"tests/fixtures/REPLACE_drifted_page.json",
		"workflow.yaml",
	}
	for _, rel := range wantFiles {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("expected scaffolded file %q, got: %v", rel, err)
		}
	}

	primitiveContent, err := os.ReadFile(filepath.Join(dir, "primitive.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(primitiveContent)
	if !strings.Contains(s, "name: scaffold-web-test") {
		t.Errorf("expected REPLACE_KEBAB_NAME to be substituted with the given name, got:\n%s", s)
	}
	if !strings.Contains(s, "publisher: dev.telara") {
		t.Errorf("expected the given publisher to replace the template default, got:\n%s", s)
	}
	if !strings.Contains(s, "REPLACE_ORIGIN_SLOT") {
		t.Errorf("expected content-specific REPLACE_* placeholders to remain for the author to fill in, got:\n%s", s)
	}

	pkg, err := model.LoadPackage(dir)
	if err != nil {
		t.Fatalf("LoadPackage: %v", err)
	}
	if pkg.WorkflowErr != nil {
		t.Fatalf("workflow.yaml should parse structurally even as a placeholder skeleton: %v", pkg.WorkflowErr)
	}
}

func TestScaffold_RefusesToOverwrite(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "pkg")
	if _, err := Scaffold(Options{Dir: dir, Name: "dev.telara/x"}); err != nil {
		t.Fatalf("first scaffold: %v", err)
	}
	if _, err := Scaffold(Options{Dir: dir, Name: "dev.telara/x"}); err == nil {
		t.Fatalf("expected refusal to overwrite an existing package")
	}
}
