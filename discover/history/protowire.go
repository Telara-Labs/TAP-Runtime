package history

// ProtoField is one field of a protobuf message: its number and either a
// varint value or the bytes of a length-delimited value. Only what the
// readers need from stores that keep protobuf records (no protobuf
// dependency in this module).
type ProtoField struct {
	Num   uint64
	Int   uint64
	Bytes []byte
}

// ProtoFields splits one protobuf message into its top-level fields. It stops
// at the first malformed field and returns what it read so far.
func ProtoFields(b []byte) []ProtoField {
	var out []ProtoField
	for i := 0; i < len(b); {
		key, n := uvarint(b[i:])
		if n <= 0 {
			return out
		}
		i += n
		f := ProtoField{Num: key >> 3}
		switch key & 7 {
		case 0:
			v, n := uvarint(b[i:])
			if n <= 0 {
				return out
			}
			f.Int, i = v, i+n
		case 2:
			l, n := uvarint(b[i:])
			if n <= 0 || l > uint64(len(b)-i-n) {
				return out
			}
			i += n
			f.Bytes, i = b[i:i+int(l)], i+int(l)
		case 1:
			i += 8
		case 5:
			i += 4
		default:
			return out
		}
		out = append(out, f)
	}
	return out
}

// ProtoAppend encodes a field (for fixtures and tests).
func ProtoAppend(b []byte, num uint64, v any) []byte {
	putUvarint := func(b []byte, x uint64) []byte {
		for x >= 0x80 {
			b = append(b, byte(x)|0x80)
			x >>= 7
		}
		return append(b, byte(x))
	}
	switch x := v.(type) {
	case uint64:
		b = putUvarint(b, num<<3)
		return putUvarint(b, x)
	case []byte:
		b = putUvarint(b, num<<3|2)
		b = putUvarint(b, uint64(len(x)))
		return append(b, x...)
	case string:
		return ProtoAppend(b, num, []byte(x))
	}
	panic("ProtoAppend: unsupported value")
}
