package component

// The component binary format does not length-prefix individual type
// definitions, so the parser must read each one to find the next. These
// functions read (and discard) type syntax. The runner does not need the
// structure of the WASI types: every function it supplies is written against
// its known canonical ABI signature, and that signature is checked against
// the core module's import when the component is loaded.

// isPrimVal reports whether c encodes a primitive value type.
func isPrimVal(c byte) bool { return (c >= 0x73 && c <= 0x7f) || c == 0x64 }

func skipValType(r *reader) {
	if isPrimVal(r.peek()) {
		r.byte()
		return
	}
	if r.s33() < 0 {
		fail("invalid value type")
	}
}

func skipOptValType(r *reader) {
	switch r.byte() {
	case 0x00:
	case 0x01:
		skipValType(r)
	default:
		fail("invalid optional value type")
	}
}

// skipDefType reads one deftype and reports whether it defines a resource.
func skipDefType(r *reader) (resource bool) {
	c := r.byte()
	switch {
	case isPrimVal(c):
	case c == 0x72: // record
		for n := r.count(); n > 0; n-- {
			r.name()
			skipValType(r)
		}
	case c == 0x71: // variant
		for n := r.count(); n > 0; n-- {
			r.name()
			skipOptValType(r)
			if r.byte() != 0x00 {
				fail("variant case refinements are not supported")
			}
		}
	case c == 0x70, c == 0x6b: // list, option
		skipValType(r)
	case c == 0x67: // fixed-size list
		skipValType(r)
		r.u32()
	case c == 0x6f: // tuple
		for n := r.count(); n > 0; n-- {
			skipValType(r)
		}
	case c == 0x6e, c == 0x6d: // flags, enum
		for n := r.count(); n > 0; n-- {
			r.name()
		}
	case c == 0x6a: // result
		skipOptValType(r)
		skipOptValType(r)
	case c == 0x69, c == 0x68: // own, borrow
		r.u32()
	case c == 0x66, c == 0x65:
		needsPreview3("it uses async streams or futures")
	case c == 0x40: // func
		skipFuncType(r)
	case c == 0x41: // component type
		for n := r.count(); n > 0; n-- {
			if r.peek() == 0x03 {
				r.byte()
				r.externName()
				skipExternDesc(r)
				continue
			}
			skipInstanceDecl(r)
		}
	case c == 0x42: // instance type
		for n := r.count(); n > 0; n-- {
			skipInstanceDecl(r)
		}
	case c == 0x3f: // resource
		if r.byte() != 0x7f {
			fail("resource representation must be i32")
		}
		skipOpt(r, func() { r.u32() })
		return true
	case c == 0x43, c == 0x3e:
		needsPreview3("it uses async functions or resources")
	default:
		fail("unknown type encoding 0x%02x", c)
	}
	return false
}

func skipOpt(r *reader, f func()) {
	switch r.byte() {
	case 0x00:
	case 0x01:
		f()
	default:
		fail("invalid optional value")
	}
}

func skipFuncType(r *reader) {
	for n := r.count(); n > 0; n-- {
		r.name()
		skipValType(r)
	}
	switch r.byte() {
	case 0x00:
		skipValType(r)
	case 0x01:
		// Named results (older encoding); an empty list is "no result".
		for n := r.count(); n > 0; n-- {
			r.name()
			skipValType(r)
		}
	default:
		fail("invalid function result encoding")
	}
}

func skipInstanceDecl(r *reader) {
	switch r.byte() {
	case 0x00:
		skipCoreType(r)
	case 0x01:
		skipDefType(r)
	case 0x02:
		skipAlias(r)
	case 0x04:
		r.externName()
		skipExternDesc(r)
	default:
		fail("invalid instance type declaration")
	}
}

func skipExternDesc(r *reader) {
	switch r.byte() {
	case 0x00:
		if r.byte() != 0x11 {
			fail("invalid core module type reference")
		}
		r.u32()
	case 0x01, 0x04, 0x05:
		r.u32()
	case 0x02:
		switch r.byte() {
		case 0x00:
			r.u32()
		case 0x01:
			skipValType(r)
		default:
			fail("invalid value bound")
		}
	case 0x03:
		switch r.byte() {
		case 0x00:
			r.u32()
		case 0x01: // (sub resource)
		default:
			fail("invalid type bound")
		}
	default:
		fail("invalid import or export description")
	}
}

// readSort reads a sort and returns it, with core sorts as 0x100|byte.
func readSort(r *reader) int {
	c := r.byte()
	if c == 0x00 {
		return 0x100 | int(r.byte())
	}
	return int(c)
}

func skipAlias(r *reader) {
	readSort(r)
	switch r.byte() {
	case 0x00, 0x01:
		r.u32()
		r.name()
	case 0x02:
		r.u32()
		r.u32()
	default:
		fail("invalid alias target")
	}
}

func skipCoreValType(r *reader) {
	switch r.byte() {
	case 0x7f, 0x7e, 0x7d, 0x7c, 0x7b, 0x70, 0x6f:
	default:
		fail("the component uses a core value type TAP does not support")
	}
}

func skipLimits(r *reader) {
	flags := r.byte()
	if flags > 0x07 {
		fail("invalid limits")
	}
	r.u64()
	if flags&0x01 != 0 {
		r.u64()
	}
}

func skipCoreImportDesc(r *reader) {
	switch r.byte() {
	case 0x00:
		r.u32()
	case 0x01:
		skipCoreValType(r)
		skipLimits(r)
	case 0x02:
		skipLimits(r)
	case 0x03:
		skipCoreValType(r)
		r.byte()
	case 0x04:
		r.byte()
		r.u32()
	default:
		fail("invalid core import description")
	}
}

// skipCoreType reads a core:deftype: a function type or a module type.
func skipCoreType(r *reader) {
	c := r.byte()
	if c == 0x00 {
		c = r.byte()
		if c != 0x50 {
			fail("invalid core type")
		}
	}
	switch c {
	case 0x60:
		for n := r.count(); n > 0; n-- {
			skipCoreValType(r)
		}
		for n := r.count(); n > 0; n-- {
			skipCoreValType(r)
		}
	case 0x50:
		for n := r.count(); n > 0; n-- {
			switch r.byte() {
			case 0x00:
				r.name()
				r.name()
				skipCoreImportDesc(r)
			case 0x01:
				skipCoreType(r)
			case 0x02:
				if r.byte() != 0x10 || r.byte() != 0x01 {
					fail("invalid core module type alias")
				}
				r.u32()
				r.u32()
			case 0x03:
				r.name()
				skipCoreImportDesc(r)
			default:
				fail("invalid core module type declaration")
			}
		}
	default:
		fail("the component uses core types TAP does not support")
	}
}
