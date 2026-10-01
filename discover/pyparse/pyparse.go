// Package pyparse reads recorded inline Python as data. It never executes a script.
package pyparse

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// StrictInlineFileReplace recognizes exactly one Python file transform, read
// as data and never executed:
//
//	path = '<literal>'
//	text = open(path).read()
//	changed = text.replace('<old>', '<new>')   # old nonempty
//	open(path, 'w').write(changed)
//
// It is a small tokenizer and parser for that shape alone. Anything it does
// not understand is rejected, so it can only fail closed: a script Python
// would accept but this parser does not is simply not compiled.
func StrictInlineFileReplace(body string) bool {
	if len(body) > 16<<10 {
		return false
	}
	toks, ok := PyTokenize(body)
	if !ok {
		return false
	}
	stmts, ok := PyStatements(toks)
	if !ok || len(stmts) != 4 {
		return false
	}
	var parsed [4]PyStmt
	for i, st := range stmts {
		p := &PyParser{Toks: st}
		s, ok := p.Statement()
		if !ok {
			return false
		}
		parsed[i] = s
	}
	// 1. path = '<literal>'
	pathVar := parsed[0].Target
	if pathVar == "" || parsed[0].Value.Kind != PyStr {
		return false
	}
	// 2. text = open(path).read()
	dataVar := parsed[1].Target
	read := parsed[1].Value
	if dataVar == "" || !PyMethodCall(read, "read", 0) {
		return false
	}
	opened := read.Fn.Recv
	if !PyOpenCall(opened, pathVar, 1) {
		return false
	}
	if dataVar == pathVar || pathVar == "open" {
		return false
	}
	// 3. changed = text.replace('<old>', '<new>')
	changedVar := parsed[2].Target
	rep := parsed[2].Value
	if changedVar == "" || !PyMethodCall(rep, "replace", 2) || rep.Fn.Recv.Kind != PyName || rep.Fn.Recv.Name != dataVar {
		return false
	}
	if rep.Args[0].Kind != PyStr || rep.Args[1].Kind != PyStr || rep.Args[0].Str == "" {
		return false
	}
	if changedVar == pathVar {
		return false
	}
	// 4. open(path, 'w').write(changed)
	if parsed[3].Target != "" {
		return false
	}
	write := parsed[3].Value
	if !PyMethodCall(write, "write", 1) || write.Args[0].Kind != PyName || write.Args[0].Name != changedVar {
		return false
	}
	target := write.Fn.Recv
	return PyOpenCall(target, pathVar, 2) && target.Args[1].Kind == PyStr && target.Args[1].Str == "w"
}

// PyMethodCall reports a call of recv.method with n positional arguments and
// no keywords.
func PyMethodCall(e *PyExpr, method string, n int) bool {
	return e != nil && e.Kind == PyCall && !e.Keywords && len(e.Args) == n &&
		e.Fn != nil && e.Fn.Kind == PyAttr && e.Fn.Name == method
}

// PyOpenCall reports open(<pathVar>, ...) with n positional arguments.
func PyOpenCall(e *PyExpr, pathVar string, n int) bool {
	return e != nil && e.Kind == PyCall && !e.Keywords && len(e.Args) == n &&
		e.Fn != nil && e.Fn.Kind == PyName && e.Fn.Name == "open" &&
		e.Args[0].Kind == PyName && e.Args[0].Name == pathVar
}

type PyTokKind int

const (
	PyTokName PyTokKind = iota
	PyTokStr
	PyTokOp
	PyTokNewline
)

type PyTok struct {
	Kind PyTokKind
	Text string // name or operator; decoded value for a string
}

// PyKeywords cannot be names; a statement using one as a name is not Python.
var PyKeywords = map[string]bool{
	"False": true, "None": true, "True": true, "and": true, "as": true, "assert": true, "async": true, "await": true,
	"break": true, "class": true, "continue": true, "def": true, "del": true, "elif": true, "else": true, "except": true,
	"finally": true, "for": true, "from": true, "global": true, "if": true, "import": true, "in": true, "is": true,
	"lambda": true, "nonlocal": true, "not": true, "or": true, "pass": true, "raise": true, "return": true, "try": true,
	"while": true, "with": true, "yield": true,
}

// PyTokenize splits source into names, decoded string literals, the
// operators the shape uses, and logical newlines. Newlines inside brackets
// are joined, as in Python. Anything else is rejected.
func PyTokenize(src string) ([]PyTok, bool) {
	var out []PyTok
	depth := 0
	atLineStart := true
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == '\n':
			if depth == 0 {
				out = append(out, PyTok{Kind: PyTokNewline})
				atLineStart = true
			}
			i++
		case c == ' ' || c == '\t' || c == '\r' || c == '\f':
			// Indentation at the start of a top-level line is an error in
			// Python unless the line is blank or a comment.
			if atLineStart && depth == 0 {
				j := i
				for j < len(src) && (src[j] == ' ' || src[j] == '\t' || src[j] == '\r' || src[j] == '\f') {
					j++
				}
				if j < len(src) && src[j] != '\n' && src[j] != '#' {
					return nil, false
				}
				i = j
				continue
			}
			i++
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '\\':
			// Explicit line joining.
			if i+1 < len(src) && src[i+1] == '\n' {
				i += 2
				continue
			}
			if i+2 < len(src) && src[i+1] == '\r' && src[i+2] == '\n' {
				i += 3
				continue
			}
			return nil, false
		case c == '(' || c == ')' || c == '.' || c == ',' || c == '=' || c == ';':
			if c == '(' {
				depth++
			} else if c == ')' {
				if depth == 0 {
					return nil, false
				}
				depth--
			}
			if c == '=' && i+1 < len(src) && src[i+1] == '=' {
				return nil, false // comparison
			}
			out = append(out, PyTok{Kind: PyTokOp, Text: string(c)})
			atLineStart = false
			i++
		case c == '\'' || c == '"':
			s, n, ok := PyString(src[i:], false)
			if !ok {
				return nil, false
			}
			out = append(out, PyTok{Kind: PyTokStr, Text: s})
			atLineStart = false
			i += n
		case PyIdentStart(src[i:]):
			j := i
			for j < len(src) {
				r, n := utf8.DecodeRuneInString(src[j:])
				if r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.In(r, unicode.Mn, unicode.Mc, unicode.Pc) {
					break
				}
				j += n
			}
			word := src[i:j]
			// A one-letter prefix on a string literal: r and u keep it a
			// str; b, f and longer prefixes make it something else.
			if j < len(src) && (src[j] == '\'' || src[j] == '"') {
				switch word {
				case "r", "R":
					s, n, ok := PyString(src[j:], true)
					if !ok {
						return nil, false
					}
					out = append(out, PyTok{Kind: PyTokStr, Text: s})
					atLineStart = false
					i = j + n
					continue
				case "u", "U":
					s, n, ok := PyString(src[j:], false)
					if !ok {
						return nil, false
					}
					out = append(out, PyTok{Kind: PyTokStr, Text: s})
					atLineStart = false
					i = j + n
					continue
				default:
					return nil, false
				}
			}
			out = append(out, PyTok{Kind: PyTokName, Text: word})
			atLineStart = false
			i = j
		default:
			return nil, false
		}
	}
	if depth != 0 {
		return nil, false
	}
	out = append(out, PyTok{Kind: PyTokNewline})
	return out, true
}

// PyString reads one quoted literal at the start of s and returns its value
// and length. Triple quotes may span lines; single quotes may not, except by
// an escaped newline.
func PyString(s string, raw bool) (string, int, bool) {
	q := s[0]
	triple := len(s) >= 3 && s[1] == q && s[2] == q
	start := 1
	if triple {
		start = 3
	}
	var b strings.Builder
	i := start
	for i < len(s) {
		c := s[i]
		if triple && c == q && i+2 < len(s) && s[i+1] == q && s[i+2] == q {
			return b.String(), i + 3, true
		}
		if !triple && c == q {
			return b.String(), i + 1, true
		}
		if !triple && c == '\n' {
			return "", 0, false
		}
		if c != '\\' {
			b.WriteByte(c)
			i++
			continue
		}
		if i+1 >= len(s) {
			return "", 0, false
		}
		n := s[i+1]
		if raw {
			// A raw string keeps the backslash; it still cannot end on it.
			b.WriteByte('\\')
			b.WriteByte(n)
			i += 2
			continue
		}
		i += 2
		switch n {
		case '\n':
		case '\\', '\'', '"':
			b.WriteByte(n)
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'v':
			b.WriteByte('\v')
		case 'x', 'u', 'U':
			width := map[byte]int{'x': 2, 'u': 4, 'U': 8}[n]
			if i+width > len(s) {
				return "", 0, false
			}
			v, err := strconv.ParseUint(s[i:i+width], 16, 32)
			if err != nil || v > 0x10FFFF {
				return "", 0, false
			}
			b.WriteRune(rune(v))
			i += width
		case 'N':
			// Named characters need Unicode's name table, which Go does not
			// carry; reject rather than guess (the check fails closed).
			return "", 0, false
		default:
			if n >= '0' && n <= '7' {
				j := i - 1
				v := 0
				for k := 0; k < 3 && j < len(s) && s[j] >= '0' && s[j] <= '7'; k++ {
					v = v*8 + int(s[j]-'0')
					j++
				}
				b.WriteRune(rune(v))
				i = j
				continue
			}
			// An unknown escape keeps its backslash in Python.
			b.WriteByte('\\')
			b.WriteByte(n)
		}
	}
	return "", 0, false
}

// PyStatements splits tokens into simple statements at newlines and
// semicolons, dropping empty lines. A semicolon may end a line.
func PyStatements(toks []PyTok) ([][]PyTok, bool) {
	var out [][]PyTok
	var cur []PyTok
	for _, t := range toks {
		switch {
		case t.Kind == PyTokNewline:
			if len(cur) > 0 {
				out = append(out, cur)
				cur = nil
			}
		case t.Kind == PyTokOp && t.Text == ";":
			if len(cur) == 0 {
				return nil, false // ';;' or a line starting with ';'
			}
			out = append(out, cur)
			cur = nil
		default:
			cur = append(cur, t)
		}
	}
	return out, true
}

type PyExprKind int

const (
	PyName PyExprKind = iota
	PyStr
	PyAttr
	PyCall
)

type PyExpr struct {
	Kind     PyExprKind
	Name     string    // pyName: the name; pyAttr: the attribute
	Str      string    // pyStr: the decoded value
	Recv     *PyExpr   // pyAttr: the object
	Fn       *PyExpr   // pyCall: the called expression
	Args     []*PyExpr // pyCall: positional arguments
	Keywords bool      // pyCall: had keyword arguments
}

type PyStmt struct {
	Target string // "" for an expression statement
	Value  *PyExpr
}

type PyParser struct {
	Toks []PyTok
	Pos  int
}

func (p *PyParser) Peek() (PyTok, bool) {
	if p.Pos >= len(p.Toks) {
		return PyTok{}, false
	}
	return p.Toks[p.Pos], true
}

func (p *PyParser) Op(text string) bool {
	if t, ok := p.Peek(); ok && t.Kind == PyTokOp && t.Text == text {
		p.Pos++
		return true
	}
	return false
}

// statement is `name = expr` or `expr`, consuming every token.
func (p *PyParser) Statement() (PyStmt, bool) {
	var s PyStmt
	if len(p.Toks) >= 2 && p.Toks[0].Kind == PyTokName && p.Toks[1].Kind == PyTokOp && p.Toks[1].Text == "=" {
		if PyKeywords[p.Toks[0].Text] {
			return s, false
		}
		s.Target = p.Toks[0].Text
		p.Pos = 2
	}
	e, ok := p.Expr()
	if !ok || p.Pos != len(p.Toks) {
		return s, false
	}
	s.Value = e
	return s, true
}

// expr is an atom followed by attribute and call trailers.
func (p *PyParser) Expr() (*PyExpr, bool) {
	e, ok := p.Atom()
	if !ok {
		return nil, false
	}
	for {
		switch {
		case p.Op("."):
			t, ok := p.Peek()
			if !ok || t.Kind != PyTokName {
				return nil, false
			}
			p.Pos++
			e = &PyExpr{Kind: PyAttr, Name: t.Text, Recv: e}
		case p.Op("("):
			call := &PyExpr{Kind: PyCall, Fn: e}
			for !p.Op(")") {
				// A keyword argument: name '=' expr.
				if p.Pos+1 < len(p.Toks) && p.Toks[p.Pos].Kind == PyTokName && p.Toks[p.Pos+1].Kind == PyTokOp && p.Toks[p.Pos+1].Text == "=" {
					p.Pos += 2
					call.Keywords = true
					if _, ok := p.Expr(); !ok {
						return nil, false
					}
				} else {
					if call.Keywords {
						return nil, false // positional after keyword
					}
					a, ok := p.Expr()
					if !ok {
						return nil, false
					}
					call.Args = append(call.Args, a)
				}
				if p.Op(")") {
					break
				}
				if !p.Op(",") {
					return nil, false
				}
			}
			e = call
		default:
			return e, true
		}
	}
}

// atom is a name, one or more adjacent string literals (joined, as in
// Python), or a parenthesized expression.
func (p *PyParser) Atom() (*PyExpr, bool) {
	t, ok := p.Peek()
	if !ok {
		return nil, false
	}
	switch t.Kind {
	case PyTokName:
		if PyKeywords[t.Text] {
			return nil, false
		}
		p.Pos++
		return &PyExpr{Kind: PyName, Name: t.Text}, true
	case PyTokStr:
		var b strings.Builder
		for {
			t, ok := p.Peek()
			if !ok || t.Kind != PyTokStr {
				break
			}
			b.WriteString(t.Text)
			p.Pos++
		}
		return &PyExpr{Kind: PyStr, Str: b.String()}, true
	case PyTokOp:
		if t.Text == "(" {
			p.Pos++
			e, ok := p.Expr()
			if !ok || !p.Op(")") {
				return nil, false // a tuple or empty parentheses
			}
			return e, true
		}
	}
	return nil, false
}

// PyIdentStart reports a Python identifier start: an underscore or a letter,
// in any script.
func PyIdentStart(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return r == '_' || unicode.IsLetter(r)
}

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
