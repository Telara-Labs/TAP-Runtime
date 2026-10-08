package redact

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
	"github.com/Telara-Labs/TAP-Runtime/discover/util"
)

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
	{"JWT", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
	{"AWS access key", regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"AWS secret", regexp.MustCompile(`(?i)aws_secret_access_key\s*[=:]\s*[^\s'"]{16,}`)},
	{"GitHub token", regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{40,})`)},
	{"GitLab token", regexp.MustCompile(`\bgl(pat|dt|rt|cbt|ptt|oas|soat|ffct)-[A-Za-z0-9_-]{16,}`)},
	{"Slack token", regexp.MustCompile(`\bxox[abprsoe]-[A-Za-z0-9-]{10,}`)},
	{"Stripe key", regexp.MustCompile(`\b(sk|rk)_(live|test)_[A-Za-z0-9]{16,}`)},
	{"model provider key", regexp.MustCompile(`\bsk-(ant-|proj-)?[A-Za-z0-9_-]{20,}`)},
	{"Google key", regexp.MustCompile(`\b(AIza[0-9A-Za-z_-]{35}|ya29\.[0-9A-Za-z_-]{20,})`)},
	{"Telara key", regexp.MustCompile(`\btlr[a-z]{0,3}_[A-Za-z0-9]{16,}`)},
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

// UserFlagPrograms take -u / --user as user:password.
var UserFlagPrograms = map[string]bool{"curl": true, "wget": true, "http": true, "https": true, "xh": true}

// SecretShape names the credential shape v contains, or "".
func SecretShape(v string) string {
	for _, s := range SecretShapes {
		if s.re.MatchString(v) {
			return s.name
		}
	}
	return ""
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
	if SensitiveName.MatchString(strings.TrimSuffix(name, "=")) {
		return true
	}
	if (name == "-u=" || name == "--user=") && UserFlagPrograms[strings.SplitN(strings.TrimPrefix(label, "sh:"), " ", 2)[0]] && strings.Contains(sl.Value, ":") {
		return true
	}
	return SecretShape(sl.Value) != ""
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
	sensitive := SensitiveName.MatchString(name)
	if value, ok := structured(s); ok {
		out, changed := redactValue(value, sensitive)
		if !changed {
			return s
		}
		var b bytes.Buffer
		e := json.NewEncoder(&b)
		e.SetEscapeHTML(false)
		if e.Encode(out) == nil {
			return strings.TrimSuffix(b.String(), "\n")
		}
	}
	if sensitive && s != "" && !variableReference(s) {
		return "<redacted credential>"
	}
	return redactText(s)
}

// Only a complete reference is safe: ${TOKEN:-literal-secret} also contains
// a credential. Do not interpret expressions or read the environment.
var reference = regexp.MustCompile(`^(\$[A-Za-z_][A-Za-z0-9_]*|\$\{[A-Za-z_][A-Za-z0-9_]*\}|\{env:[A-Za-z_][A-Za-z0-9_]*\})$`)

// An assignment word can concatenate quoted/unquoted parts. Keep its exact
// surrounding command bytes; only credential-bearing words are replaced.
var assignmentWord = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_-]*)\s*([=:])\s*((?:(?i:Bearer|Basic)\s+)?(?:"(?:\\.|[^"\\])*"|'[^']*'|\\.|[^\s;|&"'\\])+)`)

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
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
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

func redactValue(value any, sensitive bool) (any, bool) {
	changed := false
	switch v := value.(type) {
	case map[string]any:
		for k, old := range v {
			next, dirty := redactValue(old, sensitive || SensitiveName.MatchString(k))
			v[k], changed = next, changed || dirty
		}
	case []any:
		for i, old := range v {
			next, dirty := redactValue(old, sensitive)
			v[i], changed = next, changed || dirty
		}
	case string:
		name := ""
		if sensitive {
			name = "credential"
		}
		next := Argument(name, v)
		return next, next != v
	case nil:
		return value, false
	default:
		if sensitive {
			return "<redacted credential>", true
		}
	}
	return value, changed
}

func redactText(s string) string {
	// Cover short opaque env assignments too, preserving the rest of the
	// command and complete variable references. Include named text fields.
	if strings.ContainsAny(s, "=:") && assignmentWord.MatchString(s) {
		s = assignmentWord.ReplaceAllStringFunc(s, func(word string) string {
			parts := assignmentWord.FindStringSubmatch(word)
			if SensitiveName.MatchString(parts[1]) && !variableReference(strings.Trim(parts[3], `'"`)) {
				return parts[1] + parts[2] + "'<redacted credential>'"
			}
			return word
		})
	}
	for _, sh := range SecretShapes {
		if sh.name == "assigned secret" {
			// Named values above include short and quoted credentials and
			// preserve references; the old length-only shape does neither.
			continue
		}
		if sh.re.MatchString(s) {
			s = sh.re.ReplaceAllString(s, "<redacted "+sh.name+">")
		}
	}
	return s
}

// ScanArtifacts returns one finding per file line that still looks like a
// credential. It never returns the credential itself.
func ScanArtifacts(files map[string][]byte) []string {
	var out []string
	for name, body := range files {
		for i, line := range strings.Split(string(body), "\n") {
			if sh := SecretShape(line); sh != "" {
				out = append(out, name+" line "+util.Itoa(i+1)+": "+sh)
			}
		}
	}
	sort.Strings(out)
	return out
}
