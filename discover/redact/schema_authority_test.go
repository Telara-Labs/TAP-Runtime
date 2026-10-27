package redact

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSchemaChildAuthorityPreservesCleanCompositions(t *testing.T) {
	for _, key := range schemaChildKeywords {
		t.Run(key, func(t *testing.T) {
			ordinary := map[string]any{"type": "boolean", "default": false}
			properties := map[string]any{"ordinary": ordinary}
			element := map[string]any{"properties": properties}
			var child any = element
			if key == "allOf" || key == "anyOf" || key == "oneOf" || key == "prefixItems" || key == "items" {
				child = []any{false, element}
			}
			dependency := map[string]any{key: child}
			dependencies := map[string]any{"password": dependency}
			root := map[string]any{"dependencies": dependencies}
			context := enterFieldContext(t, RootFieldContext(), root, true)
			context = childFieldContext(t, context, "dependencies", dependencies, true, true)
			context = enterFieldContext(t, context, dependencies, true)
			context = childFieldContext(t, context, "password", dependency, true, true)
			context = enterFieldContext(t, context, dependency, true)
			context = childFieldContext(t, context, key, child, true, true)
			context = enterFieldContext(t, context, child, true)
			if isSequenceContainer(child) {
				context = enterFieldContext(t, context, element, true)
			}
			context = childFieldContext(t, context, "properties", properties, true, true)
			context = enterFieldContext(t, context, properties, true)
			context = childFieldContext(t, context, "ordinary", ordinary, true, true)
			context = enterFieldContext(t, context, ordinary, true)
			context = childFieldContext(t, context, "default", false, true, true)
			if got := context.Scalar(false, "false"); got != "false" {
				t.Fatal("a validated schema child tainted an ordinary declaration")
			}
			wire, err := json.Marshal(root)
			if err != nil {
				t.Fatal(err)
			}
			if got := Redact(string(wire)); got != string(wire) {
				t.Fatal("the JSON text consumer disagreed with incremental classification")
			}
			// Keep a block YAML root with a JSON-flow dependency value. A plain
			// long keyword directly after password on the following line is an
			// unconditional frozen assignment-floor span, tested separately below.
			flow, err := json.Marshal(dependency)
			if err != nil {
				t.Fatal(err)
			}
			block := []byte("dependencies:\n  password: " + string(flow) + "\n")
			var decoded map[string]any
			if err := yaml.Unmarshal(block, &decoded); err != nil {
				t.Fatal(err)
			}
			decodedWire, err := json.Marshal(decoded)
			if err != nil || string(decodedWire) != string(wire) {
				t.Fatal("the YAML fixture changed the typed schema structure")
			}
			if got := Redact(string(block)); got != string(block) {
				t.Fatal("the YAML text consumer disagreed with incremental classification")
			}
			properties["password"] = map[string]any{"default": "syntheticOpaqueValue42"}
			wire, err = json.Marshal(root)
			if err != nil {
				t.Fatal(err)
			}
			got := Redact(string(wire))
			if strings.Contains(got, "syntheticOpaqueValue42") || !strings.Contains(got, `"default":false`) {
				t.Fatal("validated composition cleared a genuine credential or changed ordinary data")
			}
		})
	}
}

func TestSchemaChildAuthorityRejectsMalformedShapes(t *testing.T) {
	good := map[string]any{"properties": map[string]any{"ordinary": map[string]any{"default": false}}}
	for name, value := range map[string]any{
		"object instead of array": good,
		"empty array":             []any{},
		"null":                    nil,
		"scalar":                  "ordinary",
		"scalar array member":     []any{"ordinary", good},
		"null array member":       []any{nil, good},
		"malformed properties":    []any{map[string]any{"properties": []any{good}}},
		"malformed type sibling":  []any{map[string]any{"type": false, "properties": good["properties"]}},
		"malformed required":      []any{map[string]any{"required": "ordinary", "properties": good["properties"]}},
		"malformed nested child":  []any{map[string]any{"anyOf": good}},
		"normalized vocabulary duplicate": []any{map[string]any{
			"type": "object", "ty\u200bpe": "object", "properties": good["properties"],
		}},
	} {
		t.Run(name, func(t *testing.T) {
			valid, admitted := validatedSchemaChild("allOf", value, func(int) bool { return true })
			if !admitted || valid {
				t.Fatal("a malformed composition acquired schema declaration authority")
			}
		})
	}
}

func TestSchemaChildAuthorityRefusesBeforeUnreservedValidation(t *testing.T) {
	value := []any{map[string]any{"properties": map[string]any{"ordinary": map[string]any{"default": false}}}}
	work := 0
	valid, admitted := validatedSchemaChild("allOf", value, func(cost int) bool { work += cost; return true })
	if !valid || !admitted || work <= 1 {
		t.Fatal("a valid composition had no measurable validation work")
	}
	for _, limit := range []int{0, 1, work - 1} {
		left := limit
		valid, admitted := validatedSchemaChild("allOf", value, func(cost int) bool {
			if cost < 0 || cost > left {
				return false
			}
			left -= cost
			return true
		})
		if admitted || valid {
			t.Fatal("a refused validation budget returned partial authority")
		}
	}
	dependency := map[string]any{"allOf": value}
	dependencies := map[string]any{"password": dependency}
	context := enterFieldContext(t, RootFieldContext(), map[string]any{"dependencies": dependencies}, true)
	context = childFieldContext(t, context, "dependencies", dependencies, true, true)
	context = enterFieldContext(t, context, dependencies, true)
	context = childFieldContext(t, context, "password", dependency, true, true)
	context = enterFieldContext(t, context, dependency, true)
	left := 1 // Admit Child's ordinary entry cost, then refuse validation.
	_, admitted = context.Child("allOf", FieldOccurrence{Value: value, KeyUnique: true, ValueUnique: true}, func(cost int) bool {
		if cost < 0 || cost > left {
			return false
		}
		left -= cost
		return true
	})
	if admitted {
		t.Fatal("FieldContext returned a context after validation refusal")
	}
}

func TestSchemaChildAuthorityReusesOnlyKnownSubtreeProof(t *testing.T) {
	properties := map[string]any{"ordinary": map[string]any{"default": false}}
	nested := []any{map[string]any{"properties": properties}}
	element := map[string]any{"anyOf": nested}
	outer := []any{element}
	dependency := map[string]any{"allOf": outer}
	dependencies := map[string]any{"password": dependency}
	context := enterFieldContext(t, RootFieldContext(), map[string]any{"dependencies": dependencies}, true)
	context = childFieldContext(t, context, "dependencies", dependencies, true, true)
	context = enterFieldContext(t, context, dependencies, true)
	context = childFieldContext(t, context, "password", dependency, true, true)
	context = enterFieldContext(t, context, dependency, true)
	context = childFieldContext(t, context, "allOf", outer, true, true)
	context = enterFieldContext(t, context, outer, true)
	context = enterFieldContext(t, context, element, true)
	left := 1 // Only Child's ordinary cost; the whole nested shape is proven.
	_, admitted := context.Child("anyOf", FieldOccurrence{Value: nested, KeyUnique: true, ValueUnique: true}, func(cost int) bool {
		if cost < 0 || cost > left {
			return false
		}
		left -= cost
		return true
	})
	if !admitted || left != 0 {
		t.Fatal("a known schema edge redundantly revalidated its proven subtree")
	}
	ambiguous := enterFieldContext(t, context, element, false)
	if ambiguous.state.schemaStructureValidated {
		t.Fatal("an ambiguous container retained a subtree proof")
	}
}

func TestSchemaChildAuthorityNestedDependenciesHaveLinearWork(t *testing.T) {
	for _, depth := range []int{1, 4, 16, 64, 128} {
		leaf := map[string]any{"type": "boolean", "default": false}
		root := leaf
		for level := 0; level < depth; level++ {
			root = map[string]any{"dependencies": map[string]any{"password": map[string]any{
				"allOf": []any{map[string]any{"properties": map[string]any{"ordinary": root}}},
			}}}
		}
		work := 0
		charge := func(cost int) bool { work += cost; return true }
		context, admitted := RootFieldContext().Enter(root, true, charge)
		if !admitted {
			t.Fatal("fresh root refused")
		}
		value := root
		for level := 0; level < depth; level++ {
			dependencies := value["dependencies"].(map[string]any)
			dependency := dependencies["password"].(map[string]any)
			children := dependency["allOf"].([]any)
			element := children[0].(map[string]any)
			properties := element["properties"].(map[string]any)
			value = properties["ordinary"].(map[string]any)
			for _, step := range []struct {
				key   string
				value any
			}{{"dependencies", dependencies}, {"password", dependency}, {"allOf", children}, {"properties", properties}, {"ordinary", value}} {
				context, admitted = context.Child(step.key, FieldOccurrence{Value: step.value, KeyUnique: true, ValueUnique: true}, charge)
				if !admitted {
					t.Fatal("known source child refused")
				}
				context, admitted = context.Enter(step.value, true, charge)
				if !admitted {
					t.Fatal("known source container refused")
				}
				if step.key == "allOf" {
					context, admitted = context.Enter(element, true, charge)
					if !admitted {
						t.Fatal("known array element refused")
					}
				}
			}
		}
		context, admitted = context.Child("default", FieldOccurrence{Value: false, KeyUnique: true, ValueUnique: true}, charge)
		if !admitted || context.Scalar(false, "false") != "false" {
			t.Fatal("deep valid ordinary default changed")
		}
		// This absolute linear envelope rejects repeated full-suffix scans.
		// It includes known-schema validation and ordinary Child/Enter work.
		if work > 256*depth+256 {
			t.Fatalf("depth %d used %d work, beyond the linear envelope", depth, work)
		}
	}
}

func TestSchemaChildAuthorityReplacementStartsFreshContext(t *testing.T) {
	// A replacement document requires a new context. Reusing the prior
	// context would violate the documented caller-supplied occurrence contract.
	previous := []any{map[string]any{"type": "boolean"}}
	previousDependency := map[string]any{"allOf": previous}
	previousNames := map[string]any{"password": previousDependency}
	previousContext := enterFieldContext(t, RootFieldContext(), map[string]any{"dependencies": previousNames}, true)
	previousContext = childFieldContext(t, previousContext, "dependencies", previousNames, true, true)
	previousContext = enterFieldContext(t, previousContext, previousNames, true)
	previousContext = childFieldContext(t, previousContext, "password", previousDependency, true, true)
	previousContext = enterFieldContext(t, previousContext, previousDependency, true)
	previousContext = childFieldContext(t, previousContext, "allOf", previous, true, true)
	if !previousContext.state.schemaStructureValidated {
		t.Fatal("the prior valid document did not establish a traversal fact")
	}
	malformed := []any{map[string]any{"type": false, "properties": map[string]any{
		"ordinary": map[string]any{"default": "syntheticOpaqueValue42"},
	}}}
	dependency := map[string]any{"allOf": malformed}
	dependencies := map[string]any{"password": dependency}
	context := enterFieldContext(t, RootFieldContext(), map[string]any{"dependencies": dependencies}, true)
	context = childFieldContext(t, context, "dependencies", dependencies, true, true)
	context = enterFieldContext(t, context, dependencies, true)
	context = childFieldContext(t, context, "password", dependency, true, true)
	context = enterFieldContext(t, context, dependency, true)
	context = childFieldContext(t, context, "allOf", malformed, true, true)
	if context.state.schemaStructureValidated || context.state.dependencyNameSensitive {
		t.Fatal("a fresh malformed replacement acquired structural authority")
	}
}

func TestSchemaChildAuthorityCannotCancelFrozenBlockFloor(t *testing.T) {
	for _, key := range []string{"prefixItems", "additionalItems", "additionalProperties", "unevaluatedProperties", "unevaluatedItems", "contentSchema", "contains", "propertyNames"} {
		t.Run(key, func(t *testing.T) {
			child := any(map[string]any{"properties": map[string]any{"ordinary": map[string]any{"type": "boolean", "default": false}}})
			if key == "prefixItems" {
				child = []any{false, child}
			}
			// Preserve the original full yaml.Marshal fixture from the failing
			// composition test, including the long key and ordinary annotation.
			root := map[string]any{"dependencies": map[string]any{"password": map[string]any{key: child}}}
			encoded, err := yaml.Marshal(root)
			if err != nil {
				t.Fatal(err)
			}
			block := string(encoded)
			spans := frozenSourceSpans(block)
			t.Logf("original block YAML %q frozen source spans %+v", block, spans)
			if len(spans) == 0 {
				t.Fatal("fixture did not exercise the unconditional frozen source floor")
			}
			got := Redact(block)
			if got == block {
				t.Fatal("schema authority canceled a frozen credential span")
			}
			for _, span := range spans {
				if strings.Contains(got, block[span.start:span.end]) {
					t.Fatal("a frozen source value survived schema authority")
				}
			}
		})
	}
}
