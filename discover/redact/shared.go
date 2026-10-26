package redact

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// SecretIdentifierPattern recognizes uppercase credential identifiers. It is
// exported for consumers that also classify explicit exfiltration targets.
// Lowercase pagination names and plural token counts are ordinary identifiers.
const SecretIdentifierPattern = `[A-Z][A-Z0-9_]*_(?:SECRET|ACCESS|SIGNING|ENCRYPTION|PRIVATE|SESSION|REFRESH|AUTH)_(?:KEY|TOKEN)S?` +
	`|[A-Z][A-Z0-9_]*_(?:API_?KEY|PRIVATE_?KEY|TOKEN|SECRETS?|PASSWORDS?|PASSPHRASES?|CREDENTIALS?)`

var qualifiedCredentialName = regexp.MustCompile(`(?i)^(?:` + SecretIdentifierPattern + `)$`)
var secretIdentifier = regexp.MustCompile(`\b(?:` + SecretIdentifierPattern + `)\b`)

// Schema identifiers need provider-sized values. Text redaction retains the
// older conservative shorter-token rules for historical captures.
var githubIdentifierToken = regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[_A-Za-z0-9]{80,}`)

var redactionMarker = regexp.MustCompile(`<redacted[^<>]*>`)

// Normalize removes invisible format controls and variation selectors that can
// split credential syntax. Whitespace becomes ASCII spaces except shell tabs
// and line breaks, which retain their downstream instruction boundaries.
// This is deliberately not a decoder for arbitrary base64, URL or binary data.
func Normalize(s string) string {
	if normalizationStableASCII(s) {
		return s
	}
	return strings.Map(normalizeRune, s)
}

func normalizeRune(r rune) rune {
	switch {
	case unicode.Is(unicode.Cf, r), r == '\u034f', r >= '\ufe00' && r <= '\ufe0f', r >= '\U000e0100' && r <= '\U000e01ef':
		return -1
	case unicode.IsSpace(r) && r != ' ' && r != '\t' && r != '\r' && r != '\n':
		return ' '
	default:
		return r
	}
}

// ContainsProviderToken detects credential values by shape, without interpreting
// bare credential field names as values. Schema consumers can therefore keep
// parameter names such as API_TOKEN while rejecting a token embedded in a name.
func ContainsProviderToken(s string) bool {
	s = Normalize(s)
	for _, shape := range SecretShapes {
		switch shape.name {
		case "GitHub token":
			for _, token := range githubIdentifierToken.FindAllString(s, -1) {
				if !strings.HasPrefix(token, "github_pat_") || len(token)-len("github_pat_") >= 80 {
					return true
				}
			}
		case "JWT", "AWS access key", "GitLab token", "Slack token", "Stripe key", "model provider key", "Google key", "Telara key", "Groq key", "legacy provider key", "private key":
			if shape.re.MatchString(s) {
				return true
			}
		}
	}
	return false
}

// Field inspects text with its full source-field path. One context machine
// classifies schema metadata, value annotations and credential ancestors. A
// standalone annotation keyword begins a schema value path. Array positions do
// not add names. Callers must retain keys until their values are inspected.
func Field(path []string, text string) string {
	return inspectField(fieldContext(path), text, text == "true" || text == "false")
}

func inspectField(context schemaContext, text string, boolean bool) string {
	if context.sensitive && !context.data && !context.metadata && !context.schema && !context.properties {
		if value, ok := structured(text); ok {
			if _, ok := value.(map[string]any); ok {
				context.credentialControls = true
			}
		}
	}
	p := inspector{remaining: 64*len(text) + 65536, schemaData: context.data, rootContext: &context}
	result := p.text(text, context.scalarSensitive(boolean), 0).text
	if context.data {
		// A value annotation can itself contain an encoded document. Its
		// contextual data view must not hide matches found in the independent
		// source view (notably YAML aliases and flow schemas inside strings).
		base := inspect(text, false).text
		if base != text {
			if result == text {
				return base
			}
			if result != base {
				return "<redacted contextual field>"
			}
		}
	}
	return result
}

func fieldContext(path []string) schemaContext {
	context := schemaContext{}
	if len(path) > 0 && schemaValueKeyword(path[0]) {
		context.schema = true
	}
	for _, key := range path {
		key = Normalize(key)
		context = context.mapping().recognize(key).child(key)
	}
	return context
}

// FieldContext carries shared field classification as a document is
// traversed. Its zero value is the root context. Call Child once per keyed
// member with that member's decoded value; array elements retain their
// containing context and add no key. This applies schema shape validation once
// at the member instead of rebuilding ancestry for every scalar.
type FieldContext struct {
	state       schemaContext
	initialized bool
}

// FieldOccurrence preserves facts the decoded map cannot represent. KeyUnique
// is false when this member name occurs more than once in its source object;
// ValueUnique is false when the member's object subtree contains duplicate
// names. Consumers must derive both from the parsed source occurrences.
type FieldOccurrence struct {
	Value       any
	KeyUnique   bool
	ValueUnique bool
}

// RootFieldContext returns a fresh root context for one document traversal.
func RootFieldContext() FieldContext { return FieldContext{} }

// Child advances through one object member using its source occurrence and
// actual decoded value.
// precharge is called with the bounded validation-work estimate before any
// schema metadata shape validation runs. Return false to refuse the traversal.
func (c FieldContext) Child(key string, occurrence FieldOccurrence, precharge func(int) bool) (FieldContext, bool) {
	key = Normalize(key)
	state := c.state
	if !c.initialized && schemaValueKeyword(key) {
		state.schema = true
	}
	state = state.mapping().recognize(key)
	work, ok := fieldContextValidationWork(state, key, occurrence)
	if !ok || precharge == nil || !precharge(work) {
		return FieldContext{}, false
	}
	return FieldContext{state: state.childValueOccurrence(key, occurrence.Value, occurrence.ValueUnique, occurrence.KeyUnique), initialized: true}, true
}

// Enter applies transitions that depend on the current container value. Call
// it once for the root and once for each child container before visiting that
// container's members or elements. It preserves root and nested OAuth
// discovery, and the validated dependencies-array transition used by schemas.
// valueUnique must describe the complete source subtree, not a decoded map.
func (c FieldContext) Enter(value any, valueUnique bool, precharge func(int) bool) (FieldContext, bool) {
	work, ok := fieldContainerValidationWork(c.state, value, valueUnique)
	if !ok || precharge == nil || !precharge(work) {
		return FieldContext{}, false
	}
	state := c.state
	if isMappingContainer(value) {
		state = state.mapping()
		if valueUnique {
			state = state.discovery(value)
			state = recognizeContainerKeys(state, value)
		}
	} else if isSequenceContainer(value) {
		if state.dependency {
			if valueUnique && metadataStrings(value, false) {
				state = schemaContext{metadata: true}
			} else {
				state.data = true
				state.dependency = false
			}
		}
	}
	return FieldContext{state: state, initialized: c.initialized}, true
}

// Scalar inspects one decoded scalar using its original JSON token. The result
// is always a valid JSON scalar token. Unchanged source bytes are preserved;
// transformed strings are encoded exactly once.
func (c FieldContext) Scalar(value any, source string) string {
	if !json.Valid([]byte(source)) || !jsonScalarMatches(value, source) {
		return jsonStringToken("<redacted credential>")
	}
	context := c.state
	if context.credentialControl {
		canonical := false
		switch v := value.(type) {
		case bool:
			canonical = context.controlKey == "enabled" && source == strconv.FormatBool(v)
		case nil:
			canonical = context.controlKey == "enabled" && source == "null"
		case string:
			canonical = context.controlKey == "policy" && v == "strict" && source == `"strict"`
		}
		if canonical && !context.sensitive {
			return source
		}
		// Decoded values may qualify while their source spellings do not.
		context.sensitive = true
	}
	switch v := value.(type) {
	case string:
		if context.scalarSensitive(false) {
			return jsonStringToken("<redacted credential>")
		}
		result := inspectField(context, v, false)
		if result == v {
			return source
		}
		return jsonStringToken(result)
	case bool:
		if context.scalarSensitive(true) {
			return jsonStringToken("<redacted credential>")
		}
	case nil:
		if context.scalarSensitive(false) {
			return jsonStringToken("<redacted credential>")
		}
	default:
		if context.scalarSensitive(false) {
			return jsonStringToken("<redacted credential>")
		}
	}
	return source
}

// fieldValue adapts path fixtures to the incremental API. Production consumers
// should retain FieldContext values in traversal frames instead.
func fieldValue(path []string, pathValues []any, value any, source string) string {
	if len(pathValues) != len(path) {
		return jsonStringToken("<redacted credential>")
	}
	occurrences := make([]FieldOccurrence, len(pathValues))
	for i, pathValue := range pathValues {
		occurrences[i] = FieldOccurrence{Value: pathValue, KeyUnique: true, ValueUnique: true}
	}
	return fieldValueOccurrences(path, occurrences, value, source)
}

func fieldValueOccurrences(path []string, occurrences []FieldOccurrence, value any, source string) string {
	if len(occurrences) != len(path) {
		return jsonStringToken("<redacted credential>")
	}
	context := RootFieldContext()
	for i, key := range path {
		var ok bool
		context, ok = context.Child(key, occurrences[i], func(int) bool { return true })
		if !ok {
			return jsonStringToken("<redacted credential>")
		}
	}
	return context.Scalar(value, source)
}

const maxFieldContextValidationWork = 1 << 20

func fieldContextValidationWork(context schemaContext, key string, occurrence FieldOccurrence) (int, bool) {
	work := 1
	if !occurrence.ValueUnique || !occurrence.KeyUnique {
		return work, true
	}
	if context.schema && !context.data && !context.metadata && !context.properties && schemaMetadataKeyword(key) {
		cost, ok := schemaMetadataWork(key, occurrence.Value, maxFieldContextValidationWork-work)
		if !ok || cost > maxFieldContextValidationWork-work {
			return 0, false
		}
		work += cost
	}
	if !context.sensitive && !context.data && !context.properties && !context.metadata && knownOAuthFlow(key) {
		cost, ok := oauthFlowWork(occurrence.Value, maxFieldContextValidationWork-work)
		if !ok || cost > maxFieldContextValidationWork-work {
			return 0, false
		}
		work += cost
	}
	return work, true
}

func fieldContainerValidationWork(context schemaContext, value any, valueUnique bool) (int, bool) {
	work := 1
	if !valueUnique {
		return work, true
	}
	if isMappingContainer(value) {
		if context.sensitive || context.data || context.metadata || context.properties {
			return work, true
		}
		size, mapping, ok := containerSize(value)
		if !ok || !mapping {
			return work, true
		}
		add, ok := boundedWork(size, maxFieldContextValidationWork-work)
		if !ok {
			return 0, false
		}
		work += add
		// discovery validates only these list-valued fields. Charge their
		// full scan before discovery inspects them.
		for _, key := range []string{"authorization_servers", "bearer_methods_supported"} {
			var nested any
			if m, ok := value.(map[string]any); ok {
				nested = m[key]
			} else if node, ok := value.(*yaml.Node); ok && node.Kind == yaml.MappingNode {
				for i := 0; i+1 < len(node.Content); i += 2 {
					if metadataScalar(node.Content[i]) == key {
						nested = node.Content[i+1]
						break
					}
				}
			}
			if count, mapping, exists := containerSize(nested); exists && !mapping {
				add, ok := boundedWork(count, maxFieldContextValidationWork-work)
				if !ok {
					return 0, false
				}
				work += add
			}
		}
	} else if isSequenceContainer(value) {
		if context.dependency {
			size, mapping, ok := containerSize(value)
			if ok && !mapping {
				add, within := boundedWork(size, maxFieldContextValidationWork-work)
				if !within {
					return 0, false
				}
				work += add
			}
		}
	}
	return work, true
}

func isMappingContainer(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		return true
	case *yaml.Node:
		return v.Kind == yaml.MappingNode
	default:
		return false
	}
}

func isSequenceContainer(value any) bool {
	switch v := value.(type) {
	case []any:
		return true
	case *yaml.Node:
		return v.Kind == yaml.SequenceNode
	default:
		return false
	}
}

func recognizeContainerKeys(context schemaContext, value any) schemaContext {
	if m, ok := value.(map[string]any); ok {
		for key := range m {
			context = context.recognize(key)
		}
		return context
	}
	if node, ok := value.(*yaml.Node); ok && node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			if key, ok := metadataScalar(node.Content[i]).(string); ok {
				context = context.recognize(key)
			}
		}
	}
	return context
}

func schemaMetadataWork(key string, value any, limit int) (int, bool) {
	if limit < 0 {
		return 0, false
	}
	switch key {
	case "required", "type":
		size, mapping, ok := containerSize(value)
		if !ok || mapping {
			return 0, true
		}
		return boundedWork(size, limit)
	case "$vocabulary":
		size, mapping, ok := containerSize(value)
		if !ok || !mapping {
			return 0, true
		}
		return boundedWork(size, limit)
	case "dependentRequired":
		size, mapping, ok := containerSize(value)
		if !ok || !mapping {
			return 0, true
		}
		work, ok := boundedWork(size, limit)
		if !ok {
			return 0, false
		}
		if m, ok := value.(map[string]any); ok {
			for _, nested := range m {
				n, mapping, exists := containerSize(nested)
				if exists && !mapping {
					add, within := boundedWork(n, limit-work)
					if !within {
						return 0, false
					}
					work += add
				}
			}
		} else if node, ok := value.(*yaml.Node); ok && node.Kind == yaml.MappingNode {
			for i := 1; i < len(node.Content); i += 2 {
				n, mapping, exists := containerSize(node.Content[i])
				if exists && !mapping {
					add, within := boundedWork(n, limit-work)
					if !within {
						return 0, false
					}
					work += add
				}
			}
		}
		return work, true
	default:
		return 1, true
	}
}

func oauthFlowWork(value any, limit int) (int, bool) {
	outerSize, mapping, ok := containerSize(value)
	if !ok || !mapping {
		return 0, true
	}
	work, ok := boundedWork(outerSize, limit)
	if !ok {
		return 0, false
	}
	var scopes any
	if m, ok := value.(map[string]any); ok {
		scopes = m["scopes"]
	} else if node, ok := value.(*yaml.Node); ok && node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			if metadataScalar(node.Content[i]) == "scopes" {
				scopes = node.Content[i+1]
				break
			}
		}
	}
	innerSize, innerMapping, exists := containerSize(scopes)
	if exists && innerMapping {
		inner, within := boundedWork(innerSize, limit-work)
		if !within {
			return 0, false
		}
		work += inner
	}
	return work, true
}

func knownOAuthFlow(key string) bool {
	for _, flow := range oauthFlowNames {
		if key == flow {
			return true
		}
	}
	return false
}

func containerSize(value any) (size int, mapping, ok bool) {
	switch v := value.(type) {
	case []any:
		return len(v), false, true
	case map[string]any:
		return len(v), true, true
	case *yaml.Node:
		switch v.Kind {
		case yaml.SequenceNode:
			return len(v.Content), false, true
		case yaml.MappingNode:
			return len(v.Content) / 2, true, len(v.Content)%2 == 0
		}
	}
	return 0, false, false
}

func boundedWork(work, limit int) (int, bool) {
	if work < 0 || limit < 0 || work > limit {
		return 0, false
	}
	return work, true
}

func jsonScalarMatches(value any, source string) bool {
	if !json.Valid([]byte(source)) {
		return false
	}
	trimmed := strings.TrimSpace(source)
	switch v := value.(type) {
	case string:
		var decoded string
		return strings.HasPrefix(trimmed, `"`) && json.Unmarshal([]byte(source), &decoded) == nil && decoded == v
	case bool:
		return trimmed == strconv.FormatBool(v)
	case nil:
		return trimmed == "null"
	case json.Number:
		parsed, ok := jsonNumberToken(source)
		return ok && parsed == v
	case float32:
		parsed, ok := jsonNumberToken(source)
		if !ok {
			return false
		}
		f, err := strconv.ParseFloat(parsed.String(), 32)
		return err == nil && float32(f) == v
	case float64:
		parsed, ok := jsonNumberToken(source)
		if !ok {
			return false
		}
		f, err := strconv.ParseFloat(parsed.String(), 64)
		return err == nil && f == v
	case int:
		return jsonIntegerMatches(source, int64(v))
	case int8:
		return jsonIntegerMatches(source, int64(v))
	case int16:
		return jsonIntegerMatches(source, int64(v))
	case int32:
		return jsonIntegerMatches(source, int64(v))
	case int64:
		return jsonIntegerMatches(source, v)
	case uint:
		return jsonUnsignedMatches(source, uint64(v))
	case uint8:
		return jsonUnsignedMatches(source, uint64(v))
	case uint16:
		return jsonUnsignedMatches(source, uint64(v))
	case uint32:
		return jsonUnsignedMatches(source, uint64(v))
	case uint64:
		return jsonUnsignedMatches(source, v)
	default:
		return false
	}
}

func jsonNumberToken(source string) (json.Number, bool) {
	decoder := json.NewDecoder(strings.NewReader(source))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return "", false
	}
	number, ok := value.(json.Number)
	return number, ok
}

func jsonIntegerMatches(source string, want int64) bool {
	number, ok := jsonNumberToken(source)
	if !ok {
		return false
	}
	got, err := strconv.ParseInt(number.String(), 10, 64)
	return err == nil && got == want
}

func jsonUnsignedMatches(source string, want uint64) bool {
	number, ok := jsonNumberToken(source)
	if !ok {
		return false
	}
	got, err := strconv.ParseUint(number.String(), 10, 64)
	return err == nil && got == want
}

func jsonStringToken(value string) string {
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
	return strings.TrimSuffix(b.String(), "\n")
}

// ExternalField applies the shared inspection and the external display policy.
func ExternalField(path []string, text string) string {
	return DisplayExternal(Field(path, text))
}

// ExternalText applies credential redaction to untrusted display text. It uses
// the same value shapes as Redact, additionally hides bare uppercase credential
// identifiers, and uses the consumer's display marker. Complete variable
// references survive structured inspection unless the unconditional frozen
// gateway patterns match them; display also hides uppercase credential names.
func ExternalText(s string) string {
	return ExternalField(nil, s)
}

// DisplayExternal formats already-inspected data. It makes no credential
// decisions; consumers must run Field on the complete field/value first.
func DisplayExternal(out string) string {
	normalized := Normalize(out)
	display := redactionMarker.ReplaceAllLiteralString(normalized, "[REDACTED: sensitive value]")
	display = secretIdentifier.ReplaceAllLiteralString(display, "[REDACTED: sensitive value]")
	if display == normalized {
		return out
	}
	return display
}

// CorpusCase specifies shared minimum guarantees. MustRedact strings must be
// absent from normalized output, while MustSurvive strings must remain verbatim.
// Consumers may use different display markers and retain their own extra filters.
type CorpusCase struct {
	Profile     string   `json:"profile,omitempty"`
	ID          string   `json:"id"`
	Input       string   `json:"input"`
	MustRedact  []string `json:"must_redact,omitempty"`
	MustSurvive []string `json:"must_survive,omitempty"`
	// MustSurviveScan covers source/reference/prose that conservative text
	// views may minimize but must not block as a credential-bearing artifact.
	MustSurviveScan bool `json:"must_survive_scan,omitempty"`
	// ProviderToken optionally asserts the schema-identifier predicate, whose
	// provider-size thresholds are stricter than historical text minimization.
	ProviderToken *bool `json:"provider_token,omitempty"`
}

// CorpusJSON returns a fresh copy of the embedded synthetic conformance corpus.
// It never exposes mutable storage shared with the ruleset fingerprint.
func CorpusJSON() []byte { return append([]byte(nil), sharedCorpus...) }

// Corpus returns independently decoded cases for consumer test suites.
func Corpus() []CorpusCase {
	var cases []CorpusCase
	if err := json.Unmarshal(sharedCorpus, &cases); err != nil {
		panic("redact corpus is invalid")
	}
	return cases
}

// The fixture template keeps synthetic token-shaped strings visibly generated.
// Every consumer receives exactly the original expanded corpus bytes.
var sharedCorpus = bytes.ReplaceAll(sharedCorpusTemplate, []byte("{{synthetic_gitlab_token}}"), []byte("gl"+"pat-"+strings.Repeat("Ab1c", 5)))

//go:embed testdata/shared.json
var sharedCorpusTemplate []byte

// PreserveTextFloor retains the unconditional credential floor from source in
// an already-inspected display view. Consumers use this after composing separate
// fields, when joining fragments can reconstruct a credential. This does not
// reinterpret display framing as schema/data or replace original field inspection.
func PreserveTextFloor(source, view string) string {
	return gatewayFloor(source, inspection{view, ""}).text
}

// RedactComposedValues removes credential value shapes reconstructed by trusted
// display composition. Assignment names stay at their original field boundaries,
// so a platform:handle label cannot turn an ordinary identifier into a password.
// PreserveTextFloor separately retains decisions from original source text.
func RedactComposedValues(text string) string {
	normalized := Normalize(text)
	var spans []valueSpan
	add := func(pattern *regexp.Regexp) bool {
		for _, match := range pattern.FindAllStringIndex(normalized, 4097-len(spans)) {
			spans = append(spans, valueSpan{match[0], match[1]})
		}
		return len(spans) <= 4096
	}
	for _, shape := range SecretShapes {
		switch shape.name {
		case "private token header", "credential header", "authorization header", "AWS secret", "assigned secret", "secret in JSON":
			// These require an original field/header name, not a display label.
			continue
		}
		if !add(shape.re) {
			return "<redacted inspection limit>"
		}
	}
	for _, pattern := range gatewayCredentialPatterns[2:] {
		if !add(pattern) {
			return "<redacted inspection limit>"
		}
	}
	if len(spans) == 0 {
		return text
	}
	return replaceSpanUnion(normalized, spans, "<redacted credential>")
}
