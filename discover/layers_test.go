package discover

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const modPath = "github.com/Telara-Labs/TAP-Runtime/discover/"

// allowed lists, for each subpackage, the sibling subpackages it may import.
// A package may only import packages in layers below it; the compiler already
// forbids cycles, this keeps the direction.
var allowed = map[string][]string{
	"client":     {},
	"util":       {},
	"shellparse": {},
	"pyparse":    {},
	"trace":      {"client", "util", "shellparse", "pyparse"},
	"redact":     {"client", "util", "trace"},
	"history":    {"client", "util", "trace"},
	"primitive":  {"client", "util", "shellparse", "pyparse", "trace", "redact"},
	"model":      {"client", "util", "trace", "redact"},
	"pack":       {"client", "util", "trace", "model", "redact"},
	"retrieval":  {"client", "util", "shellparse", "pyparse", "trace", "history", "model", "redact"},
	"routine":    {"client", "util", "shellparse", "pyparse", "trace", "redact", "model", "pack", "retrieval"},
	"codegen":    {"client", "util", "shellparse", "pyparse", "trace", "redact", "model", "pack", "retrieval", "routine"},
	"author":     {"client", "util", "shellparse", "pyparse", "trace", "redact", "history", "model", "pack", "retrieval", "routine", "codegen"},
	"genreview":  {"client", "util", "shellparse", "pyparse", "trace", "redact", "history", "model", "pack", "retrieval", "routine", "codegen", "author"},
	"pipeline":   {"client", "util", "shellparse", "pyparse", "trace", "redact", "history", "model", "pack", "retrieval", "routine", "codegen", "author", "genreview", "eval"},
	"eval":       {"client", "util", "shellparse", "pyparse", "trace", "redact", "history", "model", "pack", "retrieval", "routine", "codegen", "author", "genreview"},
}

func TestSubpackagesImportOnlyLowerLayers(t *testing.T) {
	for pkg, ok := range allowed {
		okSet := map[string]bool{}
		for _, p := range ok {
			okSet[p] = true
		}
		files, _ := filepath.Glob(filepath.Join(pkg, "*.go"))
		if len(files) == 0 {
			t.Errorf("%s: no files", pkg)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			af, err := parser.ParseFile(token.NewFileSet(), f, src, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, im := range af.Imports {
				p := strings.Trim(im.Path.Value, `"`)
				if !strings.HasPrefix(p, modPath) {
					continue
				}
				dep := strings.TrimPrefix(p, modPath)
				if !okSet[dep] {
					t.Errorf("%s imports %s: %s is not below %s", f, dep, dep, pkg)
				}
			}
		}
	}
}

// codegen reads recorded code as data; it must not be able to run anything.
func TestPyparseNeverExecutesCode(t *testing.T) {
	files, _ := filepath.Glob("pyparse/*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, _ := os.ReadFile(f)
		af, err := parser.ParseFile(token.NewFileSet(), f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range af.Imports {
			if p := strings.Trim(im.Path.Value, `"`); p == "os/exec" {
				t.Errorf("%s imports os/exec; pyparse must read recorded code as data and never run it", f)
			}
		}
	}
}

// internal/testkit is for tests only: no production file may import it.
func TestProductionCodeNeverImportsTestkit(t *testing.T) {
	files, _ := filepath.Glob("*/*.go")
	more, _ := filepath.Glob("*.go")
	for _, f := range append(files, more...) {
		if strings.HasSuffix(f, "_test.go") || strings.HasPrefix(f, "internal/") {
			continue
		}
		src, _ := os.ReadFile(f)
		af, err := parser.ParseFile(token.NewFileSet(), f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range af.Imports {
			if strings.Contains(im.Path.Value, "/internal/testkit") {
				t.Errorf("%s imports internal/testkit", f)
			}
		}
	}
}
