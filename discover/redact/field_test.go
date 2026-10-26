package redact

import (
	"strings"
	"testing"
)

func TestFieldContext(t *testing.T) {
	for _, path := range [][]string{{"password"}, {"password", "nested", "value"}, {"rows", "api\u200b_key", "value"}} {
		for _, value := range []string{"syntheticOpaqueValue42", `{"value":"syntheticOpaqueValue42"}`, `["syntheticOpaqueValue42"]`} {
			if strings.Contains(Field(path, value), "syntheticOpaqueValue42") {
				t.Fatalf("field context lost for %q", path)
			}
		}
	}
	if strings.Contains(Field([]string{"properties", "password", "default"}, "syntheticOpaqueValue42"), "syntheticOpaqueValue42") {
		t.Fatal("schema literal lost field ancestry")
	}
	if Field([]string{"properties", "password", "description"}, "ordinary documentation") != "ordinary documentation" {
		t.Fatal("schema metadata classified as credential")
	}
	if Field([]string{"properties", "password"}, "false") != "false" {
		t.Fatal("boolean subschema classified as credential")
	}
	value := `{"properties":{"password":{"type":"syntheticOpaqueValue42"}}}`
	if strings.Contains(Field([]string{"default"}, value), "syntheticOpaqueValue42") {
		t.Fatal("instance literal acquired a schema exemption")
	}

	if IsSensitiveName("token_hint") {
		t.Fatal("credential hint metadata classified as credential value")
	}
	if Field([]string{"properties", "token_hint", "examples"}, `"plain example"`) != `"plain example"` {
		t.Fatal("hint example lost metadata role")
	}
	provider := "ghp_" + strings.Repeat("A", 36)
	if strings.Contains(Field([]string{"properties", "token_hint", "examples"}, provider), provider) {
		t.Fatal("hint metadata bypassed provider shapes")
	}

	for _, input := range []string{`"properties: {password: {type: string, default: [tiny]}}"`, `"properties: {password: &shared {default: tiny}, ordinary: *shared}"`} {
		if strings.Contains(Field([]string{"default"}, input), "tiny") {
			t.Fatal("annotation source view bypassed contextual inspection")
		}
	}

	for _, c := range Corpus() {
		if Field(nil, c.Input) != Redact(c.Input) || ExternalField(nil, c.Input) != ExternalText(c.Input) {
			t.Fatalf("empty path changed %s", c.ID)
		}
	}
	if Field([]string{"properties", "next_page_token", "type"}, "string") != "string" {
		t.Fatal("pagination schema metadata mistaken for credential")
	}
}
