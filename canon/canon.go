// Package canon is RFC 8785, the JSON Canonicalization Scheme: one way to
// write a JSON value, whatever language writes it.
//
// A capability's identity is the hash of its contract (doc 34 section 11.1),
// and "two publishers who write the same contract get the same id" has to
// hold between a publisher writing Go and one writing Python. Go's own
// encoder does not give that: it writes < > and & as escapes, and formats
// numbers its own way. This does.
//
// It is the encoder of telara-agents tap-runtime/artifact/canonical.go,
// copied. That module is being retired (ruling 10), and the two must agree
// until it is.
package canon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// CanonicalJSON returns the RFC 8785 (JSON Canonicalization Scheme) encoding of
// a JSON document. Input is raw JSON bytes; numbers are interpreted as IEEE-754
// doubles exactly as JCS requires. Duplicate object keys and invalid UTF-8 are
// rejected rather than silently resolved.
func CanonicalJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := decodeStrict(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err == nil {
		return nil, fmt.Errorf("canonical json: trailing data after document")
	}
	var buf bytes.Buffer
	if err := encodeJCS(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// CanonicalValue canonicalizes an in-memory Go value (anything encoding/json
// can marshal) by marshaling it first.
func CanonicalValue(v interface{}) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return CanonicalJSON(raw)
}

// decodeStrict decodes one JSON value from dec, rejecting duplicate keys.
func decodeStrict(dec *json.Decoder) (interface{}, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("canonical json: %w", err)
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := map[string]interface{}{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, fmt.Errorf("canonical json: %w", err)
				}
				key, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("canonical json: non-string object key")
				}
				if !utf8.ValidString(key) {
					return nil, fmt.Errorf("canonical json: invalid UTF-8 in key")
				}
				if _, dup := obj[key]; dup {
					return nil, fmt.Errorf("canonical json: duplicate object key %q", key)
				}
				val, err := decodeStrict(dec)
				if err != nil {
					return nil, err
				}
				obj[key] = val
			}
			if _, err := dec.Token(); err != nil {
				return nil, fmt.Errorf("canonical json: %w", err)
			}
			return obj, nil
		case '[':
			arr := []interface{}{}
			for dec.More() {
				val, err := decodeStrict(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, val)
			}
			if _, err := dec.Token(); err != nil {
				return nil, fmt.Errorf("canonical json: %w", err)
			}
			return arr, nil
		}
		return nil, fmt.Errorf("canonical json: unexpected delimiter %v", t)
	case string:
		if !utf8.ValidString(t) {
			return nil, fmt.Errorf("canonical json: invalid UTF-8 in string")
		}
		return t, nil
	case json.Number:
		f, err := strconv.ParseFloat(string(t), 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, fmt.Errorf("canonical json: number %q is not a finite IEEE-754 double", string(t))
		}
		return f, nil
	case bool, nil:
		return t, nil
	}
	return nil, fmt.Errorf("canonical json: unexpected token %T", tok)
}

func encodeJCS(buf *bytes.Buffer, v interface{}) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case float64:
		buf.WriteString(formatES6Number(t))
	case string:
		writeJCSString(buf, t)
	case []interface{}:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encodeJCS(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		// JCS sorts keys by their UTF-16 code units.
		sort.Slice(keys, func(i, j int) bool { return lessUTF16(keys[i], keys[j]) })
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeJCSString(buf, k)
			buf.WriteByte(':')
			if err := encodeJCS(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("canonical json: unsupported value type %T", v)
	}
	return nil
}

func lessUTF16(a, b string) bool {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}

// writeJCSString escapes exactly as ECMAScript JSON.stringify does.
func writeJCSString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(buf, `\u%04x`, r)
			} else {
				buf.WriteRune(r)
			}
		}
	}
	buf.WriteByte('"')
}

// formatES6Number implements ECMAScript Number::toString for finite doubles,
// which RFC 8785 §3.2.2.3 mandates.
func formatES6Number(f float64) string {
	if f == 0 {
		return "0" // also -0
	}
	neg := f < 0
	if neg {
		f = -f
	}
	// Shortest round-trip digits and exponent.
	e := strconv.FormatFloat(f, 'e', -1, 64) // d.ddddde±XX
	mant, expStr, _ := strings.Cut(e, "e")
	digits := strings.Replace(mant, ".", "", 1)
	exp, _ := strconv.Atoi(expStr)
	k := len(digits)
	n := exp + 1 // position of the decimal point relative to digits
	var out string
	switch {
	case k <= n && n <= 21:
		out = digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		out = digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		out = "0." + strings.Repeat("0", -n) + digits
	default:
		sign := "+"
		if n-1 < 0 {
			sign = "-"
		}
		abs := n - 1
		if abs < 0 {
			abs = -abs
		}
		if k == 1 {
			out = digits + "e" + sign + strconv.Itoa(abs)
		} else {
			out = digits[:1] + "." + digits[1:] + "e" + sign + strconv.Itoa(abs)
		}
	}
	if neg {
		return "-" + out
	}
	return out
}
