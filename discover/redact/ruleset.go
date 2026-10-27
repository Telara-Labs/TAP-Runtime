package redact

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"sync"

	"github.com/Telara-Labs/TAP-Runtime/contract/canon"
)

// RulesetEpoch numbers the redaction rules. It goes up by one whenever a
// rule changes what is redacted, so a record can say which rules redacted
// it and a store can tell what to redact again. TestRulesetDigestNeedsAnEpoch
// fails when the rules change and the epoch does not.
const RulesetEpoch = 53

// Ruleset identifies the redaction rules in force: their epoch, and a digest
// derived from the rules themselves (never typed in), so two programs that
// claim the same epoch can check they hold the same rules.
type Ruleset struct {
	Epoch  int    `json:"epoch"`
	Digest string `json:"digest"`
}

var (
	rulesetOnce sync.Once
	ruleset     Ruleset
)

// CurrentRuleset is the ruleset this package applies. Its digest is the
// hex SHA-256 of the RFC 8785 encoding of every rule's name and pattern, in
// the order they are applied: the value shapes (SecretShapes), then the
// patterns that recognise credential names, assignments and variable
// references, the semantic word tables, and outputs of the behavior corpus.
func CurrentRuleset() Ruleset {
	rulesetOnce.Do(func() {
		ruleset = Ruleset{Epoch: RulesetEpoch, Digest: rulesDigest()}
	})
	return ruleset
}

type rule struct {
	Name    string `json:"name"`
	Pattern string `json:"pattern"`
}

func rules() []rule {
	var out []rule
	for i, re := range gatewayCredentialPatterns {
		out = append(out, rule{"frozen gateway credential " + strconv.Itoa(i), re.String()})
	}
	for _, sh := range SecretShapes {
		out = append(out, rule{sh.name, sh.re.String()})
	}
	return append(out,
		rule{"metadata email", metadataEmail.String()},
		rule{"metadata IPv4", metadataIPv4.String()},
		rule{"metadata IPv6", metadataIPv6.String()},
		rule{"metadata webhook URL", metadataURL.String()},
		rule{"metadata Groq", metadataGroq.String()},
		rule{"profiles", strings.Join((Ruleset{}).Profiles(), ",")},
		rule{"sensitive name", SensitiveName.String()},
		rule{"mixed credential name", mixedCredentialName.String()},
		rule{"credential class metadata", credentialClassName.String()},
		rule{"assignment header", assignmentHead.String()},
		rule{"assignment value", assignmentValue.String()},
		rule{"following mapping field", followingMappingField.String()},
		rule{"yaml mapping head", yamlMappingHead.String()},
		rule{"yaml flow head", yamlFlowHead.String()},
		rule{"schema value keywords", strings.Join(schemaValueKeywords, ",")},
		rule{"schema child keywords", strings.Join(schemaChildKeywords, ",")},
		rule{"schema property keywords", strings.Join(schemaPropertyKeywords, ",")},
		rule{"schema metadata keywords", strings.Join(schemaMetadataKeywords, ",")},
		rule{"OAuth flow names", strings.Join(oauthFlowNames, ",")},
		rule{"OAuth bearer methods", strings.Join(oauthBearerMethods, ",")},
		rule{"variable reference", reference.String()},
		rule{"raw variable reference", referencePrefix.String()},
		rule{"quoted field", quotedField.String()},
		rule{"qualified credential name", qualifiedCredentialName.String()},
		rule{"github schema token", githubIdentifierToken.String()},
		rule{"query name", queryName.String()},
		rule{"query value", queryValue.String()},
		rule{"signed query name", signedQueryName.String()},
		rule{"external identifier", secretIdentifier.String()},
		rule{"display marker", redactionMarker.String()},
		rule{"inspection marker", inspectionMarkerPattern.String()},
	)
}

func rulesDigest() string {
	var corpus []struct {
		Name  string `json:"name"`
		Input string `json:"input"`
	}
	if err := json.Unmarshal(behaviorCorpus, &corpus); err != nil {
		panic(err)
	}
	behavior := make([]string, 0, len(corpus))
	for _, c := range corpus {
		behavior = append(behavior, Argument(c.Name, c.Input))
	}
	b, err := canon.CanonicalValue(struct {
		Rules                                []rule
		Credentials, Configuration, Behavior []string
		SharedCorpus                         []CorpusCase
		SharedBehavior                       []string
		Engine                               int
	}{rules(), credentialWords, configurationWords, behavior, Corpus(), sharedBehavior(), 44})
	if err != nil {
		panic("redact: rules do not encode: " + err.Error())
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

//go:embed testdata/behavior.json
var behaviorCorpus []byte

// Include both public views and normalization in the fingerprint. Changes to
// embedded expectations also require a new epoch even if outputs are unchanged.
func sharedBehavior() []string {
	var out []string
	for _, c := range Corpus() {
		for _, profile := range (Ruleset{}).Profiles() {
			out = append(out, (Ruleset{}).RedactProfile(profile, c.Input))
		}
		out = append(out, Field(nil, c.Input), Field([]string{"password", "value"}, c.Input), ExternalField([]string{"password", "value"}, c.Input), Field([]string{"properties", "password", "default"}, c.Input), Field([]string{"properties", "password", "description"}, c.Input), Field([]string{"default"}, c.Input))
		out = append(out, Normalize(c.Input), Redact(c.Input), ExternalText(c.Input), SecretShape(c.Input), strconv.FormatBool(ContainsProviderToken(c.Input)), strconv.FormatBool(len(ScanArtifacts(map[string][]byte{"corpus": []byte(c.Input)})) != 0))
	}
	out = append(out,
		fieldValue([]string{"result", "password", "enabled"}, []any{map[string]any{}, map[string]any{}, false}, false, "false"),
		fieldValue([]string{"result", "password", "enabled"}, []any{map[string]any{}, map[string]any{}, "false"}, "false", `"false"`),
		fieldValue([]string{"result", "password", "enabled"}, []any{map[string]any{}, map[string]any{}, nil}, nil, "null"),
		fieldValue([]string{"result", "password", "enabled"}, []any{map[string]any{}, map[string]any{}, "null"}, "null", `"null"`),
		fieldValue([]string{"result", "password", "enabled"}, []any{map[string]any{}, map[string]any{}, json.Number("0")}, json.Number("0"), "0"),
		fieldValue([]string{"result", "password", "enabled"}, []any{map[string]any{}, map[string]any{}, "0"}, "0", `"0"`),
		fieldValue([]string{"result", "password", "policy"}, []any{map[string]any{}, map[string]any{}, "strict"}, "strict", `"strict"`),
		fieldValue([]string{"result", "password", "policy"}, []any{map[string]any{}, map[string]any{}, "strict"}, "strict", `"\u0073trict"`),
		fieldValue([]string{"properties", "password"}, []any{map[string]any{}, false}, false, "false"),
		fieldValue([]string{"properties", "password"}, []any{map[string]any{}, "false"}, "false", `"false"`),
		fieldValueOccurrences([]string{"result", "password", "enabled"}, []FieldOccurrence{
			{Value: map[string]any{}, KeyUnique: true, ValueUnique: true},
			{Value: map[string]any{"enabled": false, "enabled_shadow": false}, KeyUnique: true, ValueUnique: false},
			{Value: false, KeyUnique: false, ValueUnique: true},
		}, false, "false"),
		fieldValueOccurrences([]string{"result", "ordinary"}, []FieldOccurrence{
			{Value: map[string]any{}, KeyUnique: true, ValueUnique: true},
			{Value: "plain", KeyUnique: false, ValueUnique: true},
		}, "plain", `"plain"`),
	)
	duplicateResult := map[string]any{
		"type": "object",
		"default": map[string]any{"properties": map[string]any{
			"password": map[string]any{"description": "syntheticOpaqueValue42"},
		}},
		"password": map[string]any{"enabled": false},
		"dup":      2,
	}
	duplicateControls := map[string]any{
		"password": map[string]any{"enabled": nil, "policy": "strict"},
		"ordinary": 2,
	}
	duplicateWithSchema := map[string]any{"properties": map[string]any{
		"password": map[string]any{
			"description": "ordinary schema prose",
			"type":        "string",
			"default":     "syntheticOpaqueValue42",
		},
	}, "ordinary": 2}
	resultOccurrences := func(result map[string]any, values []any, unique []bool) []FieldOccurrence {
		occurrences := []FieldOccurrence{{Value: result, KeyUnique: true, ValueUnique: false}}
		for i := range values {
			occurrences = append(occurrences, FieldOccurrence{Value: values[i], KeyUnique: true, ValueUnique: unique[i]})
		}
		return occurrences
	}
	out = append(out,
		fieldValueWithContainers(map[string]any{"result": duplicateResult}, true,
			[]string{"result", "default", "properties", "password", "description"}, resultOccurrences(duplicateResult,
				[]any{duplicateResult["default"], duplicateResult["default"].(map[string]any)["properties"], duplicateResult["default"].(map[string]any)["properties"].(map[string]any)["password"], "syntheticOpaqueValue42"},
				[]bool{true, true, true, true}), "syntheticOpaqueValue42", `"syntheticOpaqueValue42"`),
		fieldValueWithContainers(map[string]any{"result": duplicateResult}, true,
			[]string{"result", "password", "enabled"},
			[]FieldOccurrence{{Value: duplicateResult, KeyUnique: true, ValueUnique: false},
				{Value: duplicateResult["password"], KeyUnique: true, ValueUnique: true},
				{Value: false, KeyUnique: true, ValueUnique: true}}, false, "false"),
		fieldValueWithContainers(map[string]any{"result": duplicateControls}, true,
			[]string{"result", "password", "enabled"},
			[]FieldOccurrence{{Value: duplicateControls, KeyUnique: true, ValueUnique: false},
				{Value: duplicateControls["password"], KeyUnique: true, ValueUnique: true},
				{Value: nil, KeyUnique: true, ValueUnique: true}}, nil, "null"),
		fieldValueWithContainers(map[string]any{"result": duplicateControls}, true,
			[]string{"result", "password", "policy"},
			[]FieldOccurrence{{Value: duplicateControls, KeyUnique: true, ValueUnique: false},
				{Value: duplicateControls["password"], KeyUnique: true, ValueUnique: true},
				{Value: "strict", KeyUnique: true, ValueUnique: true}}, "strict", `"strict"`),
		fieldValueWithContainers(map[string]any{"result": duplicateWithSchema}, true,
			[]string{"result", "properties", "password", "default"},
			[]FieldOccurrence{{Value: duplicateWithSchema, KeyUnique: true, ValueUnique: false},
				{Value: duplicateWithSchema["properties"], KeyUnique: true, ValueUnique: true},
				{Value: duplicateWithSchema["properties"].(map[string]any)["password"], KeyUnique: true, ValueUnique: true},
				{Value: "syntheticOpaqueValue42", KeyUnique: true, ValueUnique: true}},
			"syntheticOpaqueValue42", `"syntheticOpaqueValue42"`),
		fieldValueWithContainers(map[string]any{"password": []any{false, map[string]any{"dup": 2}}}, false,
			[]string{"password"},
			[]FieldOccurrence{{Value: []any{false, map[string]any{"dup": 2}}, KeyUnique: true, ValueUnique: false}}, false, "false"),
		fieldValueWithContainers(map[string]any{"properties": map[string]any{"password": nil}}, true,
			[]string{"properties", "password"},
			[]FieldOccurrence{{Value: map[string]any{"password": nil}, KeyUnique: true, ValueUnique: true},
				{Value: nil, KeyUnique: true, ValueUnique: true}}, nil, "null"),
		fieldValueWithContainers(map[string]any{"password": nil}, true,
			[]string{"password"}, []FieldOccurrence{{Value: nil, KeyUnique: true, ValueUnique: true}}, nil, "null"),
		fieldValueWithContainers(map[string]any{"properties": map[string]any{"password": map[string]any{"type": "string"}}}, false,
			[]string{"properties", "password", "type"}, []FieldOccurrence{
				{Value: map[string]any{"password": map[string]any{"type": "string"}}, KeyUnique: true, ValueUnique: false},
				{Value: map[string]any{"type": "string"}, KeyUnique: false, ValueUnique: true},
				{Value: "string", KeyUnique: true, ValueUnique: true},
			}, "string", `"string"`),
		fieldValueWithContainers(map[string]any{"properties": map[string]any{"password": map[string]any{"default": nil}}}, true,
			[]string{"properties", "password", "default"}, []FieldOccurrence{
				{Value: map[string]any{"password": map[string]any{"default": nil}}, KeyUnique: true, ValueUnique: true},
				{Value: map[string]any{"default": nil}, KeyUnique: true, ValueUnique: true},
				{Value: nil, KeyUnique: true, ValueUnique: true},
			}, nil, "null"),
		fieldValueWithContainers(map[string]any{"dependencies": map[string]any{"password": map[string]any{"enum": []any{"syntheticOpaqueValue42"}}}}, false,
			[]string{"dependencies", "password", "enum"}, []FieldOccurrence{
				{Value: map[string]any{"password": map[string]any{"enum": []any{"syntheticOpaqueValue42"}}}, KeyUnique: true, ValueUnique: false},
				{Value: map[string]any{"enum": []any{"syntheticOpaqueValue42"}}, KeyUnique: false, ValueUnique: true},
				{Value: []any{"syntheticOpaqueValue42"}, KeyUnique: true, ValueUnique: true},
			}, "syntheticOpaqueValue42", `"syntheticOpaqueValue42"`),
		fieldValueWithContainers(map[string]any{"properties": map[string]any{"password": map[string]any{"tokenUrl": "https://example.invalid", "scopes": map[string]any{"custom_password": "syntheticOpaqueValue42"}}}}, true,
			[]string{"properties", "password", "scopes", "custom_password"}, []FieldOccurrence{
				{Value: map[string]any{"password": map[string]any{"tokenUrl": "https://example.invalid", "scopes": map[string]any{"custom_password": "syntheticOpaqueValue42"}}}, KeyUnique: true, ValueUnique: false},
				{Value: map[string]any{"tokenUrl": "https://example.invalid", "scopes": map[string]any{"custom_password": "syntheticOpaqueValue42"}}, KeyUnique: true, ValueUnique: true},
				{Value: map[string]any{"custom_password": "syntheticOpaqueValue42"}, KeyUnique: true, ValueUnique: true},
				{Value: "syntheticOpaqueValue42", KeyUnique: true, ValueUnique: true},
			}, "syntheticOpaqueValue42", `"syntheticOpaqueValue42"`),
		fieldValueWithContainers(map[string]any{"properties": map[string]any{"password": map[string]any{"enabled": false}}}, true,
			[]string{"properties", "password", "enabled"}, []FieldOccurrence{
				{Value: map[string]any{"password": map[string]any{"enabled": false}}, KeyUnique: true, ValueUnique: false},
				{Value: map[string]any{"enabled": false}, KeyUnique: true, ValueUnique: true},
				{Value: false, KeyUnique: true, ValueUnique: true},
			}, false, "false"),
		fieldValueWithContainers(map[string]any{"password": map[string]any{"required": []any{"syntheticOpaqueValue42"}}}, true,
			[]string{"password", "required"}, []FieldOccurrence{
				{Value: map[string]any{"required": []any{"syntheticOpaqueValue42"}}, KeyUnique: true, ValueUnique: false},
				{Value: []any{"syntheticOpaqueValue42"}, KeyUnique: true, ValueUnique: true},
			}, "syntheticOpaqueValue42", `"syntheticOpaqueValue42"`),
		fieldValueWithContainers(map[string]any{"password": map[string]any{"properties": map[string]any{"ordinary": map[string]any{"description": "syntheticOpaqueValue42"}}}}, true,
			[]string{"password", "properties", "ordinary", "description"}, []FieldOccurrence{
				{Value: map[string]any{"properties": map[string]any{"ordinary": map[string]any{"description": "syntheticOpaqueValue42"}}}, KeyUnique: true, ValueUnique: false},
				{Value: map[string]any{"ordinary": map[string]any{"description": "syntheticOpaqueValue42"}}, KeyUnique: true, ValueUnique: true},
				{Value: map[string]any{"description": "syntheticOpaqueValue42"}, KeyUnique: true, ValueUnique: true},
				{Value: "syntheticOpaqueValue42", KeyUnique: true, ValueUnique: true},
			}, "syntheticOpaqueValue42", `"syntheticOpaqueValue42"`),
	)
	out = append(out, explicitSchemaBehavior()...)
	out = append(out, dependencyAmbiguityBehavior()...)
	identifierSource := `{"dependencies":{"password":{"allOf":[{"required":["ghp_` + strings.Repeat("A", 36) + `","ghp_` + strings.Repeat("B", 36) + `"],"properties":{"ordinary":{"default":false}}}]}}}`
	identifierProjection := Redact(identifierSource)
	out = append(out, identifierProjection, Redact(identifierProjection))
	metadataSource := `{"dependencies":{"password":{"allOf":[{"required":["a@example.invalid","b@example.invalid"],"properties":{"ordinary":{"default":false}}}]}}}`
	metadataProjection := (Ruleset{}).RedactProfile(PushMetadataProfile, metadataSource)
	out = append(out, metadataProjection, (Ruleset{}).RedactProfile(PushMetadataProfile, metadataProjection))
	for _, source := range []string{
		`{"dependencies":{"password":{"allOf":[{"properties":{"ordinary":{"default":false}}}]}}}`,
		`{"dependencies":{"password":{"allOf":{"properties":{"ordinary":{"default":"syntheticOpaqueValue42"}}}}}}`,
		`{"dependencies":{"password":{"allOf":[{"type":false,"properties":{"ordinary":{"default":"syntheticOpaqueValue42"}}}]}}}`,
		`{"dependencies":{"password":{"unknown":{"allOf":[{"properties":{"ordinary":{"default":"syntheticOpaqueValue42"}}}]}}}}`,
	} {
		out = append(out, Redact(source))
	}
	direct, ok := RootFieldContext().Child("password", FieldOccurrence{Value: nil, KeyUnique: true, ValueUnique: true}, func(int) bool { return true })
	if !ok {
		out = append(out, "<redacted inspection limit>")
	} else {
		out = append(out, strconv.FormatBool(direct.CredentialValue()), direct.Scalar(nil, "null"))
	}
	ordinary := map[string]any{"type": "boolean", "default": false}
	definitions := map[string]any{"ordinary": ordinary}
	dependency := map[string]any{"definitions": definitions}
	dependencies := map[string]any{"password": dependency}
	out = append(out, fieldValueWithContainers(map[string]any{"dependencies": dependencies}, true,
		[]string{"dependencies", "password", "definitions", "ordinary", "default"}, []FieldOccurrence{
			{Value: dependencies, KeyUnique: true, ValueUnique: true},
			{Value: dependency, KeyUnique: true, ValueUnique: true},
			{Value: definitions, KeyUnique: true, ValueUnique: true},
			{Value: ordinary, KeyUnique: true, ValueUnique: true},
			{Value: false, KeyUnique: true, ValueUnique: true},
		}, false, "false"))
	vocabularySource := `{"dependencies":{"password":{"allOf":[{"$vocabulary":{"https://ghp_` + strings.Repeat("A", 36) + `.example.invalid/vocabulary":false,"urn:ordinary":true},"properties":{"ordinary":{"default":false}}}]}}}`
	for _, profile := range (Ruleset{}).Profiles() {
		projected := (Ruleset{}).RedactProfile(profile, vocabularySource)
		out = append(out, projected, (Ruleset{}).RedactProfile(profile, projected))
	}
	for _, source := range []string{
		`{"$vocabulary":{"https://ghp_` + strings.Repeat("A", 36) + `.example.invalid/vocabulary":false,"urn:telara:red\u200bacted:vocabulary:1":true}}`,
		`{"$vocabu\u200blary":{"https://ghp_` + strings.Repeat("A", 36) + `.example.invalid/vocabulary":false}}`,
		`{"all\u200bOf":[{"$vocabulary":{"https://ghp_` + strings.Repeat("A", 36) + `.example.invalid/vocabulary":false}}]}`,
	} {
		projected := Redact(source)
		out = append(out, projected, Redact(projected))
	}
	return out
}

// Bind the explicit consumer-established schema role, including ambiguous
// occurrences, independently of keyword-based root schema inference.
func explicitSchemaBehavior() []string {
	var out []string
	for _, keyword := range schemaValueKeywords {
		password := map[string]any{"description": "syntheticOpaqueValue42"}
		properties := map[string]any{"password": password}
		literal := map[string]any{"properties": properties}
		var annotation any = literal
		if keyword == "enum" || keyword == "examples" {
			annotation = []any{literal}
		}
		schema := map[string]any{keyword: annotation}
		for _, unique := range []bool{true, false} {
			reserve := func(int) bool { return true }
			context, ok := RootFieldContext().Child("value", FieldOccurrence{Value: schema, KeyUnique: unique, ValueUnique: true}, reserve)
			context = context.Schema(unique)
			for _, step := range []struct {
				key   string
				value any
			}{{keyword, annotation}, {"properties", properties}, {"password", password}, {"description", "syntheticOpaqueValue42"}} {
				if !ok {
					break
				}
				context, ok = context.Child(step.key, FieldOccurrence{Value: step.value, KeyUnique: true, ValueUnique: true}, reserve)
				if ok && (isMappingContainer(step.value) || isSequenceContainer(step.value)) {
					context, ok = context.Enter(step.value, true, reserve)
				}
			}
			if !ok {
				out = append(out, "<redacted inspection limit>")
			} else {
				out = append(out, context.Scalar("syntheticOpaqueValue42", `"syntheticOpaqueValue42"`))
			}
		}
	}
	return out
}

// Bind the source uniqueness required before dependency-name sensitivity may
// end at a nested declaration, including a unique schema-combinator hop.
func dependencyAmbiguityBehavior() []string {
	ordinary := map[string]any{"default": "syntheticOpaqueValue42"}
	properties := map[string]any{"ordinary": ordinary}
	declaration := map[string]any{"properties": properties}
	children := []any{declaration}
	composed := map[string]any{"allOf": children}
	var out []string
	for _, composedPath := range []bool{false, true} {
		dependency := declaration
		path := []string{"dependencies", "password"}
		if composedPath {
			dependency = composed
		}
		dependencies := map[string]any{"password": dependency}
		occurrences := []FieldOccurrence{
			{Value: dependencies, KeyUnique: true, ValueUnique: false},
			{Value: dependency, KeyUnique: composedPath, ValueUnique: !composedPath},
		}
		if composedPath {
			path = append(path, "allOf")
			occurrences = append(occurrences, FieldOccurrence{Value: children, KeyUnique: true, ValueUnique: true})
		}
		path = append(path, "properties", "ordinary", "default")
		occurrences = append(occurrences,
			FieldOccurrence{Value: properties, KeyUnique: true, ValueUnique: true},
			FieldOccurrence{Value: ordinary, KeyUnique: true, ValueUnique: true},
			FieldOccurrence{Value: "syntheticOpaqueValue42", KeyUnique: true, ValueUnique: true},
		)
		out = append(out, fieldValueWithContainers(map[string]any{"dependencies": dependencies}, false,
			path, occurrences, "syntheticOpaqueValue42", `"syntheticOpaqueValue42"`))
	}
	return out
}
