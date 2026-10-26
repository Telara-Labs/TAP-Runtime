package redact

import (
	"encoding/json"
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

func compareInspectionPaths(t *testing.T, source string) {
	t.Helper()
	for _, mode := range []struct{ sensitive, metadata, data bool }{{}, {true, false, false}, {false, true, false}, {false, false, true}} {
		a := inspector{remaining: 64*len(source) + 65536, metadataProfile: mode.metadata, schemaData: mode.data}
		b := referenceInspector{remaining: a.remaining, metadataProfile: mode.metadata, schemaData: mode.data}
		fast, full := a.text(source, mode.sensitive, 0), b.text(source, mode.sensitive, 0)
		if !mode.sensitive && !mode.metadata && !mode.data && JSONUnchanged([]byte(source)) && (full.text != source || full.shape != "") {
			t.Fatalf("JSON predicate bypassed full decision for %q: %+v", source, full)
		}
		if fast != full {
			t.Fatalf("fast/full differ for %q mode=%+v\nfast=%+v\nfull=%+v", source, mode, fast, full)
		}
	}
}

func TestInspectionFastPathDifferential(t *testing.T) {
	for _, c := range Corpus() {
		t.Run(c.ID, func(t *testing.T) { compareInspectionPaths(t, c.Input) })
	}
	for _, s := range []string{"api\vkey=syntheticOpaqueValue42", "api\fkey=syntheticOpaqueValue42", "ghp_" + strings.Repeat("A", 36) + "\vtrailer"} {
		compareInspectionPaths(t, s)
	}
	rng := rand.New(rand.NewSource(12937))
	alphabet := []byte("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-=:{[}]/\\\"'?& \t\r\n")
	for i := 0; i < 500; i++ {
		words := []string{"ordinary prose", "src/main.go", "2.0", "true", "null", "line one\nline two", "password", "cookie", "Bearer abcdefghijklmnop", "api\vkey=syntheticOpaqueValue42"}
		keys := []string{"jsonrpc", "id", "text", "content", "password", "api key", "a", "a"}
		wire, _ := json.Marshal(map[string]any{keys[rng.Intn(len(keys))]: []any{words[rng.Intn(len(words))], rng.Intn(10), false, nil}})
		compareInspectionPaths(t, string(wire))
	}
	for i := 0; i < 1200; i++ {
		data := make([]byte, rng.Intn(200))
		for j := range data {
			data[j] = alphabet[rng.Intn(len(alphabet))]
		}
		compareInspectionPaths(t, string(data))
		wire, _ := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": string(data)}}})
		compareInspectionPaths(t, string(wire))
	}
}

func FuzzInspectionFastPath(f *testing.F) {
	for _, c := range Corpus() {
		f.Add(c.Input)
	}
	for _, s := range []string{"ordinary prose line with a path src/main.go and id 12345\n", `{"password":"short"}`, `{"a":null,"a":false}`, `{"x":"Bearer\nabcdefghijklmnop"}`, `1e999999`, `{"x":"\ud800"}`, strings.Repeat("[", 70) + "null" + strings.Repeat("]", 70)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 16384 {
			t.Skip()
		}
		compareInspectionPaths(t, s)
	})
}

func TestFastPredicateBoundaries(t *testing.T) {
	for _, s := range []string{"eyJaaaaaaaa.bbbbbbbbb.ccccccccc", "AKIA0123456789ABCDEF", "ASIA0123456789ABCDEF", "AIza" + strings.Repeat("a", 35), "ya29." + strings.Repeat("a", 25), "bEaReR abcdefghijklmnop", "BASIC abcdefghijklmnop", "password=x", "gsk_" + strings.Repeat("a", 48), "x\u200by", "1e99999", "api\vkey=syntheticOpaqueValue42", "api\fkey=syntheticOpaqueValue42"} {
		if plainASCII(s) {
			t.Fatalf("unsafe trigger accepted %q", s)
		}
	}
	for _, s := range []string{`{"a":1,"a":2}`, `{"a":"\u0061"}`, `{"a":1e9999}`, `{"password":false}`, `{"x":"bearer\nabcdefghijklmnop"}`, `{"a":"\ud800"}`, strings.Repeat("[", 18) + "null" + strings.Repeat("]", 18)} {
		if simpleJSON(s) {
			t.Fatalf("uncertain JSON accepted %q", s)
		}
	}
	for _, s := range []string{`{"jsonrpc":"2.0","id":null,"result":{"content":[{"type":"text","text":"ordinary prose\nmore prose"}]}}`, `{"a":[null,true,false,1,2]}`} {
		if !simpleJSON(s) {
			t.Fatalf("simple JSON not accepted %q", s)
		}
		compareInspectionPaths(t, s)
	}
}

func BenchmarkInspectionTypical(b *testing.B) {
	for _, n := range []int{2 << 10, 32 << 10, 256 << 10} {
		s := strings.Repeat("ordinary prose line with a path src/main.go and id 12345\n", n/58)
		for _, kind := range []string{"Field", "Text"} {
			b.Run(kind+"/"+sizeLabel(n), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if kind == "Field" {
						benchmarkText = Field(nil, s)
					} else {
						benchmarkText = Redact(s)
					}
				}
			})
		}
	}
}

var benchmarkText string

func sizeLabel(n int) string {
	switch n {
	case 2 << 10:
		return "2KiB"
	case 32 << 10:
		return "32KiB"
	default:
		return "256KiB"
	}
}

func TestFastPathFallsBackForChangedPublicShapes(t *testing.T) {
	saved := SecretShapes
	t.Cleanup(func() { SecretShapes = saved })
	SecretShapes = append(SecretShapes[:0:0], SecretShapes...)
	SecretShapes[0].re = regexp.MustCompile("ordinary")
	if JSONUnchanged([]byte(`{"text":"ordinary prose"}`)) {
		t.Fatal("changed public rule bypassed")
	}
	compareInspectionPaths(t, "ordinary prose")
	if Field(nil, "ordinary prose") == "ordinary prose" {
		t.Fatal("changed public rule not applied")
	}
}

func TestJSONPredicateResourceAndSyntaxBounds(t *testing.T) {
	for _, n := range []int{16, 17} {
		var fields []string
		for i := 0; i < n; i++ {
			fields = append(fields, `"field`+string(rune('a'+i))+`":null`)
		}
		source := "{" + strings.Join(fields, ",") + "}"
		if JSONUnchanged([]byte(source)) != (n == 16) {
			t.Fatalf("object member bound %d", n)
		}
		compareInspectionPaths(t, source)
	}
	for _, s := range []string{`{"x":0}junk`, `[true false]`, `{"x":01}`, "{\"x\":\"raw\tcontrol\"}", `{"x":"escaped\"quote"}`, `{"x":"literal\\continuation"}`, `{"x":-0}`, `{"x":12}`, `{"x":1e999}`} {
		if JSONUnchanged([]byte(s)) {
			t.Fatalf("uncertain syntax accepted %q", s)
		}
		compareInspectionPaths(t, s)
	}
	for _, s := range []string{`{"x":[0,1,2,3,4,5,6,7,8,9,null,true,false]}`, strings.Repeat("[", 16) + "null" + strings.Repeat("]", 16)} {
		if !JSONUnchanged([]byte(s)) {
			t.Fatalf("simple syntax refused %q", s)
		}
		compareInspectionPaths(t, s)
	}
	// Budget-sensitive recursive callers must keep the unoptimized accounting.
	for _, budget := range []int{1, 16, 128, 65536} {
		s := `{"x":"ordinary prose"}`
		a := inspector{remaining: budget}
		b := referenceInspector{remaining: budget}
		if x, y := a.text(s, false, 2), b.text(s, false, 2); x != y || a.remaining != b.remaining {
			t.Fatalf("recursive budget %d differs: %+v %+v %d %d", budget, x, y, a.remaining, b.remaining)
		}
	}
}
