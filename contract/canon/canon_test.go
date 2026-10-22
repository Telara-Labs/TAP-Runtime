package canon

import "testing"

// Valid input keeps the encoding it always had: these outputs were recorded
// before invalid input was rejected.
func TestCanonicalJSONValidInputIsUnchanged(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"b":1,"a":[true,false,null],"c":{"y":"x","x":1e21}}`, `{"a":[true,false,null],"b":1,"c":{"x":1e+21,"y":"x"}}`},
		{` {"€":"€","😀":"😀","n":-0.0,"f":1.5e-7,"big":123456789012345678901} `, `{"big":123456789012345680000,"f":1.5e-7,"n":0,"€":"€","😀":"😀"}`},
		{`"<tag>&amp;\u0001\t"`, `"<tag>&amp;\u0001\t"`},
		{`[1,2.50,1E2,"é"]`, `[1,2.5,100,"é"]`},
		{"\n[]\r\n", `[]`},
		{`"a \\ud800 written out, not an escape"`, `"a \\ud800 written out, not an escape"`},
		{`"\"𐀀\""`, "\"\\\"\U00010000\\\"\""},
		{`"�"`, "\"�\""},
	} {
		got, err := CanonicalJSON([]byte(tc.in))
		if err != nil || string(got) != tc.want {
			t.Errorf("%q: got %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestCanonicalJSONRejectsInputThatIsNotOneIJSONValue(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"trailing garbage", `{}x`},
		{"trailing garbage after whitespace", "[1] \n x"},
		{"second value", `{} {}`},
		{"trailing comma-separated value", `1,2`},
		{"invalid UTF-8 in a string", "\"a\xffb\""},
		{"invalid UTF-8 in a key", "{\"\xc3\x28\":1}"},
		{"invalid UTF-8 outside strings", "[1]\xff"},
		{"lone high surrogate", `"\ud800"`},
		{"high surrogate before a letter", `"\ud800x"`},
		{"high surrogate before another high", `"\ud800\ud800"`},
		{"lone low surrogate", `"\udc00"`},
		{"low then high", `"\udc00\ud800"`},
		{"lone surrogate in a key", `{"\uDBFF":1}`},
		{"duplicate key", `{"a":1,"a":2}`},
		{"empty", ``},
	} {
		if got, err := CanonicalJSON([]byte(tc.in)); err == nil {
			t.Errorf("%s: %q accepted as %q", tc.name, tc.in, got)
		}
	}
}
