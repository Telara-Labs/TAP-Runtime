package validate

import (
	"os"
	"path/filepath"
	"testing"

	"telara.dev/tap/internal/model"
)

func examplesDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join("..", "..", "..", "..", "telara-documentation", "architecture", "tap", "examples")
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Skip("cannot resolve examples dir")
	}
	if _, err := os.Stat(abs); err != nil {
		t.Skipf("examples checkout not found at %s: %v", abs, err)
	}
	return abs
}

func TestGolden_BothExamplesValidateClean(t *testing.T) {
	base := examplesDir(t)
	for _, name := range []string{"gitlab-pipeline-triage", "web-changelog-watch"} {
		t.Run(name, func(t *testing.T) {
			pkg, err := model.LoadPackage(filepath.Join(base, name))
			if err != nil {
				t.Fatalf("LoadPackage: %v", err)
			}
			findings := Validate(pkg)
			if findings.HasErrors() {
				t.Fatalf("expected zero errors, got: %v", findings.Errors())
			}
		})
	}
}
