package redact

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/Telara-Labs/TAP-Runtime/contract/canon"
	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
	"gopkg.in/yaml.v3"
)

// maxInspectionDepth bounds recursive JSON, YAML and text-fragment traversal.
// Keep it high enough for real catalog schemas while the byte budget in each
// inspector continues to bound total work.
const maxInspectionDepth = 2048

// SecretShapes are value patterns that are credentials wherever they appear.
var SecretShapes = []struct {
	name string
	re   *regexp.Regexp
}{
	{"bearer token", regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{12,}`)},
	{"basic auth", regexp.MustCompile(`(?i)\bbasic\s+[A-Za-z0-9+/]{12,}={0,2}`)},
	{"private token header", regexp.MustCompile(`(?i)\b(private|job|deploy)-token\s*[:=]\s*[^\s'"]{8,}`)},
	// Authorization: <scheme> <token> is caught by the bearer and basic
	// shapes; a raw token after it must itself look like one (a variable
	// reference such as $GITLAB_TOKEN is not a credential).
	{"credential header", regexp.MustCompile(`(?i)\b(x-api-key|api-key|x-auth-token|cookie|x-vault-token)\s*:\s*[^\s'"$]{6,}`)},
	{"authorization header", regexp.MustCompile(`(?i)\bauthorization\s*:\s*[A-Za-z0-9._~+/=-]{20,}`)},
	{"JWT", regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
	{"AWS access key", regexp.MustCompile(`(AKIA|ASIA)[0-9A-Z]{16}`)},
	{"AWS secret", regexp.MustCompile(`(?i)aws_secret_access_key\s*[=:]\s*[^\s'"]{16,}`)},
	{"GitHub token", regexp.MustCompile(`(gh[pousr]_[A-Za-z0-9]{30,}|github_pat_(?:[_A-Za-z0-9]{80,}|[A-Za-z0-9]{20,}_[A-Za-z0-9]{40,}|[A-Za-z0-9]{40,}))`)},
	{"GitLab token", regexp.MustCompile(`gl(pat|dt|rt|cbt|ptt|oas|soat|ffct)-[A-Za-z0-9_-]{16,}`)},
	{"Slack token", regexp.MustCompile(`xox[abprsoe]-[A-Za-z0-9-]{10,}`)},
	{"Stripe key", regexp.MustCompile(`(sk|rk)_(live|test)_[A-Za-z0-9]{16,}`)},
	{"model provider key", regexp.MustCompile(`(^|[^A-Za-z0-9])sk-(ant-|proj-)?[A-Za-z0-9_-]{20,}`)},
	{"Groq key", regexp.MustCompile(`gsk_[A-Za-z0-9]{48,}`)},
	{"legacy provider key", regexp.MustCompile(`(^|[^A-Za-z0-9])(ghp|gsk|sk|xox[baprs])-[_A-Za-z0-9]{16,}`)},
	{"Google key", regexp.MustCompile(`(AIza[0-9A-Za-z_-]{35}|ya29\.[0-9A-Za-z_-]{20,})`)},
	{"Telara key", regexp.MustCompile(`tlr[a-z]{0,3}_[A-Za-z0-9]{16,}`)},
	// Consume the body too, including an incomplete block. Removing only
	// the header leaves the encoded private key available to the reader.
	{"private key", regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`)},
	{"password in URL", regexp.MustCompile(`\b[a-z][a-z0-9+.-]*://[^/\s:@'"]+:[^/\s@'"]+@`)},
	{"signed URL", regexp.MustCompile(`(?i)[?&](x-amz-signature|x-amz-credential|x-amz-security-token|x-goog-signature|x-goog-credential|signature|sig|access_token|id_token|refresh_token|api_key|apikey|client_secret)=[^&\s'"]{8,}`)},
	// NAME=value where the name ends in a credential word. "tokens" (a
	// count) does not end in "token".
	{"assigned secret", regexp.MustCompile(`(?i)\b[A-Za-z0-9_]*(password|passwd|secret|api_?key|token|private_?key)\s*[=:]\s*['"]?[^\s'"$]{8,}`)},
	{"secret in JSON", regexp.MustCompile(`(?i)"[A-Za-z0-9_-]*(password|passwd|secret|token|api_?key|authorization|cookie|private_?key|credential)[A-Za-z0-9_-]*"\s*:\s*"[^"$]{6,}"`)},
}

// SensitiveName matches argument, flag and header names that carry
// credentials. Names that merely end in "key" (issue_key) or count tokens
// (max_output_tokens) do not match.
var SensitiveName = regexp.MustCompile(`(?i)^-{0,2}(authorization|auth|cookie|set-cookie|x-api-key|api[_-]?key|apikey|secret|secret[_-]?key|aws[_-]?secret[_-]?access[_-]?key|client[_-]?secret|password|passwd|pwd|pass|private[_-]?key|private[_-]?token|access[_-]?token|refresh[_-]?token|auth[_-]?token|id[_-]?token|session[_-]?token|bearer|token|credentials?|[a-z0-9_-]*[_-](token|secret|secret[_-]?key|password|passwd|api[_-]?key))=?$`)

// Preserve the gateway's optional and mixed word separators, including when
// a qualified prefix precedes an otherwise contiguous compound name.
var mixedCredentialName = regexp.MustCompile(`(?i)^-{0,2}(?:[a-z0-9]+[_-])*(?:api[\s_-]*key|access[\s_-]*token|refresh[\s_-]*token|auth[\s_-]*token|private[\s_-]*key)=?$`)

// IsSensitiveName also recognizes complete credential words inside camel,
// snake and kebab names. Counts, lengths and other numeric configuration
// labels are distinct from credential values (passwordHash is still secret).
// Scan name boundaries without allocating a normalized copy for every field.
func IsSensitiveName(name string) bool {
	name = Normalize(name)
	if name == "deviceCode" || name == "device_code" {
		return true
	}
	if credentialClassName.MatchString(name) {
		return false
	}
	if paginationTokenName.MatchString(name) {
		return false
	}
	configurationLabel := hasConfigurationWordAfterCredential(name)
	if SensitiveName.MatchString(name) {
		return !configurationLabel
	}
	if qualifiedCredentialName.MatchString(name) || mixedCredentialName.MatchString(name) {
		return true
	}
	// A spaced label must end in its credential field, rather than merely
	// mention one earlier in an explanatory phrase. Compound names above keep
	// the established mixed-separator grammar.
	if strings.ContainsAny(name, " \t\r\n\v\f") {
		words := strings.Fields(name)
		return len(words) > 0 && IsSensitiveName(words[len(words)-1])
	}
	credential, configuration := false, configurationLabel
	previous := ""
	start := 0
	for i := 0; i <= len(name); i++ {
		if i < len(name) && nameLetter(name[i]) {
			// dbPassword and APIToken have different capital boundaries.
			if i == start || !nameUpper(name[i]) || (!nameLower(name[i-1]) && !(nameUpper(name[i-1]) && i+1 < len(name) && nameLower(name[i+1]))) {
				continue
			}
		}
		if i > start {
			word := name[start:i]
			credential = credential || credentialWord(word) || (strings.EqualFold(word, "key") && (strings.EqualFold(previous, "api") || strings.EqualFold(previous, "private")))
			previous = word
		}
		start = i
		if i < len(name) && !nameLetter(name[i]) {
			start++
		}
	}
	return credential && !configuration
}

var paginationTokenName = regexp.MustCompile(`(?i)^(?:next|previous|prev|before|after)[_-]?page[_-]?token$`)

func hasConfigurationWordAfterCredential(name string) bool {
	lastCredentialEnd, lastConfigurationStart := -1, -1
	previous := ""
	start := 0
	for i := 0; i <= len(name); i++ {
		if i < len(name) && nameLetter(name[i]) {
			// Use the same camel-case boundaries as IsSensitiveName.
			if i == start || !nameUpper(name[i]) || (!nameLower(name[i-1]) && !(nameUpper(name[i-1]) && i+1 < len(name) && nameLower(name[i+1]))) {
				continue
			}
		}
		if i > start {
			word := name[start:i]
			if configurationWord(word) {
				lastConfigurationStart = start
			}
			if credentialWord(word) || strings.EqualFold(word, "key") && (strings.EqualFold(previous, "api") || strings.EqualFold(previous, "private")) {
				lastCredentialEnd = i
			}
			previous = word
		}
		start = i
		if i < len(name) && !nameLetter(name[i]) {
			start++
		}
	}
	return lastCredentialEnd >= 0 && lastConfigurationStart >= lastCredentialEnd
}

func nameUpper(c byte) bool { return c >= 'A' && c <= 'Z' }
func nameLower(c byte) bool { return c >= 'a' && c <= 'z' }
func nameLetter(c byte) bool {
	return nameUpper(c) || nameLower(c) || (c >= '0' && c <= '9')
}

var credentialWords = []string{"password", "passwd", "pwd", "secret", "token", "credential", "credentials", "cookie", "authorization", "bearer"}
var configurationWords = []string{"count", "length", "limit", "interval", "budget", "size", "duration", "timeout", "policy", "url", "uri", "endpoint", "enabled", "required", "format", "algorithm", "class", "type", "kind", "mode", "status", "hint", "name", "page", "ttl", "seconds"}

// These public enum constants describe credential handling, never a value.
// Match their exact source spelling; generic inline-password fields remain
// sensitive, as do provider-shaped strings under any metadata name.
var credentialClassName = regexp.MustCompile(`^(?:Wire)?Credential(?:Inline|EnvReferenced|OAuthManaged|None|Unknown|Unspecified)$`)

func credentialWord(word string) bool {
	for _, w := range credentialWords {
		if strings.EqualFold(word, w) {
			return true
		}
	}
	return false
}
func configurationWord(word string) bool {
	for _, w := range configurationWords {
		if strings.EqualFold(word, w) {
			return true
		}
	}
	return false
}

// UserFlagPrograms take -u / --user as user:password.
var UserFlagPrograms = map[string]bool{"curl": true, "wget": true, "http": true, "https": true, "xh": true}

// SecretShape names the credential shape v contains, or "".
func SecretShape(v string) string {
	return inspect(v, false).shape
}

// The legacy raw-text expression intentionally remains public. Apply the
// same name semantics to its field matches as to decoded objects, rather
// than treating tokenizer or password_length as credential values.
func sensitiveJSONField(field string) bool {
	end := strings.IndexByte(field[1:], '"') + 1
	if end < 1 || !IsSensitiveName(field[1:end]) {
		return false
	}
	var value string
	raw := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(field[end+1:]), ":"))
	if json.Unmarshal([]byte(raw), &value) == nil && variableReference(value) {
		return false
	}
	return true
}

// SensitiveSlot reports whether a recorded argument must never be written:
// its name says it carries a credential, a flag before it does (-H with an
// Authorization header is caught by shape), or its value has a credential's
// shape. -u user:password is a credential by position, but only for the
// programs where -u means a user (curl, wget, http); date -u and sort -u do
// not.
func SensitiveSlot(label string, sl trace.Slot) bool {
	if sl.Sub || sl.Type == trace.SlotFlag {
		return false
	}
	name := strings.SplitN(sl.Key, "#", 2)[0]
	if IsSensitiveName(strings.TrimSuffix(name, "=")) {
		return true
	}
	if (name == "-u=" || name == "--user=") && UserFlagPrograms[strings.SplitN(strings.TrimPrefix(label, "sh:"), " ", 2)[0]] && strings.Contains(sl.Value, ":") {
		return true
	}
	value := sl.Value
	// Environment references are inert text, even when a credential-name
	// field contains one. The source redactor may still remove a reference if
	// it overlaps the frozen compatibility floor, but it is not a hardcoded
	// credential value for slot admission.
	if decoded, ok := structured(value); ok {
		if stripVariableReferences(decoded) {
			value = encodeValue(decoded)
		}
	}
	return SecretShape(value) != ""
}

func stripVariableReferences(value any) bool {
	changed := false
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			if text, ok := child.(string); ok && variableReference(text) {
				v[key] = ""
				changed = true
				continue
			}
			changed = stripVariableReferences(child) || changed
		}
	case []any:
		for i, child := range v {
			if text, ok := child.(string); ok && variableReference(text) {
				v[i] = ""
				changed = true
				continue
			}
			changed = stripVariableReferences(child) || changed
		}
	}
	return changed
}

// Redact replaces every credential-shaped part of s with <redacted>. It is
// applied to all text the report prints or writes.
func Redact(s string) string {
	return Argument("", s)
}

// Argument preserves a recorded value's structure while removing credential
// values identified by their field names. History readers flatten structured
// arguments to JSON strings; nested objects, arrays and environment maps must
// retain that context instead of relying on a token's recognizable shape.
func Argument(name, s string) string {
	return Field([]string{"arguments", name}, s)
}

// Only a complete reference is safe: ${TOKEN:-literal-secret} also contains
// a credential. Do not interpret expressions or read the environment.
var reference = regexp.MustCompile(`^(\$[A-Za-z_][A-Za-z0-9_]*|\$\{[A-Za-z_][A-Za-z0-9_]*\}|\{env:[A-Za-z_][A-Za-z0-9_]*\})$`)

// Parse headers separately from shell-style values, so a benign outer
// label cannot consume a nested assignment (config: password=q).
var assignmentHead = regexp.MustCompile(`\b((?i:(?:[a-z0-9]+[_-])*(?:api[\s_-]*key|access[\s_-]*token|refresh[\s_-]*token|auth[\s_-]*token|private[\s_-]*key))|[A-Za-z_][A-Za-z0-9_-]*(?:[\t ]+[A-Za-z0-9_-]+)*)["']?\s*([=:])\s*`)
var assignmentValue = regexp.MustCompile(`^(?:(?i:Bearer|Basic)\s+)?(?:<redacted[^<>]*>|"(?:\\.|[^"\\])*"|'[^']*'|\\.|[^\s;|&"'\\])+`)

// A following mapping field describes an input rather than supplying its
// scalar value. Require whitespace/end after ':' so scalar URLs remain values.
var followingMappingField = regexp.MustCompile(`^(?:[A-Za-z_][A-Za-z0-9_-]*|"(?:\\.|[^"\\])+"|'[^']+')[\t ]*:(?:[\t \r\n]|$)`)

type valueSpan struct{ start, end int }

func asciiSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\v' || c == '\f'
}

func assignmentSensitiveName(name string) bool {
	if !strings.ContainsAny(name, " \t\r\n\v\f") && IsSensitiveName(name) {
		return true
	}
	// A configuration word in preceding prose must not hide the actual
	// assignment label. Find its last possible beginning in one pass, then
	// classify that suffix once rather than rescanning every suffix.
	candidate := -1
	for start := 0; start < len(name); {
		end := start
		for end < len(name) && !asciiSpace(name[end]) {
			end++
		}
		word := name[start:end]
		lastWord := word[strings.LastIndexAny(word, "_-")+1:]
		if IsSensitiveName(word) || strings.EqualFold(lastWord, "api") || strings.EqualFold(lastWord, "access") || strings.EqualFold(lastWord, "refresh") || strings.EqualFold(lastWord, "auth") || strings.EqualFold(lastWord, "private") {
			candidate = start
		}
		start = end + 1
	}
	if candidate < 0 {
		return false
	}
	suffix := name[candidate:]
	// A credential word earlier in explanatory prose is not the field label.
	// Spaced compound labels must match their complete credential grammar.
	return mixedCredentialName.MatchString(suffix) || !strings.ContainsAny(suffix, " \t\r\n\v\f") && IsSensitiveName(suffix)
}

func credentialAssignments(s string, restore func(string) string) []valueSpan {
	var spans []valueSpan
	consumed := 0
	for _, head := range assignmentHead.FindAllStringSubmatchIndex(s, -1) {
		if head[1] < consumed || !assignmentSensitiveName(s[head[2]:head[3]]) {
			continue
		}
		start := head[1]
		for start < len(s) && asciiSpace(s[start]) {
			start++
		}
		value := assignmentValue.FindString(s[start:])
		// Inspected quoted fields may already be protected as fragments. Restore
		// only this candidate when deciding whether it is mapping metadata.
		if s[head[4]:head[5]] == ":" && (followingMappingField.MatchString(s[start:]) || followingMappingField.MatchString(restore(value))) {
			continue
		}
		if value == "" {
			continue
		}
		literal := strings.Trim(value, `'"`)
		if ref := referencePrefix.FindStringSubmatch(s[start:]); len(ref) > 1 {
			tail := strings.TrimSpace(s[start+len(ref[1]):])
			if tail == "" || tail[0] == ',' || tail[0] == '}' || tail[0] == ']' {
				continue
			}
		}
		// Unquoted short colon prose also includes source references such
		// as Token: token. Quoted values and equality are explicit literals.
		explicit := s[head[4]:head[5]] == "=" || value[0] == '\'' || value[0] == '"' || len(literal) >= 8
		if explicit && !variableReference(literal) {
			consumed = start + len(value)
			spans = append(spans, valueSpan{start, consumed})
		}
	}
	return spans
}

func replaceValues(s string, spans []valueSpan) string {
	return replaceSpanUnion(s, spans, "'<redacted credential>'")
}
func replaceSpanUnion(s string, spans []valueSpan, replacement string) string {
	if len(spans) == 0 {
		return s
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	merged := spans[:0]
	for _, span := range spans {
		if n := len(merged); n > 0 && span.start <= merged[n-1].end {
			merged[n-1].end = max(merged[n-1].end, span.end)
		} else {
			merged = append(merged, span)
		}
	}
	var out strings.Builder
	last := 0
	for _, span := range merged {
		out.WriteString(s[last:span.start])
		out.WriteString(replacement)
		last = span.end
	}
	out.WriteString(s[last:])
	return out.String()
}

func variableReference(s string) bool {
	if reference.MatchString(s) {
		return true
	}
	for _, scheme := range []string{"Bearer ", "Basic "} {
		if len(s) > len(scheme) && strings.EqualFold(s[:len(scheme)], scheme) {
			return reference.MatchString(s[len(scheme):])
		}
	}
	return false
}

func structured(s string) (any, bool) {
	trimmed := strings.TrimSpace(s)
	if len(trimmed) == 0 {
		return nil, false
	}
	d := json.NewDecoder(strings.NewReader(trimmed))
	d.UseNumber() // keep exact integer/configuration values
	var value any
	if d.Decode(&value) != nil {
		return nil, false
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		return nil, false
	}
	return value, true
}

// inspection is the shared result of normalization, recursive JSON inspection,
// logical-line assembly and credential detection. A clean result retains the
// caller's exact bytes, including formatting and invisible characters.
type inspection struct {
	text, shape string
}

type inspector struct {
	remaining int
	// Value annotations contain data, even when objects/strings resemble schemas.
	schemaData      bool
	metadataProfile bool
	rootContext     *schemaContext
}

func inspect(s string, sensitive bool) inspection {
	return inspectProfile(s, sensitive, false)
}

func inspectProfile(s string, sensitive, metadataProfile bool) inspection {
	p := inspector{remaining: 64*len(s) + 65536, metadataProfile: metadataProfile}
	return p.text(s, sensitive, 0)
}

// Run the floor independently on every decoded string as well as the outer
// source. JSON escapes must not move a value outside the original-text floor.
func (p *inspector) text(s string, sensitive bool, depth int) inspection {
	if depth > maxInspectionDepth || len(s) > p.remaining {
		return inspection{"<redacted inspection limit>", "inspection limit"}
	}
	if !sensitive && !p.metadataProfile && fastRulesCurrent() && (plainASCII(s) || depth == 0 && p.remaining == 64*len(s)+65536 && (simpleJSON(s) || candidateText(s, 0))) {
		p.remaining -= len(s)
		return inspection{s, ""}
	}
	// All original/pristine/current frozen spans contribute to the floor.
	// Protect spans extending beyond current normalization BEFORE parsing;
	// current-view spans remain independently enforced by text and gatewayFloor.
	// Never replace only a prefix of an original frozen value.
	spans := frozenSourceSpans(s)
	current := spans
	if !normalizationStableASCII(s) {
		current = normalizedFloorSpans(s, normalizeRune)
	}
	// Sort and scan coverage once; hostile repeated assignments must not
	// turn the independent floor into a quadratic comparison.
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	sort.Slice(current, func(i, j int) bool { return current[i].start < current[j].start })
	var extending []valueSpan
	at, coveredEnd := 0, -1
	for _, span := range spans {
		for at < len(current) && current[at].start <= span.start {
			coveredEnd = max(coveredEnd, current[at].end)
			at++
		}
		if coveredEnd < span.end {
			extending = append(extending, span)
		}
	}
	// A frozen prefix must not erase a normalized PEM header while leaving
	// its multiline body. Add every intersecting complete/incomplete PEM span
	// to the same source union before any replacement happens.
	if len(extending) > 0 {
		prefixEnd := make([]int, len(extending))
		end := 0
		for i, span := range extending {
			end = max(end, span.end)
			prefixEnd[i] = end
		}
		var blocks []valueSpan
		for _, shape := range SecretShapes {
			if shape.name != "private key" {
				continue
			}
			blocks = projectedSpans(s, normalizeRune, func(view string) []valueSpan {
				var found []valueSpan
				for _, m := range shape.re.FindAllStringIndex(view, -1) {
					found = append(found, valueSpan{m[0], m[1]})
				}
				return found
			})
		}
		originalExtending := extending
		for _, block := range blocks {
			i := sort.Search(len(originalExtending), func(i int) bool { return originalExtending[i].start >= block.end })
			if i > 0 && prefixEnd[i-1] > block.start {
				extending = append(extending, block)
			}
		}
	}
	marker := ""
	protected := s
	if len(extending) > 0 {
		marker = inspectionMarker(s, "redactedcredential")
		protected = replaceSpanUnion(s, extending, marker)
	}
	if protected != s && json.Valid([]byte(s)) && !json.Valid([]byte(protected)) {
		protected = encodeValue("<redacted credential>")
	}
	result := gatewayFloorSpans(s, p.textView(protected, sensitive, depth), spans)
	if marker != "" {
		result.text = strings.ReplaceAll(result.text, marker, "<redacted credential>")
	}
	if len(spans) > 0 {
		result.shape = detectionClass(Normalize(s), "credential")
	}
	return result
}

// The pristine gateway removed exactly these six controls, independently of
// newer inspection normalization. Keep that view and the literal input too.
func pristineFloorRune(r rune) rune {
	switch r {
	case '\u200b', '\u200c', '\u200d', '\u2060', '\ufeff', '\u00ad':
		return -1
	}
	return r
}

func frozenSourceSpans(source string) []valueSpan {
	spans := gatewayCredentialSpans(source)
	if normalizationStableASCII(source) {
		return spans
	}
	spans = append(spans, normalizedFloorSpans(source, pristineFloorRune)...)
	return append(spans, normalizedFloorSpans(source, normalizeRune)...)
}

func normalizedFloorSpans(source string, normalize func(rune) rune) []valueSpan {
	return projectedSpans(source, normalize, gatewayCredentialSpans)
}

func projectedSpans(source string, normalize func(rune) rune, collect func(string) []valueSpan) []valueSpan {
	view := strings.Map(normalize, source)
	if view == source {
		return collect(source)
	}
	// Each output byte points to its original rune. The terminal offset
	// includes removed controls at a match's trailing boundary safely.
	offsets := make([]int, 0, len(view)+1)
	for at, r := range source {
		if next := normalize(r); next >= 0 {
			for range len(string(next)) {
				offsets = append(offsets, at)
			}
		}
	}
	offsets = append(offsets, len(source))
	var spans []valueSpan
	for _, span := range collect(view) {
		spans = append(spans, valueSpan{offsets[span.start], offsets[span.end]})
	}
	return spans
}

func (p *inspector) textView(original string, sensitive bool, depth int) (result inspection) {
	rawMarker := ""
	defer func() {
		if rawMarker != "" {
			result.text = strings.ReplaceAll(result.text, rawMarker, "<redacted credential>")
		}
	}()
	if depth > maxInspectionDepth || len(original) > p.remaining {
		return inspection{"<redacted inspection limit>", "inspection limit"}
	}
	p.remaining -= len(original)
	// Decode original values before normalization. Detection views may change;
	// unrelated decoded siblings must retain their exact contents.
	if value, ok := structured(original); ok {
		context := schemaContext{sensitive: sensitive, data: p.schemaData}
		if depth == 0 && p.rootContext != nil {
			context = *p.rootContext
		}
		result := p.json(original, value, context, depth)
		if result.shape != "" {
			result.shape = detectionClass(assembleLine(Normalize(original)), result.shape)
		}
		if result.shape == "" {
			result.text = original
		}
		return result
	}
	s := Normalize(original)
	if sensitive && s != "" && !variableReference(s) {
		return inspection{"<redacted credential>", "credential"}
	}
	if !sensitive && !p.schemaData && strings.Contains(s, ":") {
		if result, ok := p.yamlSchema(assembleLine(s), depth); ok {
			result = gatewayFloor(assembleLine(s), result)
			if result.shape == "" {
				result.text = original
			}
			return result
		}
	}
	// Compute independent raw-text spans before fragment protection. No later
	// parser can hide a frozen credential match. YAML contributes complete
	// scalar spans to the same union, including all physical continuation lines.
	logical := assembleLine(s)
	rawSpans := append(gatewayCredentialSpans(logical), yamlCredentialSpans(logical)...)
	rawSpans = append(rawSpans, yamlFlowCredentialSpans(logical)...)
	for _, sh := range SecretShapes {
		if sh.name == "private key" {
			for _, m := range sh.re.FindAllStringIndex(logical, -1) {
				rawSpans = append(rawSpans, valueSpan{m[0], m[1]})
			}
		}
	}
	if len(rawSpans) > 0 {
		rawMarker = inspectionMarker(logical, "redactedcredential")
	}
	raw := replaceSpanUnion(logical, rawSpans, rawMarker)
	rawShape := ""
	if raw != logical {
		rawShape = "credential"
		s = raw
	}
	// Decode complete JSON fragments before text patterns. Every descendant
	// is inspected before an outer fragment is consumed; failed outer parses
	// advance only one byte so inner fragments cannot disappear from view.
	s, shape, restore := p.fragments(s, depth, rawMarker)
	if p.metadataProfile {
		if spans := metadataSpans(s); len(spans) > 0 {
			s = replaceSpanUnion(s, spans, "<redacted metadata>")
			shape = "metadata"
		}
	}
	if rawShape != "" {
		shape = rawShape
	}
	for _, sh := range SecretShapes {
		if sh.name == "private key" && sh.re.MatchString(s) {
			s = sh.re.ReplaceAllString(s, "<redacted private key>")
			shape = sh.name
		}
	}
	// Assemble before detection, including continuations inside quotes. The
	// resulting replacement covers all pieces of a raw continued credential.
	// Assignment parsing inspects multiline scalars without consuming mapping metadata.
	line := assembleLine(s)
	if line != s {
		// A continuation can complete JSON syntax too. Re-enter the same
		// decoder so encoded names/tokens cannot hide behind that composition.
		r := p.text(line, false, depth+1)
		if r.shape != "" {
			r.text = restore(r.text)
			return r
		}
	}
	next := redactQuotedFields(line, IsSensitiveName, restore)
	for _, sh := range SecretShapes {
		if sh.name == "private key" {
			next = sh.re.ReplaceAllString(next, "<redacted private key>")
		}
	}
	next = replaceValues(next, credentialAssignments(next, restore))
	next = redactShapes(next)
	if next != line {
		if shape == "" {
			shape = "credential"
		}
		s = next
	}
	if shape == "" {
		return inspection{original, ""}
	}
	return inspection{restore(s), detectionClass(assembleLine(Normalize(original)), shape)}
}

// Preserve established human-readable classes after the shared pipeline has
// established a finding. Legacy patterns alone never decide whether to block.
func detectionClass(s, fallback string) string {
	for _, sh := range SecretShapes {
		if sh.re.MatchString(s) {
			return sh.name
		}
	}
	return fallback
}

func encodeValue(v any) string {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	_ = e.Encode(v) // decoded JSON values and replacement strings always encode
	return strings.TrimSuffix(b.String(), "\n")
}

func (p *inspector) json(raw string, value any, context schemaContext, depth int) inspection {
	// A map decoder alone would discard shadowed values. Refuse duplicate
	// keys and other ambiguous encodings for whole roots and prose fragments.
	if _, err := canon.CanonicalJSON([]byte(raw)); err != nil {
		return inspection{"<redacted ambiguous JSON>", "ambiguous JSON"}
	}
	next, shape := p.valueContext(value, context, depth+1)
	if shape == "" {
		return inspection{raw, ""}
	}
	return inspection{encodeValue(next), shape}
}

func (p *inspector) value(value any, sensitive bool, depth int) (any, string) {
	return p.valueContext(value, schemaContext{sensitive: sensitive, data: p.schemaData}, depth)
}

// Schema maps describe fields. Only annotations carrying actual values inherit
// the field's credential classification. Metadata still undergoes shape checks.
func (p *inspector) valueContext(value any, context schemaContext, depth int) (any, string) {
	if depth > maxInspectionDepth {
		return "<redacted inspection limit>", "inspection limit"
	}
	shape := ""
	switch v := value.(type) {
	case map[string]any:
		context = context.mapping().discovery(value)
		for key := range v {
			context = context.recognize(key)
		}
		// Sort traversal so the reported reason and resource accounting are
		// deterministic even when several fields carry credentials.
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if r := p.text(key, false, depth); r.shape != "" {
				delete(v, key)
				shape = r.shape
				continue
			}
			child := context.childValue(key, v[key])
			beforeData := p.schemaData
			p.schemaData = child.data
			next, found := p.valueContext(v[key], child, depth+1)
			p.schemaData = beforeData
			v[key] = next
			if found != "" {
				shape = "secret in JSON"
			}
		}
	case []any:
		if context.dependency {
			if metadataStrings(value, false) {
				context = schemaContext{metadata: true}
			} else {
				context.data = true
				context.dependency = false
			}
		}
		for i, child := range v {
			next, found := p.valueContext(child, context, depth+1)
			v[i] = next
			if found != "" {
				shape = found
			}
		}
	case string:
		r := p.text(v, context.scalarSensitive(false), depth)
		return r.text, r.shape
	case nil:
	case bool:
		if context.scalarSensitive(true) {
			return "<redacted credential>", "credential"
		}
	default:
		if context.scalarSensitive(false) {
			return "<redacted credential>", "credential"
		}
	}
	return value, shape
}

const (
	fragmentComposites = iota
	fragmentQuotedCandidates
	fragmentQuotedProtection
)

// Composite roots take precedence over surrounding prose quotes. Quoted
// candidates are then inspected at every possible opening, including overlaps;
// only after inspection do we protect clean quote spans from text patterns.
func (p *inspector) fragments(s string, depth int, rawMarker string) (string, string, func(string) string) {
	composites, shape, restoreComposites := p.fragmentPass(s, depth, fragmentComposites, rawMarker)
	quoted, found, _ := p.fragmentPass(composites, depth, fragmentQuotedCandidates, rawMarker)
	if found != "" {
		shape = found
	}
	protected, found, restoreQuoted := p.fragmentPass(quoted, depth, fragmentQuotedProtection, rawMarker)
	if found != "" {
		shape = found
	}
	return protected, shape, func(s string) string { return restoreComposites(restoreQuoted(s)) }
}

type fragmentChange struct {
	start, end int
	text       string
}

func (p *inspector) fragmentPass(s string, depth, phase int, rawMarker string) (string, string, func(string) string) {
	var out strings.Builder
	last, shape := 0, ""
	skipClosing := make(map[int]bool)
	// Protect every fully inspected fragment from later raw-text patterns.
	// Otherwise a closing source-code quote can become a fictitious opening
	// assignment quote. Tokens are collision-free and cannot carry secrets.
	prefix := inspectionMarker(s, "fragmentplaceholder")
	var replacements []string
	var changes []fragmentChange
	record := func(start, end int, text, found string) {
		if phase == fragmentQuotedCandidates {
			if found == "" {
				return
			}
			if n := len(changes); n > 0 && start < changes[n-1].end {
				changes[n-1].end = max(changes[n-1].end, end)
				changes[n-1].text = "<redacted credential>"
			} else {
				changes = append(changes, fragmentChange{start, end, text})
			}
			return
		}
		token := prefix + "fragment00000000" + strconv.Itoa(len(replacements)/2) + prefix
		out.WriteString(s[last:start])
		out.WriteString(token)
		replacements = append(replacements, token, text)
		last = end
	}
	finish := func(masked, shape string) (string, string, func(string) string) {
		if phase == fragmentQuotedCandidates {
			var result strings.Builder
			last := 0
			for _, change := range changes {
				result.WriteString(s[last:change.start])
				result.WriteString(change.text)
				last = change.end
			}
			result.WriteString(s[last:])
			return result.String(), shape, func(s string) string { return s }
		}
		if len(replacements) == 0 {
			return masked, shape, func(s string) string { return s }
		}
		return masked, shape, strings.NewReplacer(replacements...).Replace
	}
	for i := 0; i < len(s); i++ {
		if phase == fragmentComposites && s[i] != '{' && s[i] != '[' {
			continue
		}
		if phase != fragmentComposites && s[i] != '"' && s[i] != '\'' {
			continue
		}
		if s[i] == '\'' {
			// Shell/source single-quoted strings are literal spans too. Inspect
			// their contents before protecting their quote boundaries.
			close := strings.IndexByte(s[i+1:], '\'')
			if close < 0 {
				continue
			}
			end := i + close + 2
			inner := s[i+1 : end-1]
			r := p.text(inner, false, depth+1)
			following := end
			for following < len(s) && asciiSpace(s[following]) {
				following++
			}
			key := following < len(s) && (s[following] == ':' || s[following] == '=')
			if r.shape == "" && (variableReference(inner) || key) {
				if phase != fragmentQuotedCandidates {
					i = end - 1
				}
				continue
			}
			if r.shape != "" {
				shape = r.shape
			}
			record(i, end, "'"+r.text+"'", r.shape)
			if phase != fragmentQuotedCandidates {
				i = end - 1
			}
			continue
		}
		if s[i] != '{' && s[i] != '[' && s[i] != '"' {
			continue
		}
		if phase != fragmentQuotedCandidates && skipClosing[i] {
			continue
		}
		// Bound attempted decoding by work remaining. Small successful
		// fragments and syntax errors are charged by the consumed prefix.
		available := min(len(s)-i, p.remaining)
		if available <= 0 {
			record(i, len(s), "<redacted inspection limit>", "inspection limit")
			out.WriteString(s[last:])
			return finish(out.String(), "inspection limit")
		}
		d := json.NewDecoder(strings.NewReader(s[i : i+available]))
		d.UseNumber()
		var value any
		if err := d.Decode(&value); err != nil {
			if s[i] == '"' {
				// A malformed string's closing quote must not consume the
				// colon/value boundary as a new standalone string. Leave the
				// complete raw field available to the tolerant field parser.
				for close := i + 1; close < i+available; close++ {
					if s[close] == '\\' {
						close++
						continue
					}
					if s[close] == '"' {
						skipClosing[close] = true
						break
					}
				}
			}
			cost := available
			if syntax, ok := err.(*json.SyntaxError); ok {
				cost = int(syntax.Offset)
			}
			p.remaining -= cost
			if available < len(s)-i && cost == available {
				record(i, len(s), "<redacted inspection limit>", "inspection limit")
				out.WriteString(s[last:])
				return finish(out.String(), "inspection limit")
			}
			continue
		}
		p.remaining -= int(d.InputOffset())
		end := i + int(d.InputOffset())
		raw := s[i:end]
		field, keySyntax := false, false
		if _, isString := value.(string); isString {
			// A quoted key/value pair outside braces is a field fragment too.
			// Decode its complete value before shielding the key, including
			// nested objects and arrays rather than swallowing their contents.
			colon := end
			for colon < len(s) && asciiSpace(s[colon]) {
				colon++
			}
			keySyntax = colon < len(s) && (s[colon] == ':' || s[colon] == '=')
			if colon < len(s) && s[colon] == ':' {
				available := min(len(s)-colon-1, p.remaining)
				d := json.NewDecoder(strings.NewReader(s[colon+1 : colon+1+available]))
				d.UseNumber()
				var child any
				if err := d.Decode(&child); err == nil {
					p.remaining -= int(d.InputOffset())
					childEnd := colon + 1 + int(d.InputOffset())
					following := childEnd
					for following < len(s) && asciiSpace(s[following]) {
						following++
					}
					_, childString := child.(string)
					if childString && following < len(s) && s[following] == ':' {
						// The decoded string is a nested YAML mapping key,
						// rather than the outer field's credential scalar.
						keySyntax = true
					} else {
						end = childEnd
						raw = "{" + s[i:end] + "}"
						value = map[string]any{value.(string): child}
						field = true
					}
				} else {
					cost := available
					if syntax, ok := err.(*json.SyntaxError); ok {
						cost = int(syntax.Offset)
					}
					p.remaining -= cost
				}
			}
		}
		r := p.json(raw, value, schemaContext{data: p.schemaData}, depth)
		// A compatibility-layer replacement is a real edit even if the
		// decoded value no longer has a provider shape. Serialize the decoded
		// fragment so downstream view filters still see reconstructed syntax.
		if r.shape == "" && rawMarker != "" && strings.Contains(raw, rawMarker) {
			r = inspection{encodeValue(value), "credential"}
		}
		if r.shape != "" {
			shape = r.shape
		}
		if field {
			if r.shape == "" {
				r.text = s[i:end]
			} else if strings.HasPrefix(r.text, "{") && strings.HasSuffix(r.text, "}") {
				r.text = r.text[1 : len(r.text)-1]
			}
		}
		if ref, ok := value.(string); ok && r.shape == "" && (variableReference(ref) || keySyntax && !field) {
			// Keep complete references visible to shell assignment parsing.
			if phase != fragmentQuotedCandidates {
				i = end - 1
			}
			continue
		}
		record(i, end, r.text, r.shape)
		if phase != fragmentQuotedCandidates {
			i = end - 1
		}
	}
	out.WriteString(s[last:])
	return finish(out.String(), shape)
}

// Keep physical line boundaries unless a backslash explicitly joins them.
// Noncontinued YAML/source lines must not donate a value to the prior name.
func logicalLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			if s[i+1] == '\r' && i+2 < len(s) && s[i+2] == '\n' {
				i += 2
			} else {
				i++
			}
		} else if s[i] == '\n' {
			lines = append(lines, s[start:i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func assembleLine(s string) string {
	if !strings.Contains(s, "\\\n") && !strings.Contains(s, "\\\r\n") {
		return s
	}
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			if s[i+1] == '\n' {
				i++
				continue
			}
			if s[i+1] == '\r' && i+2 < len(s) && s[i+2] == '\n' {
				i += 2
				continue
			}
			out.WriteByte(s[i])
			i++
		}
		out.WriteByte(s[i])
	}
	return out.String()
}

var quotedField = regexp.MustCompile(`(["'])((?:\\.|[^"'\\])*)(["']\s*[:=]\s*)("(?:\\.|[^"\\])*"|'[^']*'|[^,\s}\]\[{]+)`)

func redactText(s string) string { return inspect(s, false).text }

// Decode quoted names and values even in JSON fragments embedded in prose.
// A raw-text regex alone cannot recognize an escaped credential field name.
func redactQuotedFields(s string, sensitive func(string) bool, restore func(string) string) string {
	offset := 0
	s = quotedField.ReplaceAllStringFunc(s, func(field string) string {
		start := offset + strings.Index(s[offset:], field)
		offset = start + len(field)
		m := quotedField.FindStringSubmatch(field)
		if followingMappingField.MatchString(s[offset-len(m[4]):]) {
			return field
		}
		name := m[2]
		if m[1] == `"` {
			var decoded string
			if json.Unmarshal([]byte(`"`+name+`"`), &decoded) == nil {
				name = decoded
			}
		}
		if followingMappingField.MatchString(restore(m[4])) {
			return field
		}
		value := strings.Trim(m[4], `'"`)
		if strings.HasPrefix(m[4], `"`) {
			_ = json.Unmarshal([]byte(m[4]), &value)
		}
		if redactShapes(Normalize(name)) != Normalize(name) {
			return "<redacted credential>"
		}
		if sensitive(name) && !variableReference(value) {
			return m[1] + m[2] + m[3] + `"<redacted credential>"`
		}
		if next := redactShapes(Normalize(value)); next != Normalize(value) {
			var b bytes.Buffer
			e := json.NewEncoder(&b)
			e.SetEscapeHTML(false)
			_ = e.Encode(next) // a string always encodes
			return m[1] + m[2] + m[3] + strings.TrimSuffix(b.String(), "\n")
		}
		return field
	})
	return s
}

// redactShapes applies shared value patterns without inferring argument names.
// Query names are decoded once, as by standard URL query parsing. Replace
// complete raw value spans without re-encoding or changing adjacent parameters.
var queryName = regexp.MustCompile(`[?&]([^=?&#\s'"<>]+)=`)
var queryValue = regexp.MustCompile(`^[^&#\s'"<>]*`)

func redactQueryValues(s string) string {
	var out strings.Builder
	last := 0
	for _, head := range queryName.FindAllStringSubmatchIndex(s, -1) {
		if head[0] < last {
			continue
		}
		name, err := url.QueryUnescape(s[head[2]:head[3]])
		if err != nil || !IsSensitiveName(name) && !signedQueryName.MatchString(name) {
			continue
		}
		start := head[1]
		value := queryValue.FindString(s[start:])
		if value == "" || variableReference(value) {
			continue
		}
		out.WriteString(s[last:start])
		out.WriteString("<redacted credential>")
		last = start + len(value)
	}
	if last == 0 {
		return s
	}
	out.WriteString(s[last:])
	return out.String()
}

var signedQueryName = regexp.MustCompile(`(?i)^(?:x-amz-(?:signature|credential|security-token)|x-goog-(?:signature|credential)|signature|sig)$`)

func redactShapes(s string) string {
	s = redactQueryValues(s)
	for _, sh := range SecretShapes {
		if sh.name == "assigned secret" || sh.name == "signed URL" {
			// Named values above include short and quoted credentials and
			// preserve references; the old length-only shape does neither.
			continue
		}
		if sh.re.MatchString(s) {
			if sh.name == "secret in JSON" {
				s = sh.re.ReplaceAllStringFunc(s, func(field string) string {
					if sensitiveJSONField(field) {
						return "<redacted " + sh.name + ">"
					}
					return field
				})
			} else if sh.name == "model provider key" || sh.name == "legacy provider key" {
				// The short sk- prefix needs a left boundary so task-... is
				// ordinary. Preserve the matched delimiter, including '_'.
				s = sh.re.ReplaceAllString(s, "${1}<redacted "+sh.name+">")
			} else {
				s = sh.re.ReplaceAllString(s, "<redacted "+sh.name+">")
			}
		}
	}
	return s
}

// ScanArtifacts inspects the complete artifact through the same pipeline as
// both text views. Findings contain only the file name and detection class.
func ScanArtifacts(files map[string][]byte) []string {
	var out []string
	for name, body := range files {
		text := string(body)
		if sh := SecretShape(text); sh != "" {
			before, lineNumber := len(out), 1
			for _, line := range logicalLines(text) {
				if found := SecretShape(line); found != "" {
					out = append(out, name+" line "+strconv.Itoa(lineNumber)+": "+found)
				}
				lineNumber += strings.Count(line, "\n")
			}
			if len(out) == before {
				out = append(out, name+": "+sh)
			}
		}
	}
	sort.Strings(out)
	return out
}

var schemaValueKeywords = []string{"default", "examples", "example", "const", "enum"}

func schemaValueKeyword(key string) bool {
	for _, word := range schemaValueKeywords {
		if key == word {
			return true
		}
	}
	return false
}

var schemaChildKeywords = []string{"items", "prefixItems", "additionalItems", "additionalProperties", "unevaluatedProperties", "unevaluatedItems", "contentSchema", "contains", "allOf", "anyOf", "oneOf", "not", "if", "then", "else", "propertyNames"}

func schemaChildKeyword(key string) bool {
	for _, word := range schemaChildKeywords {
		if key == word {
			return true
		}
	}
	return false
}

// Frozen credential patterns from gateway origin/main 2ae3aef. The separate
// uppercase-identifier display policy remains in ExternalText. These patterns
// are independent of all structured parsers and credential-name classifiers.
var gatewayCredentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:` + SecretIdentifierPattern + `)\b["']?\s*[:=]\s*["']?[^"'\s,}]{8,}`),
	regexp.MustCompile(`(?i)\b(?:[a-z0-9]+[_-])*(?:api[\s_-]*key|access[\s_-]*token|refresh[\s_-]*token|auth[\s_-]*token|secret|password|passwd|credential|private[\s_-]*key)\b["']?\s*[:=]\s*["']?[^"'\s,}]{8,}`),
	regexp.MustCompile(`\b(ghp|glpat|gsk|sk|xox[baprs])-[_A-Za-z0-9]{16,}\b`),
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[_A-Za-z0-9]{80,}|gsk_[A-Za-z0-9]{48,}`),
}

func gatewayCredentialSpans(s string) []valueSpan {
	var spans []valueSpan
	for i, re := range gatewayCredentialPatterns {
		// These mandatory bytes are present in every match of the frozen rule.
		if i < 2 && !strings.ContainsAny(s, ":=") || i == 2 && !strings.Contains(s, "-") || i == 3 && !strings.Contains(s, "_") {
			continue
		}
		for _, m := range re.FindAllStringIndex(s, -1) {
			start, end := m[0], m[1]
			if i < 2 {
				separator := strings.IndexAny(s[start:end], ":=") + start
				fieldName := strings.Trim(strings.TrimSpace(s[start:separator]), `"'`)
				if !IsSensitiveName(fieldName) {
					continue
				}
				at := separator + 1
				for at < len(s) && asciiSpace(s[at]) {
					at++
				}
				if at < end && (s[at] == '"' || s[at] == '\'') {
					at++
				}

				start = at
				// The frozen gateway expression stops at whitespace, which can
				// split its own multiword redaction marker on a second pass.
				// Extend the span through a complete marker so replacing it is
				// idempotent and cannot leave a marker fragment behind.
				if markerEnd := redactionMarkerEnd(s[start:]); markerEnd > 0 {
					end = start + markerEnd
				}
			}
			spans = append(spans, valueSpan{start, end})
		}
	}
	return spans
}

func redactionMarkerEnd(s string) int {
	if marker := redactionMarker.FindStringIndex(s); marker != nil && marker[0] == 0 {
		return marker[1]
	}
	const externalMarker = "[REDACTED: sensitive value]"
	if strings.HasPrefix(s, externalMarker) {
		return len(externalMarker)
	}
	return 0
}

func yamlSequenceValue(s string) bool {
	return len(s) > 0 && s[0] == '-' && (len(s) == 1 || asciiSpace(s[1]))
}

// Track sequence subtrees in one physical-line pass. Short plain scalars are
// explicit data inside a sequence, including nested and flow mapping entries.
func yamlSequenceRanges(s string) []valueSpan {
	var spans []valueSpan
	start, indent := -1, 0
	for at := 0; at < len(s); {
		end := len(s)
		if newline := strings.IndexByte(s[at:], '\n'); newline >= 0 {
			end = at + newline
		}
		content := at
		for content < end && (s[content] == ' ' || s[content] == '\t' || s[content] == '\r') {
			content++
		}
		if content < end && s[content] != '#' {
			leading := content - at
			sequence := yamlSequenceValue(s[content:end])
			if start >= 0 && leading <= indent && !sequence {
				spans = append(spans, valueSpan{start, at})
				start = -1
			}
			if sequence {
				if start < 0 {
					start, indent = at, leading
				} else {
					indent = min(indent, leading)
				}
			}
		}
		at = end + 1
	}
	if start >= 0 {
		spans = append(spans, valueSpan{start, len(s)})
	}
	return spans
}

func yamlInSequence(spans []valueSpan, at int) bool {
	i := sort.Search(len(spans), func(i int) bool { return spans[i].end > at })
	return i < len(spans) && spans[i].start <= at
}

// Parse physical YAML scalar spans independently of shell word boundaries.
// This tolerant layer also handles incomplete captures, so intervening comments
// cannot hide a scalar when a strict YAML decoder would reject the document.
var yamlMappingHead = regexp.MustCompile(`(?m)^([\t ]*(?:-[\t ]+)*)(?:\?[\t ]+)?(?:"((?:\\.|[^"\\\r\n])+)"|'([^'\r\n]+)'|([A-Za-z_][A-Za-z0-9_-]*))\s*:[\t ]*`)

func yamlCredentialSpans(s string) []valueSpan {
	var spans []valueSpan
	sequences := yamlSequenceRanges(s)
	budget := 64*len(s) + 65536
	for _, head := range yamlMappingHead.FindAllStringSubmatchIndex(s, -1) {
		name := ""
		for _, group := range []int{4, 6, 8} {
			if head[group] >= 0 {
				name = s[head[group]:head[group+1]]
				break
			}
		}
		if head[4] >= 0 {
			var decoded string
			if json.Unmarshal([]byte(`"`+name+`"`), &decoded) == nil {
				name = decoded
			}
		}
		if !IsSensitiveName(name) {
			continue
		}
		indent := head[3] - head[2]
		start := head[1]
		// Skip blank lines and comments between a key and its scalar. Keep those
		// source bytes; a block header and its entire body form a single value.
		for start < len(s) {
			end := start
			for end < len(s) && s[end] != '\n' {
				end++
				budget--
			}
			if budget < 0 {
				return []valueSpan{{0, len(s)}}
			}
			budget -= end - start + 1
			if budget < 0 {
				return []valueSpan{{0, len(s)}}
			}
			line := strings.TrimSpace(s[start:end])
			if line != "" && !strings.HasPrefix(line, "#") {
				break
			}
			if end == len(s) {
				start = end
				break
			}
			start = end + 1
			for start < len(s) && (s[start] == ' ' || s[start] == '\t' || s[start] == '\r') {
				start++
			}
		}
		if start >= len(s) || followingMappingField.MatchString(s[start:]) {
			continue
		}
		multiline := strings.Contains(s[head[0]:start], "\n")
		if multiline {
			lineStart := strings.LastIndexByte(s[:start], '\n') + 1
			if start-lineStart < indent || start-lineStart == indent && !yamlSequenceValue(s[start:]) {
				continue
			}
		}
		end := start
		if s[start] == '"' || s[start] == '\'' {
			quote := s[start]
			end++
			for end < len(s) {
				budget--
				if budget < 0 {
					return []valueSpan{{0, len(s)}}
				}
				if s[end] == '\\' && quote == '"' {
					end += 2
					continue
				}
				if s[end] == quote {
					end++
					if quote == '\'' && end < len(s) && s[end] == quote {
						end++
						continue
					}
					break
				}
				end++
			}
			literal := s[start:min(end, len(s))]
			decoded := strings.Trim(literal, "\"'")
			if quote == '"' {
				_ = json.Unmarshal([]byte(literal), &decoded)
			}
			if variableReference(decoded) {
				continue
			}
		} else {
			block := s[start] == '|' || s[start] == '>'
			first := assignmentValue.FindString(s[start:])
			// Same-line short colon labels remain source/prose. Multiline scalars
			// and block bodies are explicit values, including short multiword text.
			if !yamlInSequence(sequences, head[0]) && !multiline && !block && len(first) < 8 && (len(strings.Fields(strings.SplitN(s[start:], "\n", 2)[0])) < 2 || strings.EqualFold(name, "credentials") || strings.EqualFold(name, "credential")) {
				continue
			}
			for end < len(s) && s[end] != '\n' {
				end++
				budget--
			}
			if budget < 0 {
				return []valueSpan{{0, len(s)}}
			}
			if !block {
				if comment := strings.Index(s[start:end], " #"); comment >= 0 {
					end = start + comment
				}
			}
			scan := end
			if scan < len(s) && s[scan] != '\n' {
				if newline := strings.IndexByte(s[scan:], '\n'); newline >= 0 {
					scan += newline
				} else {
					scan = len(s)
				}
			}
			scalarEnd := end
			for scan < len(s) {
				next := scan + 1
				stop := next
				for stop < len(s) && s[stop] != '\n' {
					stop++
				}
				budget -= stop - next + 1
				if budget < 0 {
					return []valueSpan{{0, len(s)}}
				}
				line := strings.TrimRight(s[next:stop], "\r")
				trim := strings.TrimSpace(line)
				leading := len(line) - len(strings.TrimLeft(line, " \t"))
				if trim != "" && !strings.HasPrefix(trim, "#") && (leading < indent || leading == indent && !(yamlSequenceValue(s[start:]) && yamlSequenceValue(trim))) {
					break
				}
				if !block && s[start] != '-' && trim != "" && followingMappingField.MatchString(trim) {
					break
				}
				if trim != "" && !strings.HasPrefix(trim, "#") || block {
					scalarEnd = stop
				}
				scan = stop
			}
			end = scalarEnd
		}
		if end > start {
			// Only a complete scalar can be a reference. A reference prefix
			// followed by folded literal text remains credential material.
			if variableReference(strings.TrimSpace(s[start:min(end, len(s))])) {
				continue
			}
			spans = append(spans, yamlScalarSpan(s, start, min(end, len(s))))
		}
	}
	return spans
}

// Independently enforce every frozen value span after structured inspection.
// Even a future parser that accidentally shields a credential cannot return
// bytes the compatibility layer identified for removal. Both layers add spans;
// neither layer can cancel the other's decision. No reference, mapping or schema
// exemption is permitted in this frozen layer.
func gatewayFloor(source string, result inspection) inspection {
	return gatewayFloorSpans(source, result, frozenSourceSpans(source))
}

func gatewayFloorSpans(source string, result inspection, spans []valueSpan) inspection {
	if len(spans) == 0 {
		return result
	}
	output := Normalize(result.text)
	var remaining []valueSpan
	budget := 64*len(source) + 65536
	checked := make(map[string]bool)
	values := make([]string, 0)
	for _, span := range spans {
		value := source[span.start:span.end]
		for depth := 0; ; depth++ {
			normalized := Normalize(value)
			if checked[normalized] {
				break
			}
			checked[normalized] = true
			values = append(values, normalized)
			// Decoding can turn an originally contiguous frozen value into
			// delimiter-separated pieces. Structured inspection may already
			// remove one piece; retain coverage of every remaining piece too.
			// This only adds decisions from the original frozen value.
			if pieces := strings.FieldsFunc(normalized, func(r rune) bool {
				return unicode.IsSpace(r) || r == ',' || r == '}' || r == '"' || r == '\''
			}); len(pieces) > 1 {
				for _, piece := range pieces {
					if !checked[piece] {
						checked[piece] = true
						values = append(values, piece)
					}
				}
			}
			// Structured inspection can decode and reserialize a different field,
			// changing this frozen value's escape spelling without removing it.
			// Keep its decoded representations in the unconditional floor too.
			if !strings.Contains(value, `\`) {
				break
			}
			if depth >= 64 || len(value) > budget {
				return inspection{"<redacted inspection limit>", "inspection limit"}
			}
			budget -= len(value)
			var decoded string
			if json.Unmarshal([]byte(`"`+value+`"`), &decoded) != nil || decoded == "" || decoded == value {
				break
			}
			value = decoded
		}
	}
	for _, value := range values {
		if len(output) > budget {
			return inspection{"<redacted inspection limit>", "inspection limit"}
		}
		budget -= len(output)
		for at := 0; at < len(output); {
			found := strings.Index(output[at:], value)
			if found < 0 {
				break
			}
			start := at + found
			end := start + len(value)
			remaining = append(remaining, valueSpan{start, end})
			if len(remaining) > 4096 {
				return inspection{"<redacted inspection limit>", "inspection limit"}
			}
			at = end
		}
	}
	if len(remaining) == 0 {
		return result
	}
	projected := projectedSpans(result.text, normalizeRune, func(string) []valueSpan { return remaining })
	next := replaceSpanUnion(result.text, projected, "<redacted credential>")
	// A residual numeric/container span can cross JSON token boundaries. Keep
	// valid JSON valid even if the structured parser failed to remove that value.
	if json.Valid([]byte(result.text)) && !json.Valid([]byte(next)) {
		next = encodeValue("<redacted credential>")
	}
	return inspection{next, "credential"}
}

var schemaPropertyKeywords = []string{"dependencies", "properties", "patternProperties", "$defs", "definitions", "dependentSchemas"}

func schemaPropertyKeyword(key string) bool {
	for _, word := range schemaPropertyKeywords {
		if key == word {
			return true
		}
	}
	return false
}

var schemaMetadataKeywords = []string{"id", "$recursiveRef", "$recursiveAnchor", "$schema", "$id", "$ref", "$anchor", "$dynamicAnchor", "$dynamicRef", "$vocabulary", "$comment", "type", "title", "description", "format", "required", "dependentRequired", "readOnly", "writeOnly", "deprecated", "minLength", "maxLength", "pattern", "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf", "minItems", "maxItems", "minContains", "maxContains", "uniqueItems", "minProperties", "maxProperties", "contentEncoding", "contentMediaType"}

func schemaMetadataKeyword(key string) bool {
	for _, word := range schemaMetadataKeywords {
		if key == word {
			return true
		}
	}
	return false
}

// A raw compatibility match can stop at a JSON closing quote or a reference's
// closing brace. Classify the complete source reference rather than that
// truncated match. Decoded/concatenated literal values are still inspected.
var referencePrefix = regexp.MustCompile(`^((?:\$[A-Za-z_][A-Za-z0-9_]*|\$\{[A-Za-z_][A-Za-z0-9_]*\}|\{env:[A-Za-z_][A-Za-z0-9_]*\}))(?:$|[\s"',;|&\]}])`)

// Flow mappings use commas/brackets as scalar boundaries rather than shell
// word boundaries. Quoted values retain their own delimiter and escape grammar.
var yamlFlowHead = regexp.MustCompile(`([,{][\t ]*)(?:"((?:\\.|[^"\\\r\n])+)"|'([^'\r\n]+)'|([A-Za-z_][A-Za-z0-9_-]*))\s*:\s*`)

func yamlFlowCredentialSpans(s string) []valueSpan {
	var spans []valueSpan
	sequences := yamlSequenceRanges(s)
	budget := 64*len(s) + 65536
	for _, head := range yamlFlowHead.FindAllStringSubmatchIndex(s, -1) {
		name := ""
		for _, group := range []int{4, 6, 8} {
			if head[group] >= 0 {
				name = s[head[group]:head[group+1]]
				break
			}
		}
		if head[4] >= 0 {
			var decoded string
			if json.Unmarshal([]byte(`"`+name+`"`), &decoded) == nil {
				name = decoded
			}
		}
		if !IsSensitiveName(name) {
			continue
		}
		start := head[1]
		if start >= len(s) || s[start] == '{' && !strings.HasPrefix(s[start:], "{env:") || s[start] == '[' {
			continue
		}
		end := start
		literal := ""
		if s[start] == '"' || s[start] == '\'' {
			quote := s[start]
			end++
			for end < len(s) {
				budget--
				if budget < 0 {
					return []valueSpan{{0, len(s)}}
				}
				if s[end] == '\\' && quote == '"' {
					end += 2
					continue
				}
				if s[end] == quote {
					end++
					if quote == '\'' && end < len(s) && s[end] == quote {
						end++
						continue
					}
					break
				}
				end++
			}
			end = min(end, len(s))
			literal = strings.Trim(s[start:end], "\"'")
			if quote == '"' {
				_ = json.Unmarshal([]byte(s[start:end]), &literal)
			}
		} else {
			if strings.HasPrefix(s[start:], "${") || strings.HasPrefix(s[start:], "{env:") {
				if close := strings.IndexByte(s[start:], '}'); close >= 0 {
					end = start + close + 1
				}
			}
			for end < len(s) && s[end] != ',' && s[end] != '}' && s[end] != ']' {
				end++
				budget--
			}
			if budget < 0 {
				return []valueSpan{{0, len(s)}}
			}
			literal = strings.TrimSpace(s[start:end])
			fields := strings.Fields(literal)
			if len(fields) == 0 || len(fields) == 1 && len(literal) < 8 && !yamlInSequence(sequences, head[0]) {
				continue
			}
		}
		if end > start && !variableReference(literal) {
			spans = append(spans, yamlScalarSpan(s, start, end))
		}
	}
	return spans
}

// Keep surrounding quotes in place when masking a scalar before JSON fragment
// decoding. A YAML view of a JSON field must not break its containing object.
func yamlScalarSpan(s string, start, end int) valueSpan {
	if end > start+1 && (s[start] == '"' || s[start] == '\'') && s[end-1] == s[start] {
		return valueSpan{start + 1, end - 1}
	}
	return valueSpan{start, end}
}

// Pick a short absent marker in one source pass. Repeating an input-derived
// prefix for every fragment can otherwise multiply a hostile input's size.
var inspectionMarkerPattern = regexp.MustCompile(`(?:redactedcredential|fragmentplaceholder)([0-9]+)end`)

func inspectionMarker(s, base string) string {
	used := make(map[string]bool)
	for _, match := range inspectionMarkerPattern.FindAllStringSubmatch(s, -1) {
		used[match[1]] = true
	}
	for i := 0; ; i++ {
		number := strconv.Itoa(i)
		if !used[number] {
			return base + number + "end"
		}
	}
}

// One context machine serves JSON, block YAML and flow YAML. Keyword names
// are case-sensitive. Unknown extensions remain data; value annotations cannot
// reclassify a descendant as a schema, even through a decoded string.
type schemaContext struct {
	schema, properties, sensitive, data, metadata, metadataMap, credentialControls bool
	schemaAmbiguous                                                                bool
	schemaDeclarationMap, schemaDeclaration, schemaNullAnnotation                  bool
	credentialControl                                                              bool
	controlKey                                                                     string
	dependencies, dependency                                                       bool
	oauthFlow, oauthScopes, oauthResource, oauthIssuer                             bool
}

// A subschema may be an object or boolean. Other scalar types do not gain
// metadata treatment merely by occupying a schema slot.
func (c schemaContext) scalarSensitive(boolean bool) bool {
	return c.sensitive && !c.metadata && !(c.schema && !c.schemaAmbiguous && boolean) && !(c.dependency && boolean)
}

// Only vocabulary declarations and dependency-name maps have metadata keys.
// An object in a scalar/array metadata slot is malformed instance data, so its
// named values must still be inspected rather than inheriting an exemption.
func (c schemaContext) mapping() schemaContext {
	if c.dependency {
		c.dependency = false
		c.schema = true
		c.properties = false
		c.dependencies = false
		c.schemaDeclarationMap = false
		c.schemaDeclaration = true
	}
	if c.metadata && !c.metadataMap {
		return schemaContext{data: true}
	}
	return c
}

func (c schemaContext) recognize(key string) schemaContext {
	key = Normalize(key) // classify a view; retain the original map key
	if !c.schemaAmbiguous && !c.data && !c.metadata && !c.sensitive && !c.properties && (schemaPropertyKeyword(key) || schemaChildKeyword(key) || schemaMetadataKeyword(key)) {
		c.schema = true
	}
	return c
}

func (c schemaContext) child(key string) schemaContext {
	key = Normalize(key) // classify a view; retain the original map key
	child := schemaContext{sensitive: c.sensitive || IsSensitiveName(key), data: c.data}
	switch {
	case c.data:
	case c.oauthIssuer && key == "authorization_response_iss_parameter_supported":
		child.sensitive = false
	case c.oauthResource && (key == "authorization_servers" || key == "bearer_methods_supported"):
		child.sensitive = false
		child.metadata = true
	case c.oauthScopes:
		child.sensitive = false
		child.data = true
	case c.oauthFlow && key == "scopes":
		child.sensitive = false
		child.oauthScopes = true
	case c.metadata:
		child.sensitive = false
		child.metadata = true
	case c.schemaDeclarationMap:
		child.schema = !c.dependencies
		child.properties = c.dependencies
		child.dependencies = c.dependencies
		child.dependency = c.dependencies
		child.schemaDeclaration = !c.dependencies
	case c.properties:
		child.schema = !c.dependencies
		child.dependency = c.dependencies
		child.schemaDeclaration = !c.dependencies
	case c.schema && schemaValueKeyword(key):
		child.sensitive = c.sensitive
		child.data = true
		child.schemaNullAnnotation = !c.schemaAmbiguous || c.schemaDeclaration
	case c.schema && schemaPropertyKeyword(key):
		child.properties = true
		child.dependencies = key == "dependencies"
	case c.schema && schemaChildKeyword(key):
		child.schema = true
	case c.schema && schemaMetadataKeyword(key):
		child.sensitive = false
		child.metadata = true
		child.metadataMap = key == "dependentRequired" || key == "$vocabulary"
	}
	return child
}

// Metadata exemptions require the complete keyword value to have its declared
// shape. Malformed declarations become instance data and retain the enclosing
// credential classification. The same validator serves decoded JSON and YAML.
func (c schemaContext) childValue(key string, value any) schemaContext {
	return c.childValueOccurrence(key, value, true, true)
}

func (c schemaContext) childValueOccurrence(key string, value any, valueUnique, keyUnique bool) schemaContext {
	child := c.child(key)
	if c.schemaAmbiguous {
		switch {
		case schemaValueKeyword(Normalize(key)):
			// A default/const/enum/example under an ambiguous schema candidate
			// is instance data, not an annotation exemption.
			child = schemaContext{sensitive: c.sensitive || IsSensitiveName(key), data: true, schemaNullAnnotation: c.schemaDeclaration}
		case schemaMetadataKeyword(Normalize(key)):
			// A duplicate-bearing parent cannot authorize whole-map metadata.
			// These exact scalar/list declarations are independently shape checked.
			individuallySafe := c.schemaDeclaration && (!c.sensitive || Normalize(key) == "type" || Normalize(key) == "required")
			if !c.data && !c.metadata && individuallySafe && valueUnique && keyUnique && validSchemaMetadata(Normalize(key), value) {
				child = schemaContext{metadata: true, metadataMap: Normalize(key) == "dependentRequired" || Normalize(key) == "$vocabulary"}
			} else {
				child = schemaContext{sensitive: c.sensitive || IsSensitiveName(key), data: c.data}
			}
		case schemaPropertyKeyword(Normalize(key)):
			if !c.data && !c.metadata && (!c.sensitive || c.schemaDeclaration) {
				if _, ok := value.(map[string]any); ok {
					child.schema = true
					child.properties = true
					child.dependencies = Normalize(key) == "dependencies"
					child.schemaDeclarationMap = true
					child.schemaAmbiguous = true
				} else {
					child = schemaContext{sensitive: c.sensitive || IsSensitiveName(key), data: true}
				}
			} else {
				child = schemaContext{sensitive: c.sensitive || IsSensitiveName(key), data: true}
			}
		case schemaChildKeyword(Normalize(key)):
			if !c.data && !c.metadata && (!c.sensitive || c.schemaDeclaration) &&
				(isMappingContainer(value) || isSequenceContainer(value)) {
				child.schema = true
				child.schemaDeclaration = true
				child.schemaAmbiguous = !valueUnique
			} else {
				child = schemaContext{sensitive: c.sensitive || IsSensitiveName(key), data: true}
			}
		}
	}
	if c.schemaDeclarationMap && !c.dependencies && !c.data && !c.sensitive && !c.metadata {
		child.schemaDeclaration = true
		child.schemaAmbiguous = !valueUnique
	}
	if c.credentialControls && !c.schemaDeclarationMap && !c.schemaDeclaration && keyUnique {
		switch v := value.(type) {
		case bool, nil:
			if key == "enabled" {
				child.sensitive = false
				child.credentialControl = true
				child.controlKey = Normalize(key)
			}
		case string:
			if key == "policy" && v == "strict" {
				child.sensitive = false
				child.credentialControl = true
				child.controlKey = Normalize(key)
			}
		}
	}
	if valueUnique && keyUnique && !c.sensitive && !c.data && !c.metadata && (!c.schema || c.schemaAmbiguous) && !c.properties && !c.schemaDeclarationMap && !c.schemaDeclaration && Normalize(key) == "password" {
		if _, ok := value.(map[string]any); ok {
			child.credentialControls = true
		}
	}
	// A flow name can also name an actual authorization-code credential.
	// Only a complete, well-shaped metadata object changes that interpretation.
	// Credential ancestors and schema value annotations never gain exemptions.
	if valueUnique && keyUnique && !c.sensitive && !c.data && !c.properties && !c.schemaDeclarationMap && !c.schemaDeclaration && !c.metadata && validOAuthFlow(key, value) {
		return schemaContext{oauthFlow: true}
	}
	if c.schema && !c.schemaAmbiguous && !c.data && !c.metadata && !c.properties && schemaMetadataKeyword(Normalize(key)) && (!valueUnique || !keyUnique || !validSchemaMetadata(Normalize(key), value)) {
		return schemaContext{data: true, sensitive: c.sensitive || IsSensitiveName(key)}
	}
	return child
}

// OAuth flow objects describe endpoints and named scope descriptions. Their
// scalar values still undergo provider, assignment and encoded-value inspection.
var oauthBearerMethods = []string{"header", "body", "query"}

var oauthFlowNames = []string{"authorizationCode", "clientCredentials", "implicit", "password", "deviceCode"}

func metadataMap(value any) (map[string]any, bool) {
	if n, ok := value.(*yaml.Node); ok {
		if n.Kind != yaml.MappingNode || len(n.Content)%2 != 0 {
			return nil, false
		}
		out := make(map[string]any, len(n.Content)/2)
		for i := 0; i < len(n.Content); i += 2 {
			key, ok := metadataScalar(n.Content[i]).(string)
			if !ok {
				return nil, false
			}
			if _, duplicate := out[key]; duplicate {
				return nil, false
			}
			out[key] = n.Content[i+1]
		}
		return out, true
	}
	m, ok := value.(map[string]any)
	return m, ok
}

// Protected-resource discovery has its own shape. Do not infer metadata
// solely from an authorization or bearer field name in an arbitrary payload.
func (c schemaContext) discovery(value any) schemaContext {
	if c.sensitive || c.data || c.metadata || c.properties {
		return c
	}
	m, ok := metadataMap(value)
	if !ok {
		return c
	}
	issuer, issuerOK := metadataScalar(m["issuer"]).(string)
	if issuerOK && absoluteMetadataURI(issuer) {
		authorize, aOK := metadataScalar(m["authorization_endpoint"]).(string)
		token, tOK := metadataScalar(m["token_endpoint"]).(string)
		flag, present := m["authorization_response_iss_parameter_supported"]
		_, boolean := metadataScalar(flag).(bool)
		c.oauthIssuer = aOK && tOK && absoluteMetadataURI(authorize) && absoluteMetadataURI(token) && (!present || boolean)
	}
	resource, ok := metadataScalar(m["resource"]).(string)
	if !ok {
		return c
	}
	uri, err := url.Parse(resource)
	if err != nil || !uri.IsAbs() {
		return c
	}
	servers, exists := m["authorization_servers"]
	if !exists || !metadataStringsMatching(servers, absoluteMetadataURI) {
		return c
	}
	if methods, exists := m["bearer_methods_supported"]; exists && !metadataStringsMatching(methods, func(method string) bool {
		for _, known := range oauthBearerMethods {
			if method == known {
				return true
			}
		}
		return false
	}) {
		return c
	}
	c.oauthResource = true
	return c
}

func metadataStringsMatching(value any, valid func(string) bool) bool {
	if !metadataStrings(value, false) {
		return false
	}
	if n, ok := value.(*yaml.Node); ok {
		for _, node := range n.Content {
			if !valid(metadataScalar(node).(string)) {
				return false
			}
		}
		return true
	}
	for _, value := range value.([]any) {
		if !valid(metadataScalar(value).(string)) {
			return false
		}
	}
	return true
}

func absoluteMetadataURI(text string) bool {
	uri, err := url.Parse(text)
	return err == nil && uri.IsAbs()
}

func validOAuthFlow(name string, value any) bool {
	known := false
	for _, flow := range oauthFlowNames {
		known = known || name == flow
	}
	if !known {
		return false
	}
	m, ok := metadataMap(value)
	if !ok {
		return false
	}
	scopes, ok := metadataMap(m["scopes"])
	if !ok {
		return false
	}
	for _, description := range scopes {
		if _, ok := metadataScalar(description).(string); !ok {
			return false
		}
	}
	for key, endpoint := range m {
		switch key {
		case "scopes":
		case "authorizationUrl", "tokenUrl", "refreshUrl":
			if text, ok := metadataScalar(endpoint).(string); !ok || !absoluteMetadataURI(text) {
				return false
			}
		default:
			return false
		}
	}
	_, authorization := m["authorizationUrl"]
	_, token := m["tokenUrl"]
	switch name {
	case "authorizationCode":
		return authorization && token
	case "implicit":
		return authorization
	default:
		return token
	}
}

func metadataScalar(value any) any {
	if n, ok := value.(*yaml.Node); ok {
		if n.Kind != yaml.ScalarNode {
			return nil
		}
		switch n.Tag {
		case "!!str":
			return n.Value
		case "!!bool":
			var b bool
			if n.Decode(&b) == nil {
				return b
			}
		case "!!int", "!!float":
			var f float64
			if n.Decode(&f) == nil {
				return f
			}
		}
		return nil
	}
	return value
}

func metadataStrings(value any, types bool) bool {
	seen := make(map[string]bool)
	valid := func(v any) bool {
		s, ok := metadataScalar(v).(string)
		if !ok || seen[s] {
			return false
		}
		if types && s != "null" && s != "boolean" && s != "object" && s != "array" && s != "number" && s != "string" && s != "integer" {
			return false
		}
		seen[s] = true
		return true
	}
	if n, ok := value.(*yaml.Node); ok {
		if n.Kind != yaml.SequenceNode {
			return false
		}
		for _, v := range n.Content {
			if !valid(v) {
				return false
			}
		}
	} else {
		a, ok := value.([]any)
		if !ok {
			return false
		}
		for _, v := range a {
			if !valid(v) {
				return false
			}
		}
	}
	return !types || len(seen) > 0
}

func validSchemaMetadata(key string, value any) bool {
	scalar := metadataScalar(value)
	switch key {
	case "$vocabulary", "dependentRequired":
		valid := func(name string, v any) bool {
			if key == "dependentRequired" {
				return metadataStrings(v, false)
			}
			uri, err := url.Parse(name)
			if err != nil || !uri.IsAbs() {
				return false
			}
			_, ok := metadataScalar(v).(bool)
			return ok
		}
		if n, ok := value.(*yaml.Node); ok {
			if n.Kind != yaml.MappingNode || len(n.Content)%2 != 0 {
				return false
			}
			seen := make(map[string]bool)
			for i := 0; i < len(n.Content); i += 2 {
				name, ok := metadataScalar(n.Content[i]).(string)
				if !ok || seen[name] || !valid(name, n.Content[i+1]) {
					return false
				}
				seen[name] = true
			}
			return true
		}
		m, ok := value.(map[string]any)
		if !ok {
			return false
		}
		for name, v := range m {
			if !valid(name, v) {
				return false
			}
		}
		return true
	case "required":
		return metadataStrings(value, false)
	case "type":
		if s, ok := scalar.(string); ok {
			return metadataStrings([]any{s}, true)
		}
		return metadataStrings(value, true)
	case "$recursiveAnchor", "readOnly", "writeOnly", "deprecated", "uniqueItems":
		_, ok := scalar.(bool)
		return ok
	case "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf", "minLength", "maxLength", "minItems", "maxItems", "minContains", "maxContains", "minProperties", "maxProperties":
		var number float64
		switch n := scalar.(type) {
		case json.Number:
			var err error
			number, err = n.Float64()
			if err != nil {
				return false
			}
		case float64:
			number = n
		default:
			return false
		}
		if math.IsNaN(number) || math.IsInf(number, 0) {
			return false
		}
		switch key {
		case "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum":
			return true
		case "multipleOf":
			return number > 0
		default:
			return number >= 0 && math.Trunc(number) == number
		}
	default:
		// All remaining listed metadata keywords are strings. Unknown keywords
		// never reach this validator and never gain an exemption.
		_, ok := scalar.(string)
		return ok
	}
}

// Parse collections rather than treating a flow mapping as a scalar. Clean
// documents retain exact source bytes; changed documents are serialized from
// the inspected nodes. Invalid YAML falls back to tolerant raw inspection.
// Anchors/aliases, duplicate/complex keys and resource violations fail closed.
// Alias targets are never expanded or used to restore inspected content.
func (p *inspector) yamlSchema(s string, depth int) (inspection, bool) {
	var document yaml.Node
	decoder := yaml.NewDecoder(strings.NewReader(s))
	if decoder.Decode(&document) != nil || len(document.Content) != 1 {
		return inspection{}, false
	}
	var trailing yaml.Node
	if decoder.Decode(&trailing) != io.EOF {
		return inspection{}, false
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode && root.Kind != yaml.SequenceNode {
		return inspection{}, false
	}
	seen, invalid := false, false
	shape := ""
	var walk func(*yaml.Node, schemaContext, int)
	walk = func(n *yaml.Node, c schemaContext, d int) {
		if d > maxInspectionDepth || p.remaining < 1 {
			invalid = true
			return
		}
		p.remaining--
		if n.Anchor != "" || n.Kind == yaml.AliasNode {
			// Treat identifiers as text too. Conservative refusal prevents both
			// alias-context leaks and restoration of an already redacted scalar.
			name := n.Anchor
			if n.Kind == yaml.AliasNode {
				name = n.Value
			}
			if r := p.text(name, false, d+1); r.shape != "" {
				shape = r.shape
			}
			invalid = true
			if n.Kind == yaml.AliasNode {
				return // Never follow n.Alias, including cyclic graphs.
			}
		}
		if n.Tag != "" && n.Tag != "!!map" && n.Tag != "!!seq" && n.Tag != "!!str" && n.Tag != "!!int" && n.Tag != "!!float" && n.Tag != "!!bool" && n.Tag != "!!null" && n.Tag != "!!timestamp" {
			invalid = true
		}
		for _, comment := range []*string{&n.HeadComment, &n.LineComment, &n.FootComment} {
			if *comment != "" {
				r := p.text(*comment, false, d+1)
				if r.shape != "" {
					*comment = r.text
					shape = r.shape
				}
			}
		}
		switch n.Kind {
		case yaml.MappingNode:
			c = c.mapping().discovery(n)
			for i := 0; i+1 < len(n.Content); i += 2 {
				c = c.recognize(n.Content[i].Value)
			}
			seen = seen || c.schema || c.properties || c.oauthFlow || c.oauthScopes || c.oauthResource || c.oauthIssuer || c.sensitive
			keys := make(map[string]bool)
			for i := 0; i+1 < len(n.Content); i += 2 {
				key := n.Content[i]
				if key.Kind != yaml.ScalarNode || keys[key.Value] || key.Tag != "!!str" {
					invalid = true
					continue
				}
				keys[key.Value] = true
				name := key.Value
				walk(key, schemaContext{metadata: true}, d+1)
				walk(n.Content[i+1], c.childValue(name, n.Content[i+1]), d+1)
			}
		case yaml.DocumentNode, yaml.SequenceNode:
			if n.Kind == yaml.SequenceNode && c.dependency {
				if metadataStrings(n, false) {
					c = schemaContext{metadata: true}
				} else {
					c.data = true
					c.dependency = false
				}
			}
			for _, child := range n.Content {
				walk(child, c, d+1)
			}
		case yaml.ScalarNode:
			before := p.schemaData
			p.schemaData = c.data
			r := p.text(n.Value, c.scalarSensitive(n.Tag == "!!bool"), d+1)
			p.schemaData = before
			if r.shape != "" {
				n.Value = r.text
				n.Tag = "!!str"
				n.Style = yaml.DoubleQuotedStyle
				shape = r.shape
			}
		default:
			invalid = true
		}
	}
	walk(&document, schemaContext{}, depth+1)
	if !seen {
		return inspection{}, false
	}
	if invalid {
		return inspection{"<redacted ambiguous YAML>", "ambiguous YAML"}, true
	}
	if shape == "" {
		return inspection{s, ""}, true
	}
	encoded, err := yaml.Marshal(&document)
	if err != nil {
		return inspection{"<redacted ambiguous YAML>", "ambiguous YAML"}, true
	}
	return inspection{string(encoded), shape}, true
}
