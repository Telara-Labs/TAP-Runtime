// Package glob matches the wildcard patterns a manifest may declare: command
// arguments, environment names, file paths and fetch hosts. Doc 34 section
// 13.16, rulings 31 to 34.
//
// The patterns are bash-style: * matches any run of characters, ? matches
// one, [...] matches one of a set. What a run of characters may contain
// depends on what is being matched, so each kind has its own function.
package glob

import "strings"

// HasMeta reports whether s contains a wildcard.
func HasMeta(s string) bool { return strings.ContainsAny(s, "*?[") }

// match reports whether name matches pattern. stop is a byte that * and ?
// never match, or 0 for none: '/' keeps a path wildcard inside one segment,
// '.' keeps a host wildcard inside one label.
func match(pattern, name string, stop byte) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case '*':
			for len(pattern) > 0 && pattern[0] == '*' {
				pattern = pattern[1:]
			}
			if len(pattern) == 0 {
				return stop == 0 || strings.IndexByte(name, stop) < 0
			}
			for i := 0; i <= len(name); i++ {
				if match(pattern, name[i:], stop) {
					return true
				}
				if i < len(name) && stop != 0 && name[i] == stop {
					return false
				}
			}
			return false
		case '?':
			if len(name) == 0 || (stop != 0 && name[0] == stop) {
				return false
			}
			pattern, name = pattern[1:], name[1:]
		case '[':
			end := strings.IndexByte(pattern[1:], ']')
			if end < 1 || len(name) == 0 {
				// An unclosed or empty class is a literal bracket.
				if len(name) == 0 || name[0] != '[' {
					return false
				}
				pattern, name = pattern[1:], name[1:]
				continue
			}
			class := pattern[1 : 1+end]
			negate := class[0] == '!' || class[0] == '^'
			if negate {
				class = class[1:]
			}
			in := false
			for i := 0; i < len(class); i++ {
				if i+2 < len(class) && class[i+1] == '-' {
					if class[i] <= name[0] && name[0] <= class[i+2] {
						in = true
					}
					i += 2
					continue
				}
				if class[i] == name[0] {
					in = true
				}
			}
			if in == negate {
				return false
			}
			pattern, name = pattern[2+end:], name[1:]
		default:
			if len(name) == 0 || pattern[0] != name[0] {
				return false
			}
			pattern, name = pattern[1:], name[1:]
		}
	}
	return len(name) == 0
}

// Word matches one command argument or one environment name. A wildcard
// matches any character, including a slash: an argument is one word whatever
// it contains.
func Word(pattern, word string) bool { return match(pattern, word, 0) }

// Args matches a command's arguments against one declared pattern. Each
// pattern word is matched against one argument. A final bare * matches
// whatever remains, including nothing (ruling 31).
func Args(pattern, args []string) bool {
	rest := len(pattern) > 0 && pattern[len(pattern)-1] == "*"
	if rest {
		pattern = pattern[:len(pattern)-1]
		if len(args) < len(pattern) {
			return false
		}
	} else if len(args) != len(pattern) {
		return false
	}
	for i, p := range pattern {
		if !Word(p, args[i]) {
			return false
		}
	}
	return true
}

// Path matches a slash-separated path against a pattern. * and ? stay within
// one segment. A segment that is exactly ** matches any number of segments,
// including none (ruling 33).
func Path(pattern, path string) bool {
	return segments(strings.Split(strings.Trim(pattern, "/"), "/"), strings.Split(strings.Trim(path, "/"), "/"))
}

func segments(pattern, path []string) bool {
	if len(pattern) == 0 {
		return len(path) == 0
	}
	if pattern[0] == "**" {
		for i := 0; i <= len(path); i++ {
			if segments(pattern[1:], path[i:]) {
				return true
			}
		}
		return false
	}
	if len(path) == 0 || !match(pattern[0], path[0], '/') {
		return false
	}
	return segments(pattern[1:], path[1:])
}

// Host matches a host name against a declared one. The only wildcard allowed
// is a leading "*.", which matches exactly one label (ruling 34):
// *.atlassian.net matches telara.atlassian.net and neither atlassian.net nor
// a.b.atlassian.net.
func Host(pattern, host string) bool {
	pattern, host = strings.ToLower(pattern), strings.ToLower(host)
	if !strings.HasPrefix(pattern, "*.") {
		return pattern == host
	}
	suffix := pattern[1:] // ".atlassian.net"
	if !strings.HasSuffix(host, suffix) {
		return false
	}
	label := host[:len(host)-len(suffix)]
	return label != "" && !strings.Contains(label, ".")
}
