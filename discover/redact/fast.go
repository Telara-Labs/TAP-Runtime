package redact

import "strings"

// plainASCII is a sufficient condition for the absence of every local-profile
// credential rule, never a detector. Non-ASCII falls back before normalization.
// Every assignment/query/structured/fragment/YAML rule needs one of the excluded
// punctuation bytes; continuations need a backslash. All provider formats need
// '-' or '_' except JWT (eyJ), AWS (AKIA/ASIA), Google (AIza/ya29.), and the
// case-insensitive bearer/basic schemes. Reject those prefixes even inside words.
// Numeric JSON with exponents or long mantissas may fail finite-double
// canonicalization. Short exponent-free mantissas are finite, even if the
// structured decoder interprets them as numbers rather than prose.
// Metadata profiles have additional address rules and do not use this predicate.
func plainASCII(s string) bool {
	trimmed := strings.TrimSpace(s)
	if len(trimmed) > 0 && trimmed[0] >= '0' && trimmed[0] <= '9' && (len(trimmed) > 16 || strings.ContainsAny(trimmed, "eE")) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x80 || c < 0x20 && c != '\n' && c != '\r' && c != '\t' {
			return false
		}
		switch c {
		case '_', '-', ':', '=', '{', '[', '"', '\'', '\\', '?', '&':
			return false
		case 'e':
			if strings.HasPrefix(s[i:], "eyJ") {
				return false
			}
		case 'A':
			if strings.HasPrefix(s[i:], "AKIA") || strings.HasPrefix(s[i:], "ASIA") || strings.HasPrefix(s[i:], "AIza") {
				return false
			}
		case 'y':
			if strings.HasPrefix(s[i:], "ya29.") {
				return false
			}
		case 'b', 'B':
			if len(s)-i >= 5 && strings.EqualFold(s[i:i+5], "basic") || len(s)-i >= 6 && strings.EqualFold(s[i:i+6], "bearer") {
				return false
			}
		}
	}
	return true
}

// simpleJSON validates unchanged JSON with the same candidate scan as text.
// Fragments are parsed in place. Duplicate keys, sensitive-name candidates,
// uncertain numbers/escapes and bounded nesting select complete inspection.
// A decoded string is allocated only when nontrivial escapes require it.
func simpleJSON(s string) bool {
	p := simpleParser{s: s}
	return p.value(0) && p.space() == len(s)
}

type simpleParser struct {
	s     string
	at    int
	depth int
}

func (p *simpleParser) space() int {
	for p.at < len(p.s) && (p.s[p.at] == ' ' || p.s[p.at] == '\n' || p.s[p.at] == '\r' || p.s[p.at] == '\t') {
		p.at++
	}
	return p.at
}

func (p *simpleParser) quoted(key bool) (string, bool) {
	if p.at >= len(p.s) || p.s[p.at] != '"' {
		return "", false
	}
	start := p.at
	escaped := false
	for p.at++; p.at < len(p.s); p.at++ {
		c := p.s[p.at]
		if c == '\\' {
			escaped = true
			if p.at+1 >= len(p.s) || !strings.ContainsRune(`"\\/bfnrt`, rune(p.s[p.at+1])) {
				return "", false
			}
			p.at++
			continue
		}
		if c < 0x20 || c >= 0x80 {
			return "", false
		}
		if c != '"' {
			continue
		}
		p.at++
		value := p.s[start+1 : p.at-1]
		if escaped && !key && plainEscapedASCII(value) {
			return value, true
		}
		if escaped {
			var out strings.Builder
			out.Grow(len(value))
			last := 0
			for i := 0; i < len(value); i++ {
				if value[i] != '\\' {
					continue
				}
				out.WriteString(value[last:i])
				i++
				switch value[i] {
				case 'n':
					out.WriteByte('\n')
				case 'r':
					out.WriteByte('\r')
				case 't':
					out.WriteByte('\t')
				case '"', '/', '\\':
					out.WriteByte(value[i])
				default:
					return "", false
				}
				last = i + 1
			}
			out.WriteString(value[last:])
			value = out.String()
		}
		return value, candidateText(value, p.depth+1)
	}
	return "", false
}

func (p *simpleParser) value(depth int) bool {
	p.depth = depth
	if depth > 16 || p.space() >= len(p.s) {
		return false
	}
	switch p.s[p.at] {
	case '"':
		_, ok := p.quoted(false)
		return ok
	case '{':
		p.at++
		if p.space() < len(p.s) && p.s[p.at] == '}' {
			p.at++
			return true
		}
		// Small response envelopes need no allocation. Large objects fall back
		// rather than doing quadratic duplicate comparisons or allocating a tree.
		var keys [16]string
		count := 0
		for {
			p.space()
			key, ok := p.quoted(true)
			if !ok || count == len(keys) {
				return false
			}
			for _, prior := range keys[:count] {
				if prior == key {
					return false
				}
			}
			keys[count] = key
			count++
			if p.space() >= len(p.s) || p.s[p.at] != ':' {
				return false
			}
			p.at++
			if !p.value(depth+1) || p.space() >= len(p.s) {
				return false
			}
			c := p.s[p.at]
			p.at++
			if c == '}' {
				return true
			}
			if c != ',' {
				return false
			}
		}
	case '[':
		p.at++
		if p.space() < len(p.s) && p.s[p.at] == ']' {
			p.at++
			return true
		}
		for {
			if !p.value(depth+1) || p.space() >= len(p.s) {
				return false
			}
			c := p.s[p.at]
			p.at++
			if c == ']' {
				return true
			}
			if c != ',' {
				return false
			}
		}
	default:
		for _, literal := range [...]string{"null", "true", "false"} {
			if strings.HasPrefix(p.s[p.at:], literal) {
				p.at += len(literal)
				return true
			}
		}
		if p.s[p.at] >= '0' && p.s[p.at] <= '9' {
			p.at++
			return true
		}
		return false
	}
}

// JSONUnchanged proves a restricted ASCII JSON document needs no redaction.
// False means unknown, and callers must run Field on the complete source. The
// predicate validates the whole document, duplicate keys and each decoded leaf;
// it never exposes a partial result or relies on a provenance/trust marker.
// The byte slice is borrowed only for this call and never retained.
func JSONUnchanged(raw []byte) bool { return fastRulesCurrent() && simpleJSON(string(raw)) }

// ASCII without vertical tab/form feed is stable under both normalizers.
func normalizationStableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 || s[i] == '\v' || s[i] == '\f' {
			return false
		}
	}
	return true
}

// SecretShapes is historically mutable. A caller adding or replacing a rule
// disables the proof shortcut until that rule has an explicit trigger proof.
var fastShapes = append(SecretShapes[:0:0], SecretShapes...)
var fastSensitiveName = SensitiveName
var fastSensitivePattern = SensitiveName.String()
var fastShapePatterns = func() []string {
	out := make([]string, len(SecretShapes))
	for i, s := range SecretShapes {
		out[i] = s.re.String()
	}
	return out
}()

func fastRulesCurrent() bool {
	if SensitiveName != fastSensitiveName || SensitiveName.String() != fastSensitivePattern || len(SecretShapes) != len(fastShapes) {
		return false
	}
	for i, shape := range fastShapes {
		if SecretShapes[i].re != shape.re || SecretShapes[i].name != shape.name || SecretShapes[i].re.String() != fastShapePatterns[i] {
			return false
		}
	}
	return true
}

// candidateText scans ordinary text once without allocating lines or tokens.
// Credential words and provider prefixes select the complete recognizers. JSON
// fragments are validated in place, once each, including duplicate keys. False
// means uncertain, never safe. The root uses this only with a fresh budget.
func candidateText(s string, depth int) bool {
	if depth > 16 {
		return false
	}
	trimmed := strings.TrimSpace(s)
	if len(trimmed) > 0 && (trimmed[0] >= '0' && trimmed[0] <= '9' || trimmed[0] == '-') && (len(trimmed) > 16 || strings.ContainsAny(trimmed, "eE")) {
		// Negative prose is harmless, but an uncertain numeric root can fail
		// finite-double canonicalization and must retain that refusal.
		if trimmed[0] != '-' || len(trimmed) > 1 && trimmed[1] >= '0' && trimmed[1] <= '9' {
			return false
		}
	}
	yamlHead := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x80 || c < 0x20 && c != '\n' && c != '\r' && c != '\t' {
			return false
		}
		if candidatePrefix(s[i:]) {
			return false
		}
		switch c {
		case '\\', '%', '@', '&', '!', '?', '$', '"':
			// Escapes can reconstruct names, queries or continuations. Userinfo,
			// encoded query names and YAML aliases/tags keep complete inspection.
			return false
		case '{', '[':
			p := simpleParser{s: s, at: i, depth: depth}
			if !p.value(depth + 1) {
				return false
			}
			i = p.at - 1
			yamlHead = false
		case '\'':
			// Single-quoted source and YAML have different escape rules.
			return false
		case ':':
			if yamlHead && (i+1 == len(s) || asciiSpace(s[i+1])) {
				return false
			}
		case '\n', '\r':
			yamlHead = true
		default:
			if !(nameLetter(c) || c == '_' || c == '-' || c == ' ' || c == '\t' || c == '\r') {
				yamlHead = false
			}
		}
	}
	return true
}

// All credential-name grammars contain one of these words, including mixed
// separators, qualified names and camel case. Prefixes are deliberately checked
// at every byte, so adjacency never shields a value. Broad matches merely cost
// the slow path and cannot change the ruleset's decisions.
func candidatePrefix(s string) bool {
	c := s[0] | 0x20
	switch c {
	case 'a':
		return foldPrefix(s, "auth") || foldPrefix(s, "akia") || foldPrefix(s, "asia") || foldPrefix(s, "aiza")
	case 'b':
		return foldPrefix(s, "bearer") || foldPrefix(s, "basic")
	case 'c':
		return foldPrefix(s, "cookie") || foldPrefix(s, "credential")
	case 'd':
		return foldPrefix(s, "devicecode") || foldPrefix(s, "device_code")
	case 'e':
		return strings.HasPrefix(s, "eyJ")
	case 'g':
		return strings.HasPrefix(s, "github_pat_") || strings.HasPrefix(s, "gh") || strings.HasPrefix(s, "gl") || strings.HasPrefix(s, "gsk")
	case 'k':
		return foldPrefix(s, "key")
	case 'p':
		return foldPrefix(s, "pass") || foldPrefix(s, "pwd")
	case 'r':
		return strings.HasPrefix(s, "rk_")
	case 's':
		return foldPrefix(s, "secret") || foldPrefix(s, "sig") || strings.HasPrefix(s, "sk-") || strings.HasPrefix(s, "sk_")
	case 't':
		return foldPrefix(s, "token") || strings.HasPrefix(s, "tlr")
	case 'x':
		return strings.HasPrefix(s, "xox")
	case 'y':
		return strings.HasPrefix(s, "ya29.")
	}
	return false
}

func foldPrefix(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	for i := range prefix {
		if s[i] != prefix[i] && s[i]|0x20 != prefix[i] {
			return false
		}
	}
	return true
}

// The original punctuation-light proof needs no decoded allocation for JSON
// whitespace escapes. Values cannot introduce credential ancestry. Keys use
// the candidate/name proof instead.
func plainEscapedASCII(value string) bool {
	for {
		slash := strings.IndexByte(value, '\\')
		part := value
		if slash >= 0 {
			part = value[:slash]
		}
		if !plainASCII(part) {
			return false
		}
		if slash < 0 {
			return true
		}
		if slash+1 >= len(value) || value[slash+1] != 'n' && value[slash+1] != 'r' && value[slash+1] != 't' {
			return false
		}
		value = value[slash+2:]
	}
}
