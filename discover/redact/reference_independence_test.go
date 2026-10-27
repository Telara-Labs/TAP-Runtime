package redact

import (
	"go/ast"
	"go/parser"
	"go/token"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// The differential oracle must not reach an optimized production procedure.
// Every function declared in fast.go, the production inspector type and the
// production normalization/floor procedures with shortcuts are forbidden as
// bare identifiers anywhere in the oracle file, including as function values.
func TestReferenceOracleUsesNoOptimizedProcedure(t *testing.T) {
	fset := token.NewFileSet()
	fast, err := parser.ParseFile(fset, "fast.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	forbidden := map[string]bool{
		"inspector": true, "Normalize": true, "normalizeRune": true, "pristineFloorRune": true,
		"projectedSpans": true, "gatewayCredentialSpans": true, "normalizedFloorSpans": true,
		"frozenSourceSpans": true, "gatewayFloor": true, "gatewayFloorSpans": true, "assembleLine": true,
	}
	for _, decl := range fast.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
			forbidden[fn.Name.Name] = true
		}
	}
	if !forbidden["normalizationStableASCII"] {
		t.Fatal("fast.go declarations were not derived")
	}
	reference, err := parser.ParseFile(fset, "reference_test.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	selected := map[*ast.Ident]bool{}
	ast.Inspect(reference, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			selected[sel.Sel] = true
		}
		return true
	})
	ast.Inspect(reference, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && !selected[id] && forbidden[id.Name] {
			t.Errorf("oracle references optimized procedure %s at %s", id.Name, fset.Position(id.Pos()))
		}
		return true
	})
}

// Shared ruleset helpers still call production Normalize. Prove its ASCII
// shortcut is exactly the frozen mapping for every rune and for mixed text.
func TestNormalizeMatchesFrozenNormalization(t *testing.T) {
	for r := rune(0); r <= utf8.MaxRune; r++ {
		if r >= 0xd800 && r <= 0xdfff {
			continue
		}
		for _, s := range []string{string(r), "api" + string(r) + "key=syntheticOpaqueValue42"} {
			if got, want := Normalize(s), referenceNormalize(s); got != want {
				t.Fatalf("Normalize(%q) = %q, frozen %q", s, got, want)
			}
		}
	}
	for _, c := range Corpus() {
		if got, want := Normalize(c.Input), referenceNormalize(c.Input); got != want {
			t.Fatalf("%s: Normalize differs from frozen normalization", c.ID)
		}
	}
}

// Mandatory-byte prefilters in production span collection must be exact.
func TestCredentialSpansMatchFrozenCollection(t *testing.T) {
	inputs := []string{"", "password: syntheticOpaqueValue42", "ghp_" + strings.Repeat("A1b2", 9), "sk-" + strings.Repeat("a", 20)}
	for _, c := range Corpus() {
		inputs = append(inputs, c.Input, referenceNormalize(c.Input), strings.Map(referencePristineFloorRune, c.Input))
	}
	rng := rand.New(rand.NewSource(20))
	alphabet := []rune("abcdefgkpstuwxyhoAKIZ0123456789_-=:'\" \t\n,}\v\f ​")
	words := []string{"password", "api_key", "API_TOKEN", "secret", "ghp", "gsk", "xoxb", "github_pat", "token"}
	for i := 0; i < 4000; i++ {
		var b strings.Builder
		for j := rng.Intn(6); j >= 0; j-- {
			if rng.Intn(3) == 0 {
				b.WriteString(words[rng.Intn(len(words))])
			}
			for k := rng.Intn(40); k > 0; k-- {
				b.WriteRune(alphabet[rng.Intn(len(alphabet))])
			}
		}
		inputs = append(inputs, b.String())
	}
	for _, s := range inputs {
		if got, want := gatewayCredentialSpans(s), referenceGatewayCredentialSpans(s); !reflect.DeepEqual(got, want) {
			t.Fatalf("spans differ for %q\ngot  %v\nwant %v", s, got, want)
		}
		for _, normalize := range []func(rune) rune{normalizeRune, pristineFloorRune} {
			got := projectedSpans(s, normalize, gatewayCredentialSpans)
			want := referenceProjectedSpans(s, normalize, referenceGatewayCredentialSpans)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("projected spans differ for %q", s)
			}
		}
	}
}
