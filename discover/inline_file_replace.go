package discover

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// strictInlineFileReplace recognizes exactly one Python file transform, read
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
func strictInlineFileReplace(body string) bool {
	if len(body) > 16<<10 {
		return false
	}
	toks, ok := pyTokenize(body)
	if !ok {
		return false
	}
	stmts, ok := pyStatements(toks)
	if !ok || len(stmts) != 4 {
		return false
	}
	var parsed [4]pyStmt
	for i, st := range stmts {
		p := &pyParser{toks: st}
		s, ok := p.statement()
		if !ok {
			return false
		}
		parsed[i] = s
	}
	// 1. path = '<literal>'
	pathVar := parsed[0].target
	if pathVar == "" || parsed[0].value.kind != pyStr {
		return false
	}
	// 2. text = open(path).read()
	dataVar := parsed[1].target
	read := parsed[1].value
	if dataVar == "" || !pyMethodCall(read, "read", 0) {
		return false
	}
	opened := read.fn.recv
	if !pyOpenCall(opened, pathVar, 1) {
		return false
	}
	if dataVar == pathVar || pathVar == "open" {
		return false
	}
	// 3. changed = text.replace('<old>', '<new>')
	changedVar := parsed[2].target
	rep := parsed[2].value
	if changedVar == "" || !pyMethodCall(rep, "replace", 2) || rep.fn.recv.kind != pyName || rep.fn.recv.name != dataVar {
		return false
	}
	if rep.args[0].kind != pyStr || rep.args[1].kind != pyStr || rep.args[0].str == "" {
		return false
	}
	if changedVar == pathVar {
		return false
	}
	// 4. open(path, 'w').write(changed)
	if parsed[3].target != "" {
		return false
	}
	write := parsed[3].value
	if !pyMethodCall(write, "write", 1) || write.args[0].kind != pyName || write.args[0].name != changedVar {
		return false
	}
	target := write.fn.recv
	return pyOpenCall(target, pathVar, 2) && target.args[1].kind == pyStr && target.args[1].str == "w"
}

// pyMethodCall reports a call of recv.method with n positional arguments and
// no keywords.
func pyMethodCall(e *pyExpr, method string, n int) bool {
	return e != nil && e.kind == pyCall && !e.keywords && len(e.args) == n &&
		e.fn != nil && e.fn.kind == pyAttr && e.fn.name == method
}

// pyOpenCall reports open(<pathVar>, ...) with n positional arguments.
func pyOpenCall(e *pyExpr, pathVar string, n int) bool {
	return e != nil && e.kind == pyCall && !e.keywords && len(e.args) == n &&
		e.fn != nil && e.fn.kind == pyName && e.fn.name == "open" &&
		e.args[0].kind == pyName && e.args[0].name == pathVar
}

type pyTokKind int

const (
	pyTokName pyTokKind = iota
	pyTokStr
	pyTokOp
	pyTokNewline
)

type pyTok struct {
	kind pyTokKind
	text string // name or operator; decoded value for a string
}

// pyKeywords cannot be names; a statement using one as a name is not Python.
var pyKeywords = map[string]bool{
	"False": true, "None": true, "True": true, "and": true, "as": true, "assert": true, "async": true, "await": true,
	"break": true, "class": true, "continue": true, "def": true, "del": true, "elif": true, "else": true, "except": true,
	"finally": true, "for": true, "from": true, "global": true, "if": true, "import": true, "in": true, "is": true,
	"lambda": true, "nonlocal": true, "not": true, "or": true, "pass": true, "raise": true, "return": true, "try": true,
	"while": true, "with": true, "yield": true,
}

// pyTokenize splits source into names, decoded string literals, the
// operators the shape uses, and logical newlines. Newlines inside brackets
// are joined, as in Python. Anything else is rejected.
func pyTokenize(src string) ([]pyTok, bool) {
	var out []pyTok
	depth := 0
	atLineStart := true
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == '\n':
			if depth == 0 {
				out = append(out, pyTok{kind: pyTokNewline})
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
			out = append(out, pyTok{kind: pyTokOp, text: string(c)})
			atLineStart = false
			i++
		case c == '\'' || c == '"':
			s, n, ok := pyString(src[i:], false)
			if !ok {
				return nil, false
			}
			out = append(out, pyTok{kind: pyTokStr, text: s})
			atLineStart = false
			i += n
		case pyIdentStart(src[i:]):
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
					s, n, ok := pyString(src[j:], true)
					if !ok {
						return nil, false
					}
					out = append(out, pyTok{kind: pyTokStr, text: s})
					atLineStart = false
					i = j + n
					continue
				case "u", "U":
					s, n, ok := pyString(src[j:], false)
					if !ok {
						return nil, false
					}
					out = append(out, pyTok{kind: pyTokStr, text: s})
					atLineStart = false
					i = j + n
					continue
				default:
					return nil, false
				}
			}
			out = append(out, pyTok{kind: pyTokName, text: word})
			atLineStart = false
			i = j
		default:
			return nil, false
		}
	}
	if depth != 0 {
		return nil, false
	}
	out = append(out, pyTok{kind: pyTokNewline})
	return out, true
}

// pyString reads one quoted literal at the start of s and returns its value
// and length. Triple quotes may span lines; single quotes may not, except by
// an escaped newline.
func pyString(s string, raw bool) (string, int, bool) {
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

// pyStatements splits tokens into simple statements at newlines and
// semicolons, dropping empty lines. A semicolon may end a line.
func pyStatements(toks []pyTok) ([][]pyTok, bool) {
	var out [][]pyTok
	var cur []pyTok
	for _, t := range toks {
		switch {
		case t.kind == pyTokNewline:
			if len(cur) > 0 {
				out = append(out, cur)
				cur = nil
			}
		case t.kind == pyTokOp && t.text == ";":
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

type pyExprKind int

const (
	pyName pyExprKind = iota
	pyStr
	pyAttr
	pyCall
)

type pyExpr struct {
	kind     pyExprKind
	name     string    // pyName: the name; pyAttr: the attribute
	str      string    // pyStr: the decoded value
	recv     *pyExpr   // pyAttr: the object
	fn       *pyExpr   // pyCall: the called expression
	args     []*pyExpr // pyCall: positional arguments
	keywords bool      // pyCall: had keyword arguments
}

type pyStmt struct {
	target string // "" for an expression statement
	value  *pyExpr
}

type pyParser struct {
	toks []pyTok
	pos  int
}

func (p *pyParser) peek() (pyTok, bool) {
	if p.pos >= len(p.toks) {
		return pyTok{}, false
	}
	return p.toks[p.pos], true
}

func (p *pyParser) op(text string) bool {
	if t, ok := p.peek(); ok && t.kind == pyTokOp && t.text == text {
		p.pos++
		return true
	}
	return false
}

// statement is `name = expr` or `expr`, consuming every token.
func (p *pyParser) statement() (pyStmt, bool) {
	var s pyStmt
	if len(p.toks) >= 2 && p.toks[0].kind == pyTokName && p.toks[1].kind == pyTokOp && p.toks[1].text == "=" {
		if pyKeywords[p.toks[0].text] {
			return s, false
		}
		s.target = p.toks[0].text
		p.pos = 2
	}
	e, ok := p.expr()
	if !ok || p.pos != len(p.toks) {
		return s, false
	}
	s.value = e
	return s, true
}

// expr is an atom followed by attribute and call trailers.
func (p *pyParser) expr() (*pyExpr, bool) {
	e, ok := p.atom()
	if !ok {
		return nil, false
	}
	for {
		switch {
		case p.op("."):
			t, ok := p.peek()
			if !ok || t.kind != pyTokName {
				return nil, false
			}
			p.pos++
			e = &pyExpr{kind: pyAttr, name: t.text, recv: e}
		case p.op("("):
			call := &pyExpr{kind: pyCall, fn: e}
			for !p.op(")") {
				// A keyword argument: name '=' expr.
				if p.pos+1 < len(p.toks) && p.toks[p.pos].kind == pyTokName && p.toks[p.pos+1].kind == pyTokOp && p.toks[p.pos+1].text == "=" {
					p.pos += 2
					call.keywords = true
					if _, ok := p.expr(); !ok {
						return nil, false
					}
				} else {
					if call.keywords {
						return nil, false // positional after keyword
					}
					a, ok := p.expr()
					if !ok {
						return nil, false
					}
					call.args = append(call.args, a)
				}
				if p.op(")") {
					break
				}
				if !p.op(",") {
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
func (p *pyParser) atom() (*pyExpr, bool) {
	t, ok := p.peek()
	if !ok {
		return nil, false
	}
	switch t.kind {
	case pyTokName:
		if pyKeywords[t.text] {
			return nil, false
		}
		p.pos++
		return &pyExpr{kind: pyName, name: t.text}, true
	case pyTokStr:
		var b strings.Builder
		for {
			t, ok := p.peek()
			if !ok || t.kind != pyTokStr {
				break
			}
			b.WriteString(t.text)
			p.pos++
		}
		return &pyExpr{kind: pyStr, str: b.String()}, true
	case pyTokOp:
		if t.text == "(" {
			p.pos++
			e, ok := p.expr()
			if !ok || !p.op(")") {
				return nil, false // a tuple or empty parentheses
			}
			return e, true
		}
	}
	return nil, false
}

// pyIdentStart reports a Python identifier start: an underscore or a letter,
// in any script.
func pyIdentStart(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return r == '_' || unicode.IsLetter(r)
}
