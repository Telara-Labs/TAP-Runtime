package component

import (
	"fmt"
	"unicode/utf8"
)

// formatError is raised (as a panic) by the binary reader and the parser and
// recovered at the package boundary, so a malformed or unsupported component
// is refused with one message instead of crashing the runner.
type formatError struct{ msg string }

func (e formatError) Error() string { return e.msg }

func fail(format string, a ...any) { panic(formatError{fmt.Sprintf(format, a...)}) }

// catch turns a formatError panic into an error and lets any other panic
// continue, so programming errors are not reported as malformed input.
func catch(err *error) {
	if r := recover(); r != nil {
		if m, ok := r.(preview3Marker); ok {
			*err = fmt.Errorf("%w: %s; this loader runs WASI 0.2 components, and the runner hands Preview 3 ones to wasmtime", ErrPreview3, m.why)
			return
		}
		fe, ok := r.(formatError)
		if !ok {
			panic(r)
		}
		*err = fe
	}
}

// reader decodes the WebAssembly binary encoding. Every read is bounds
// checked; counts are checked against the bytes left so a hostile length
// cannot make the runner allocate more than the input's size.
type reader struct {
	b   []byte
	off int
}

func (r *reader) done() bool { return r.off >= len(r.b) }

func (r *reader) left() int { return len(r.b) - r.off }

func (r *reader) byte() byte {
	if r.off >= len(r.b) {
		fail("component binary ends unexpectedly")
	}
	c := r.b[r.off]
	r.off++
	return c
}

func (r *reader) peek() byte {
	if r.off >= len(r.b) {
		fail("component binary ends unexpectedly")
	}
	return r.b[r.off]
}

func (r *reader) bytes(n int) []byte {
	if n < 0 || n > r.left() {
		fail("component binary ends unexpectedly")
	}
	s := r.b[r.off : r.off+n]
	r.off += n
	return s
}

func (r *reader) u64() uint64 {
	var v uint64
	for shift := uint(0); ; shift += 7 {
		if shift >= 64 {
			fail("integer too long in component binary")
		}
		c := r.byte()
		v |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			return v
		}
	}
}

func (r *reader) u32() uint32 {
	v := r.u64()
	if v > 0xffffffff {
		fail("integer out of range in component binary")
	}
	return uint32(v)
}

// idx reads an index and checks it against the size of its index space.
func (r *reader) idx(size int, what string) int {
	v := r.u32()
	if int64(v) >= int64(size) {
		fail("%s index %d is out of range", what, v)
	}
	return int(v)
}

// count reads a vector length. Every element takes at least one byte.
func (r *reader) count() int {
	n := r.u32()
	if int64(n) > int64(r.left()) {
		fail("vector length %d exceeds the component's size", n)
	}
	return int(n)
}

// s33 reads a signed LEB128 value (used for value-type indexes).
func (r *reader) s33() int64 {
	var v int64
	var shift uint
	var c byte
	for {
		if shift >= 35 {
			fail("integer too long in component binary")
		}
		c = r.byte()
		v |= int64(c&0x7f) << shift
		shift += 7
		if c&0x80 == 0 {
			break
		}
	}
	if shift < 64 && c&0x40 != 0 {
		v |= -1 << shift
	}
	return v
}

func (r *reader) name() string {
	n := r.count()
	s := r.bytes(n)
	if !utf8.Valid(s) {
		fail("name is not valid UTF-8")
	}
	return string(s)
}

// externName reads an import or export name. 0x00 is a plain name; 0x01
// carries a second string (a version suffix or, in older encodings, a URL).
func (r *reader) externName() string {
	switch r.byte() {
	case 0x00:
		return r.name()
	case 0x01:
		n := r.name()
		r.name()
		return n
	default:
		fail("unknown import or export name encoding")
	}
	return ""
}

func appendU32(b []byte, v uint32) []byte {
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b = append(b, c|0x80)
			continue
		}
		return append(b, c)
	}
}

func appendName(b []byte, s string) []byte {
	b = appendU32(b, uint32(len(s)))
	return append(b, s...)
}
