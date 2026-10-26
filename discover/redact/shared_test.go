package redact

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSharedCorpus(t *testing.T) {
	for _, c := range Corpus() {
		t.Run(c.ID, func(t *testing.T) {
			if c.Profile != "" {
				checkProfileCase(t, c)
				return
			}
			if c.ProviderToken != nil && ContainsProviderToken(c.Input) != *c.ProviderToken {
				t.Error("schema provider-token classification disagrees with corpus")
			}
			if c.MustSurviveScan {
				if SecretShape(c.Input) != "" || len(ScanArtifacts(map[string][]byte{"source": []byte(c.Input)})) != 0 {
					t.Error("artifact scanner blocked ordinary source, reference or prose")
				}
			}
			if len(c.MustRedact) > 0 && (SecretShape(c.Input) == "" || len(ScanArtifacts(map[string][]byte{"corpus": []byte(c.Input)})) == 0) {
				t.Error("artifact scanner missed synthetic credential")
			}
			for name, redact := range map[string]func(string) string{"history": Redact, "display": ExternalText} {
				out := Normalize(redact(c.Input))
				for _, secret := range c.MustRedact {
					if strings.Contains(out, Normalize(secret)) {
						t.Errorf("%s retained synthetic credential in case %s", name, c.ID)
					}
				}
				for _, ordinary := range c.MustSurvive {
					if !strings.Contains(out, ordinary) {
						t.Errorf("%s lost ordinary text in case %s", name, c.ID)
					}
				}
			}
		})
	}
}

func TestCorpusCopiesAndValidity(t *testing.T) {
	b := CorpusJSON()
	if !json.Valid(b) || len(Corpus()) == 0 {
		t.Fatal("missing corpus")
	}
	b[0] = '!'
	cases := Corpus()
	cases[0].Input = "mutated"
	cases[0].MustRedact[0] = "mutated"
	if !json.Valid(CorpusJSON()) || Corpus()[0].Input == "mutated" || Corpus()[0].MustRedact[0] == "mutated" {
		t.Fatal("corpus storage is mutable")
	}
}

func TestNormalizationAndProviderDetection(t *testing.T) {
	token := "gsk_" + strings.Repeat("A1b2", 12)
	for _, r := range []rune{'\u200b', '\u200c', '\u200d', '\u2060', '\ufeff', '\u00ad', '\u202e', '\u2066', '\u034f', '\ufe0f', '\U000e0100'} {
		in := token[:2] + string(r) + token[2:]
		if Normalize(in) != token || !ContainsProviderToken(in) || SecretShape(in) == "" || strings.Contains(Normalize(Redact(in)), token) {
			t.Errorf("invisible separator %U bypassed detection", r)
		}
	}
	for _, r := range []rune{'\u00a0', '\u0085', '\u1680', '\u2003', '\u2028', '\u2029', '\u202f', '\u205f', '\u3000'} {
		in := "api" + string(r) + "key" + string(r) + "=" + string(r) + "syntheticOpaqueValue42"
		if strings.Contains(Redact(in), "syntheticOpaqueValue42") {
			t.Errorf("Unicode space %U bypassed assignment", r)
		}
	}
	if Normalize("a\nb\r\nc\td") != "a\nb\r\nc\td" {
		t.Fatal("ASCII instruction boundaries changed")
	}
	for _, ordinary := range []string{"API_TOKEN", "next_page_token", "gsk_configurationregistry", "github_pat_integration_test_fixture.json"} {
		if ContainsProviderToken(ordinary) {
			t.Fatal("ordinary schema identifier classified as provider token")
		}
	}
}

func TestNormalizationPreservesOrdinaryHistory(t *testing.T) {
	for _, s := range []string{"ordinary\u2003prose", "emoji\ufe0f", "local\u200bidentifier", "$GITLAB_TOKEN", "Authorization: Bearer $GITLAB_TOKEN"} {
		if Redact(s) != s {
			t.Fatal("ordinary history or variable reference changed")
		}
	}
	if !IsSensitiveName("pass\u200bword") || Argument("pass\u200bword", "tiny") == "tiny" {
		t.Fatal("invisible field name bypassed redaction")
	}
	if ExternalText("AWS_SECRET_ACCESS_KEY") != "[REDACTED: sensitive value]" || ExternalText("next_page_token") != "next_page_token" {
		t.Fatal("external identifier policy changed")
	}
}

func TestDisplayProseAndEncodedReferences(t *testing.T) {
	for _, ordinary := range []string{
		"password_policy: strict-enough-for-all", "secret_count: 1234567890",
		"parameter next_page_token is optional", "MAX_TOKENS: 100000000",
		"the api key rotation is documented here", "credentials: see the vault entry later",
	} {
		if ExternalText(ordinary) != ordinary {
			t.Fatal("ordinary display prose changed")
		}
	}
	for _, ordinary := range []string{
		`request {"password":"\u0024TOKEN","tokenizer":"ordinary-model"}`,
		`request {"dbPassword":"{env:PASS}"}`,
	} {
		frozen := pristineGateway(ordinary) != ordinary
		if (Redact(ordinary) != ordinary) != frozen || (SecretShape(ordinary) != "") != frozen {
			t.Fatalf("frozen-matched encoded reference was preserved %q -> %q (%s)", ordinary, Redact(ordinary), SecretShape(ordinary))
		}
	}
	for _, input := range []string{
		`request {"ghp_\u0041` + strings.Repeat("A1b2", 9) + `":"ordinary"}`,
		`request {"value":"ghp_\u0041` + strings.Repeat("A1b2", 9) + `"}`,
	} {
		if strings.Contains(Redact(input), strings.Repeat("A1b2", 9)) || strings.Contains(ExternalText(input), strings.Repeat("A1b2", 9)) {
			t.Fatal("encoded provider credential survived")
		}
	}
}

func TestShortProviderPrefixesKeepDelimiters(t *testing.T) {
	for _, prefix := range []string{"sk-", "gsk-", "ghp-", "xoxb-"} {
		token := prefix + strings.Repeat("A1b2", 9)
		for _, left := range []string{"", "_", "=", " ", ":"} {
			input := left + token + " after"
			if !ContainsProviderToken(input) || SecretShape(input) == "" {
				t.Fatal("delimited synthetic provider token missed")
			}
			if got := Redact(input); !strings.HasPrefix(got, left+"<redacted") || !strings.HasSuffix(got, " after") {
				t.Fatal("provider token replacement lost its delimiter or trailing prose")
			}
		}
	}
}

// Equality is an explicit assignment, unlike an unquoted colon in source or
// prose. Even a one-byte literal must block a generated artifact.
func TestArtifactScannerShortExplicitAssignments(t *testing.T) {
	for _, input := range []string{"DB_SIGNING_KEY=q", "api-key=q", "api key=q"} {
		if SecretShape(input) == "" || len(ScanArtifacts(map[string][]byte{"main": []byte(input)})) != 1 {
			t.Error("artifact scanner missed a short explicit credential assignment")
		}
		if Redact(input) == input {
			t.Error("history retained a short explicit credential assignment")
		}
	}
}

func TestCredentialConfigurationMetadata(t *testing.T) {
	for _, name := range []string{"token_class", "credential_type", "token_kind", "token_mode", "credential_status", "WireCredentialUnspecified"} {
		t.Run(name, func(t *testing.T) {
			if IsSensitiveName(name) || Argument(name, "ordinary-value") != "ordinary-value" {
				t.Fatal("credential metadata classified as a credential value")
			}
			input, _ := json.Marshal(map[string]string{name: "ordinary-value"})
			if Redact(string(input)) != string(input) || SecretShape(string(input)) != "" {
				t.Fatal("credential metadata was corrupted or blocked")
			}
			token := "ghp_" + strings.Repeat("A", 36)
			if strings.Contains(Argument(name, token), token) || SecretShape(token) == "" {
				t.Fatal("metadata exemption hid a provider credential")
			}
		})
	}
}

func TestArtifactScannerFrozenFloorIncludesYAMLMetadata(t *testing.T) {
	input := "properties:\n  vault_write_token:\n    description: A credential supplied at execution time\n    type: string\n"
	if findings := ScanArtifacts(map[string][]byte{"primitive.yaml": []byte(input)}); len(findings) == 0 {
		t.Fatal("metadata exemption suppressed the unconditional frozen match")
	}
}

func TestSecurityReviewCredentialComposition(t *testing.T) {
	for _, c := range Corpus() {
		if !strings.HasPrefix(c.ID, "security-review-") {
			continue
		}
		t.Run(c.ID, func(t *testing.T) {
			if SecretShape(c.Input) == "" || len(ScanArtifacts(map[string][]byte{"artifact": []byte(c.Input)})) == 0 {
				t.Error("credential-bearing composition bypassed artifact scanning")
			}
			for _, redact := range []func(string) string{Redact, ExternalText} {
				out := redact(c.Input)
				for _, secret := range c.MustRedact {
					if strings.Contains(out, secret) {
						t.Error("credential-bearing composition bypassed text redaction")
					}
				}
			}
		})
	}
}

func TestDocumentationLabelsAreNotCredentialFields(t *testing.T) {
	for _, name := range []string{"The password documentation", "api key rotation guide", "credential help page"} {
		if IsSensitiveName(name) || Argument(name, "ordinary material") != "ordinary material" {
			t.Fatalf("prose label classified as credential: %q", name)
		}
		input, _ := json.Marshal(map[string]string{name: "https://example.invalid/help"})
		if Redact(string(input)) != string(input) || SecretShape(string(input)) != "" {
			t.Fatal("documentation object field corrupted")
		}
	}
}

// Every format uses the same keyword roles. Keep exact bytes when clean.
func TestRound7SchemaMetadata(t *testing.T) {
	values := map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema", "$id": "https://example.invalid/schema", "$ref": "#/$defs/entry", "$anchor": "entry", "$dynamicRef": "#entry", "$dynamicAnchor": "entry", "$vocabulary": map[string]any{"https://example.invalid/vocab": true}, "$comment": "ordinary notes",
		"type": "string", "title": "Catalog input", "description": "ordinary catalog prose", "format": "password", "required": []any{"password"}, "dependentRequired": map[string]any{"password": []any{"ok"}},
		"minLength": 1, "maxLength": 100, "pattern": "^[a-z]+$", "minimum": 0, "maximum": 100, "exclusiveMinimum": 0, "exclusiveMaximum": 100, "multipleOf": 2, "minItems": 0, "maxItems": 100, "uniqueItems": true, "minContains": 0, "maxContains": 100, "minProperties": 0, "maxProperties": 100,
		"readOnly": true, "writeOnly": true, "deprecated": false, "contentEncoding": "base64", "contentMediaType": "application/json",
		"contentSchema": map[string]any{"type": "string"}, "unevaluatedItems": false, "unevaluatedProperties": false,
	}
	for key, value := range values {
		for _, name := range []string{"password", "api_key", "clientSecret"} {
			schema := map[string]any{"properties": map[string]any{name: map[string]any{"type": "string", key: value}}}
			jsonBytes, _ := json.Marshal(schema)
			block, _ := yaml.Marshal(schema)
			flowNode := yaml.Node{}
			_ = yaml.Unmarshal(block, &flowNode)
			var order func(*yaml.Node)
			order = func(n *yaml.Node) {
				for i := 0; n.Kind == yaml.MappingNode && i+1 < len(n.Content); i += 2 {
					if n.Content[i].Value == "type" {
						pair := append([]*yaml.Node(nil), n.Content[i:i+2]...)
						n.Content = append(pair, append(n.Content[:i], n.Content[i+2:]...)...)
						break
					}
				}
				for _, c := range n.Content {
					order(c)
				}
			}
			order(&flowNode)
			block, _ = yaml.Marshal(&flowNode)
			var flow func(*yaml.Node)
			flow = func(n *yaml.Node) {
				if n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode {
					n.Style = yaml.FlowStyle
				}
				for _, c := range n.Content {
					flow(c)
				}
			}
			flow(&flowNode)
			flowBytes, _ := yaml.Marshal(&flowNode)
			for format, input := range map[string]string{"json": string(jsonBytes), "block": string(block), "flow": string(flowBytes)} {
				t.Run(fmt.Sprintf("%s/%s/%s", key, name, format), func(t *testing.T) { assertRound7SchemaClean(t, input) })
			}
		}
	}
	for _, input := range []string{
		`{"properties":{"password":{"type":"string","contentEncoding":"base64"}}}`,
		"properties:\n  password: {type: string}\n",
		"properties:\n  password: {type: string, description: ordinary catalog prose}\n",
	} {
		assertRound7SchemaClean(t, input)
	}
}

func assertRound7SchemaClean(t *testing.T, input string) {
	t.Helper()
	if len(gatewayCredentialSpans(Normalize(input))) != 0 {
		t.Fatalf("frozen metadata match must be recorded, never exempted: %q", input)
	}
	for name, out := range map[string]string{"redact": Redact(input), "external": ExternalText(input), "argument": Argument("", input)} {
		if out != input {
			t.Errorf("%s changed clean schema %q -> %q", name, input, out)
		}
	}
	if SecretShape(input) != "" || len(ScanArtifacts(map[string][]byte{"schema": []byte(input)})) != 0 {
		t.Errorf("scanner rejected clean schema %q", input)
	}
}

func TestRound7SchemaValueInspection(t *testing.T) {
	for _, key := range []string{"default", "examples", "const", "enum", "x-custom"} {
		for _, name := range []string{"password", "api_key", "clientSecret"} {
			value := any("tiny")
			if key == "examples" || key == "enum" {
				value = []any{value}
			}
			schema := map[string]any{"properties": map[string]any{name: map[string]any{"type": "string", key: value}}}
			j, _ := json.Marshal(schema)
			y, _ := yaml.Marshal(schema)
			for _, input := range []string{string(j), string(y), fmt.Sprintf("properties: {%s: {type: string, %s: [tiny]}}", name, key)} {
				for _, out := range []string{Redact(input), ExternalText(input), Argument("", input)} {
					if strings.Contains(out, "tiny") {
						t.Errorf("%s value escaped %q -> %q", key, input, out)
					}
				}
				if SecretShape(input) == "" || len(ScanArtifacts(map[string][]byte{"schema": []byte(input)})) == 0 {
					t.Errorf("scanner missed annotation %q", input)
				}
			}
		}
	}
}

func TestRound7SchemaAdversarial(t *testing.T) {
	token := "gsk_" + strings.Repeat("A", 48)
	for _, input := range []string{
		"properties: {password: {type: string, description: " + token + "}}",
		"properties: {password: {type: string, default: {type: object, dbPassword: tiny}}}",
		"properties: {password: {type: string, examples: [{type: object, dbPassword: tiny}]}}",
		"properties:\n  password: # " + token + "\n    type: string\n",
		"# " + token + "\nproperties: {password: {type: string}}\n",
		"properties: {password: {type: string, default: \"{\\\"dbPassword\\\":\\\"tiny\\\"}\"}}",
		"properties: {password: {type: string, enum: [tiny]}}\n# " + token,
	} {
		if SecretShape(input) == "" || len(ScanArtifacts(map[string][]byte{"schema": []byte(input)})) == 0 {
			t.Errorf("scanner missed schema payload %q", input)
		}
		for _, out := range []string{Redact(input), ExternalText(input), Argument("", input)} {
			if strings.Contains(out, token) || strings.Contains(out, "tiny") {
				t.Errorf("schema payload retained %q -> %q", input, out)
			}
		}
	}
	for _, input := range []string{
		"properties: {password: &x {type: string, default: tiny}, other: *x}",
		"&root {properties: {password: {type: string, default: tiny}}}",
		"properties: {password: {type: string, default: tiny, default: safe}}",
		"properties: {password: {type: string, default: !opaque tiny}}",
		"properties: {password: {type: string}, ? [a, b]: tiny}",
	} {
		if SecretShape(input) == "" || strings.Contains(Redact(input), "tiny") {
			t.Errorf("ambiguous YAML failed open %q", input)
		}
	}
}

func TestRound7FrozenMetadataCollisionsStayEnforced(t *testing.T) {
	for _, name := range []string{"password", "api_key", "clientSecret"} {
		input := fmt.Sprintf("{properties: {%s: {type: string, dependentRequired: {password: [confirm]}}}}\n", name)
		if len(gatewayCredentialSpans(input)) == 0 || SecretShape(input) == "" || strings.Contains(Redact(input), "confirm") {
			t.Fatalf("frozen metadata collision was exempted %q", input)
		}
		t.Logf("frozen metadata collision (report exact input to owner): %q", input)
	}
}

func TestRound7MalformedMetadataStillInspectsData(t *testing.T) {
	for _, keyword := range []string{"description", "required", "type", "contentEncoding", "dependentRequired", "$vocabulary"} {
		value := any(map[string]any{"dbPassword": "tiny"})
		if keyword == "dependentRequired" || keyword == "$vocabulary" {
			value = map[string]any{"entry": value}
		}
		j, _ := json.Marshal(map[string]any{"properties": map[string]any{"password": map[string]any{"type": "string", keyword: value}}})
		var node yaml.Node
		_ = yaml.Unmarshal(j, &node)
		var block func(*yaml.Node)
		block = func(n *yaml.Node) {
			n.Style = 0
			for _, c := range n.Content {
				block(c)
			}
		}
		block(&node)
		y, _ := yaml.Marshal(&node)
		for _, input := range []string{string(j), string(y)} {
			if SecretShape(input) == "" || strings.Contains(Redact(input), "tiny") {
				t.Errorf("malformed %s exempted credential data %q", keyword, input)
			}
		}
	}
}

func TestRound7SchemaShapeContexts(t *testing.T) {
	for _, container := range []string{"properties", "patternProperties", "$defs", "dependentSchemas"} {
		for _, keyword := range []string{"items", "prefixItems", "contains", "allOf", "anyOf", "oneOf", "not", "if", "then", "else", "propertyNames", "contentSchema", "additionalProperties", "unevaluatedProperties", "unevaluatedItems"} {
			t.Run(container+"/"+keyword, func(t *testing.T) {
				child := "{type: string, contentEncoding: base64, description: ordinary catalog prose}"
				if keyword == "prefixItems" || keyword == "allOf" || keyword == "anyOf" || keyword == "oneOf" {
					child = "[" + child + "]"
				}
				input := container + ": {password: {type: object, " + keyword + ": " + child + "}}\n"
				assertRound7SchemaClean(t, input)
				var document yaml.Node
				if err := yaml.Unmarshal([]byte(input), &document); err != nil {
					t.Fatal(err)
				}
				var block func(*yaml.Node)
				block = func(n *yaml.Node) {
					n.Style = 0
					for _, c := range n.Content {
						block(c)
					}
				}
				block(&document)
				encoded, err := yaml.Marshal(&document)
				if err != nil {
					t.Fatal(err)
				}
				assertRound7SchemaClean(t, string(encoded))
			})
		}
	}
}

func TestRound7YAMLLogicalContinuations(t *testing.T) {
	token := "gsk_" + strings.Repeat("A", 48)
	for _, quote := range []string{"'", "\""} {
		input := "{type: object, description: " + quote + token[:28] + "\\\n" + token[28:] + quote + "}"
		if SecretShape(input) == "" || len(ScanArtifacts(map[string][]byte{"schema": []byte(input)})) == 0 {
			t.Errorf("logical YAML token not inspected %q", input)
		}
		for _, out := range []string{Redact(input), ExternalText(input)} {
			if strings.Contains(assembleLine(out), token) || strings.Contains(out, strings.Repeat("A", 24)) {
				t.Errorf("logical YAML token retained %q -> %q", input, out)
			}
		}
	}
}

func TestRound7InvalidSchemaScalars(t *testing.T) {
	for _, input := range []string{`{"properties":{"dbPassword":"tiny"}}`, `properties: {dbPassword: tiny}`, `{"properties":{"dbPassword":1234}}`, `properties: {dbPassword: 1234}`} {
		if SecretShape(input) == "" {
			t.Errorf("invalid subschema exempted named value %q", input)
		}
		for _, out := range []string{Redact(input), ExternalText(input), Argument("", input)} {
			if strings.Contains(out, "tiny") || strings.Contains(out, "1234") {
				t.Errorf("invalid subschema retained value %q -> %q", input, out)
			}
		}
	}
	for _, input := range []string{`{"properties":{"password":false,"dbPassword":true}}`, `properties: {password: false, dbPassword: true}`} {
		assertRound7SchemaClean(t, input)
	}
}

func checkProfileCase(t *testing.T, c CorpusCase) {
	t.Helper()
	out := (Ruleset{}).RedactProfile(c.Profile, c.Input)
	for _, secret := range c.MustRedact {
		if strings.Contains(Normalize(out), Normalize(secret)) {
			t.Errorf("profile retained secret in %s", c.ID)
		}
	}
	for _, ordinary := range c.MustSurvive {
		if !strings.Contains(out, ordinary) {
			t.Errorf("profile lost ordinary text in %s: %q", c.ID, out)
		}
	}
}

func TestRound8OriginalFloor(t *testing.T) {
	for _, space := range []string{"\u00a0", "\u0085", "\u1680", "\u2003", "\u2028", "\u2029", "\u202f", "\u205f", "\u3000"} {
		for _, control := range []string{"", "\u200b", "\u2060", "\ufeff"} {
			for _, frame := range []string{"api_key=prefix%s%sSyntheticTail42", `{"description":"api_key=prefix%s%sSyntheticTail42"}`, "properties: {password: {description: 'api_key=prefix%s%sSyntheticTail42'}}"} {
				input := fmt.Sprintf(frame, control, space)
				for _, out := range []string{Redact(input), ExternalText(input), Argument("", input), (Ruleset{}).RedactProfile(PushMetadataProfile, input)} {
					if strings.Contains(out, "SyntheticTail42") {
						t.Fatalf("original floor lost tail for %q -> %q", input, out)
					}
				}
			}
		}
	}
}

func TestRound8OlderSchemaVocabulary(t *testing.T) {
	for _, input := range []string{
		`{"dependencies":{"password":["username"]}}`,
		`{"properties":{"password":{"id":"#entry","$recursiveRef":"#entry","$recursiveAnchor":true}}}`,
		`{"dependencies":{"password":{"properties":{"password":{"type":"string","description":"a credential"}},"required":["username"]}}}`,
		`{"properties":{"password":{"dependencies":{"password":["username"],"other":true}}}}`,
		"dependencies: {password: [user]}\n",
		"properties: {password: {id: '#entry', $recursiveRef: '#entry', $recursiveAnchor: true}}\n",
		"dependencies:\n  ok:\n    properties:\n      password:\n        type: string\n    required: [username]\n",
	} {
		if out := Redact(input); out != input || SecretShape(input) != "" || len(ScanArtifacts(map[string][]byte{"schema": []byte(input)})) != 0 {
			t.Errorf("older schema lost metadata %q -> %q", input, out)
		}
	}
	token := "ghp_" + strings.Repeat("A", 36)
	for _, input := range []string{
		`{"dependencies":{"password":{"properties":{"password":{"default":"tiny"}}}}}`,
		`{"dependencies":{"password":["` + token + `"]}}`,
		`{"properties":{"password":{"$recursiveRef":{"dbPassword":"tiny"}}}}`,
		"dependencies: {password: {properties: {password: {default: tiny}}}}",
		"dependencies: {password: ['" + token + "']}",
		"properties: {password: {$recursiveAnchor: {dbPassword: tiny}}}",
	} {
		if out := Redact(input); strings.Contains(out, "tiny") || strings.Contains(out, token) || SecretShape(input) == "" {
			t.Errorf("older schema hid credential %q -> %q", input, out)
		}
	}
}

func TestRound8YAMLAliasContexts(t *testing.T) {
	for _, input := range []string{
		"properties: {region: &r {type: string}, zone: *r}",
		"&root {properties: {region: {type: string}}}",
		"properties:\n  region: &r\n    type: string\n  zone: *r\n",
		"properties: {password: &p {type: string}, other: *p}",
		"dependencies: {password: &d [username], other: *d}",
		"properties: {region: &r {type: string}, zone: &z {allOf: [*r]}, other: *z}",
	} {
		if out := Redact(input); out != "<redacted ambiguous YAML>" || SecretShape(input) == "" {
			t.Errorf("alias schema failed conservative handling %q -> %q", input, out)
		}
	}
	for _, input := range []string{
		"properties: {region: {title: &s tiny}, password: {default: *s}}",
		"properties: {password: {default: &s tiny}, region: {title: *s}}",
		"properties: {region: &r {default: tiny}, password: *r}",
		"properties: {region: &r {description: &s tiny}, password: {default: *s}}",
		"properties: &cycle {region: *cycle}",
	} {
		out := Redact(input)
		if strings.Contains(out, "password: {default: tiny}") || SecretShape(input) == "" {
			t.Errorf("alias credential/cycle failed open %q -> %q", input, out)
		}
		if strings.Contains(input, "cycle") && out != "<redacted ambiguous YAML>" {
			t.Errorf("cycle accepted %q", out)
		}
	}
	input := "properties:\n  p0: &p0 {type: string}\n"
	for i := 1; i < 20; i++ {
		input += fmt.Sprintf("  p%d: &p%d {allOf: [*p%d, *p%d]}\n", i, i, i-1, i-1)
	}
	if out := Redact(input); out != "<redacted ambiguous YAML>" {
		t.Fatal("alias expansion budget did not fail closed")
	}
}

func TestRound8MetadataProfile(t *testing.T) {
	rs := CurrentRuleset()
	profiles := rs.Profiles()
	if strings.Join(profiles, ",") != "local,push-metadata" {
		t.Fatal("missing profile advertisement")
	}
	profiles[0] = "mutated"
	if rs.Profiles()[0] != "local" {
		t.Fatal("mutable profile storage")
	}
	for _, unknown := range []string{"", "push-conclusions", "PUSH-METADATA", "unknown"} {
		if rs.RedactProfile(unknown, "private text") != "<redacted unsupported profile>" {
			t.Fatal("unknown profile failed open")
		}
	}
	for _, c := range Corpus() {
		if c.Profile == PushMetadataProfile {
			checkProfileCase(t, c)
		}
	}
	for _, plain := range []string{"person@corp.test", "192.0.2.17", "2001:db8::17"} {
		if rs.RedactProfile("local", plain) != plain || Redact(plain) != plain {
			t.Fatal("metadata extras changed local profile")
		}
		for _, frame := range []string{"%s", `{"value":"%s"}`, `{"%s":"ordinary"}`, `{"value":"{\"value\":\"%s\"}"}`, "properties: {region: {description: '%s'}}"} {
			in := fmt.Sprintf(frame, plain)
			if out := rs.RedactProfile(PushMetadataProfile, in); strings.Contains(out, plain) {
				t.Errorf("decoded profile value/key retained %q -> %q", in, out)
			}
		}
	}
	// The email documentation exemption never cancels the credential floor.
	if out := rs.RedactProfile(PushMetadataProfile, "password=person@example.invalid"); strings.Contains(out, "person@example.invalid") {
		t.Fatal("email exemption weakened base credential floor")
	}
}

func TestRound8NestedNormalizationFloor(t *testing.T) {
	for _, separator := range []string{"\u00a0", "\u0085", "\u2003"} {
		input := "api_key=prefix" + separator + "SyntheticTail42"
		for layer := 0; layer < 4; layer++ {
			encoded, _ := json.Marshal(input)
			input = strings.ReplaceAll(string(encoded), separator, fmt.Sprintf(`\u%04x`, []rune(separator)[0]))
			if out := Redact(input); strings.Contains(out, "SyntheticTail42") {
				t.Fatalf("decoded original floor lost tail at layer %d: %q", layer, out)
			}
		}
	}
}

func TestRound8ProfileURLAndIPBoundaries(t *testing.T) {
	for _, input := range []string{
		"192.0.2.17.", "IP [2001:db8::17]:443", "2001:DB8:0:0:0:0:2:17", "http://192.0.2.17:80/path", "fe80::17%en0.",
	} {
		out := (Ruleset{}).RedactProfile(PushMetadataProfile, input)
		if out == input {
			t.Errorf("address escaped profile %q", input)
		}
	}
	for _, input := range []string{"hostname.test", "v1.2.3.4", "1.2.3.4.5", "2001:wrong::17", "999.999.999.999", "http://hostname.test/docs", "sha256:abcdef0123456789"} {
		if out := (Ruleset{}).RedactProfile(PushMetadataProfile, input); out != input {
			t.Errorf("ordinary identifier redacted %q -> %q", input, out)
		}
	}
	for _, input := range []string{
		"https://hooks.slack-gov.com/services/T/B/S", "https://canary.discord.com/api/webhooks/123/opaque", "https://ptb.discord.com/api/webhooks/123/opaque", "https://corp.test/hook/receive?key=tiny", "https://corp.test/webhook?code=tiny", "https://corp.test/webhook?sig=tiny", "https://corp.test/webhook?signature=tiny", "https://hooks.corp.test/?key=tiny", "https://corp.test/hooks?%74oken=tiny", "https://corp.test/hooks?token=&token=tiny",
	} {
		if !secretWebhook(input) {
			t.Errorf("tokenized webhook missed %q", input)
		}
	}
	for _, input := range []string{
		"https://%broken/hooks", "https://corp.test/docs", "https://corp.test/hooks?region=west", "https://corp.test/hooks?key=", "https://corp.test/hooks?token=$TOKEN", "https://discord.com/api/webhooks/123", "https://hooks.slack.com/services/T/B", "https://hooks.corp.test/", "https://corp.test/hooks/$TOKEN",
	} {
		if secretWebhook(input) {
			t.Errorf("credential-free webhook classified %q", input)
		}
	}
}

func TestRound8DirectFloorCannotNormalizeAwayOriginalSpans(t *testing.T) {
	for _, sep := range []string{"\u00a0", "\u0085", "\u2003"} {
		source := "api_key=prefix" + sep + "SyntheticTail42"
		result := gatewayFloor(source, inspection{source, ""})
		if strings.Contains(result.text, "SyntheticTail42") {
			t.Fatalf("floor itself dropped original tail: %q", result.text)
		}
	}
}

func TestRound8PEMNormalizationComposition(t *testing.T) {
	input := "api_key=-----BEGIN\u00a0PRIVATE KEY-----\nSyntheticPEMBody42\n-----END PRIVATE KEY-----"
	for _, out := range []string{Redact(input), (Ruleset{}).RedactProfile(PushMetadataProfile, input)} {
		if strings.Contains(out, "SyntheticPEMBody42") {
			t.Fatalf("normalization/floor composition exposed PEM body: %q", out)
		}
	}
}

func TestRound8LongWebhookPath(t *testing.T) {
	input := "https://corp.test/" + strings.Repeat("hooks/", 8192)
	if secretWebhook(input) {
		t.Fatal("token-free path classified as credential")
	}
	if !secretWebhook(input + "SyntheticWebhookValue42") {
		t.Fatal("long path hid terminal credential")
	}
}

func TestRound8EscapedWhitespaceCannotWeakenOriginalFloor(t *testing.T) {
	for _, space := range []string{`\u0020`, `\n`, `\t`, `\u002c`, `\u007d`, `\u0022`, `\u0027`} {
		input := `{"description":"api_key=prefixLong` + space + `SyntheticTail42"}`
		out := Redact(input)
		if strings.Contains(out, "SyntheticTail42") {
			t.Fatalf("original escaped floor exposed tail: %q", out)
		}
	}
}

func TestRound9YAMLNeverRestoresAliasContent(t *testing.T) {
	for _, input := range []string{
		"properties:\n  region:\n    type: string\n    description: &s SyntheticOpaqueValue42 person@corp.test\n  password:\n    type: string\n    default: *s\n",
		"properties:\n  password:\n    type: string\n    default: &s SyntheticOpaqueValue42 person@corp.test\n  region:\n    type: string\n    description: *s\n",
		"properties: &AKIA" + strings.Repeat("A", 16) + " {region: {type: string}}",
		"properties: {region: &r {description: 'AKIA" + strings.Repeat("A", 16) + "'}, zone: *r}",
	} {
		for _, profile := range CurrentRuleset().Profiles() {
			out := CurrentRuleset().RedactProfile(profile, input)
			if strings.Contains(out, "SyntheticOpaqueValue42") || strings.Contains(out, "AKIA"+strings.Repeat("A", 16)) {
				t.Errorf("%s retained anchor or scalar credential %q", profile, out)
			}
		}
		if SecretShape(input) == "" || len(ScanArtifacts(map[string][]byte{"schema": []byte(input)})) == 0 {
			t.Error("anchor/alias schema escaped scanner")
		}
	}
}

func TestRound9IPv6Boundaries(t *testing.T) {
	for _, c := range Corpus() {
		if strings.HasPrefix(c.ID, "round9-ipv6-") {
			checkProfileCase(t, c)
			// An escaped JSON spelling must be inspected after decoding. Checking
			// only raw output would pass even if its escapes hid the address.
			if json.Valid([]byte(c.Input)) {
				out := CurrentRuleset().RedactProfile(c.Profile, c.Input)
				var value any
				if err := json.Unmarshal([]byte(out), &value); err != nil {
					t.Fatalf("profile broke JSON in %s: %v", c.ID, err)
				}
				decoded, _ := json.Marshal(value)
				for _, secret := range c.MustRedact {
					if strings.Contains(string(decoded), secret) {
						t.Errorf("decoded JSON retained address in %s", c.ID)
					}
				}
			}
		}
	}
	// Each bounded address search must keep working beyond a hostile long run.
	input := strings.Repeat("a:", 8192) + "ip:2001:db8::17"
	if out := CurrentRuleset().RedactProfile(PushMetadataProfile, input); strings.Contains(out, "2001:db8::17") {
		t.Fatal("long prefix concealed address")
	}
}

func TestRound9IPv6FullSpanCoverage(t *testing.T) {
	for _, input := range []string{"ip:::1", "ip:::abcd", "ip:2001:db8::17:", "[fe80::17%en0]:443"} {
		first := strings.Index(input, "::")
		last := len(input)
		switch input {
		case "ip:2001:db8::17:":
			first, last = 3, len(input)-1
		case "[fe80::17%en0]:443":
			first, last = 1, strings.IndexByte(input, ']')
		case "ip:::1", "ip:::abcd":
			first = 3
		}
		covered := make([]bool, len(input))
		for _, span := range metadataIPv6Spans(input) {
			for i := span.start; i < span.end; i++ {
				covered[i] = true
			}
		}
		for i := first; i < last; i++ {
			if !covered[i] {
				t.Errorf("IPv6 byte %d escaped coverage in %q", i, input)
			}
		}
	}
}

func TestRound9SecurityPunctuation(t *testing.T) {
	for _, input := range []string{"ip:2001:db8::17...", "ip:2001:db8::17..", "[2001:db8::17]:443", "2001:db8::17:", "ip:fe80::1%en0"} {
		out := CurrentRuleset().RedactProfile(PushMetadataProfile, input)
		if strings.Contains(out, "2001:db8::17") || strings.Contains(out, "fe80::1%en0") {
			t.Errorf("retained IPv6 %q -> %q", input, out)
		}
	}
}
