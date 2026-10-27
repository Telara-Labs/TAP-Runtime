package redact

import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func vocabularyContext(t *testing.T, value any) FieldContext {
	t.Helper()
	return childFieldContext(t, RootFieldContext().Schema(true), "$vocabulary", value, true, true)
}

func TestVocabularyProjectionKeepsValidUniqueKeysAndOrder(t *testing.T) {
	first := "https://ghp_" + strings.Repeat("A", 36) + ".example.invalid/vocabulary"
	second := "https://ghp_" + strings.Repeat("B", 36) + ".example.invalid/vocabulary"
	keys := []string{first, "urn:telara:redacted:vocabulary:1", "urn:telara:redacted:vocabulary:1:1", second, "https://example.invalid/ordinary"}
	value := map[string]any{}
	for i, key := range keys {
		value[key] = i%2 == 0
	}
	got, admitted := vocabularyContext(t, value).ProjectVocabularyKeys(value, keys, nil, func(int) bool { return true })
	want := []string{"urn:telara:redacted:vocabulary:1:2", keys[1], keys[2], "urn:telara:redacted:vocabulary:4", keys[4]}
	if !admitted || !reflect.DeepEqual(got, want) {
		t.Fatal("URI projection lost order, unchanged keys or collision reservation")
	}
	next := map[string]any{}
	for i, key := range got {
		uri, err := url.Parse(key)
		if err != nil || !uri.IsAbs() {
			t.Fatal("projection generated an invalid absolute URI")
		}
		next[key] = value[keys[i]]
	}
	again, admitted := vocabularyContext(t, next).ProjectVocabularyKeys(next, got, nil, func(int) bool { return true })
	if !admitted || !reflect.DeepEqual(got, again) || len(next) != len(value) {
		t.Fatal("URI projection lost cardinality or repeat stability")
	}
}

func TestVocabularyProjectionRequiresCompleteOriginalOccurrences(t *testing.T) {
	key := "https://ghp_" + strings.Repeat("A", 36) + ".example.invalid/vocabulary"
	value := map[string]any{key: false, "urn:ordinary": true}
	context := vocabularyContext(t, value)
	for _, keys := range [][]string{nil, {key}, {key, key}, {key, "urn:missing"}, {key, "urn:ordinary", "urn:extra"}} {
		got, admitted := context.ProjectVocabularyKeys(value, keys, nil, func(int) bool { return true })
		if !admitted || got != nil {
			t.Fatal("partial, duplicate or invented key order gained authority")
		}
	}
	for _, facts := range []FieldOccurrence{{Value: value, KeyUnique: false, ValueUnique: true}, {Value: value, KeyUnique: true, ValueUnique: false}} {
		ambiguous, admitted := RootFieldContext().Schema(true).Child("$vocabulary", facts, func(int) bool { return true })
		if !admitted {
			t.Fatal("ordinary classification refused")
		}
		ambiguous = enterFieldContext(t, ambiguous, value, true)
		got, admitted := ambiguous.ProjectVocabularyKeys(value, []string{key, "urn:ordinary"}, nil, func(int) bool { return true })
		if !admitted || got != nil {
			t.Fatal("ambiguous Child regained URI projection at Enter")
		}
	}
	ambiguous := enterFieldContext(t, context, value, false)
	ambiguous = enterFieldContext(t, ambiguous, value, true)
	got, admitted := ambiguous.ProjectVocabularyKeys(value, []string{key, "urn:ordinary"}, nil, func(int) bool { return true })
	if !admitted || got != nil {
		t.Fatal("unique Enter restored revoked URI projection")
	}
	// A path-only metadata role is not an occurrence receipt.
	pathOnly := FieldContext{state: schemaContext{schema: true}.child("$vocabulary"), initialized: true}
	got, admitted = pathOnly.ProjectVocabularyKeys(value, []string{key, "urn:ordinary"}, nil, func(int) bool { return true })
	if !admitted || got != nil {
		t.Fatal("path-only metadata minted URI authority")
	}
}

func TestVocabularyProjectionRejectsMalformedAndProtectedContexts(t *testing.T) {
	for _, value := range []map[string]any{
		{"relative": false}, {"https://[invalid": false}, {"urn:ordinary": "false"}, {"urn:ordinary": nil},
		{"urn:ordinary:key": false, "urn:ordinary:\u200bkey": true},
	} {
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		got, admitted := vocabularyContext(t, value).ProjectVocabularyKeys(value, keys, nil, func(int) bool { return true })
		if !admitted || got != nil {
			t.Fatal("malformed vocabulary acquired projection")
		}
	}
	value := map[string]any{"https://ghp_" + strings.Repeat("A", 36) + ".example.invalid/vocabulary": false}
	var key string
	for name := range value {
		key = name
	}
	for _, role := range []string{"default", "unknown", "api_key"} {
		parent := childFieldContext(t, RootFieldContext().Schema(true), role, map[string]any{"$vocabulary": value}, true, true)
		parent = enterFieldContext(t, parent, map[string]any{"$vocabulary": value}, true)
		context := childFieldContext(t, parent, "$vocabulary", value, true, true)
		got, admitted := context.ProjectVocabularyKeys(value, []string{key}, nil, func(int) bool { return true })
		if !admitted || got != nil {
			t.Fatal("data, unknown or credential ancestor acquired URI projection")
		}
	}
	// The same traversal's positive fact cannot survive a fresh encoded-text view.
	context := vocabularyContext(t, value)
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	inspected := inspectField(context.state, string(encoded), false)
	if strings.Contains(inspected, "urn:telara:redacted:vocabulary:") || strings.Contains(inspected, strings.Repeat("A", 36)) {
		t.Fatal("encoded metadata text reused projection authority or bypassed the floor")
	}
}

func TestVocabularyProjectionWorkAndAtomicRefusal(t *testing.T) {
	for _, size := range []int{1, 4, 16, 64, 128} {
		value := map[string]any{}
		keys := make([]string, 0, 2*size)
		for i := 0; i < size; i++ {
			key := fmt.Sprintf("https://example.invalid/%d/ghp_%s", i, strings.Repeat("A", 36))
			keys = append(keys, key)
			value[key] = false
		}
		for i := 0; i < size; i++ {
			key := fmt.Sprintf("urn:telara:redacted:vocabulary:%d", i+1)
			keys = append(keys, key)
			value[key] = true
		}
		context := vocabularyContext(t, value)
		work := 0
		got, admitted := context.ProjectVocabularyKeys(value, keys, nil, func(cost int) bool { work += cost; return true })
		if !admitted || len(got) != 2*size || work > 4096*size+1024 {
			t.Fatal("URI projection lost its scaled linear work bound")
		}
		before, _ := json.Marshal(value)
		for _, limit := range []int{0, 1, work - 1} {
			left := limit
			partial, admitted := context.ProjectVocabularyKeys(value, keys, nil, func(cost int) bool {
				if cost < 0 || cost > left {
					return false
				}
				left -= cost
				return true
			})
			after, _ := json.Marshal(value)
			if admitted || partial != nil || string(before) != string(after) {
				t.Fatal("refusal returned partial output or mutated the source")
			}
		}
	}
}

func TestVocabularyProjectionCoreConsumersRepeat(t *testing.T) {
	key := "https://ghp_" + strings.Repeat("A", 36) + ".example.invalid/vocabulary"
	for _, profile := range []string{"local", PushMetadataProfile} {
		root := map[string]any{"dependencies": map[string]any{"password": map[string]any{"allOf": []any{map[string]any{
			"$vocabulary": map[string]any{key: false, "urn:ordinary": true}, "properties": map[string]any{"ordinary": map[string]any{"default": false}},
		}}}}}
		for _, yamlInput := range []bool{false, true} {
			var raw []byte
			var err error
			if yamlInput {
				raw, err = yaml.Marshal(root)
			} else {
				raw, err = json.Marshal(root)
			}
			if err != nil {
				t.Fatal(err)
			}
			got := (Ruleset{}).RedactProfile(profile, string(raw))
			if strings.Contains(got, strings.Repeat("A", 36)) || !strings.Contains(got, "urn:telara:redacted:vocabulary:") {
				t.Fatal("typed URI projection retained a credential or lost URI authority")
			}
			var decoded map[string]any
			if yamlInput {
				err = yaml.Unmarshal([]byte(got), &decoded)
			} else {
				err = json.Unmarshal([]byte(got), &decoded)
			}
			if err != nil {
				t.Fatal("projection broke the consumer document")
			}
			body := decoded["dependencies"].(map[string]any)["password"].(map[string]any)["allOf"].([]any)[0].(map[string]any)
			vocabulary := body["$vocabulary"].(map[string]any)
			if len(vocabulary) != 2 || vocabulary["urn:ordinary"] != true || body["properties"].(map[string]any)["ordinary"].(map[string]any)["default"] != false {
				t.Fatal("projection changed Boolean values, cardinality or ordinary data")
			}
			for uri, value := range vocabulary {
				u, e := url.Parse(uri)
				if e != nil || !u.IsAbs() {
					t.Fatal("invalid projected URI")
				}
				if uri != "urn:ordinary" && value != false {
					t.Fatal("vocabulary Boolean changed")
				}
			}
			if (Ruleset{}).RedactProfile(profile, got) != got {
				t.Fatal("consumer URI projection changed on repeat")
			}
		}
	}
}

func TestVocabularyProjectionTrustedSchemaCannotRepairAmbiguity(t *testing.T) {
	key := "https://ghp_" + strings.Repeat("A", 36) + ".example.invalid/vocabulary"
	value := map[string]any{key: false}
	schema := map[string]any{"$vocabulary": value}
	envelope := childFieldContext(t, RootFieldContext(), "inputSchema", schema, true, true)
	// A protocol consumer has independently validated this actual occurrence.
	trusted := enterFieldContext(t, envelope.Schema(true), schema, true)
	trusted = childFieldContext(t, trusted, "$vocabulary", value, true, true)
	got, admitted := trusted.ProjectVocabularyKeys(value, []string{key}, nil, func(int) bool { return true })
	if !admitted || len(got) != 1 {
		t.Fatal("trusted schema occurrence could not cross protocol envelope")
	}
	ambiguous := enterFieldContext(t, envelope.Schema(false).Schema(true), schema, true)
	ambiguous = childFieldContext(t, ambiguous, "$vocabulary", value, true, true)
	got, admitted = ambiguous.ProjectVocabularyKeys(value, []string{key}, nil, func(int) bool { return true })
	if !admitted || got != nil {
		t.Fatal("explicit schema reset repaired prior ambiguity")
	}
}

func TestVocabularyProjectionPreservesProfileAndYAMLKeyInspection(t *testing.T) {
	input := `{"$vocabulary":{"https://example.invalid/person@corp.test":false,"https://example.invalid/person@example.invalid":true}}`
	if Redact(input) != input {
		t.Fatal("local URI projection changed a clean profile value")
	}
	got := (Ruleset{}).RedactProfile(PushMetadataProfile, input)
	if strings.Contains(got, "person@corp.test") || !strings.Contains(got, "person@example.invalid") || !strings.Contains(got, "urn:telara:redacted:vocabulary:") {
		t.Fatal("URI projection bypassed privacy or documentation exemption")
	}
	if (Ruleset{}).RedactProfile(PushMetadataProfile, got) != got {
		t.Fatal("profile URI projection changed on repeat")
	}
	block := "$vocabulary:\n  https://ghp_" + strings.Repeat("A", 36) + ".example.invalid/vocabulary: false # API_TOKEN=syntheticOpaqueValue42\n"
	got = Redact(block)
	if strings.Contains(got, strings.Repeat("A", 36)) || strings.Contains(got, "syntheticOpaqueValue42") || !strings.Contains(got, "urn:telara:redacted:vocabulary:") {
		t.Fatal("URI projection bypassed original YAML key or value comments")
	}
	if Redact(got) != got {
		t.Fatal("comment-bearing URI projection changed on repeat")
	}
	var document yaml.Node
	if err := yaml.Unmarshal([]byte("$vocabulary: &bound\n  urn:ordinary: false\n"), &document); err != nil {
		t.Fatal(err)
	}
	anchored := document.Content[0].Content[1]
	projection, admitted := vocabularyContext(t, anchored).ProjectVocabularyKeys(anchored, []string{"urn:ordinary"}, nil, func(int) bool { return true })
	if !admitted || projection != nil {
		t.Fatal("anchored YAML acquired URI projection")
	}
}

func TestVocabularyProjectionUsesExactDecodedKeywordAuthority(t *testing.T) {
	key := "https://ghp_" + strings.Repeat("A", 36) + ".example.invalid/vocabulary"
	value := map[string]any{key: false}
	for _, kind := range []string{"$vocabu\u200blary", "all\u200bOf", "prope\u200brties"} {
		var context FieldContext
		switch kind {
		case "$vocabu\u200blary":
			context = childFieldContext(t, RootFieldContext().Schema(true), kind, value, true, true)
		case "all\u200bOf":
			element := map[string]any{"$vocabulary": value}
			children := []any{element}
			context = childFieldContext(t, RootFieldContext().Schema(true), kind, children, true, true)
			context = enterFieldContext(t, context, children, true)
			context = enterFieldContext(t, context, element, true)
			context = childFieldContext(t, context, "$vocabulary", value, true, true)
		default:
			schema := map[string]any{"$vocabulary": value}
			names := map[string]any{"ordinary": schema}
			context = childFieldContext(t, RootFieldContext().Schema(true), kind, names, true, true)
			context = enterFieldContext(t, context, names, true)
			context = childFieldContext(t, context, "ordinary", schema, true, true)
			context = enterFieldContext(t, context, schema, true)
			context = childFieldContext(t, context, "$vocabulary", value, true, true)
		}
		got, admitted := context.ProjectVocabularyKeys(value, []string{key}, nil, func(int) bool { return true })
		if !admitted || got != nil {
			t.Fatal("normalized unknown keyword or ancestor gained new URI authority")
		}
	}
	// Arbitrary declaration names are legitimate names, not schema keyword edges.
	schema := map[string]any{"$vocabulary": value}
	names := map[string]any{"ordi\u200bnary": schema}
	context := childFieldContext(t, RootFieldContext().Schema(true), "properties", names, true, true)
	context = enterFieldContext(t, context, names, true)
	context = childFieldContext(t, context, "ordi\u200bnary", schema, true, true)
	context = enterFieldContext(t, context, schema, true)
	context = childFieldContext(t, context, "$vocabulary", value, true, true)
	got, admitted := context.ProjectVocabularyKeys(value, []string{key}, nil, func(int) bool { return true })
	if !admitted || len(got) != 1 {
		t.Fatal("arbitrary declaration name lost typed URI authority")
	}
	for _, body := range []string{
		`{"$vocabu\u200blary":{"` + key + `":false}}`,
		`{"all\u200bOf":[{"$vocabulary":{"` + key + `":false}}]}`,
		`{"prope\u200brties":{"ordinary":{"$vocabulary":{"` + key + `":false}}}}`,
	} {
		got := Redact(body)
		if strings.Contains(got, strings.Repeat("A", 36)) || strings.Contains(got, "urn:telara:redacted:vocabulary:") {
			t.Fatal("raw unknown keyword bypassed floor or gained URI authority")
		}
	}
	// JSON escapes are decoded source spelling, unlike an extra invisible rune.
	escaped := `{"\u0024vocabulary":{"` + key + `":false}}`
	output := Redact(escaped)
	if strings.Contains(output, strings.Repeat("A", 36)) || !strings.Contains(output, "urn:telara:redacted:vocabulary:") || Redact(output) != output {
		t.Fatal("genuine decoded keyword escape lost stable URI projection")
	}
}

func TestVocabularyProjectionReservesNormalizedUnchangedKeys(t *testing.T) {
	key := "https://ghp_" + strings.Repeat("A", 36) + ".example.invalid/vocabulary"
	unchanged := "urn:telara:red\u200bacted:vocabulary:1"
	value := map[string]any{key: false, unchanged: true}
	keys := []string{key, unchanged}
	got, admitted := vocabularyContext(t, value).ProjectVocabularyKeys(value, keys, nil, func(int) bool { return true })
	want := []string{"urn:telara:redacted:vocabulary:1:1", unchanged}
	if !admitted || !reflect.DeepEqual(got, want) {
		t.Fatal("generated URI collided with normalized unchanged original")
	}
	next := map[string]any{got[0]: false, got[1]: true}
	again, admitted := vocabularyContext(t, next).ProjectVocabularyKeys(next, got, nil, func(int) bool { return true })
	if !admitted || !reflect.DeepEqual(got, again) || Normalize(got[0]) == Normalize(got[1]) {
		t.Fatal("normalized output uniqueness or public repeat projection changed")
	}
	duplicate := map[string]any{"urn:telara:redacted:vocabulary:1": false, unchanged: true}
	projection, admitted := vocabularyContext(t, duplicate).ProjectVocabularyKeys(duplicate, []string{"urn:telara:redacted:vocabulary:1", unchanged}, nil, func(int) bool { return true })
	if !admitted || projection != nil {
		t.Fatal("preexisting normalized duplicates acquired projection")
	}
	raw, err := json.Marshal(map[string]any{"$vocabulary": value})
	if err != nil {
		t.Fatal(err)
	}
	output := Redact(string(raw))
	var decoded map[string]any
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		t.Fatal(err)
	}
	projected := decoded["$vocabulary"].(map[string]any)
	if len(projected) != 2 || projected[want[0]] != false || projected[unchanged] != true || Redact(output) != output {
		t.Fatal("core changed normalized collision reservation, Boolean values or repeat output")
	}
}

func TestVocabularyProjectionPrechargesAllYAMLKeyBytes(t *testing.T) {
	key := "urn:" + strings.Repeat("a", 65536)
	value := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "false"},
	}}
	context := vocabularyContext(t, value)
	for _, ordered := range []string{key, "urn:mismatched"} {
		charges := []int{}
		inspections := 0
		// Decline the first ordered-key byte reservation, or decline the actual
		// long node-key reservation after admitting a shorter mismatched order key.
		decline := 2
		if ordered != key {
			decline = 3
		}
		got, admitted := context.projectVocabularyKeys(value, []string{ordered}, nil, func(text string) string { inspections++; return text }, func(cost int) bool { charges = append(charges, cost); return len(charges) < decline })
		if admitted || got != nil || inspections != 0 || len(charges) != decline || charges[0] != 13 || charges[1] != 1+4*len(ordered) {
			t.Fatal("YAML projection inspected key contents before declined key-byte reservation")
		}
		if decline == 3 && charges[2] != 1+4*len(key) {
			t.Fatal("actual YAML key bytes were not reserved before equality or hashing")
		}
	}
	oversized := strings.Repeat("a", maxFieldContextValidationWork)
	got, admitted := context.ProjectVocabularyKeys(value, []string{oversized}, nil, func(int) bool { return true })
	if admitted || got != nil {
		t.Fatal("unbounded key-byte arithmetic admitted projection")
	}
}
