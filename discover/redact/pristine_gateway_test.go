package redact

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// Frozen from the pristine gateway's result_security.go at 2ae3aef875dfa7b1cd4e6de683d9ec8e236a3f9e.
// Keep this reference independent of production rules and name classifiers.
const pristineIdentifier = `[A-Z][A-Z0-9_]*_(?:SECRET|ACCESS|SIGNING|ENCRYPTION|PRIVATE|SESSION|REFRESH|AUTH)_(?:KEY|TOKEN)S?` +
	`|[A-Z][A-Z0-9_]*_(?:API_?KEY|PRIVATE_?KEY|TOKEN|SECRETS?|PASSWORDS?|PASSPHRASES?|CREDENTIALS?)`

var pristineGatewayPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:` + pristineIdentifier + `)\b["']?\s*[:=]\s*["']?[^"'\s,}]{8,}`),
	regexp.MustCompile(`\b(?:` + pristineIdentifier + `)\b`),
	regexp.MustCompile(`(?i)\b(?:[a-z0-9]+[_-])*(?:api[\s_-]*key|access[\s_-]*token|refresh[\s_-]*token|auth[\s_-]*token|secret|password|passwd|credential|private[\s_-]*key)\b["']?\s*[:=]\s*["']?[^"'\s,}]{8,}`),
	regexp.MustCompile(`\b(ghp|glpat|gsk|sk|xox[baprs])-[_A-Za-z0-9]{16,}\b`),
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[_A-Za-z0-9]{80,}|gsk_[A-Za-z0-9]{48,}`),
}

// Frozen independently from the pristine schema provider-token predicate.
var pristineProviderPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\b(ghp|glpat|gsk|sk|xox[baprs])-[_A-Za-z0-9]{16,}\b`),
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[_A-Za-z0-9]{80,}|gsk_[A-Za-z0-9]{48,}`),
}

func pristineGateway(s string) string {
	// The original sanitizer normalizes before applying these expressions.
	s = strings.NewReplacer("\u200b", "", "\u200c", "", "\u200d", "", "\u2060", "", "\ufeff", "", "\u00ad", "").Replace(s)
	for _, re := range pristineGatewayPatterns {
		s = re.ReplaceAllLiteralString(s, "[REDACTED: sensitive value]")
	}
	return s
}

func TestPristineGatewayDifferential(t *testing.T) {
	checked, legacyHits, providerHits := 0, 0, 0
	check := func(id, input string, secrets []string) {
		t.Helper()
		checked++
		old := pristineGateway(input)
		for _, re := range pristineProviderPatterns {
			if re.MatchString(Normalize(input)) {
				providerHits++
				if !ContainsProviderToken(input) {
					t.Fatalf("schema predicate missed pristine provider in %s", id)
				}
				break
			}
		}
		// Frozen value removals must reach every text and scanner consumer.
		// Bare uppercase identifiers are display-only, not credential values.
		for i, re := range pristineGatewayPatterns {
			if i == 1 {
				continue
			}
			for _, match := range re.FindAllString(Normalize(input), -1) {
				value := match
				if i == 0 || i == 2 {
					value = strings.Trim(strings.TrimSpace(value[strings.IndexAny(value, "=:")+1:]), `"'`)
				}
				secrets = append(append([]string(nil), secrets...), value)
			}
		}
		out := ExternalText(input)
		if old != strings.NewReplacer("\u200b", "", "\u200c", "", "\u200d", "", "\u2060", "", "\ufeff", "", "\u00ad", "").Replace(input) {
			legacyHits++
			// A second pristine pass must have nothing left to remove. This checks
			// every frozen pattern, including bare uppercase identifiers.
			withoutMarkers := strings.ReplaceAll(out, "[REDACTED: sensitive value]", "x")
			if pristineGateway(withoutMarkers) != Normalize(withoutMarkers) {
				t.Fatalf("pristine still redacts shared output for %s", id)
			}
			// Check removed values independently of the names, since replacing only
			// an uppercase name could otherwise conceal a retained opaque value.
			for i, re := range pristineGatewayPatterns {
				for _, match := range re.FindAllString(Normalize(input), -1) {
					value := match
					if i == 0 || i == 2 {
						if at := strings.IndexAny(value, "=:"); at >= 0 {
							value = strings.Trim(strings.TrimSpace(value[at+1:]), `"'`)
						}
					}
					if strings.Contains(Normalize(out), value) {
						t.Fatalf("shared output retained pristine removal in %s (pattern %d)", id, i)
					}
				}
			}
		}
		// Encodings and nesting may be newer than the frozen reference. Assert
		// those expected values too, through every public credential entry point.
		if len(secrets) > 0 {
			if SecretShape(input) == "" || len(ScanArtifacts(map[string][]byte{"input": []byte(input)})) == 0 {
				t.Fatalf("scanner missed generated credential %s", id)
			}
			for name, result := range map[string]string{"redact": Redact(input), "external": out, "argument": Argument("", input)} {
				if result == input {
					t.Fatalf("%s returned an unchanged generated credential %s", name, id)
				}
				logical := strings.ReplaceAll(strings.ReplaceAll(Normalize(result), "\\\r\n", ""), "\\\n", "")
				for _, secret := range secrets {
					if strings.Contains(logical, Normalize(secret)) {
						t.Fatalf("%s retained generated credential %s", name, id)
					}
				}
			}
		}
	}
	for _, c := range Corpus() {
		// Schema declarations are an intentional correction of the pristine
		// regex's description-as-value false positive, covered separately by
		// the shared corpus and actual YAML scanner tests.
		// Complete references are also deliberate corrections of the pristine
		// length-only regex, rather than credential values. Their trailing-
		// literal counterparts below must still redact the complete scalar.
		if c.Profile != "" || strings.HasPrefix(c.ID, "metadata-") || strings.HasPrefix(c.ID, "r5-complete-reference-control-") {
			continue
		}
		check(c.ID, c.Input, c.MustRedact)
	}
	opaque := "syntheticOpaqueValue42"
	names := []string{"api key", "access token", "refresh token", "auth token", "private key", "secret", "password", "passwd", "credential", "token", "passphrase", "client secret", "aws secret access key", "db password", "session token", "signing key", "encryption key"}
	separators := []string{"", " ", "_", "-", " _ ", " -_ ", "__--", "\t_\t", "\n_\n", "\v_\f", "\u2003_\u00a0"}
	for _, baseName := range names {
		for si, sep := range separators {
			name := strings.ReplaceAll(baseName, " ", sep)
			for _, prefix := range []string{"", "SERVICE_"} {
				for _, key := range []string{name, strings.ToUpper(name)} {
					key = prefix + key
					// Only use credential spellings recognized by this public name API.
					// Every pristine-positive spelling is checked regardless of this flag.
					known := IsSensitiveName(key)
					for _, delimiter := range []string{"=", ": "} {
						for ci, continuation := range []string{"", "\\\n", "\\\r\n", "\n", "\n  ", "\r\n", "\r\n  "} {
							for qi, quote := range []string{"", "\"", "'"} {
								for vi, raw := range []string{key + delimiter + continuation + quote + opaque + quote, key + delimiter + quote + continuation + opaque + quote} {
									var want []string
									if known {
										want = []string{opaque}
									}
									check(fmt.Sprintf("assignment/%s/%d/%d/%d/%d", key, si, ci, qi, vi), raw, want)
								}
							}
						}
					}
					if !known {
						continue
					}
					// All JSON roots, escaped names, recursive encoded JSON strings,
					// multiple fragments and nested values under an outer benign field.
					for _, encode := range []func(string) string{encodeJSONString, func(s string) string { return `"` + escapeJSONLetters(s) + `"` }} {
						field := `{` + encode(key) + `:"` + opaque + `"}`
						for ni, nested := range []string{field, `[{"config":` + field + `}]`, `{"config":{"rows":[` + field + `]}}`, encodeJSONString(field)} {
							for _, frame := range []string{"%s", "\ufeff%s", "before %s after", "before {} %s [] after", `before "unclosed %s after`} {
								check(fmt.Sprintf("json/%s/%d", key, ni), fmt.Sprintf(frame, nested), []string{opaque})
							}
						}
					}
				}
			}
		}
	}
	providers := []string{}
	for _, prefix := range []string{"ghp-", "glpat-", "gsk-", "sk-", "xoxb-", "xoxa-", "xoxp-", "xoxr-", "xoxs-"} {
		providers = append(providers, prefix+strings.Repeat("A", 36))
	}
	for _, prefix := range []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_", "gsk_"} {
		providers = append(providers, prefix+strings.Repeat("A", 80))
	}
	providers = append(providers, "github_pat_"+strings.Repeat("_", 80), "github_pat_"+strings.Repeat("A_", 40))
	for pi, token := range providers {
		for _, edge := range []string{"%s", "log_%s_bak", " %s ", "\ufeff%s"} {
			raw := fmt.Sprintf(edge, token)
			check(fmt.Sprintf("provider/%d", pi), raw, []string{token})
			for _, encoded := range []string{encodeJSONString(raw), `"` + escapeJSONLetters(raw) + `"`} {
				for _, nested := range []string{encoded, `[{"config":{"rows":[` + encoded + `]}}]`, encodeJSONString(encoded)} {
					check(fmt.Sprintf("provider-json/%d", pi), "request "+nested+" done", []string{strings.Repeat("A", 36)})
				}
			}
			for cut := 1; cut < len(token); cut++ {
				for _, nl := range []string{"\n", "\r\n"} {
					input := token[:cut] + "\\" + nl + token[cut:]
					check(fmt.Sprintf("provider-continued/%d/%d", pi, cut), input, []string{token})
				}
			}
		}
	}
	t.Logf("checked %d inputs, %d pristine gateway positives, %d schema provider positives", checked, legacyHits, providerHits)
}

func encodeJSONString(s string) string { b, _ := json.Marshal(s); return string(b) }
func escapeJSONLetters(s string) string {
	var b strings.Builder
	for _, r := range s {
		fmt.Fprintf(&b, `\u%04x`, r)
	}
	return b.String()
}

func TestInspectionPreservesCleanBytesAndFindsLogicalLines(t *testing.T) {
	for _, input := range []string{"\ufeff 42 ", "\u200b[1, true, null]", "ordinary\u2003prose", "ordinary\vprose", "REGION=we\\\nst tool", "request {\"token_count\":24}", "request \"ordinary\""} {
		if Redact(input) != input || ExternalText(input) != input || SecretShape(input) != "" || len(ScanArtifacts(map[string][]byte{"source": []byte(input)})) != 0 {
			t.Fatalf("clean input bytes changed or scanner blocked %q", input)
		}
	}
	for _, input := range []string{"api_\\\nkey=syntheticOpaqueValue42", "password=\"alpha\\\nbravo\"", "api _ key = \\\nsyntheticOpaqueValue42", `request {"outer":{"config":{"dbPass\u0077ord":"tiny"}}}`} {
		if SecretShape(input) == "" || len(ScanArtifacts(map[string][]byte{"source": []byte(input)})) != 1 || Redact(input) == input || ExternalText(input) == input {
			t.Fatal("logical/structured inspection bypass")
		}
	}
}

func TestInspectionCompositionAndLimits(t *testing.T) {
	token := "ghp_" + strings.Repeat("A", 36)
	for _, input := range []string{
		`"\u0067hp_` + strings.Repeat("A", 16) + "\\\n" + strings.Repeat("A", 20) + `"`,
		`request {"broken":{bad {"config":{"dbPass\u0077ord":"tiny"}}}}`,
		"limit ordinary SERVICE_api _ key=syntheticOpaqueValue42",
	} {
		for _, out := range []string{Redact(input), ExternalText(input)} {
			for _, secret := range []string{strings.Repeat("A", 16), "tiny", "syntheticOpaqueValue42"} {
				if strings.Contains(out, secret) {
					t.Fatalf("composed credential retained in %q -> %q", input, out)
				}
			}
		}
		if SecretShape(input) == "" || len(ScanArtifacts(map[string][]byte{"source": []byte(input)})) == 0 {
			t.Fatal("composed credential not scanned")
		}
	}
	ordinary := strings.Repeat(`ordinary "small quoted text" {code; } `, 2000)
	if Redact(ordinary) != ordinary || ExternalText(ordinary) != ordinary || SecretShape(ordinary) != "" {
		t.Fatal("fragment accounting blocked ordinary prose/source")
	}
	input := strings.Repeat("[", 100) + encodeJSONString(token) + strings.Repeat("]", 100)
	if strings.Contains(Redact(input), token) || SecretShape(input) == "" {
		t.Fatal("inspection limit failed open")
	}
	longName := strings.Repeat("ordinary policy ", 2000) + "SERVICE_api _ key=syntheticOpaqueValue42"
	if strings.Contains(Redact(longName), "syntheticOpaqueValue42") || SecretShape(longName) == "" {
		t.Fatal("long name prefix hid credential")
	}
}

func TestSecurityReviewFragmentCollisionAndOverlaps(t *testing.T) {
	token := "ghp_" + strings.Repeat("A", 36)
	for _, prefix := range []string{"", `request "bad quote `, `request "bad quote "ordinary `, "\x00fragment000000000\x00"} {
		for _, fragment := range []string{`{"config":{"dbPass\u0077ord":"tiny"}}`, `"dbPass\u0077ord":"tiny"`, `"\u0067hp_` + strings.Repeat("A", 36) + `"`} {
			input := prefix + fragment + " after"
			if SecretShape(input) == "" || len(ScanArtifacts(map[string][]byte{"source": []byte(input)})) == 0 {
				t.Fatal("overlapping fragment not inspected")
			}
			for _, out := range []string{Redact(input), ExternalText(input)} {
				if strings.Contains(out, "tiny") || strings.Contains(out, strings.Repeat("A", 36)) || !strings.Contains(out, " after") {
					t.Fatal("fragment overlapping prose retained credential or lost suffix")
				}
				if strings.Contains(prefix, "\x00") && !strings.HasPrefix(out, prefix) {
					t.Fatal("fragment placeholder collided with source")
				}
			}
		}
	}
	input := `request "` + token + ` "\u0067hp_` + strings.Repeat("B", 36) + `" after`
	for _, out := range []string{Redact(input), ExternalText(input)} {
		if strings.Contains(out, strings.Repeat("A", 36)) || strings.Contains(out, strings.Repeat("B", 36)) {
			t.Fatal("overlapping dirty spans hid a descendant")
		}
	}
}

// Must-survive cases are independently gated by the pristine implementation.
// Credential words in an explanatory phrase must not turn it into a field.
func TestPristineGatewayMustSurviveDifferential(t *testing.T) {
	checked := 0
	for _, word := range []string{"password", "api key", "access token", "refresh token", "auth token", "private key", "secret", "credential", "cookie", "authorization"} {
		for _, prefix := range []string{"The ", "Read the ", "Information about ", ""} {
			for _, suffix := range []string{" documentation", " documentation link", " help page", " rotation guide", " reference material", " setup instructions"} {
				for _, delimiter := range []string{": ", " = ", ":\n  ", ":\r\n  "} {
					for _, value := range []string{"https://example.invalid/help", "ordinary explanatory material"} {
						input := prefix + word + suffix + delimiter + value
						if pristineGateway(input) != input {
							t.Fatal("invalid must-survive reference fixture")
						}
						checked++
						for name, out := range map[string]string{"redact": Redact(input), "argument": Argument("", input), "external": ExternalText(input)} {
							if out != input {
								t.Fatalf("%s corrupted pristine-preserved prose %q -> %q", name, input, out)
							}
						}
						if SecretShape(input) != "" || len(ScanArtifacts(map[string][]byte{"doc": []byte(input)})) != 0 || ContainsProviderToken(input) {
							t.Fatalf("predicate blocked pristine-preserved prose %q", input)
						}
					}
				}
			}
		}
	}
	t.Logf("checked %d pristine-preserved prose inputs", checked)
}

// Assert the compatibility layer cannot drift from the independent frozen
// reference when the structured pipeline evolves.
func TestFrozenGatewayCredentialLayer(t *testing.T) {
	for i, frozen := range []int{0, 2, 3, 4} {
		if gatewayCredentialPatterns[i].String() != pristineGatewayPatterns[frozen].String() {
			t.Fatalf("frozen credential pattern %d drifted", i)
		}
	}
	checked := 0
	for _, name := range []string{"api_key", "password", "SERVICE_ACCESS_TOKEN", "private key"} {
		for _, quote := range []string{"", "\"", "'"} {
			for _, before := range []string{"", " ", "\t", "\n", "\r\n  "} {
				for _, delimiter := range []string{":", "="} {
					for _, after := range []string{"", " ", "\t", "\n", "\r\n  "} {
						input := quote + name + quote + before + delimiter + after + "syntheticOpaqueValue42"
						if pristineGateway(input) == input {
							t.Fatal("invalid frozen-positive fixture")
						}
						checked++
						for path, out := range map[string]string{"redact": Redact(input), "external": ExternalText(input), "argument": Argument("", input)} {
							if strings.Contains(out, "syntheticOpaqueValue42") {
								t.Fatalf("%s retained frozen credential %q", path, input)
							}
						}
						if SecretShape(input) == "" || len(ScanArtifacts(map[string][]byte{"input": []byte(input)})) == 0 {
							t.Fatalf("scanner missed %q", input)
						}
					}
				}
			}
		}
	}
	t.Logf("checked %d quoted/delimiter frozen positives", checked)
}

func TestPristineGatewaySchemaMustSurviveDifferential(t *testing.T) {
	checked := 0
	for _, name := range []string{"api_key", "password", "access_token", "clientSecret", "credentials"} {
		for _, metadata := range []map[string]any{
			{"type": "string", "description": "ordinary catalog prose", "format": "password"},
			{"type": "object", "properties": map[string]any{"password": map[string]any{"type": "string", "description": "ordinary help"}}},
			{"allOf": []any{map[string]any{"type": "string", "description": "ordinary help"}}},
		} {
			for _, container := range []string{"properties", "$defs", "definitions"} {
				raw, _ := json.Marshal(map[string]any{container: map[string]any{name: metadata}})
				for _, prefix := range []string{"", "Example ", "before ", "catalog definition "} {
					input := prefix + string(raw)
					if pristineGateway(input) != input {
						t.Fatal("invalid schema must-survive fixture")
					}
					checked++
					for path, out := range map[string]string{"redact": Redact(input), "external": ExternalText(input), "argument": Argument("", input)} {
						if out != input {
							t.Fatalf("%s corrupted schema %q -> %q", path, input, out)
						}
					}
					if SecretShape(input) != "" || len(ScanArtifacts(map[string][]byte{"schema": []byte(input)})) != 0 {
						t.Fatalf("scanner blocked schema %q", input)
					}
				}
			}
		}
	}
	t.Logf("checked %d pristine-preserved schema definitions", checked)
}

func TestFrozenGatewayFloorCannotBeCancelledByStructuredInspection(t *testing.T) {
	for _, input := range []string{"before \"api_key\":\n  syntheticOpaqueValue42 after", `api_key=syntheticOpaqueValue42`, `{"api_key":"syntheticOpaqueValue42"}`, `{"password":123456789012345}`} {
		// Deliberately simulate a future structured-parser regression that returns
		// the original credential untouched. The independent layer must still win.
		result := gatewayFloor(input, inspection{input, ""})
		if result.shape == "" || result.text == input || strings.Contains(result.text, "syntheticOpaqueValue42") || strings.Contains(result.text, "123456789012345") {
			t.Fatalf("structured result cancelled frozen spans in %q", input)
		}
		if json.Valid([]byte(input)) && !json.Valid([]byte(result.text)) {
			t.Fatal("floor produced invalid JSON")
		}
	}
}

func TestFrozenGatewayFloorBounds(t *testing.T) {
	input := "api_key=syntheticOpaqueValue42"
	for _, output := range []string{strings.Repeat("ordinary ", 10000) + "syntheticOpaqueValue42", strings.Repeat("syntheticOpaqueValue42 ", 4097)} {
		result := gatewayFloor(input, inspection{output, ""})
		if result.shape != "inspection limit" || strings.Contains(result.text, "syntheticOpaqueValue42") {
			t.Fatal("frozen floor did not bound residual scanning")
		}
	}
	source := strings.Repeat(input+"\n", 10000)
	result := gatewayFloor(source, inspection{"syntheticOpaqueValue42", ""})
	if result.shape != "credential" || strings.Contains(result.text, "syntheticOpaqueValue42") {
		t.Fatal("repeated frozen values bypassed deduplication")
	}
}

func TestFrozenGatewayFloorSpanCap(t *testing.T) {
	result := gatewayFloor("api_key=abcdefgh", inspection{strings.Repeat("abcdefgh ", 4097), ""})
	if result.shape != "inspection limit" || strings.Contains(result.text, "abcdefgh") {
		t.Fatal("residual span cap failed")
	}
}

func TestYAMLScalarInspectionBudget(t *testing.T) {
	var block strings.Builder
	for i := 0; i < 256; i++ {
		block.WriteString(strings.Repeat(" ", i) + "password: |\n")
	}
	block.WriteString(strings.Repeat(" ", 257) + "syntheticOpaqueValue42\n")
	for _, tc := range []struct {
		source string
		scan   func(string) []valueSpan
	}{
		{block.String(), yamlCredentialSpans},
		{strings.Repeat("{password: alpha bravo ", 512) + "tail" + strings.Repeat("}", 512), yamlFlowCredentialSpans},
	} {
		spans := tc.scan(tc.source)
		if len(spans) != 1 || spans[0].start != 0 || spans[0].end != len(tc.source) {
			t.Errorf("overlapping scalar scans did not fail closed at inspection budget, spans=%d", len(spans))
		}
	}
}

func TestInspectionMarkersStayShortAndAbsent(t *testing.T) {
	source := strings.Repeat("redactedcredential000000", 4096) + strings.Repeat("\x00", 8192) + "redactedcredential0end fragmentplaceholder1end"
	for _, base := range []string{"redactedcredential", "fragmentplaceholder"} {
		marker := inspectionMarker(source, base)
		if len(marker) > 64 || strings.Contains(source, marker) {
			t.Fatal("inspection marker grew with hostile input or collided")
		}
	}
	input := source + strings.Repeat(" [] {}", 128) + ` {"password":"syntheticOpaqueValue42"}`
	out := Redact(input)
	if !strings.HasPrefix(out, source) || strings.Contains(out, "syntheticOpaqueValue42") {
		t.Fatal("compact fragment protection corrupted source or leaked credential")
	}
}

// Compare source spans against an independent frozen oracle before checking all
// consumers. No mapping, label, reference or schema classifier participates here.
func TestRound6FrozenSpanSubsetProperty(t *testing.T) {
	checked := 0
	check := func(input string) {
		t.Helper()
		source := strings.NewReplacer("\u200b", "", "\u200c", "", "\u200d", "", "\u2060", "", "\ufeff", "", "\u00ad", "").Replace(input)
		expected := []valueSpan{}
		for i, re := range pristineGatewayPatterns {
			if i == 1 {
				continue
			} // Bare uppercase labels are display policy only.
			for _, m := range re.FindAllStringIndex(source, -1) {
				start := m[0]
				if i == 0 || i == 2 {
					start += strings.IndexAny(source[start:m[1]], ":=") + 1
					for start < m[1] && asciiSpace(source[start]) {
						start++
					}
					if start < m[1] && (source[start] == '\'' || source[start] == '"') {
						start++
					}
				}
				expected = append(expected, valueSpan{start, m[1]})
			}
		}
		actual := gatewayCredentialSpans(source)
		for _, span := range expected {
			covered := false
			for _, got := range actual {
				if got.start <= span.start && got.end >= span.end {
					covered = true
					break
				}
			}
			if !covered {
				t.Fatalf("frozen span %v suppressed for %q", span, input)
			}
		}
		if len(expected) == 0 {
			return
		}
		checked++
		if SecretShape(input) == "" || len(ScanArtifacts(map[string][]byte{"source": []byte(input)})) == 0 {
			t.Fatalf("frozen span not detected by scanner for %q", input)
		}
		for path, out := range map[string]string{"redact": Redact(input), "argument": Argument("", input), "external": ExternalText(input)} {
			for _, span := range expected {
				if strings.Contains(Normalize(out), Normalize(source[span.start:span.end])) {
					t.Fatalf("%s retained frozen span %v for %q -> %q", path, span, input, out)
				}
			}
		}
	}
	for _, c := range Corpus() {
		check(c.Input)
		check(Normalize(c.Input))
	}
	// Deterministic cross product includes adversarial reference/mapping suffixes,
	// schema and label contexts, quoted data, CRLF, and overlapping assignments.
	for _, key := range []string{"api_key", "password", "SERVICE_API_KEY", "refresh_token", "private-key", "credential"} {
		for _, value := range []string{"prefix\u00a0syntheticOpaqueValue42", "prefix\u0085syntheticOpaqueValue42", "prefix\u2003syntheticOpaqueValue42", "prefix\u200b\u00a0syntheticOpaqueValue42", "syntheticOpaqueValue42:", "$REFERENCE_TOKEN", "${REFERENCE_TOKEN}", "{env:REFERENCE_TOKEN}", "description:", "syntheticOpaqueValue42", "123456789012345"} {
			for _, delim := range []string{"=", ":", ":\n  ", " = \r\n "} {
				input := key + delim + value
				for _, frame := range []string{"%s", "before %s after", "schema properties %s\n", "- %s\n- name: sibling", `{"description":%q}`, `{"properties":{"password":{"default":%q}}}`} {
					check(fmt.Sprintf(frame, input))
					check(Normalize(fmt.Sprintf(frame, input)))
				}
			}
		}
	}
	t.Logf("checked frozen spans on %d corpus/generated positive inputs", checked)
}

func TestRound6YAMLSequenceContext(t *testing.T) {
	for _, tc := range []struct {
		input           string
		inside, outside []string
	}{
		{"accounts:\n  - config:\n      password: tiny\n  - token: q\nname: ordinarySibling", []string{"- config:", "password:", "token:"}, []string{"accounts:", "name:"}},
		{"- - password: tiny\n\n# comment\n  - token: q\nname: ordinarySibling", []string{"password:", "token:", "# comment"}, []string{"name:"}},
		{"password: token\n", nil, []string{"password:"}},
		{"-\n  password: tiny\nname: ordinarySibling", []string{"password:"}, []string{"name:"}},
	} {
		ranges := yamlSequenceRanges(tc.input)
		for _, word := range tc.inside {
			if !yamlInSequence(ranges, strings.Index(tc.input, word)) {
				t.Fatalf("missed sequence context for %q", word)
			}
		}
		for _, word := range tc.outside {
			if yamlInSequence(ranges, strings.Index(tc.input, word)) {
				t.Fatalf("sequence context crossed sibling %q", word)
			}
		}
	}
	if yamlInSequence(yamlSequenceRanges(""), 0) || yamlSequenceValue("-ordinary") {
		t.Fatal("nonsequence recognized")
	}
}
