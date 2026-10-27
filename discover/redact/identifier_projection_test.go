package redact

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func identifierContext(t *testing.T, values []any) FieldContext {
	t.Helper()
	return childFieldContext(t, RootFieldContext().Schema(true), "required", values, true, true)
}

func TestIdentifierProjectionKeepsUniqueStableMetadata(t *testing.T) {
	first := "ghp_" + strings.Repeat("A", 36)
	second := "ghp_" + strings.Repeat("B", 36)
	values := []any{first, second, "ordinary", "<redacted GitHub token>", "<redacted GitHub token> [item 2]"}
	context := identifierContext(t, values)
	projection, admitted := context.ProjectIdentifiers(values, nil, func(int) bool { return true })
	if !admitted || len(projection) != len(values) {
		t.Fatal("valid metadata cardinality changed")
	}
	seen := map[string]bool{}
	for i, text := range projection {
		if text == first || text == second || seen[text] {
			t.Fatal("projection retained a credential or introduced a duplicate")
		}
		seen[text] = true
		if i >= 2 && text != values[i] {
			t.Fatal("an unchanged ordinary item moved or changed")
		}
	}
	next := make([]any, len(projection))
	for i, text := range projection {
		next[i] = text
	}
	again, admitted := identifierContext(t, next).ProjectIdentifiers(next, nil, func(int) bool { return true })
	if !admitted || strings.Join(projection, "\x00") != strings.Join(again, "\x00") {
		t.Fatal("a second public projection changed stable identifiers")
	}
}

func TestIdentifierProjectionDoesNotAuthorizeInvalidOrDataLists(t *testing.T) {
	for _, values := range [][]any{
		{"ordinary", "ordinary"},
		{"<redacted GitHub token>", "<redacted GitHub token>"},
		{"[REDACTED: sensitive value]", "[REDACTED: sensitive value]"},
		{"ordinary", nil},
	} {
		projection, admitted := identifierContext(t, values).ProjectIdentifiers(values, nil, func(int) bool { return true })
		if !admitted || projection != nil {
			t.Fatal("invalid pre-existing strings acquired projection authority")
		}
	}
	values := []any{"ghp_" + strings.Repeat("A", 36), "ghp_" + strings.Repeat("B", 36)}
	data := childFieldContext(t, RootFieldContext().Schema(true), "default", values, true, true)
	projection, admitted := data.ProjectIdentifiers(values, nil, func(int) bool { return true })
	if !admitted || projection != nil {
		t.Fatal("schema annotation data acquired identifier metadata behavior")
	}
}

func TestIdentifierProjectionBudgetRefusalHasNoPartialOutput(t *testing.T) {
	values := []any{"ghp_" + strings.Repeat("A", 36), "ghp_" + strings.Repeat("B", 36)}
	context := identifierContext(t, values)
	work := 0
	projection, admitted := context.ProjectIdentifiers(values, nil, func(cost int) bool { work += cost; return true })
	if !admitted || len(projection) != 2 || work <= 1 {
		t.Fatal("valid projection did not account for work")
	}
	for _, limit := range []int{0, 1, work - 1} {
		left := limit
		projection, admitted := context.ProjectIdentifiers(values, nil, func(cost int) bool {
			if cost < 0 || cost > left {
				return false
			}
			left -= cost
			return true
		})
		if admitted || projection != nil {
			t.Fatal("a refused projection returned partial output")
		}
	}
}

func TestIdentifierProjectionConsumersRepeatStably(t *testing.T) {
	for _, key := range []string{"required", "dependentRequired", "dependencies"} {
		t.Run(key, func(t *testing.T) {
			values := []any{"ghp_" + strings.Repeat("A", 36), "ghp_" + strings.Repeat("B", 36), "ordinary", "<redacted GitHub token> [item 2]"}
			var metadata any = values
			if key != "required" {
				metadata = map[string]any{"ordinary": values}
			}
			element := map[string]any{key: metadata, "properties": map[string]any{"ordinary": map[string]any{"default": false}}}
			root := map[string]any{"dependencies": map[string]any{"password": map[string]any{"allOf": []any{element}}}}
			wire, err := json.Marshal(root)
			if err != nil {
				t.Fatal(err)
			}
			got := Redact(string(wire))
			if got == string(wire) || strings.Contains(got, strings.Repeat("A", 36)) || strings.Contains(got, strings.Repeat("B", 36)) || !strings.Contains(got, `"default":false`) {
				t.Fatal("JSON projection lost privacy or ordinary metadata")
			}
			if Redact(got) != got {
				t.Fatal("JSON repeat pass changed projected metadata")
			}
			block, err := yaml.Marshal(root)
			if err != nil {
				t.Fatal(err)
			}
			got = Redact(string(block))
			if got == string(block) || strings.Contains(got, strings.Repeat("A", 36)) || strings.Contains(got, strings.Repeat("B", 36)) || !strings.Contains(got, "default: false") {
				t.Fatal("YAML projection lost privacy or ordinary metadata")
			}
			if Redact(got) != got {
				t.Fatal("YAML repeat pass changed projected metadata")
			}
		})
	}
}

func TestIdentifierProjectionKeepsMetadataProfileAndYAMLComments(t *testing.T) {
	input := `{"dependencies":{"password":{"allOf":[{"required":["a@corp.test","b@corp.test"],"properties":{"ordinary":{"default":false}}}]}}}`
	got := (Ruleset{}).RedactProfile(PushMetadataProfile, input)
	if strings.Contains(got, "a@corp.test") || strings.Contains(got, "b@corp.test") || !strings.Contains(got, `"default":false`) {
		t.Fatal("identifier projection bypassed the active privacy profile")
	}
	if (Ruleset{}).RedactProfile(PushMetadataProfile, got) != got {
		t.Fatal("metadata-profile projection was not stable")
	}
	// Documentation addresses are explicitly exempt from the profile email
	// extra. The base credential floor still applies independently.
	exempt := strings.ReplaceAll(input, "@corp.test", "@example.invalid")
	if got := (Ruleset{}).RedactProfile(PushMetadataProfile, exempt); got != exempt {
		t.Fatal("identifier projection changed exempt documentation addresses")
	}
	block := "dependencies:\n  password:\n    allOf:\n      - required:\n          - ghp_" + strings.Repeat("A", 36) + " # API_TOKEN=syntheticOpaqueValue42\n          - ghp_" + strings.Repeat("B", 36) + "\n        properties:\n          ordinary:\n            default: false\n"
	got = Redact(block)
	if strings.Contains(got, "syntheticOpaqueValue42") || strings.Contains(got, strings.Repeat("A", 36)) || !strings.Contains(got, "default: false") {
		t.Fatal("projection bypassed YAML comments or lost ordinary metadata")
	}
	if Redact(got) != got {
		t.Fatal("YAML comment projection changed on repetition")
	}
}

func TestDependencyIdentifierProjectionRequiresUniqueOccurrence(t *testing.T) {
	values := []any{"ghp_" + strings.Repeat("A", 36), "ghp_" + strings.Repeat("B", 36)}
	dependencies := map[string]any{"password": values}
	for _, tc := range []struct {
		name                                 string
		parentUnique, keyUnique, valueUnique bool
	}{
		{"duplicate dependency name", true, false, true},
		{"escaped duplicate dependency name", false, false, true},
		{"ambiguous dependency value", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			context := childFieldContext(t, RootFieldContext().Schema(tc.parentUnique), "dependencies", dependencies, true, tc.parentUnique)
			context = enterFieldContext(t, context, dependencies, tc.parentUnique)
			context = childFieldContext(t, context, "password", values, tc.keyUnique, tc.valueUnique)
			// Even a unique list value cannot restore its source member's revoked
			// identifier projection authority when Enter runs after Child.
			context = enterFieldContext(t, context, values, true)
			projection, admitted := context.ProjectIdentifiers(values, nil, func(int) bool { return true })
			if !admitted || projection != nil {
				t.Fatal("an ambiguous dependency occurrence regained identifier projection")
			}
			for _, value := range values {
				text := value.(string)
				if strings.Contains(context.Scalar(text, jsonStringToken(text)), text) {
					t.Fatal("refusing identifier projection bypassed credential inspection")
				}
			}
		})
	}
	context := childFieldContext(t, RootFieldContext().Schema(true), "dependencies", dependencies, true, true)
	context = enterFieldContext(t, context, dependencies, true)
	context = childFieldContext(t, context, "password", values, true, true)
	context = enterFieldContext(t, context, values, true)
	projection, admitted := context.ProjectIdentifiers(values, nil, func(int) bool { return true })
	if !admitted || len(projection) != 2 || projection[0] == projection[1] {
		t.Fatal("a unique dependency list lost collision-free projection")
	}
}
