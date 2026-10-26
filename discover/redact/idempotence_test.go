package redact

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func TestFieldRedactionIsIdempotent(t *testing.T) {
	cases := []string{
		"password: |\n  syntheticOpaqueValue42\n",
		"password: '<redacted credential>'",
		"password: '[REDACTED: sensitive value]'",
		"api_key_name: production-key-name",
		"access_token_ttl_seconds=3600000",
		"next_page_token: opaque cursor from the previous page",
	}
	for _, input := range cases {
		t.Run(input, func(t *testing.T) {
			once := Field(nil, input)
			twice := Field(nil, once)
			if twice != once {
				t.Fatalf("Field is not idempotent: once %q, twice %q", once, twice)
			}
		})
	}
}

func TestFieldKeepsCredentialMetadataNames(t *testing.T) {
	for _, input := range []string{
		"api_key_name: production-key-name",
		"access_token_ttl_seconds=3600000",
		"next_page_token: opaque cursor from the previous page",
	} {
		if got := Field(nil, input); got != input {
			t.Errorf("credential metadata changed: input %q got %q", input, got)
		}
	}
	providerToken := "next_page_token: ghp_" + strings.Repeat("A", 36)
	if got := Field(nil, providerToken); strings.Contains(got, strings.Repeat("A", 36)) {
		t.Fatalf("provider token escaped under pagination metadata: %q", got)
	}
}

func TestFieldPreservesDeepSchemaAndLiteral(t *testing.T) {
	for _, depth := range []int{64, 65, 200, 1000} {
		t.Run("depth_"+itoa(depth), func(t *testing.T) {
			var literal any = "ordinary"
			for i := 0; i < depth; i++ {
				literal = map[string]any{"ordinary": literal}
			}
			rawLiteral, err := json.Marshal(literal)
			if err != nil {
				t.Fatal(err)
			}
			if got := Field([]string{"default"}, string(rawLiteral)); got != string(rawLiteral) {
				t.Fatalf("deep clean literal changed at depth %d", depth)
			}

			var schema any = map[string]any{"properties": map[string]any{
				"password": map[string]any{
					"type": "string", "description": "ordinary catalog prose",
					"default": "syntheticOpaqueValue42",
				},
			}}
			for i := 0; i < depth; i++ {
				schema = map[string]any{"properties": map[string]any{"ordinary": schema}}
			}
			rawSchema, err := json.Marshal(schema)
			if err != nil {
				t.Fatal(err)
			}
			got := Field(nil, string(rawSchema))
			if json.Valid([]byte(got)) == false {
				t.Fatalf("deep schema became invalid JSON at depth %d: %q", depth, got)
			}
			var decoded map[string]any
			if err := json.Unmarshal([]byte(got), &decoded); err != nil {
				t.Fatal(err)
			}
			var current any = decoded
			for i := 0; i < depth; i++ {
				current = current.(map[string]any)["properties"].(map[string]any)["ordinary"]
			}
			password := current.(map[string]any)["properties"].(map[string]any)["password"].(map[string]any)
			if password["description"] != "ordinary catalog prose" {
				t.Fatalf("ordinary schema metadata changed at depth %d", depth)
			}
			if password["default"] == "syntheticOpaqueValue42" || password["default"] == nil {
				t.Fatalf("deep credential default survived or structure was lost at depth %d", depth)
			}
		})
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
