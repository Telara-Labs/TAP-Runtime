package trace

import (
	"strings"
)

// ReadJSString reads the string literal opening at rs[i] and returns its
// unescaped text and the index after its closing quote.
func ReadJSString(rs []rune, i int) (string, int) {
	q := rs[i]
	var b strings.Builder
	j := i + 1
	for ; j < len(rs) && rs[j] != q; j++ {
		if rs[j] == '\\' && j+1 < len(rs) {
			j++
			switch rs[j] {
			case 'n':
				b.WriteRune('\n')
			case 't':
				b.WriteRune('\t')
			default:
				b.WriteRune(rs[j])
			}
			continue
		}
		b.WriteRune(rs[j])
	}
	return b.String(), min(j+1, len(rs))
}

// ScanJSValue returns the index of the ',' or '}' that ends the value
// starting at rs[i], skipping nested brackets and strings.
func ScanJSValue(rs []rune, i int) int {
	depth := 0
	for i < len(rs) {
		switch c := rs[i]; {
		case IsQuote(c):
			_, i = ReadJSString(rs, i)
			continue
		case c == '{' || c == '[' || c == '(':
			depth++
		case c == '}' || c == ']' || c == ')':
			if depth == 0 {
				return i
			}
			depth--
		case c == ',' && depth == 0:
			return i
		}
		i++
	}
	return i
}

func IsQuote(c rune) bool { return c == '"' || c == '\'' || c == '`' }
