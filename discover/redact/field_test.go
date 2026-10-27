package redact

import (
	"encoding/json"
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

func TestFieldPreservesSchemaDependencyNamesWithSourcePath(t *testing.T) {
	for _, path := range [][]string{
		{"dependencies", "password"},
		{"properties", "password", "dependencies", "password"},
		{"properties", "password", "dependentRequired", "password"},
	} {
		for _, names := range []string{`["username","API_TOKEN"]`} {
			if got := Field(path, names); got != names {
				t.Errorf("schema dependency names changed for path %q: got %s", path, got)
			}
		}
	}
	provider := `[` + `"ghp_` + strings.Repeat("A", 36) + `"]`
	if got := Field([]string{"dependencies", "password"}, provider); strings.Contains(got, strings.Repeat("A", 36)) {
		t.Fatalf("provider-shaped credential escaped inside dependency array: %s", got)
	}
	invalid := `[{"password":"syntheticOpaqueValue42"}]`
	if got := Field([]string{"dependencies", "password"}, invalid); strings.Contains(got, "syntheticOpaqueValue42") {
		t.Fatalf("malformed dependency array hid a credential value: %s", got)
	}
}

func TestFieldPreservesClosedPasswordControls(t *testing.T) {
	for _, path := range [][]string{{"password"}, {"api_key"}} {
		if got := Field(path, "false"); got == "false" {
			t.Errorf("direct credential boolean escaped for path %q", path)
		}
	}
	if got := Field(nil, `{"password":false}`); strings.Contains(got, `false`) {
		t.Fatalf("direct password boolean escaped in a record: %s", got)
	}
	clean := `{"note":"ghp_` + strings.Repeat("A", 36) + `","password":{"enabled":false,"policy":"strict"}}`
	got := Field(nil, clean)
	if strings.Contains(got, strings.Repeat("A", 36)) || !strings.Contains(got, `"enabled":false`) || !strings.Contains(got, `"policy":"strict"`) {
		t.Fatalf("closed password controls were not preserved safely: %s", got)
	}
	for _, raw := range []string{
		`{"password":{"policy":"strict-policy-metadata"}}`,
		`{"password":{"policy":42}}`,
		`{"password":{"policy":{"enabled":false}}}`,
		`{"password":{"enabled":"false"}}`,
		`{"password":{"enabled":7}}`,
		`{"password":{"unknown":"ordinary-long-nonsecret-metadata"}}`,
	} {
		if got := Field(nil, raw); strings.Contains(got, "strict-policy-metadata") || strings.Contains(got, "ordinary-long-nonsecret-metadata") || strings.Contains(got, `"enabled":"false"`) {
			t.Errorf("open-ended password metadata escaped redaction: input %s output %s", raw, got)
		}
	}
}

func TestFieldValuePreservesOnlyExactTypedPasswordControls(t *testing.T) {
	for _, test := range []struct {
		name, pathTail, source string
		value                  any
		want                   string
	}{
		{"enabled false boolean", "enabled", "false", false, "false"},
		{"enabled true boolean", "enabled", "true", true, "true"},
		{"enabled null", "enabled", "null", nil, "null"},
		{"enabled padded false token", "enabled", " false ", false, `"<redacted credential>"`},
		{"enabled padded null token", "enabled", " null ", nil, `"<redacted credential>"`},
		{"enabled false string", "enabled", `"false"`, "false", `"<redacted credential>"`},
		{"enabled null string", "enabled", `"null"`, "null", `"<redacted credential>"`},
		{"enabled zero number", "enabled", "0", json.Number("0"), `"<redacted credential>"`},
		{"enabled zero string", "enabled", `"0"`, "0", `"<redacted credential>"`},
		{"exact strict", "policy", `"strict"`, "strict", `"strict"`},
		{"encoded strict", "policy", `"\u0073trict"`, "strict", `"<redacted credential>"`},
		{"opaque policy", "policy", `"strict-policy"`, "strict-policy", `"<redacted credential>"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := []string{"result", "password", test.pathTail}
			pathValues := []any{map[string]any{}, map[string]any{}, test.value}
			if got := fieldValue(path, pathValues, test.value, test.source); got != test.want {
				t.Fatalf("fieldValue(%q, %T, %q)=%q, want %q", path, test.value, test.source, got, test.want)
			}
		})
	}
	if got := fieldValue([]string{"api_key", "password", "enabled"}, []any{map[string]any{}, map[string]any{}, false}, false, "false"); got == "false" {
		t.Fatal("a typed boolean control under api_key was exempted")
	}
	if got := fieldValue([]string{"result", "password", "enabled"}, []any{map[string]any{}, []any{map[string]any{"enabled": false}}, false}, false, "false"); got == "false" {
		t.Fatal("a password array element acquired the object control exemption")
	}
	if got := fieldValue([]string{"properties", "password"}, []any{map[string]any{}, "false"}, "false", `"false"`); got != `"<redacted credential>"` {
		t.Fatalf("a JSON string false was treated as a boolean subschema: %q", got)
	}
	if got := fieldValue([]string{"properties", "password"}, []any{map[string]any{}, false}, false, "false"); got != "false" {
		t.Fatalf("a boolean subschema did not preserve its original token: %q", got)
	}
	if got := fieldValue([]string{"result", "ordinary"}, []any{map[string]any{}, "plain"}, "plain", `"plain"`); got != `"plain"` {
		t.Fatalf("an unchanged string token was not preserved: %q", got)
	}
	if got := fieldValue([]string{"result", "password", "enabled"}, []any{map[string]any{}, map[string]any{}, false}, false, `"false"`); got != `"<redacted credential>"` {
		t.Fatalf("mismatched typed value and source token were not rejected safely: %q", got)
	}
	if got := fieldValue([]string{"result", "ordinary"}, []any{map[string]any{}, map[string]any{}}, json.Number("5"), "123"); got != `"<redacted credential>"` {
		t.Fatalf("mismatched numeric value and source token were not rejected safely: %q", got)
	}
	for _, path := range [][]string{
		{"properties", "password", "default", "password", "enabled"},
		{"properties", "password", "enum", "password", "enabled"},
	} {
		values := make([]any, len(path))
		for i := range values {
			values[i] = map[string]any{}
		}
		values[len(values)-1] = false
		if got := fieldValue(path, values, false, "false"); got == "false" {
			t.Fatalf("typed control exemption crossed schema data at %q", path)
		}
	}
	malformed := []struct {
		name       string
		path       []string
		pathValues []any
	}{
		{"object", []string{"properties", "password", "description", "ordinary"}, []any{map[string]any{}, map[string]any{}, map[string]any{"ordinary": "syntheticOpaqueValue42"}, "syntheticOpaqueValue42"}},
		{"array", []string{"properties", "password", "description"}, []any{map[string]any{}, map[string]any{}, []any{"syntheticOpaqueValue42"}}},
	}
	for _, test := range malformed {
		t.Run("malformed description "+test.name, func(t *testing.T) {
			got := fieldValue(test.path, test.pathValues, "syntheticOpaqueValue42", `"syntheticOpaqueValue42"`)
			if strings.Contains(got, "syntheticOpaqueValue42") {
				t.Fatalf("malformed schema metadata cleared credential ancestry on %q: %q", test.path, got)
			}
		})
	}
}

func childFieldContext(t *testing.T, parent FieldContext, key string, value any, keyUnique, valueUnique bool) FieldContext {
	t.Helper()
	child, ok := parent.Child(key, FieldOccurrence{Value: value, KeyUnique: keyUnique, ValueUnique: valueUnique}, func(int) bool { return true })
	if !ok {
		t.Fatalf("context refused %q", key)
	}
	return child
}

func enterFieldContext(t *testing.T, context FieldContext, value any, valueUnique bool) FieldContext {
	t.Helper()
	entered, ok := context.Enter(value, valueUnique, func(int) bool { return true })
	if !ok {
		t.Fatal("context refused container")
	}
	return entered
}

func TestIncrementalFieldContextPreparesWholeObjects(t *testing.T) {
	for _, value := range []map[string]any{
		{"type": "object", "default": map[string]any{"password": map[string]any{"enabled": false}}},
		{"type": "object", "password": map[string]any{"enabled": false}},
	} {
		root := map[string]any{"result": value}
		context := enterFieldContext(t, RootFieldContext(), root, true)
		result := childFieldContext(t, context, "result", value, true, true)
		result = enterFieldContext(t, result, value, true)
		if defaultValue, ok := value["default"]; ok {
			result = childFieldContext(t, result, "default", defaultValue, true, true)
			result = enterFieldContext(t, result, defaultValue, true)
		}
		password := value["password"]
		result = childFieldContext(t, result, "password", password, true, true)
		result = enterFieldContext(t, result, password, true)
		control := childFieldContext(t, result, "enabled", false, true, true)
		if got := control.Scalar(false, "false"); got == "false" {
			t.Fatalf("sibling schema keyword granted a password control exemption for %#v", value)
		}
	}
}

func TestIncrementalFieldContextKeepsRootSchemaValueSemantics(t *testing.T) {
	const secret = "syntheticOpaqueValue42"
	root := map[string]any{"default": map[string]any{"properties": map[string]any{
		"password": map[string]any{"description": secret},
	}}}
	context := enterFieldContext(t, RootFieldContext(), root, true)
	defaultValue := root["default"]
	context = childFieldContext(t, context, "default", defaultValue, true, true)
	context = enterFieldContext(t, context, defaultValue, true)
	properties := defaultValue.(map[string]any)["properties"]
	context = childFieldContext(t, context, "properties", properties, true, true)
	context = enterFieldContext(t, context, properties, true)
	password := properties.(map[string]any)["password"]
	context = childFieldContext(t, context, "password", password, true, true)
	context = enterFieldContext(t, context, password, true)
	description := password.(map[string]any)["description"].(string)
	context = childFieldContext(t, context, "description", description, true, true)
	if got := context.Scalar(description, jsonStringToken(description)); strings.Contains(got, secret) {
		t.Fatalf("root schema annotation cleared credential ancestry: %q", got)
	}
}

func TestIncrementalFieldContextPrechargesArrayValidation(t *testing.T) {
	root := RootFieldContext()
	properties := childFieldContext(t, root, "properties", map[string]any{}, true, true)
	properties = enterFieldContext(t, properties, map[string]any{}, true)
	password := childFieldContext(t, properties, "password", map[string]any{}, true, true)
	password = enterFieldContext(t, password, map[string]any{}, true)
	required := []any{"one", "two", "three", "four"}
	_, ok := password.Child("required", FieldOccurrence{Value: required, KeyUnique: true, ValueUnique: true}, func(work int) bool {
		return work >= 5
	})
	if !ok {
		t.Fatal("valid required array was not fully precharged")
	}
	if _, ok := password.Child("required", FieldOccurrence{Value: required, KeyUnique: true, ValueUnique: true}, func(work int) bool {
		return work <= 2
	}); ok {
		t.Fatal("required array validation exceeded its precharge budget")
	}

	dependencyRoot := RootFieldContext()
	dependencyMap := map[string]any{"password": required}
	dependencyRoot = childFieldContext(t, dependencyRoot, "dependencies", dependencyMap, true, true)
	dependencyRoot = enterFieldContext(t, dependencyRoot, dependencyMap, true)
	dependency := childFieldContext(t, dependencyRoot, "password", required, true, true)
	_, ok = dependency.Enter(required, true, func(work int) bool { return work >= 5 })
	if !ok {
		t.Fatal("valid dependency array was not fully precharged")
	}
	if _, ok := dependency.Enter(required, true, func(work int) bool { return work <= 2 }); ok {
		t.Fatal("dependency array validation exceeded its precharge budget")
	}

	dependentRequired := map[string]any{"password": required}
	_, ok = password.Child("dependentRequired", FieldOccurrence{Value: dependentRequired, KeyUnique: true, ValueUnique: true}, func(work int) bool {
		return work >= 6
	})
	if !ok {
		t.Fatal("valid dependentRequired arrays were not fully precharged")
	}
	if _, ok := password.Child("dependentRequired", FieldOccurrence{Value: dependentRequired, KeyUnique: true, ValueUnique: true}, func(work int) bool {
		return work <= 2
	}); ok {
		t.Fatal("dependentRequired validation exceeded its precharge budget")
	}
}

func TestIncrementalFieldContextPreservesDiscoveryAndDependencies(t *testing.T) {
	resource := map[string]any{
		"resource":                 "https://example.invalid/mcp",
		"authorization_servers":    []any{"https://example.invalid"},
		"bearer_methods_supported": []any{"header"},
	}
	context := enterFieldContext(t, RootFieldContext(), resource, true)
	servers := resource["authorization_servers"]
	context = childFieldContext(t, context, "authorization_servers", servers, true, true)
	context = enterFieldContext(t, context, servers, true)
	if got := context.Scalar("https://example.invalid", `"https://example.invalid"`); got != `"https://example.invalid"` {
		t.Fatalf("root protected-resource discovery changed metadata: %q", got)
	}

	issuer := map[string]any{
		"issuer":                 "https://example.invalid",
		"authorization_endpoint": "https://example.invalid/authorize",
		"token_endpoint":         "https://example.invalid/token",
		"authorization_response_iss_parameter_supported": true,
	}
	context = enterFieldContext(t, RootFieldContext(), issuer, true)
	flag := issuer["authorization_response_iss_parameter_supported"]
	context = childFieldContext(t, context, "authorization_response_iss_parameter_supported", flag, true, true)
	if got := context.Scalar(true, "true"); got != "true" {
		t.Fatalf("root issuer discovery changed its validated boolean: %q", got)
	}

	dependencies := map[string]any{"dependencies": map[string]any{"password": []any{"username"}}}
	context = enterFieldContext(t, RootFieldContext(), dependencies, true)
	dependencyMap := dependencies["dependencies"]
	context = childFieldContext(t, context, "dependencies", dependencyMap, true, true)
	context = enterFieldContext(t, context, dependencyMap, true)
	names := []any{"username"}
	context = childFieldContext(t, context, "password", names, true, true)
	context = enterFieldContext(t, context, names, true)
	if got := context.Scalar("username", `"username"`); got != `"username"` {
		t.Fatalf("validated dependency name was changed: %q", got)
	}

	token := "ghp_" + strings.Repeat("A", 36)
	context = enterFieldContext(t, RootFieldContext(), dependencies, true)
	context = childFieldContext(t, context, "dependencies", dependencyMap, true, true)
	context = enterFieldContext(t, context, dependencyMap, true)
	tokens := []any{token}
	context = childFieldContext(t, context, "password", tokens, true, true)
	context = enterFieldContext(t, context, tokens, true)
	if got := context.Scalar(token, jsonStringToken(token)); strings.Contains(got, token) {
		t.Fatalf("provider-shaped credential escaped dependency names: %q", got)
	}
}

func TestIncrementalFieldContextDuplicateSiblingKeepsAnnotationData(t *testing.T) {
	rootValue := map[string]any{"result": map[string]any{
		"type": "object",
		"default": map[string]any{"properties": map[string]any{
			"password": map[string]any{"description": "syntheticOpaqueValue42"},
		}},
		"password": map[string]any{"enabled": false},
		"dup":      2, // Source occurrence analysis reports a second "dup" member.
	}}
	root := enterFieldContext(t, RootFieldContext(), rootValue, true)
	resultValue := rootValue["result"]
	result := childFieldContext(t, root, "result", resultValue, true, false)
	result = enterFieldContext(t, result, resultValue, false)
	resultRoot := result
	defaultValue := resultValue.(map[string]any)["default"]
	result = childFieldContext(t, result, "default", defaultValue, true, true)
	result = enterFieldContext(t, result, defaultValue, true)
	properties := defaultValue.(map[string]any)["properties"]
	result = childFieldContext(t, result, "properties", properties, true, true)
	result = enterFieldContext(t, result, properties, true)
	password := properties.(map[string]any)["password"]
	result = childFieldContext(t, result, "password", password, true, true)
	result = enterFieldContext(t, result, password, true)
	description := childFieldContext(t, result, "description", "syntheticOpaqueValue42", true, true)
	if got := description.Scalar("syntheticOpaqueValue42", `"syntheticOpaqueValue42"`); strings.Contains(got, "syntheticOpaqueValue42") {
		t.Fatalf("duplicate-bearing annotation subtree regained a schema metadata exemption: %q", got)
	}
	ordinary := childFieldContext(t, resultRoot, "ordinary", "safe value", false, true)
	if got := ordinary.Scalar("safe value", `"safe value"`); got != `"safe value"` {
		t.Fatalf("ordinary duplicate-bearing data was changed: %q", got)
	}
	control := resultRoot
	passwordValue := resultValue.(map[string]any)["password"]
	control = childFieldContext(t, control, "password", passwordValue, true, true)
	control = enterFieldContext(t, control, passwordValue, true)
	control = childFieldContext(t, control, "enabled", false, true, true)
	if got := control.Scalar(false, "false"); got != "false" {
		t.Fatalf("an unrelated duplicate sibling invalidated a unique password control: %q", got)
	}
	closedControls := map[string]any{"enabled": nil, "policy": "strict"}
	controlContext := childFieldContext(t, resultRoot, "password", closedControls, true, true)
	controlContext = enterFieldContext(t, controlContext, closedControls, true)
	controlContext = childFieldContext(t, controlContext, "enabled", nil, true, true)
	if got := controlContext.Scalar(nil, "null"); got != "null" {
		t.Fatalf("an unrelated duplicate sibling invalidated a unique enabled null control: %q", got)
	}
	controlContext = childFieldContext(t, enterFieldContext(t,
		childFieldContext(t, resultRoot, "password", closedControls, true, true), closedControls, true),
		"policy", "strict", true, true)
	if got := controlContext.Scalar("strict", `"strict"`); got != `"strict"` {
		t.Fatalf("an unrelated duplicate sibling invalidated a unique strict policy control: %q", got)
	}
	uniqueSchema := map[string]any{"password": map[string]any{"description": "syntheticOpaqueValue42"}}
	schema := childFieldContext(t, resultRoot, "properties", uniqueSchema, true, true)
	schema = enterFieldContext(t, schema, uniqueSchema, true)
	passwordSchema := uniqueSchema["password"]
	schema = childFieldContext(t, schema, "password", passwordSchema, true, true)
	schema = enterFieldContext(t, schema, passwordSchema, true)
	schemaDescription := childFieldContext(t, schema, "description", "syntheticOpaqueValue42", true, true)
	if got := schemaDescription.Scalar("syntheticOpaqueValue42", `"syntheticOpaqueValue42"`); got != `"syntheticOpaqueValue42"` {
		t.Fatalf("an unrelated duplicate sibling invalidated a unique schema subtree: %q", got)
	}
}

func TestIncrementalFieldContextPreservesSchemaNullBaseline(t *testing.T) {
	propertiesValue := map[string]any{"password": nil}
	properties := childFieldContext(t, RootFieldContext(), "properties", propertiesValue, true, true)
	properties = enterFieldContext(t, properties, propertiesValue, true)
	password := childFieldContext(t, properties, "password", nil, true, true)
	if got := password.Scalar(nil, "null"); got != "null" {
		t.Fatalf("schema null compatibility changed: %q", got)
	}
	directCredential := childFieldContext(t, RootFieldContext(), "password", nil, true, true)
	if got := directCredential.Scalar(nil, "null"); got == "null" {
		t.Fatal("direct credential null escaped redaction")
	}
}

func TestIncrementalFieldContextPreservesDuplicateSchemaDeclarationRoles(t *testing.T) {
	propertiesValue := map[string]any{"password": map[string]any{"type": "string"}}
	properties := childFieldContext(t, RootFieldContext(), "properties", propertiesValue, true, true)
	properties = enterFieldContext(t, properties, propertiesValue, false)

	// The properties object is duplicate-bearing, and the source has repeated
	// password members. Each actual declaration remains structural even though
	// neither map-level uniqueness nor winner-map authority is available.
	passwordSchema := map[string]any{"type": "string", "default": "syntheticOpaqueValue42"}
	password := childFieldContext(t, properties, "password", passwordSchema, false, true)
	password = enterFieldContext(t, password, passwordSchema, true)
	typeValue := childFieldContext(t, password, "type", "string", true, true)
	if got := typeValue.Scalar("string", `"string"`); got != `"string"` {
		t.Fatalf("actual schema declaration lost its type role: %q", got)
	}
	defaultValue := childFieldContext(t, password, "default", "syntheticOpaqueValue42", true, true)
	if got := defaultValue.Scalar("syntheticOpaqueValue42", `"syntheticOpaqueValue42"`); strings.Contains(got, "syntheticOpaqueValue42") {
		t.Fatalf("schema declaration role exempted a credential default: %q", got)
	}

	// A duplicate inside one declaration keeps that declaration ambiguous, but
	// still validates a unique type occurrence and redacts non-null defaults.
	ambiguousSchema := childFieldContext(t, properties, "password", passwordSchema, false, false)
	ambiguousSchema = enterFieldContext(t, ambiguousSchema, passwordSchema, false)
	typeValue = childFieldContext(t, ambiguousSchema, "type", "string", true, true)
	if got := typeValue.Scalar("string", `"string"`); got != `"string"` {
		t.Fatalf("individually validated type was lost under duplicate descendants: %q", got)
	}
	defaultValue = childFieldContext(t, ambiguousSchema, "default", "syntheticOpaqueValue42", true, true)
	if got := defaultValue.Scalar("syntheticOpaqueValue42", `"syntheticOpaqueValue42"`); strings.Contains(got, "syntheticOpaqueValue42") {
		t.Fatalf("ambiguous declaration exposed a credential default: %q", got)
	}
}

func TestIncrementalFieldContextDependencySchemaDoesNotRetainNameRole(t *testing.T) {
	dependenciesValue := map[string]any{"password": map[string]any{"enum": []any{"syntheticOpaqueValue42"}}}
	rootValue := map[string]any{"dependencies": dependenciesValue}
	root := enterFieldContext(t, RootFieldContext(), rootValue, false)
	dependencies := childFieldContext(t, root, "dependencies", dependenciesValue, true, false)
	dependencies = enterFieldContext(t, dependencies, dependenciesValue, false)

	passwordSchema := dependenciesValue["password"]
	password := childFieldContext(t, dependencies, "password", passwordSchema, false, true)
	password = enterFieldContext(t, password, passwordSchema, true)
	enumValue := passwordSchema.(map[string]any)["enum"]
	enum := childFieldContext(t, password, "enum", enumValue, true, true)
	enum = enterFieldContext(t, enum, enumValue, true)
	if got := enum.Scalar("syntheticOpaqueValue42", `"syntheticOpaqueValue42"`); strings.Contains(got, "syntheticOpaqueValue42") {
		t.Fatalf("dependency-name role exempted a schema enum credential: %q", got)
	}
}

func TestIncrementalFieldContextDeclarationMapsDoNotGrantInstanceExemptions(t *testing.T) {
	for _, keyword := range []string{"properties", "$defs", "definitions", "dependencies", "dependentSchemas", "items", "additionalProperties"} {
		t.Run("sensitive parent cannot grant "+keyword, func(t *testing.T) {
			description := "syntheticOpaqueValue42"
			ordinarySchema := map[string]any{"description": description}
			nestedMap := map[string]any{"ordinary": ordinarySchema}
			passwordValue := map[string]any{keyword: nestedMap}
			rootValue := map[string]any{"password": passwordValue}
			root := enterFieldContext(t, RootFieldContext(), rootValue, true)
			password := childFieldContext(t, root, "password", passwordValue, true, false)
			password = enterFieldContext(t, password, passwordValue, false)
			declarations := childFieldContext(t, password, keyword, nestedMap, true, true)
			declarations = enterFieldContext(t, declarations, nestedMap, true)
			ordinary := childFieldContext(t, declarations, "ordinary", ordinarySchema, true, true)
			ordinary = enterFieldContext(t, ordinary, ordinarySchema, true)
			value := childFieldContext(t, ordinary, "description", description, true, true)
			if got := value.Scalar(description, `"`+description+`"`); strings.Contains(got, description) {
				t.Fatalf("sensitive ambiguous parent granted %s schema metadata: %q", keyword, got)
			}
		})
	}

	t.Run("oauth flow scopes", func(t *testing.T) {
		passwordFlow := map[string]any{
			"tokenUrl": "https://example.invalid",
			"scopes":   map[string]any{"custom_password": "syntheticOpaqueValue42"},
		}
		propertiesValue := map[string]any{"password": passwordFlow}
		root := enterFieldContext(t, RootFieldContext(), map[string]any{"properties": propertiesValue}, true)
		properties := childFieldContext(t, root, "properties", propertiesValue, true, true)
		properties = enterFieldContext(t, properties, propertiesValue, false)
		password := childFieldContext(t, properties, "password", passwordFlow, true, true)
		password = enterFieldContext(t, password, passwordFlow, true)
		scopesValue := passwordFlow["scopes"]
		scopes := childFieldContext(t, password, "scopes", scopesValue, true, true)
		scopes = enterFieldContext(t, scopes, scopesValue, true)
		credential := childFieldContext(t, scopes, "custom_password", "syntheticOpaqueValue42", true, true)
		if got := credential.Scalar("syntheticOpaqueValue42", `"syntheticOpaqueValue42"`); strings.Contains(got, "syntheticOpaqueValue42") {
			t.Fatalf("schema declaration acquired OAuth scope exemption: %q", got)
		}
	})

	t.Run("password controls", func(t *testing.T) {
		passwordSchema := map[string]any{"enabled": false, "policy": "strict"}
		propertiesValue := map[string]any{"password": passwordSchema}
		root := enterFieldContext(t, RootFieldContext(), map[string]any{"properties": propertiesValue}, true)
		properties := childFieldContext(t, root, "properties", propertiesValue, true, true)
		properties = enterFieldContext(t, properties, propertiesValue, false)
		password := childFieldContext(t, properties, "password", passwordSchema, true, true)
		password = enterFieldContext(t, password, passwordSchema, true)
		enabled := childFieldContext(t, password, "enabled", false, true, true)
		if got := enabled.Scalar(false, "false"); got == "false" {
			t.Fatal("schema declaration acquired the password control exemption")
		}
		policy := childFieldContext(t, password, "policy", "strict", true, true)
		if got := policy.Scalar("strict", `"strict"`); got == `"strict"` {
			t.Fatal("schema declaration acquired the password policy exemption")
		}
	})
}

func TestIncrementalFieldContextRequiredNeedsSchemaDeclarationProvenance(t *testing.T) {
	t.Run("ambiguous credential object", func(t *testing.T) {
		passwordValue := map[string]any{"required": []any{"syntheticOpaqueValue42"}}
		rootValue := map[string]any{"password": passwordValue}
		root := enterFieldContext(t, RootFieldContext(), rootValue, true)
		password := childFieldContext(t, root, "password", passwordValue, true, false)
		password = enterFieldContext(t, password, passwordValue, false)
		requiredValue := passwordValue["required"]
		required := childFieldContext(t, password, "required", requiredValue, true, true)
		if got := required.Scalar("syntheticOpaqueValue42", `"syntheticOpaqueValue42"`); strings.Contains(got, "syntheticOpaqueValue42") {
			t.Fatalf("ambiguous credential object acquired a required-name exemption: %q", got)
		}
	})

	t.Run("actual credential schema declaration", func(t *testing.T) {
		passwordValue := map[string]any{"type": "string", "required": []any{"token"}}
		propertiesValue := map[string]any{"password": passwordValue}
		rootValue := map[string]any{"properties": propertiesValue}
		root := enterFieldContext(t, RootFieldContext(), rootValue, true)
		properties := childFieldContext(t, root, "properties", propertiesValue, true, true)
		properties = enterFieldContext(t, properties, propertiesValue, false)
		password := childFieldContext(t, properties, "password", passwordValue, true, false)
		password = enterFieldContext(t, password, passwordValue, false)
		typeValue := childFieldContext(t, password, "type", "string", true, true)
		if got := typeValue.Scalar("string", `"string"`); got != `"string"` {
			t.Fatalf("individually validated schema type changed: %q", got)
		}
		required := childFieldContext(t, password, "required", passwordValue["required"], true, true)
		if got := required.Scalar("token", `"token"`); got != `"token"` {
			t.Fatalf("individually validated required name changed: %q", got)
		}
	})
}

func TestIncrementalFieldContextPreservesSchemaAnnotationNullsOnly(t *testing.T) {
	propertiesValue := map[string]any{"password": map[string]any{}}
	properties := childFieldContext(t, RootFieldContext(), "properties", propertiesValue, true, true)
	properties = enterFieldContext(t, properties, propertiesValue, true)
	passwordValue := propertiesValue["password"]
	password := childFieldContext(t, properties, "password", passwordValue, true, true)
	password = enterFieldContext(t, password, passwordValue, true)

	for _, key := range []string{"default", "const", "example", "enum", "examples"} {
		t.Run(key, func(t *testing.T) {
			value := any(nil)
			annotation := childFieldContext(t, password, key, value, true, true)
			if key == "enum" || key == "examples" {
				value = []any{nil, "syntheticOpaqueValue42"}
				annotation = childFieldContext(t, password, key, value, true, true)
				annotation = enterFieldContext(t, annotation, value, true)
				if got := annotation.Scalar(nil, "null"); got != "null" {
					t.Fatalf("typed null annotation changed: %q", got)
				}
				if got := annotation.Scalar("syntheticOpaqueValue42", `"syntheticOpaqueValue42"`); strings.Contains(got, "syntheticOpaqueValue42") {
					t.Fatalf("non-null credential annotation escaped: %q", got)
				}
				return
			}
			if got := annotation.Scalar(nil, "null"); got != "null" {
				t.Fatalf("typed null annotation changed: %q", got)
			}
			annotation = childFieldContext(t, password, key, "syntheticOpaqueValue42", true, true)
			if got := annotation.Scalar("syntheticOpaqueValue42", `"syntheticOpaqueValue42"`); strings.Contains(got, "syntheticOpaqueValue42") {
				t.Fatalf("non-null credential annotation escaped: %q", got)
			}
		})
	}
	direct := childFieldContext(t, RootFieldContext(), "password", nil, true, true)
	if got := direct.Scalar(nil, "null"); got == "null" {
		t.Fatal("direct credential null escaped redaction")
	}
}

func TestIncrementalFieldContextAmbiguousCredentialArrayKeepsBooleanSensitive(t *testing.T) {
	value := []any{false, map[string]any{"dup": 2}}
	rootValue := map[string]any{"password": value}
	root := enterFieldContext(t, RootFieldContext(), rootValue, false)
	context := childFieldContext(t, root, "password", value, true, false)
	context = enterFieldContext(t, context, value, false)
	if got := context.Scalar(false, "false"); got == "false" {
		t.Fatal("a duplicate-bearing password array granted a boolean schema exemption")
	}
}

func TestIncrementalFieldContextRestrictedMapEntryPrechargesOrSkipsScan(t *testing.T) {
	value := map[string]any{"one": 1, "two": 2, "three": 3, "four": 4}
	context := childFieldContext(t, RootFieldContext(), "api_key", value, true, true)
	work := 0
	entered, ok := context.Enter(value, true, func(cost int) bool {
		work += cost
		return work <= 2
	})
	if !ok {
		t.Fatal("restricted map entry exceeded its bounded precharge")
	}
	if work > 2 {
		t.Fatalf("restricted map entry charged excessive work: %d", work)
	}
	if got := childFieldContext(t, entered, "ordinary", "safe value", true, true).Scalar("safe value", `"safe value"`); got == `"safe value"` {
		t.Fatal("restricted map entry lost sensitive ancestry")
	}
}

func TestIncrementalFieldContextRetainsTypedAncestry(t *testing.T) {
	ordinary := childFieldContext(t, childFieldContext(t, RootFieldContext(), "result", map[string]any{}, true, true), "password", map[string]any{}, true, true)
	if got := childFieldContext(t, ordinary, "enabled", false, true, true).Scalar(false, "false"); got != "false" {
		t.Fatalf("incremental boolean control changed: %q", got)
	}
	if got := childFieldContext(t, ordinary, "enabled", "false", true, true).Scalar("false", `"false"`); got != `"<redacted credential>"` {
		t.Fatalf("incremental string lookalike escaped: %q", got)
	}
	if got := childFieldContext(t, ordinary, "policy", "strict", true, true).Scalar("strict", `"strict"`); got != `"strict"` {
		t.Fatalf("incremental exact policy changed: %q", got)
	}
	if got := childFieldContext(t, ordinary, "policy", "strict", true, true).Scalar("strict", `"\u0073trict"`); got != `"<redacted credential>"` {
		t.Fatalf("incremental escaped policy escaped: %q", got)
	}
	arrayParent := childFieldContext(t, childFieldContext(t, RootFieldContext(), "result", map[string]any{}, true, true), "password", []any{map[string]any{"enabled": false}}, true, true)
	if got := childFieldContext(t, arrayParent, "enabled", false, true, true).Scalar(false, "false"); got == "false" {
		t.Fatal("incremental password array acquired the object-control exception")
	}
	malformedContext := childFieldContext(t, childFieldContext(t,
		childFieldContext(t, RootFieldContext(), "properties", map[string]any{}, true, true),
		"password", map[string]any{}, true, true),
		"description", map[string]any{"ordinary": "syntheticOpaqueValue42"}, true, false)
	malformed := childFieldContext(t, malformedContext, "ordinary", "syntheticOpaqueValue42", true, true).
		Scalar("syntheticOpaqueValue42", `"syntheticOpaqueValue42"`)
	if strings.Contains(malformed, "syntheticOpaqueValue42") {
		t.Fatalf("incremental malformed metadata lost credential ancestry: %q", malformed)
	}
	descriptionParent := childFieldContext(t,
		childFieldContext(t, childFieldContext(t, RootFieldContext(), "properties", map[string]any{}, true, true), "password", map[string]any{}, true, true),
		"description", "valid last-winner documentation", false, true)
	duplicateMetadata := childFieldContext(t, descriptionParent, "password", map[string]any{"enabled": false}, true, false)
	if got := childFieldContext(t, duplicateMetadata, "enabled", false, true, true).Scalar(false, "false"); got == "false" {
		t.Fatal("duplicate schema metadata granted a password-control exemption")
	}
	if got := childFieldContext(t, RootFieldContext(), "default", true, true, true).Scalar(true, "true"); got != "true" {
		t.Fatalf("root schema value keyword context changed: %q", got)
	}
	duplicateControl := childFieldContext(t, ordinary, "enabled", false, false, true).Scalar(false, "false")
	if duplicateControl == "false" {
		t.Fatal("duplicate control member acquired the control exception")
	}
	ordinaryDuplicate := childFieldContext(t, RootFieldContext(), "ordinary", "plain", false, true).Scalar("plain", `"plain"`)
	if ordinaryDuplicate != `"plain"` {
		t.Fatalf("ordinary duplicate member was changed: %q", ordinaryDuplicate)
	}
	flow := map[string]any{
		"scopes":           map[string]any{"read": "Read access"},
		"authorizationUrl": "https://example.invalid/authorize",
		"tokenUrl":         "https://example.invalid/token",
	}
	flowContext := childFieldContext(t, RootFieldContext(), "password", flow, true, false)
	if got := childFieldContext(t, flowContext, "authorizationUrl", "syntheticOpaqueValue42", false, true).
		Scalar("syntheticOpaqueValue42", `"syntheticOpaqueValue42"`); strings.Contains(got, "syntheticOpaqueValue42") {
		t.Fatalf("duplicate-bearing OAuth flow acquired metadata exemption: %q", got)
	}
	budgetParent := childFieldContext(t,
		childFieldContext(t, RootFieldContext(), "properties", map[string]any{}, true, true),
		"password", map[string]any{}, true, true)
	if _, ok := budgetParent.Child("required", FieldOccurrence{Value: []any{"a", "b"}, KeyUnique: true, ValueUnique: true}, func(int) bool { return false }); ok {
		t.Fatal("context validation proceeded after budget precharge refusal")
	}
}
