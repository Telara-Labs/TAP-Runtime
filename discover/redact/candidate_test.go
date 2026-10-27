package redact

import (
	"encoding/json"
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

func realisticText(n int) string {
	line := "- **Title:** Fix retry in src/main.go (see https://gitlab.com/telara-labs/x/-/merge_requests/12) status: open, owner=alice\n"
	jsonish := `{"id":"ITEM-1","fields":{"summary":"Retry fails","status":{"name":"Open"},"url":"https://example.invalid/browse/ITEM-1"}}` + "\n"
	return strings.Repeat(line, n/2/len(line)) + "```json\n" + strings.Repeat(jsonish, n/2/len(jsonish)) + "```\n"
}

func TestCandidateRealisticDifferential(t *testing.T) {
	for _, n := range []int{2 << 10, 32 << 10, 256 << 10} {
		s := realisticText(n)
		if !candidateText(s, 0) {
			t.Fatalf("realistic %d needs fallback", n)
		}
		compareInspectionPaths(t, s)
		raw, _ := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}, "structuredContent": map[string]any{"result_text": s}})
		if !JSONUnchanged(raw) {
			t.Fatalf("wire %d needs fallback", n)
		}
		compareInspectionPaths(t, string(raw))
	}
}

func TestCandidateEveryCorpusInputInPunctuation(t *testing.T) {
	for _, c := range Corpus() {
		t.Run(c.ID, func(t *testing.T) {
			compareInspectionPaths(t, "- **Title:** status: open, owner=alice\n"+c.Input+"\n- **Result:** status: done, owner=bob\n")
		})
	}
}

func TestCandidatePunctuationDifferential(t *testing.T) {
	rng := rand.New(rand.NewSource(19331))
	words := []string{"title", "value", "summary", "Open", "id", "result_text", "foo", "type", "key", "Bearer", "Tlr", "ghp_" + strings.Repeat("A", 36), "github_pat_" + strings.Repeat("a", 80), "password", "pwd", "deviceCode", "API_KEY", "properties", "sig", "https://example.invalid/x", "null", "true", "12", "1e999", `\x41`, `\u0074oken`, "&a", "*a", "!foo", "|", "[", "]", "{", "}", "\"", "'", "**", "_", "-"}
	for i := 0; i < 1400; i++ {
		var b strings.Builder
		for j := 0; j < 1+rng.Intn(4); j++ {
			b.WriteString(words[rng.Intn(len(words))])
			b.WriteString(": ")
			b.WriteString(words[rng.Intn(len(words))])
			b.WriteString(" = ")
			b.WriteString(words[rng.Intn(len(words))])
			b.WriteByte('\n')
		}
		s := b.String()
		compareInspectionPaths(t, s)
		raw, _ := json.Marshal(map[string]any{"text": s})
		compareInspectionPaths(t, string(raw))
	}
}

func FuzzCandidatePunctuation(f *testing.F) {
	for _, s := range []string{`{"a":1,"a":2}`, `"a":"b":"c"`, `"x":"\x41"`, `type: &a [*a]`, "**Title:** x=y"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		// Cap reference work as well as production work. Every original byte in
		// this bounded input is retained, including controls and invalid UTF-8.
		if len(s) > 32 {
			s = s[:32]
		}
		s = "- **Title:** x=y " + strings.ReplaceAll(s, "\n", "\n- **Title:** x=y ") + "\n- **Result:** z=w\n"
		compareInspectionPaths(t, s)
		raw, _ := json.Marshal(map[string]any{"text": s})
		compareInspectionPaths(t, string(raw))
	})
}

func TestCandidateEncodingAndAmbiguity(t *testing.T) {
	for _, s := range []string{`{"x":"\x41"}`, `{"x":"\v"}`, `{"x":"\a"}`, `{"x":"\000"}`, `{"x":"\'"}`, `{"x":"\u0061"}`, `{"a":"x","a":"y"}`, `{"a":{"b":1e999}}`, `{"text":"password\u003dshort"}`} {
		if JSONUnchanged([]byte(s)) {
			t.Fatalf("uncertain encoding accepted %q", s)
		}
		compareInspectionPaths(t, s)
	}
	for _, s := range []string{"type: &a [*a]", "title: one\ntitle: two", "- type: &a [*a]", "\"title\": one\n\"title\": two", "foo: {\"x\":0,\"x\":1}", "- **Title:** https://bob:q@example.invalid/path = x"} {
		if candidateText(s, 0) {
			t.Fatalf("uncertain prose accepted %q", s)
		}
		compareInspectionPaths(t, s)
	}
}

func BenchmarkInspectionRealistic(b *testing.B) {
	for _, n := range []int{2 << 10, 32 << 10, 256 << 10} {
		s := realisticText(n)
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

func TestCandidateMutableNameExpression(t *testing.T) {
	saved := SensitiveName
	t.Cleanup(func() { SensitiveName = saved })
	SensitiveName = regexp.MustCompile(saved.String())
	// Pointer replacement and in-place regexp recompilation must both disable
	// unchanged proofs. Do not mutate the built-in pointer used by other tests.
	if JSONUnchanged([]byte(`{"text":"ordinary"}`)) {
		t.Fatal("replaced expression bypassed")
	}
	SensitiveName = saved
	original := saved.String()
	t.Cleanup(func() { _ = saved.UnmarshalText([]byte(original)) })
	if err := saved.UnmarshalText([]byte(`^text$`)); err != nil {
		t.Fatal(err)
	}
	if JSONUnchanged([]byte(`{"text":"ordinary"}`)) {
		t.Fatal("recompiled expression bypassed")
	}
	compareInspectionPaths(t, `{"text":"ordinary"}`)
}

func TestCandidateOverlappingFieldAmbiguity(t *testing.T) {
	for _, s := range []string{`- **Title:** owner="a":1e999`, `- **Title:** owner="a":{"x":0,"x":1}`, `- **Title:** owner="a":-1e999`, `- **Title:** foo="a" : [1e999]`} {
		compareInspectionPaths(t, s)
	}
}

func TestCandidateCarriageReturnMapping(t *testing.T) {
	for _, s := range []string{"# ordinary comment\rtitle: one\rtitle: two", "# ordinary comment\rtitle: &a [*a]"} {
		compareInspectionPaths(t, s)
	}
}

func TestCandidateSensitiveNameTriggerCoverage(t *testing.T) {
	for _, name := range []string{"deviceCode", "device_code", "--pass", "auth", "cookie", "set-cookie", "x-api-key", "api_key", "secret", "AWS_SECRET_ACCESS_KEY", "client_secret", "password", "passwd", "pwd", "pass", "private_key", "private_token", "access_token", "refresh_token", "auth_token", "id_token", "session_token", "bearer", "token", "credentials", "DB_SIGNING_KEY", "DB_PASSPHRASE"} {
		t.Run(name, func(t *testing.T) {
			if !IsSensitiveName(name) {
				t.Fatalf("fixture name is not sensitive %q", name)
			}
			raw, _ := json.Marshal(map[string]any{name: "syntheticShortValue"})
			if JSONUnchanged(raw) {
				t.Fatalf("sensitive key bypassed %s", raw)
			}
			compareInspectionPaths(t, string(raw))
		})
	}
}

func TestCandidateFoldPrefixLiteralSeparators(t *testing.T) {
	for _, s := range []string{"device_code", "Device_Code", "DEVICE_CODE"} {
		if !foldPrefix(s, "device_code") {
			t.Fatal(s)
		}
	}
	for _, s := range []string{"device-code", "device code", "device\x7fcode"} {
		if foldPrefix(s, "device_code") {
			t.Fatal(s)
		}
	}
}

func TestCandidateUncertainProse(t *testing.T) {
	for _, s := range []string{`- **Title:** "a"x":1e999"`, `- **Title:** "a"x":-1e999"`, "$id: one\n$id: two", "$schema: one\n$schema: two", "# header\r$id: one\r$id: two"} {
		if candidateText(s, 0) {
			t.Fatalf("uncertain prose accepted %q", s)
		}
		compareInspectionPaths(t, s)
		raw, _ := json.Marshal(map[string]any{"text": s})
		compareInspectionPaths(t, string(raw))
	}
}
