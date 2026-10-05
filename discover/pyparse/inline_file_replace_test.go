package pyparse

import "testing"

// The historical synthetic parity disagreement used \N{BULLET}. Python
// accepts that spelling, but this model-free recognizer does not resolve
// Unicode names. It must reject the whole transform rather than compile
// a replacement with a different meaning.
func TestInlineFileReplaceNamedUnicodeEscapeFailsClosed(t *testing.T) {
	for _, literal := range []string{`'\N{BULLET}'`, `'\N{NOT A UNICODE NAME}'`, `'\N{BULLET'`} {
		body := "p = 'a'\nt = open(p).read()\nu = t.replace(" + literal + ", 'y')\nopen(p, 'w').write(u)"
		if StrictInlineFileReplace(body) {
			t.Fatalf("unsupported Unicode name compiled: %s", literal)
		}
	}
	for _, literal := range []string{`'•'`, `'\u2022'`} {
		body := "p = 'a'\nt = open(p).read()\nu = t.replace(" + literal + ", 'y')\nopen(p, 'w').write(u)"
		if !StrictInlineFileReplace(body) {
			t.Fatalf("supported equivalent spelling rejected: %s", literal)
		}
	}
}
