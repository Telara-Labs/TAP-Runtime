package redact

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMetadataExemptionsRequireDeclaredShapes(t *testing.T) {
	for _, keyword := range schemaMetadataKeywords {
		t.Run(keyword, func(t *testing.T) {
			for _, malformed := range []any{
				map[string]any{"dbPassword": "syntheticOpaqueValue42"},
				[]any{map[string]any{"dbPassword": "syntheticOpaqueValue42"}},
			} {
				raw, _ := json.Marshal(map[string]any{"properties": map[string]any{"password": map[string]any{keyword: malformed}}})
				var tree any
				if err := yaml.Unmarshal(raw, &tree); err != nil {
					t.Fatal(err)
				}
				block, err := yaml.Marshal(tree)
				if err != nil {
					t.Fatal(err)
				}
				for _, source := range []string{string(raw), string(block)} {
					if got := Field(nil, source); strings.Contains(got, "syntheticOpaqueValue42") {
						t.Fatalf("malformed exemption leaked %s", got)
					}
					if findings := ScanArtifacts(map[string][]byte{"schema.json": []byte(source)}); len(findings) == 0 {
						t.Fatal("scanner missed malformed exemption")
					}
				}
			}
		})
	}
}

func TestMetadataValueShapeControls(t *testing.T) {
	cases := []struct {
		key            string
		valid, invalid any
	}{
		{"$vocabulary", map[string]any{"https://example.invalid/vocab": true}, map[string]any{"dbPassword": "syntheticOpaqueValue42"}},
		{"$vocabulary", map[string]any{"urn:example:vocab": false}, map[string]any{"not-a-uri": true}},
		{"dependentRequired", map[string]any{"dbPassword": []any{"API_TOKEN", "dbPassword"}}, map[string]any{"dbPassword": "syntheticOpaqueValue42"}},
		{"required", []any{"dbPassword", "API_TOKEN"}, []any{1}},
		{"type", []any{"string", "null"}, []any{"not-a-type"}},
		{"type", "string", nil},
		{"title", "syntheticOpaqueValue42", false},
		{"minimum", json.Number("-1.5"), "syntheticOpaqueValue42"},
		{"minItems", json.Number("0"), json.Number("-1")},
		{"maxLength", json.Number("4"), json.Number("1.5")},
		{"multipleOf", json.Number("0.5"), json.Number("0")},
		{"readOnly", true, "syntheticOpaqueValue42"},
	}
	for _, c := range cases {
		if !validSchemaMetadata(c.key, c.valid) || validSchemaMetadata(c.key, c.invalid) {
			t.Errorf("wrong shape classification for %s", c.key)
		}
		raw, _ := json.Marshal(map[string]any{"properties": map[string]any{"password": map[string]any{c.key: c.valid}}})
		if got := Field(nil, string(raw)); got != string(raw) {
			t.Errorf("valid metadata altered %s -> %s", raw, got)
		}
	}
	for _, keyword := range schemaMetadataKeywords {
		raw, _ := json.Marshal(map[string]any{"properties": map[string]any{"password": map[string]any{keyword: nil}}})
		var tree map[string]any
		_ = json.Unmarshal(raw, &tree)
		if validSchemaMetadata(keyword, nil) {
			t.Errorf("null exemption for %s", keyword)
		}
	}
}

func TestMalformedDependencyArraysInspectInstances(t *testing.T) {
	for _, source := range []string{
		`{"properties":{"password":{"dependencies":{"dbPassword":[{"dbPassword":"syntheticOpaqueValue42"}]}}}}`,
		"properties:\n  password:\n    dependencies:\n      dbPassword:\n        - dbPassword: syntheticOpaqueValue42\n",
	} {
		if strings.Contains(Field(nil, source), "syntheticOpaqueValue42") {
			t.Fatal("dependency-array exemption leaked")
		}
	}
}
