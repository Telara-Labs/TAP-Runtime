package component

import (
	"github.com/tetratelabs/wazero/api"
)

// coreImport is one import of a core module.
type coreImport struct {
	module, field string
	kind          byte   // 0 func, 1 table, 2 memory, 3 global, 4 tag
	desc          []byte // the encoded description, copied unchanged on rewrite
	sig           *coreSig
}

// coreSig is a core function type.
type coreSig struct{ params, results []api.ValueType }

func (s coreSig) String() string {
	b := make([]byte, 0, len(s.params)+len(s.results)+1)
	for _, p := range s.params {
		b = append(b, sigChar(p))
	}
	b = append(b, '>')
	for _, p := range s.results {
		b = append(b, sigChar(p))
	}
	return string(b)
}

func sigChar(t api.ValueType) byte {
	switch t {
	case api.ValueTypeI32:
		return 'i'
	case api.ValueTypeI64:
		return 'I'
	case api.ValueTypeF32:
		return 'f'
	case api.ValueTypeF64:
		return 'F'
	}
	return '?'
}

// coreModule is a core module embedded in a component, with its import
// section located so the imports can be renamed.
type coreModule struct {
	raw         []byte
	imports     []coreImport
	importStart int // offset of the import section's id byte
	importEnd   int // offset just after the import section
	memories    int // linear memories the module defines (not imports)
}

func parseCoreModule(b []byte) *coreModule {
	if len(b) < 8 || string(b[:8]) != string(moduleHeader) {
		fail("an embedded core module has an invalid header")
	}
	m := &coreModule{raw: b}
	r := &reader{b: b, off: 8}
	var types []coreSig
	for !r.done() {
		start := r.off
		id := r.byte()
		size := r.u32()
		body := r.bytes(int(size))
		end := r.off
		sr := &reader{b: body}
		switch id {
		case 1:
			for n := sr.count(); n > 0; n-- {
				if sr.byte() != 0x60 {
					fail("an embedded core module uses type definitions TAP does not support")
				}
				var s coreSig
				for k := sr.count(); k > 0; k-- {
					s.params = append(s.params, coreValType(sr))
				}
				for k := sr.count(); k > 0; k-- {
					s.results = append(s.results, coreValType(sr))
				}
				types = append(types, s)
			}
		case 5:
			m.memories = sr.count()
		case 2:
			m.importStart, m.importEnd = start, end
			for n := sr.count(); n > 0; n-- {
				imp := coreImport{module: sr.name(), field: sr.name()}
				d0 := sr.off
				imp.kind = sr.peek()
				if imp.kind == 0x00 {
					sr.byte()
					t := sr.idx(len(types), "core type")
					imp.sig = &types[t]
				} else {
					skipCoreImportDesc(sr)
				}
				imp.desc = append([]byte(nil), body[d0:sr.off]...)
				m.imports = append(m.imports, imp)
			}
		}
	}
	return m
}

func coreValType(r *reader) api.ValueType {
	switch c := r.byte(); c {
	case 0x7f:
		return api.ValueTypeI32
	case 0x7e:
		return api.ValueTypeI64
	case 0x7d:
		return api.ValueTypeF32
	case 0x7c:
		return api.ValueTypeF64
	case 0x70:
		return api.ValueType(0x70) // funcref: never part of a WASI signature
	case 0x6f:
		return api.ValueTypeExternref
	}
	fail("an embedded core module uses a value type TAP does not support")
	return 0
}

// rewrite returns the module with each import's module and field names
// replaced by names[i]. The rest of the module is copied byte for byte.
func (m *coreModule) rewrite(names [][2]string) []byte {
	if len(m.imports) == 0 {
		return m.raw
	}
	var sec []byte
	sec = appendU32(sec, uint32(len(m.imports)))
	for i, imp := range m.imports {
		sec = appendName(sec, names[i][0])
		sec = appendName(sec, names[i][1])
		sec = append(sec, imp.desc...)
	}
	out := make([]byte, 0, len(m.raw)+len(sec))
	out = append(out, m.raw[:m.importStart]...)
	out = append(out, 2)
	out = appendU32(out, uint32(len(sec)))
	out = append(out, sec...)
	out = append(out, m.raw[m.importEnd:]...)
	return out
}
