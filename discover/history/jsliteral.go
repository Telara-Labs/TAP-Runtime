package history

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

const omittedJS = "<omitted JavaScript expression>"

// jsLiteralData interprets only literal data, never JavaScript expressions.
// Unsupported syntax and templates are omitted without retaining their source.
func jsLiteralData(src string) any {
	v, _ := jsLiteralRead(src)
	return v
}

func jsLiteralRead(src string) (any, bool) {
	p := jsLiteralParser{r: []rune(src)}
	v := p.value(0)
	return v, p.unsafe
}

type jsLiteralParser struct {
	r      []rune
	i      int
	unsafe bool
}

func (p *jsLiteralParser) space() {
	for p.i < len(p.r) && IsSpace(p.r[p.i]) {
		p.i++
	}
}
func (p *jsLiteralParser) omit(start int) any {
	p.unsafe = true
	p.i = trace.ScanJSValue(p.r, start)
	if p.i <= start {
		p.i = len(p.r)
	}
	return omittedJS
}
func (p *jsLiteralParser) str() (string, bool) {
	start := p.i
	q := p.r[p.i]
	p.i++
	var out strings.Builder
	for p.i < len(p.r) {
		c := p.r[p.i]
		if c == q {
			p.i++
			return out.String(), true
		}
		if c == '\n' || c == '\r' {
			return "", false
		}
		if c == '\\' {
			rest := string(p.r[p.i:])
			v, _, tail, err := strconv.UnquoteChar(rest, byte(q))
			if err != nil || v == utf8.RuneError {
				return "", false
			}
			consumed := len([]rune(rest)) - len([]rune(tail))
			if consumed <= 0 {
				return "", false
			}
			out.WriteRune(v)
			p.i += consumed
			continue
		}
		out.WriteRune(c)
		p.i++
	}
	p.i = start
	return "", false
}
func (p *jsLiteralParser) value(depth int) any {
	p.space()
	start := p.i
	if start >= len(p.r) || depth > 32 {
		return p.omit(start)
	}
	switch p.r[p.i] {
	case '\'', '"':
		if s, ok := p.str(); ok {
			return s
		}
		return p.omit(start)
	case '`':
		return p.omit(start)
	case '{':
		p.i++
		out := map[string]any{}
		for {
			p.space()
			if p.i >= len(p.r) {
				return p.omit(start)
			}
			if p.r[p.i] == '}' {
				p.i++
				return out
			}
			var key string
			if p.r[p.i] == '\'' || p.r[p.i] == '"' {
				var ok bool
				key, ok = p.str()
				if !ok {
					return p.omit(start)
				}
			} else {
				begin := p.i
				if !IsIdentStart(p.r[p.i]) {
					return p.omit(start)
				}
				for p.i < len(p.r) && (IsIdentStart(p.r[p.i]) || p.r[p.i] >= '0' && p.r[p.i] <= '9') {
					p.i++
				}
				key = string(p.r[begin:p.i])
			}
			p.space()
			if p.i >= len(p.r) || p.r[p.i] != ':' {
				return p.omit(start)
			}
			p.i++
			value := p.value(depth + 1)
			if safeIdentifier(key) {
				if _, duplicate := out[key]; duplicate {
					p.unsafe = true
				}
				out[key] = value
			} else {
				p.unsafe = true
			}
			p.space()
			if p.i >= len(p.r) {
				return p.omit(start)
			}
			if p.r[p.i] == '}' {
				p.i++
				return out
			}
			if p.r[p.i] != ',' {
				return p.omit(start)
			}
			p.i++
		}
	case '[':
		p.i++
		out := []any{}
		for {
			p.space()
			if p.i >= len(p.r) {
				return p.omit(start)
			}
			if p.r[p.i] == ']' {
				p.i++
				return out
			}
			out = append(out, p.value(depth+1))
			p.space()
			if p.i >= len(p.r) {
				return p.omit(start)
			}
			if p.r[p.i] == ']' {
				p.i++
				return out
			}
			if p.r[p.i] != ',' {
				return p.omit(start)
			}
			p.i++
		}
	default:
		end := trace.ScanJSValue(p.r, p.i)
		if end <= p.i {
			return p.omit(start)
		}
		raw := strings.TrimSpace(string(p.r[p.i:end]))
		p.i = end
		if json.Valid([]byte(raw)) && !strings.ContainsAny(raw, "{}[]\"'") {
			return json.RawMessage(raw)
		}
		p.unsafe = true
		return omittedJS
	}
}
