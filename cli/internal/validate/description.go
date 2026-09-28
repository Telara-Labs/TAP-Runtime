package validate

import (
	"regexp"
	"strings"
	"unicode"

	"telara.dev/tap/internal/diag"
)

// Description rules per 03-primitive-structure.md §3.1. These become the
// projected MCP tool's description, which makes them attack surface (tool
// poisoning), so the rules are enforced, not advisory. Findings are filed
// under diag.ClassContent (04-cli.md §3 / CHANGELOG.md v1 CLI fix item 6),
// alongside the credential-slot content rules in manifest.go.

const maxDescriptionLen = 1024

var (
	urlRe        = regexp.MustCompile(`(?i)\bhttps?://\S+`)
	uuidRe       = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	tokenRe      = regexp.MustCompile(`\b(sk-|ghp_|gho_|xox[baprs]-|Bearer\s)[A-Za-z0-9_\-]{8,}`)
	injectionRes = []*regexp.Regexp{
		regexp.MustCompile(`(?i)ignore (all |any )?(previous|prior|above) instructions`),
		regexp.MustCompile(`(?i)disregard (the |any )?(system|previous|prior) prompt`),
		regexp.MustCompile(`(?i)\byou must\b.*\b(always|never)\b`),
		regexp.MustCompile(`(?i)do not (tell|inform|notify) the user`),
		regexp.MustCompile(`(?i)act as (if|though) you (are|were)`),
		regexp.MustCompile(`(?i)override (your|the) (system|safety|guard)`),
		regexp.MustCompile(`(?i)\bif this fails,?\s*try\b`), // platform rule: never teach the model workarounds
	}
)

// CheckDescription runs the 03 §3.1 rules against a single description
// string (metadata.description, output_description, or a field-level
// schema description) found at path.
func CheckDescription(path, text string) diag.Findings {
	var out diag.Findings
	if text == "" {
		return out
	}
	if len([]rune(text)) > maxDescriptionLen {
		out = append(out, diag.Error(diag.ClassContent, "description-too-long", path,
			"description exceeds 1024 characters (Skills-convention cap)",
			"shorten the description; move detail into README.md"))
	}
	if strings.Contains(text, "{{") {
		out = append(out, diag.Error(diag.ClassContent, "description-double-brace", path,
			"description contains '{{' (template-injection risk / non-native binding dialect)",
			"rewrite without template syntax"))
	}
	if urlRe.MatchString(text) {
		out = append(out, diag.Error(diag.ClassContent, "description-embedded-url", path,
			"description contains a URL to a live resource; describe shape/effect, never instance data",
			"remove the URL; describe what the primitive does in general terms"))
	}
	if uuidRe.MatchString(text) || tokenRe.MatchString(text) {
		out = append(out, diag.Error(diag.ClassContent, "description-embedded-id", path,
			"description contains an ID/token-shaped literal; IDs in descriptions cause LLM hallucination (platform rule)",
			"remove the literal ID/token; describe the shape instead"))
	}
	for _, re := range injectionRes {
		if re.MatchString(text) {
			out = append(out, diag.Error(diag.ClassContent, "description-injection-pattern", path,
				"description contains an imperative pattern resembling a prompt-injection payload",
				"describe shape and effect only; never instruct the model to compensate for behavior"))
			break
		}
	}
	if r, bad := findInvisibleOrHomoglyph(text); bad {
		out = append(out, diag.Error(diag.ClassContent, "description-invisible-unicode", path,
			"description contains invisible/formatting or mixed-script (homoglyph) unicode: U+"+runeHex(r),
			"remove non-printing characters and keep the script consistent"))
	}
	return out
}

func runeHex(r rune) string {
	const hexdigits = "0123456789ABCDEF"
	if r == 0 {
		return "0000"
	}
	var b []byte
	n := uint32(r)
	for n > 0 {
		b = append([]byte{hexdigits[n%16]}, b...)
		n /= 16
	}
	return string(b)
}

// invisibleRunes are zero-width/formatting code points with no legitimate
// use in a tool description: ZERO WIDTH SPACE, ZERO WIDTH NON-JOINER, ZERO
// WIDTH JOINER, LEFT-/RIGHT-TO-LEFT MARK, WORD JOINER, BOM, SOFT HYPHEN.
// Written as explicit \u escapes rather than literal glyphs so the source
// file itself stays plain ASCII and unambiguous.
var invisibleRunes = map[rune]bool{
	'\u200b': true, // zero width space
	'\u200c': true, // zero width non-joiner
	'\u200d': true, // zero width joiner
	'\u200e': true, // left-to-right mark
	'\u200f': true, // right-to-left mark
	'\u2060': true, // word joiner
	'\ufeff': true, // BOM
	'\u00ad': true, // soft hyphen
}

// findInvisibleOrHomoglyph flags zero-width formatting characters outright,
// and flags mixed-script text (Latin mixed with Cyrillic/Greek) as a
// pragmatic homoglyph-attack proxy -- a full Unicode-confusables table is
// out of scope for v0.1 (recorded as a FINDING).
func findInvisibleOrHomoglyph(s string) (rune, bool) {
	sawLatin, sawCyrillicOrGreek := false, false
	var offender rune
	for _, r := range s {
		if invisibleRunes[r] {
			return r, true
		}
		if unicode.Is(unicode.Latin, r) {
			sawLatin = true
		}
		if unicode.Is(unicode.Cyrillic, r) || unicode.Is(unicode.Greek, r) {
			sawCyrillicOrGreek = true
			offender = r
		}
	}
	if sawLatin && sawCyrillicOrGreek {
		return offender, true
	}
	return 0, false
}
