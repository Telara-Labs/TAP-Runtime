package component

import (
	"errors"
	"fmt"
	"strings"
)

// WASI Preview 3 (WASI 0.3) components use the Component Model's async
// features: async canonical options and built-ins, stream and future types,
// and the WASI 0.3 interfaces built on them. This package does not run them.
// It recognises them, so that the runner can hand them to an engine that
// does, and it checks what they import the same way it checks a Preview 2
// component: only WASI packages the runner is willing to provide, and no
// direct network interface.

// ErrPreview3 is wrapped by the error Load returns for a component that needs
// WASI Preview 3, so a caller can tell "run this elsewhere" from "refused".
var ErrPreview3 = errors.New("the component needs WASI Preview 3")

// preview3Marker is raised (as a panic, like formatError) where the Preview 2
// parser meets something only Preview 3 has.
type preview3Marker struct{ why string }

func needsPreview3(format string, a ...any) { panic(preview3Marker{fmt.Sprintf(format, a...)}) }

// isPreview3Version reports whether an interface version is WASI 0.3,
// release candidates included.
func isPreview3Version(version string) bool { return strings.HasPrefix(version, "0.3.") }

// Preview3 reports whether b is a component that needs WASI Preview 3, and
// why. It returns an error for a Preview 3 component that imports something
// TAP does not provide; such a component is refused before anything starts.
// A Preview 2 component, or a binary that is neither, returns false and no
// error: Load gives that one its own verdict.
func Preview3(b []byte) (p3 bool, why string, err error) {
	if len(b) < 8 || string(b[:8]) != string(componentHeader) {
		return false, "", nil
	}
	imports, exports, ok := topLevelNames(b)
	if !ok {
		return false, "", nil
	}
	for _, im := range imports {
		if _, v, _ := strings.Cut(im.name, "@"); im.kind == 0x05 && isPreview3Version(v) {
			p3, why = true, "it imports "+im.name
			break
		}
	}
	if !p3 {
		for _, name := range exports {
			if base, v, _ := strings.Cut(name, "@"); base == "wasi:cli/run" && isPreview3Version(v) {
				p3, why = true, "it exports "+name
			}
		}
	}
	if !p3 {
		// No WASI 0.3 name: it may still use the async ABI. The Preview 2
		// parser stops at the first such use and says what it was.
		func() {
			defer func() {
				if r := recover(); r != nil {
					switch m := r.(type) {
					case preview3Marker:
						p3, why = true, m.why
					case formatError:
					default:
						panic(r)
					}
				}
			}()
			parseComponent(b)
		}()
	}
	if !p3 {
		return false, "", nil
	}
	defer catch(&err)
	for _, im := range imports {
		switch im.kind {
		case 0x03: // a type the component names; it grants nothing
		case 0x05:
			preview3Interface(im.name)
		default:
			fail("the component imports %q, which is not a WASI interface; TAP supplies only WASI interfaces", im.name)
		}
	}
	run := false
	for _, name := range exports {
		base, v, _ := strings.Cut(name, "@")
		if base == "wasi:cli/run" && (isPreview3Version(v) || strings.HasPrefix(v, "0.2.")) {
			run = true
		}
	}
	if !run {
		fail("the component does not export wasi:cli/run; build it as a command (a program with a main function)")
	}
	return true, why, nil
}

// preview3Interface checks one import of a Preview 3 component against the
// packages TAP provides. The packages are those a Preview 2 component may
// import; WASI 0.3 folds wasi:io into the others.
func preview3Interface(full string) {
	base, version, _ := strings.Cut(full, "@")
	pkg, _, ok := strings.Cut(base, "/")
	if !ok || !allowedPackages[pkg] {
		if strings.HasPrefix(full, "wasi:http/") || strings.HasPrefix(full, "wasi:sockets/") {
			fail("the component imports %s; a TAP primitive gets no direct network access: send requests through the broker's fetch method", full)
		}
		fail("the component imports %s, which TAP does not provide; a primitive reaches files, commands, the network and tools through the broker", full)
	}
	if version != "" && !isPreview3Version(version) && !strings.HasPrefix(version, "0.2.") {
		fail("the component imports %s; TAP supplies WASI 0.2 and 0.3 interfaces", full)
	}
}

type importName struct {
	name string
	kind byte
}

// topLevelNames reads the names a component imports and exports, skipping
// every other section by its length. ok is false for a binary it cannot read.
func topLevelNames(b []byte) (imports []importName, exports []string, ok bool) {
	var err error
	func() {
		defer catch(&err)
		r := &reader{b: b, off: 8}
		for !r.done() {
			id := r.byte()
			sr := &reader{b: r.bytes(int(r.u32()))}
			switch id {
			case 10:
				for n := sr.count(); n > 0; n-- {
					name := sr.externName()
					kind := sr.peek()
					skipExternDesc(sr)
					imports = append(imports, importName{name, kind})
				}
			case 11:
				for n := sr.count(); n > 0; n-- {
					name := sr.externName()
					readSort(sr)
					sr.u32()
					skipOpt(sr, func() { skipExternDesc(sr) })
					exports = append(exports, name)
				}
			}
		}
	}()
	return imports, exports, err == nil
}
