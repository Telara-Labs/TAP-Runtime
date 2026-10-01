package pyparse

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// InlinePythonShape is a retrieval fingerprint, not an executable program
// graph. It retains ordered syntax and call names while erasing literal values
// and local variable names. A later compiler must independently prove data
// flow, effects, and capability reach before offering Accept.
var PythonHeredocStart = regexp.MustCompile(`(?m)(?:^|[;&|][ \t]*)(?:python3?|/usr/bin/python3?)\s+-\s+<<-?\s*['"]?([A-Za-z0-9_]+)['"]?[ \t]*\n`)

func InlinePythonShape(command string) string {
	shape, _, _ := InlinePythonSnippet(command)
	return shape
}

// A compound shell call can contain a useful authored code chunk. Its code
// shape is retrieval evidence only; embedded means the surrounding shell is
// not part of that shape and must be reviewed separately.
func InlinePythonSnippet(command string) (shape, family string, embedded bool) {
	body, embedded, ok := InlinePythonBody(command)
	if !ok {
		return "", "", false
	}
	tokens, calls, ok := PythonShapeTokens(body)
	if !ok || calls < 2 || len(tokens) < 8 {
		return "", "", false
	}
	operations, ok := PythonOperationSequence(tokens)
	if !ok || len(operations) < 2 {
		return "", "", false
	}
	sum := sha256.Sum256([]byte(strings.Join(tokens, " ")))
	broad := make([]string, 0, len(operations))
	for _, op := range operations {
		if op == "print" || len(broad) > 0 && broad[len(broad)-1] == op {
			continue
		}
		broad = append(broad, op)
	}
	if len(broad) < 2 {
		return "", "", false
	}
	familyHash := sha256.Sum256([]byte(strings.Join(broad, "\x00")))
	return "py_" + hex.EncodeToString(sum[:12]), "pyfam_" + hex.EncodeToString(familyHash[:12]) + ":" + strings.Join(broad, ">"), embedded
}

func InlinePythonBody(command string) (body string, embedded bool, ok bool) {
	if len(command) > 64<<10 {
		return "", false, false
	}
	matches := PythonHeredocStart.FindAllStringSubmatchIndex(command, 2)
	if len(matches) != 1 {
		return "", false, false
	}
	match := matches[0]
	delimiter := command[match[2]:match[3]]
	lines := strings.Split(command[match[1]:], "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != delimiter {
			continue
		}
		embedded = strings.TrimSpace(command[:match[0]]) != "" || strings.TrimSpace(strings.Join(lines[i+1:], "\n")) != ""
		body = strings.Join(lines[:i], "\n")
		if len(body) > 16<<10 {
			return "", false, false
		}
		return body, embedded, true
	}
	return "", false, false
}

// Nested argument calls execute before their enclosing call. The family is a
// broad retrieval bucket, not proof that the scripts are equivalent programs.
func PythonOperationSequence(tokens []string) ([]string, bool) {
	var stack, operations []string
	for i, token := range tokens {
		switch token {
		case "(":
			call := ""
			if i > 0 && strings.HasPrefix(tokens[i-1], "call:") {
				call = strings.TrimPrefix(tokens[i-1], "call:")
			}
			stack = append(stack, call)
		case ")":
			if len(stack) == 0 {
				return nil, false
			}
			call := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if call != "" {
				operations = append(operations, call)
			}
		}
	}
	return operations, len(stack) == 0
}

var PythonShapeKeywords = map[string]bool{
	"and": true, "as": true, "assert": true, "async": true, "await": true,
	"break": true, "class": true, "continue": true, "def": true, "del": true,
	"elif": true, "else": true, "except": true, "finally": true, "for": true,
	"from": true, "global": true, "if": true, "import": true, "in": true,
	"is": true, "lambda": true, "nonlocal": true, "not": true, "or": true,
	"pass": true, "raise": true, "return": true, "try": true, "while": true,
	"with": true, "yield": true, "True": true, "False": true, "None": true,
}

func PythonShapeTokens(body string) ([]string, int, bool) {
	var tokens []string
	calls := 0
	for i := 0; i < len(body); {
		c := body[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case c == '\n':
			tokens = append(tokens, ";")
			i++
		case c == '#':
			for i < len(body) && body[i] != '\n' {
				i++
			}
		case c == '\'' || c == '"':
			end, ok := SkipPythonString(body, i)
			if !ok {
				return nil, 0, false
			}
			tokens = append(tokens, "L")
			i = end
		case c >= '0' && c <= '9':
			j := i + 1
			for j < len(body) && ((body[j] >= '0' && body[j] <= '9') || body[j] == '.' || body[j] == '_' || body[j] == 'e' || body[j] == 'E') {
				j++
			}
			tokens = append(tokens, "L")
			i = j
		case PythonNameStart(c):
			j := i + 1
			for j < len(body) && PythonNamePart(body[j]) {
				j++
			}
			name := body[i:j]
			k := j
			for k < len(body) && (body[k] == ' ' || body[k] == '\t') {
				k++
			}
			switch {
			case PythonShapeKeywords[name]:
				tokens = append(tokens, name)
			case k < len(body) && body[k] == '(':
				tokens = append(tokens, "call:"+name)
				calls++
			default:
				tokens = append(tokens, "V")
			}
			i = j
		case strings.ContainsRune(".()[]{}=:+-*/%<>!,;@|&^~", rune(c)):
			tokens = append(tokens, string(c))
			i++
		default:
			return nil, 0, false // unsupported lexical form; do not guess
		}
	}
	return tokens, calls, true
}

func PythonNameStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func PythonNamePart(c byte) bool {
	return PythonNameStart(c) || c >= '0' && c <= '9'
}

func SkipPythonString(body string, start int) (int, bool) {
	quote := body[start]
	triple := start+2 < len(body) && body[start+1] == quote && body[start+2] == quote
	i := start + 1
	if triple {
		i = start + 3
	}
	for i < len(body) {
		if body[i] == '\\' {
			i += 2
			continue
		}
		if body[i] == quote {
			if !triple {
				return i + 1, true
			}
			if i+2 < len(body) && body[i+1] == quote && body[i+2] == quote {
				return i + 3, true
			}
		}
		if body[i] == '\n' && !triple {
			return 0, false
		}
		i++
	}
	return 0, false
}
