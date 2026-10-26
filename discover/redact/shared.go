package redact

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"regexp"
	"strings"
	"unicode"
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
	context := schemaContext{}
	if len(path) > 0 && schemaValueKeyword(path[0]) {
		context.schema = true
	}
	for _, key := range path {
		key = Normalize(key)
		context = context.mapping().recognize(key).child(key)
	}
	if context.sensitive && !context.data && !context.metadata && !context.schema && !context.properties {
		if value, ok := structured(text); ok {
			if _, ok := value.(map[string]any); ok {
				context.credentialControls = true
			}
		}
	}
	p := inspector{remaining: 64*len(text) + 65536, schemaData: context.data, rootContext: &context}
	boolean := text == "true" || text == "false"
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
