package component

import (
	"strings"
)

// Core sorts, as encoded after a 0x00 sort prefix.
const (
	coreFunc   = 0x100 | 0x00
	coreTable  = 0x100 | 0x01
	coreMemory = 0x100 | 0x02
	coreGlobal = 0x100 | 0x03
	coreType   = 0x100 | 0x10
	coreMod    = 0x100 | 0x11
	coreInst   = 0x100 | 0x12

	sortFunc      = 0x01
	sortValue     = 0x02
	sortType      = 0x03
	sortComponent = 0x04
	sortInstance  = 0x05
)

// coreItem is a core function, table, memory or global. It is either an
// export of an instantiated core module (inst, name) or a function the runner
// supplies (host).
type coreItem struct {
	sort int
	inst int
	name string
	host *hostFunc
}

// hostFunc is a core function the runner supplies: a lowered WASI function
// or a resource destructor.
type hostFunc struct {
	iface   string // versionless interface, e.g. "wasi:io/streams"
	name    string // function name, or the resource name for a drop
	drop    bool
	memory  *coreItem
	realloc *coreItem
}

func (h *hostFunc) key() string {
	if h.drop {
		return h.iface + "#[resource-drop]" + h.name
	}
	return h.iface + "#" + h.name
}

// coreInstance is a real instantiation of a core module or a bag of items
// collected from other instances.
type coreInstance struct {
	real    bool
	module  int
	args    map[string]int
	exports map[string]coreItem
	// filled for real instances when the component is planned
	imports [][2]string
	hosts   []*hostFunc
	sigs    []coreSig
}

// compFunc is a component-level function: an imported WASI function or a
// core function lifted to the component level.
type compFunc struct {
	iface, name string
	lifted      *coreItem
	postReturn  *coreItem
}

// compType records the one thing the runner needs about a type: which
// imported resource it names, if any.
type compType struct {
	iface, resource string
	local           bool // a resource the component defines itself
}

// compInstance is an imported WASI interface or a component instance whose
// exports are known.
type compInstance struct {
	iface   string // full imported name, with version
	exports map[string]any
}

type space struct {
	coreModules   []*coreModule
	coreInstances []*coreInstance
	coreFuncs     []coreItem
	coreTables    []coreItem
	coreMems      []coreItem
	coreGlobals   []coreItem
	funcs         []*compFunc
	types         []compType
	instances     []*compInstance
	components    [][]byte
	exports       map[string]any
}

// allowedPackages are the WASI 0.2 packages a component may import. Socket
// interfaces are allowed so standard libraries that link them load; every
// socket and name-lookup call is refused at run time.
var allowedPackages = map[string]bool{
	"wasi:cli": true, "wasi:io": true, "wasi:clocks": true,
	"wasi:filesystem": true, "wasi:random": true, "wasi:sockets": true,
}

// interfaceName splits "wasi:io/streams@0.2.3" into "wasi:io/streams" and
// checks that it belongs to a WASI 0.2 package the runner supplies.
func interfaceName(full string) string {
	base, version, _ := strings.Cut(full, "@")
	pkg, _, ok := strings.Cut(base, "/")
	if !ok || !allowedPackages[pkg] {
		if strings.HasPrefix(full, "wasi:http/") || strings.HasPrefix(full, "wasi:sockets/") {
			fail("the component imports %s; a TAP primitive gets no direct network access: send requests through the broker's fetch method", full)
		}
		fail("the component imports %s, which TAP does not provide; a primitive reaches files, commands, the network and tools through the broker", full)
	}
	if isPreview3Version(version) {
		needsPreview3("it imports %s", full)
	}
	if version != "" && !strings.HasPrefix(version, "0.2.") {
		fail("the component imports %s; this runner supplies WASI 0.2 and 0.3 interfaces", full)
	}
	return base
}

// parseComponent reads a top-level component and records every definition
// in its index spaces, in order.
func parseComponent(b []byte) *space {
	if len(b) < 8 || string(b[:8]) != string(componentHeader) {
		fail("not a WebAssembly component")
	}
	s := &space{exports: map[string]any{}}
	r := &reader{b: b, off: 8}
	for !r.done() {
		id := r.byte()
		size := r.u32()
		body := r.bytes(int(size))
		sr := &reader{b: body}
		switch id {
		case 0: // custom
		case 1:
			s.coreModules = append(s.coreModules, parseCoreModule(body))
		case 2:
			for n := sr.count(); n > 0; n-- {
				s.coreInstanceDef(sr)
			}
		case 3:
			for n := sr.count(); n > 0; n-- {
				skipCoreType(sr)
			}
		case 4:
			s.components = append(s.components, body)
		case 5:
			for n := sr.count(); n > 0; n-- {
				s.instanceDef(sr)
			}
		case 6:
			for n := sr.count(); n > 0; n-- {
				s.aliasDef(sr)
			}
		case 7:
			for n := sr.count(); n > 0; n-- {
				s.types = append(s.types, compType{local: skipDefType(sr)})
			}
		case 8:
			for n := sr.count(); n > 0; n-- {
				s.canonDef(sr)
			}
		case 9:
			fail("the component has a start function, which TAP does not support")
		case 10:
			for n := sr.count(); n > 0; n-- {
				s.importDef(sr)
			}
		case 11:
			for n := sr.count(); n > 0; n-- {
				s.exportDef(sr)
			}
		case 12:
			fail("the component uses values, which TAP does not support")
		default:
			fail("unknown component section %d", id)
		}
		if id != 0 && id != 1 && id != 4 && !sr.done() {
			fail("component section %d has trailing bytes", id)
		}
	}
	return s
}

func (s *space) coreInstanceDef(r *reader) {
	switch r.byte() {
	case 0x00:
		ci := &coreInstance{real: true, module: r.idx(len(s.coreModules), "core module"), args: map[string]int{}}
		for n := r.count(); n > 0; n-- {
			name := r.name()
			if r.byte() != 0x12 {
				fail("core instantiation arguments must be instances")
			}
			ci.args[name] = r.idx(len(s.coreInstances), "core instance")
		}
		s.coreInstances = append(s.coreInstances, ci)
	case 0x01:
		ci := &coreInstance{exports: map[string]coreItem{}}
		for n := r.count(); n > 0; n-- {
			name := r.name()
			ci.exports[name] = s.coreSortItem(r.byte(), r)
		}
		s.coreInstances = append(s.coreInstances, ci)
	default:
		fail("invalid core instance definition")
	}
}

// coreSortItem reads an index of the given core sort byte.
func (s *space) coreSortItem(c byte, r *reader) coreItem {
	switch 0x100 | int(c) {
	case coreFunc:
		return s.coreFuncs[r.idx(len(s.coreFuncs), "core function")]
	case coreTable:
		return s.coreTables[r.idx(len(s.coreTables), "core table")]
	case coreMemory:
		return s.coreMems[r.idx(len(s.coreMems), "core memory")]
	case coreGlobal:
		return s.coreGlobals[r.idx(len(s.coreGlobals), "core global")]
	}
	fail("core instances may export only functions, tables, memories and globals")
	return coreItem{}
}

func (s *space) addCore(it coreItem) {
	switch it.sort {
	case coreFunc:
		s.coreFuncs = append(s.coreFuncs, it)
	case coreTable:
		s.coreTables = append(s.coreTables, it)
	case coreMemory:
		s.coreMems = append(s.coreMems, it)
	case coreGlobal:
		s.coreGlobals = append(s.coreGlobals, it)
	default:
		fail("unsupported core alias")
	}
}

func (s *space) aliasDef(r *reader) {
	sort := readSort(r)
	switch r.byte() {
	case 0x00: // export of a component instance
		inst := s.instances[r.idx(len(s.instances), "instance")]
		name := r.name()
		switch sort {
		case sortFunc:
			if inst.exports == nil {
				s.funcs = append(s.funcs, &compFunc{iface: interfaceName(inst.iface), name: name})
				return
			}
			f, ok := inst.exports[name].(*compFunc)
			if !ok {
				fail("instance has no function %q", name)
			}
			s.funcs = append(s.funcs, f)
		case sortType:
			if inst.exports == nil {
				s.types = append(s.types, compType{iface: interfaceName(inst.iface), resource: name})
				return
			}
			t, _ := inst.exports[name].(compType)
			s.types = append(s.types, t)
		case sortInstance:
			if inst.exports == nil {
				fail("nested interface instances are not supported")
			}
			i, ok := inst.exports[name].(*compInstance)
			if !ok {
				fail("instance has no instance %q", name)
			}
			s.instances = append(s.instances, i)
		default:
			fail("the component aliases an export kind TAP does not support")
		}
	case 0x01: // export of a core instance
		ci := s.coreInstances[r.idx(len(s.coreInstances), "core instance")]
		name := r.name()
		var it coreItem
		if ci.real {
			it = coreItem{sort: sort, inst: s.instanceIndex(ci), name: name}
		} else {
			var ok bool
			if it, ok = ci.exports[name]; !ok {
				fail("core instance has no export %q", name)
			}
			if it.sort != sort {
				fail("core export %q has a different kind than its alias", name)
			}
		}
		s.addCore(it)
	case 0x02: // outer
		r.u32()
		r.u32()
		if sort != sortType && sort != coreType {
			fail("the component uses outer aliases TAP does not support")
		}
		if sort == sortType {
			s.types = append(s.types, compType{})
		}
	default:
		fail("invalid alias target")
	}
}

func (s *space) instanceIndex(ci *coreInstance) int {
	for i, c := range s.coreInstances {
		if c == ci {
			return i
		}
	}
	fail("unknown core instance")
	return 0
}

type canonOpts struct {
	memory, realloc, postReturn *coreItem
}

func (s *space) readOpts(r *reader) canonOpts {
	var o canonOpts
	for n := r.count(); n > 0; n-- {
		switch r.byte() {
		case 0x00: // utf8
		case 0x01, 0x02:
			fail("the component uses UTF-16 strings; TAP supports UTF-8 components")
		case 0x03:
			it := s.coreMems[r.idx(len(s.coreMems), "core memory")]
			o.memory = &it
		case 0x04:
			it := s.coreFuncs[r.idx(len(s.coreFuncs), "core function")]
			o.realloc = &it
		case 0x05:
			it := s.coreFuncs[r.idx(len(s.coreFuncs), "core function")]
			o.postReturn = &it
		default:
			needsPreview3("it uses async canonical options")
		}
	}
	for _, it := range []*coreItem{o.realloc, o.postReturn, o.memory} {
		if it != nil && it.host != nil {
			fail("a canonical option names a host function")
		}
	}
	return o
}

func (s *space) canonDef(r *reader) {
	switch r.byte() {
	case 0x00: // lift
		if r.byte() != 0x00 {
			fail("invalid canon lift")
		}
		core := s.coreFuncs[r.idx(len(s.coreFuncs), "core function")]
		o := s.readOpts(r)
		r.idx(len(s.types), "type")
		if core.host != nil {
			fail("the component re-exports a host function, which TAP does not support")
		}
		s.funcs = append(s.funcs, &compFunc{lifted: &core, postReturn: o.postReturn})
	case 0x01: // lower
		if r.byte() != 0x00 {
			fail("invalid canon lower")
		}
		f := s.funcs[r.idx(len(s.funcs), "function")]
		o := s.readOpts(r)
		if f.lifted != nil {
			fail("the component lowers one of its own functions, which TAP does not support")
		}
		s.coreFuncs = append(s.coreFuncs, coreItem{sort: coreFunc, host: &hostFunc{iface: f.iface, name: f.name, memory: o.memory, realloc: o.realloc}})
	case 0x03: // resource.drop
		t := s.types[r.idx(len(s.types), "type")]
		if t.iface == "" {
			fail("the component defines its own resource types, which TAP does not support")
		}
		s.coreFuncs = append(s.coreFuncs, coreItem{sort: coreFunc, host: &hostFunc{iface: t.iface, name: t.resource, drop: true}})
	case 0x02, 0x04:
		fail("the component defines its own resource types, which TAP does not support")
	case 0x40, 0x41, 0x42:
		fail("the component uses thread built-ins, which TAP does not support")
	default:
		needsPreview3("it uses async or error-context built-ins")
	}
}

func (s *space) importDef(r *reader) {
	name := r.externName()
	switch r.byte() {
	case 0x05:
		r.u32()
		interfaceName(name)
		s.instances = append(s.instances, &compInstance{iface: name})
	case 0x03:
		switch r.byte() {
		case 0x00:
			r.u32()
		case 0x01:
		default:
			fail("invalid type bound")
		}
		s.types = append(s.types, compType{})
	default:
		fail("the component imports %q, which is not a WASI interface; TAP supplies only WASI 0.2 interfaces", name)
	}
}

func (s *space) exportDef(r *reader) {
	name := r.externName()
	sort := readSort(r)
	var v any
	switch sort {
	case sortFunc:
		f := s.funcs[r.idx(len(s.funcs), "function")]
		s.funcs = append(s.funcs, f)
		v = f
	case sortInstance:
		i := s.instances[r.idx(len(s.instances), "instance")]
		s.instances = append(s.instances, i)
		v = i
	case sortType:
		t := s.types[r.idx(len(s.types), "type")]
		s.types = append(s.types, t)
		v = t
	default:
		fail("the component exports a kind of item TAP does not support")
	}
	skipOpt(r, func() { skipExternDesc(r) })
	s.exports[name] = v
}

func (s *space) instanceDef(r *reader) {
	switch r.byte() {
	case 0x00:
		c := s.components[r.idx(len(s.components), "component")]
		args := map[string]any{}
		for n := r.count(); n > 0; n-- {
			name := r.name()
			args[name] = s.sortItem(readSort(r), r)
		}
		s.instances = append(s.instances, &compInstance{exports: instantiateNested(c, args)})
	case 0x01:
		ex := map[string]any{}
		for n := r.count(); n > 0; n-- {
			name := r.externName()
			ex[name] = s.sortItem(readSort(r), r)
		}
		s.instances = append(s.instances, &compInstance{exports: ex})
	default:
		fail("invalid instance definition")
	}
}

func (s *space) sortItem(sort int, r *reader) any {
	switch sort {
	case sortFunc:
		return s.funcs[r.idx(len(s.funcs), "function")]
	case sortInstance:
		return s.instances[r.idx(len(s.instances), "instance")]
	case sortType:
		return s.types[r.idx(len(s.types), "type")]
	}
	fail("the component passes a kind of item TAP does not support")
	return nil
}

// instantiateNested evaluates a nested component that only re-exports what
// it is given, as toolchains emit to give an exported interface its type.
// A nested component that contains code is refused.
func instantiateNested(b []byte, args map[string]any) map[string]any {
	if len(b) < 8 || string(b[:8]) != string(componentHeader) {
		fail("a nested component has an invalid header")
	}
	n := &space{exports: map[string]any{}}
	r := &reader{b: b, off: 8}
	for !r.done() {
		id := r.byte()
		size := r.u32()
		sr := &reader{b: r.bytes(int(size))}
		switch id {
		case 0:
		case 7:
			for k := sr.count(); k > 0; k-- {
				n.types = append(n.types, compType{local: skipDefType(sr)})
			}
		case 6:
			for k := sr.count(); k > 0; k-- {
				n.aliasDef(sr)
			}
		case 10:
			for k := sr.count(); k > 0; k-- {
				name := sr.externName()
				v, ok := args[name]
				switch sr.byte() {
				case 0x01:
					sr.u32()
					f, isFunc := v.(*compFunc)
					if !ok || !isFunc {
						fail("nested component import %q was not supplied", name)
					}
					n.funcs = append(n.funcs, f)
				case 0x05:
					sr.u32()
					i, isInst := v.(*compInstance)
					if !ok || !isInst {
						fail("nested component import %q was not supplied", name)
					}
					n.instances = append(n.instances, i)
				case 0x03:
					switch sr.byte() {
					case 0x00:
						sr.u32()
					case 0x01:
					default:
						fail("invalid type bound")
					}
					t, _ := v.(compType)
					n.types = append(n.types, t)
				default:
					fail("nested component import %q has a kind TAP does not support", name)
				}
			}
		case 11:
			for k := sr.count(); k > 0; k-- {
				n.exportDef(sr)
			}
		default:
			fail("nested components with code are not supported")
		}
		if id != 0 && !sr.done() {
			fail("nested component section %d has trailing bytes", id)
		}
	}
	return n.exports
}
