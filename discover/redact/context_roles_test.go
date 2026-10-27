package redact

import (
	"strings"
	"testing"
)

func TestFieldContextExplicitSchemaAnnotations(t *testing.T) {
	for _, keyword := range schemaValueKeywords {
		t.Run(keyword, func(t *testing.T) {
			literal := map[string]any{"properties": map[string]any{
				"password": map[string]any{"description": "syntheticOpaqueValue42"},
			}}
			var annotation any = literal
			if keyword == "enum" || keyword == "examples" {
				annotation = []any{literal}
			}
			schema := map[string]any{keyword: annotation}
			rootValue := map[string]any{"ordinary": schema}
			root := enterFieldContext(t, RootFieldContext(), rootValue, true)
			ordinary := childFieldContext(t, root, "ordinary", schema, true, true)
			ordinary = enterFieldContext(t, ordinary, schema, true)
			if ordinary.state.schema {
				t.Fatal("an ordinary annotation-named field established a schema role")
			}

			context := ordinary.Schema(true)
			context = childFieldContext(t, context, keyword, annotation, true, true)
			context = enterFieldContext(t, context, annotation, true)
			if keyword == "enum" || keyword == "examples" {
				context = enterFieldContext(t, context, literal, true)
			}
			properties := literal["properties"]
			context = childFieldContext(t, context, "properties", properties, true, true)
			context = enterFieldContext(t, context, properties, true)
			password := properties.(map[string]any)["password"]
			context = childFieldContext(t, context, "password", password, true, true)
			context = enterFieldContext(t, context, password, true)
			context = childFieldContext(t, context, "description", "syntheticOpaqueValue42", true, true)
			if got := context.Scalar("syntheticOpaqueValue42", `"syntheticOpaqueValue42"`); strings.Contains(got, "syntheticOpaqueValue42") {
				t.Fatal("an explicit schema annotation regained metadata exemptions")
			}
		})
	}
}

func TestFieldContextSchemaCannotOverrideAncestry(t *testing.T) {
	object := map[string]any{"ordinary": "syntheticOpaqueValue42"}
	root := enterFieldContext(t, RootFieldContext(), map[string]any{}, true)
	credential := childFieldContext(t, root, "password", object, true, true)
	annotation := childFieldContext(t, root.Schema(true), "default", object, true, true)
	metadata := childFieldContext(t, root.Schema(true), "description", "ordinary", true, true)
	names := childFieldContext(t, root.Schema(true), "properties", object, true, true)
	ambiguous := enterFieldContext(t, root, object, false)
	for name, context := range map[string]FieldContext{
		"credential": credential, "annotation": annotation, "metadata": metadata,
		"declaration names": names, "ambiguous": ambiguous,
	} {
		t.Run(name, func(t *testing.T) {
			if context.Schema(true) != context {
				t.Fatal("an explicit schema role overrode protected ancestry")
			}
		})
	}
}

func TestFieldContextCredentialValueIsNotAnExemption(t *testing.T) {
	root := enterFieldContext(t, RootFieldContext(), map[string]any{}, true)
	direct := childFieldContext(t, root, "password", nil, true, true)
	if !direct.CredentialValue() || direct.Scalar(nil, "null") == "null" {
		t.Fatal("credential classification changed the strict null scalar policy")
	}
	names := childFieldContext(t, root.Schema(true), "properties", map[string]any{}, true, true)
	declaration := childFieldContext(t, names, "password", "", true, true)
	data := childFieldContext(t, root.Schema(true), "default", map[string]any{}, true, true)
	data = childFieldContext(t, data, "password", nil, true, true)
	for name, context := range map[string]FieldContext{"schema slot": declaration, "annotation data": data, "ordinary": root} {
		t.Run(name, func(t *testing.T) {
			if context.CredentialValue() {
				t.Fatal("a non-instance context acquired direct credential classification")
			}
		})
	}
}

func TestFieldContextDependencyDeclarationSensitivity(t *testing.T) {
	for _, keyword := range []string{"properties", "patternProperties", "definitions", "$defs", "dependentSchemas"} {
		t.Run(keyword, func(t *testing.T) {
			ordinary := map[string]any{"type": "boolean", "default": false}
			credential := map[string]any{"default": "syntheticOpaqueValue42"}
			names := map[string]any{"ordinary": ordinary, "password": credential}
			dependency := map[string]any{keyword: names, "enum": []any{"syntheticOpaqueValue42"}}
			dependencies := map[string]any{"password": dependency}
			value := map[string]any{"type": "object", "dependencies": dependencies}
			root := enterFieldContext(t, RootFieldContext(), value, true)
			depMap := childFieldContext(t, root, "dependencies", dependencies, true, true)
			depMap = enterFieldContext(t, depMap, dependencies, true)
			dep := childFieldContext(t, depMap, "password", dependency, true, true)
			dep = enterFieldContext(t, dep, dependency, true)

			enum := childFieldContext(t, dep, "enum", dependency["enum"], true, true)
			if got := enum.Scalar("syntheticOpaqueValue42", `"syntheticOpaqueValue42"`); strings.Contains(got, "syntheticOpaqueValue42") {
				t.Fatal("a direct credential dependency annotation was exempted")
			}

			declarations := childFieldContext(t, dep, keyword, names, true, true)
			declarations = enterFieldContext(t, declarations, names, true)
			plain := childFieldContext(t, declarations, "ordinary", ordinary, true, true)
			plain = enterFieldContext(t, plain, ordinary, true)
			literal := childFieldContext(t, plain, "default", false, true, true)
			if got := literal.Scalar(false, "false"); got != "false" {
				t.Fatal("a dependency name tainted an ordinary nested declaration")
			}
			secret := childFieldContext(t, declarations, "password", credential, true, true)
			secret = enterFieldContext(t, secret, credential, true)
			secret = childFieldContext(t, secret, "default", "syntheticOpaqueValue42", true, true)
			if got := secret.Scalar("syntheticOpaqueValue42", `"syntheticOpaqueValue42"`); strings.Contains(got, "syntheticOpaqueValue42") {
				t.Fatal("a nested credential declaration lost its own sensitivity")
			}
			for name, context := range map[string]FieldContext{
				"ambiguous dependency": enterFieldContext(t, childFieldContext(t, depMap, "password", dependency, true, false), dependency, false),
				"credential ancestor":  enterFieldContext(t, childFieldContext(t, root, "api_key", dependency, true, true), dependency, true),
				"annotation data":      enterFieldContext(t, childFieldContext(t, dep, "default", dependency, true, true), dependency, true),
			} {
				t.Run(name, func(t *testing.T) {
					declarations := childFieldContext(t, context, keyword, names, true, true)
					declarations = enterFieldContext(t, declarations, names, true)
					plain := childFieldContext(t, declarations, "ordinary", ordinary, true, true)
					plain = enterFieldContext(t, plain, ordinary, true)
					literal := childFieldContext(t, plain, "default", "syntheticOpaqueValue42", true, true)
					if got := literal.Scalar("syntheticOpaqueValue42", `"syntheticOpaqueValue42"`); strings.Contains(got, "syntheticOpaqueValue42") {
						t.Fatal("a declaration map cleared protected credential ancestry")
					}
				})
			}
		})
	}
}

func TestFieldContextDependencyAmbiguityCannotRegainAuthority(t *testing.T) {
	for _, tc := range []struct {
		name                                            string
		rootUnique, namesUnique, keyUnique, valueUnique bool
		childUnique, enteredUnique, combinatorKeyUnique bool
	}{
		{"duplicate dependency name", false, false, false, true, true, true, true},
		{"ambiguous dependency subtree", false, false, true, false, true, true, true},
		{"ambiguous downstream occurrence", true, true, true, true, false, false, true},
		{"ambiguous downstream container", true, true, true, true, true, false, true},
		{"duplicate downstream combinator", true, true, true, true, true, true, false},
	} {
		for _, combinator := range []string{"allOf", "anyOf", "oneOf"} {
			t.Run(tc.name+"/"+combinator, func(t *testing.T) {
				ordinary := map[string]any{"default": "syntheticOpaqueValue42"}
				properties := map[string]any{"ordinary": ordinary}
				element := map[string]any{"properties": properties}
				children := []any{element}
				dependency := map[string]any{combinator: children}
				dependencies := map[string]any{"password": dependency}
				rootValue := map[string]any{"dependencies": dependencies}
				context := enterFieldContext(t, RootFieldContext(), rootValue, tc.rootUnique)
				context = childFieldContext(t, context, "dependencies", dependencies, true, tc.namesUnique)
				context = enterFieldContext(t, context, dependencies, tc.namesUnique)
				context = childFieldContext(t, context, "password", dependency, tc.keyUnique, tc.valueUnique)
				context = enterFieldContext(t, context, dependency, tc.valueUnique)
				context = childFieldContext(t, context, combinator, children, tc.combinatorKeyUnique, tc.childUnique)
				context = enterFieldContext(t, context, children, tc.enteredUnique)
				context = enterFieldContext(t, context, element, true)
				context = childFieldContext(t, context, "properties", properties, true, true)
				context = enterFieldContext(t, context, properties, true)
				context = childFieldContext(t, context, "ordinary", ordinary, true, true)
				context = enterFieldContext(t, context, ordinary, true)
				context = childFieldContext(t, context, "default", "syntheticOpaqueValue42", true, true)
				if got := context.Scalar("syntheticOpaqueValue42", `"syntheticOpaqueValue42"`); strings.Contains(got, "syntheticOpaqueValue42") {
					t.Fatal("a unique combinator restored authority after source ambiguity")
				}
			})
		}
	}
	for _, combinator := range []string{"allOf", "anyOf", "oneOf"} {
		t.Run("malformed "+combinator, func(t *testing.T) {
			ordinary := map[string]any{"default": "syntheticOpaqueValue42"}
			properties := map[string]any{"ordinary": ordinary}
			malformed := map[string]any{"properties": properties}
			dependency := map[string]any{combinator: malformed}
			dependencies := map[string]any{"password": dependency}
			context := enterFieldContext(t, RootFieldContext(), map[string]any{"dependencies": dependencies}, true)
			context = childFieldContext(t, context, "dependencies", dependencies, true, true)
			context = enterFieldContext(t, context, dependencies, true)
			context = childFieldContext(t, context, "password", dependency, true, true)
			context = enterFieldContext(t, context, dependency, true)
			context = childFieldContext(t, context, combinator, malformed, true, true)
			context = enterFieldContext(t, context, malformed, true)
			context = childFieldContext(t, context, "properties", properties, true, true)
			context = enterFieldContext(t, context, properties, true)
			context = childFieldContext(t, context, "ordinary", ordinary, true, true)
			context = enterFieldContext(t, context, ordinary, true)
			context = childFieldContext(t, context, "default", "syntheticOpaqueValue42", true, true)
			if got := context.Scalar("syntheticOpaqueValue42", `"syntheticOpaqueValue42"`); strings.Contains(got, "syntheticOpaqueValue42") {
				t.Fatal("a malformed combinator granted declaration authority")
			}
		})
	}
}
