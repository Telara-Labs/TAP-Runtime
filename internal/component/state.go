package component

import (
	"context"
	"fmt"

	"github.com/tetratelabs/wazero/api"
)

// maxHandles bounds the resource table so a guest cannot grow host memory
// without limit by asking for handles it never drops.
const maxHandles = 1 << 16

type handle struct {
	rtype string // the resource name, e.g. "input-stream"
	v     any
}

// state is one run's resource table and core instances.
type state struct {
	cfg      Config
	insts    map[int]api.Module
	handles  map[uint32]handle
	next     uint32
	inGuest  bool // a host function is calling back into the guest (realloc)
	stdinEOF bool
}

func newState(cfg Config) *state {
	return &state{cfg: cfg, insts: map[int]api.Module{}, handles: map[uint32]handle{}, next: 1}
}

func trap(format string, a ...any) { panic(fmt.Errorf(format, a...)) }

func (st *state) newHandle(rtype string, v any) uint32 {
	if len(st.handles) >= maxHandles {
		trap("the component holds more than %d resource handles", maxHandles)
	}
	for {
		h := st.next
		st.next++
		if st.next == 0 {
			st.next = 1
		}
		if _, used := st.handles[h]; !used {
			st.handles[h] = handle{rtype: rtype, v: v}
			return h
		}
	}
}

func (st *state) get(h uint32, rtype string) any {
	e, ok := st.handles[h]
	if !ok || e.rtype != rtype {
		trap("invalid %s handle %d", rtype, h)
	}
	return e.v
}

func (st *state) drop(h uint32, rtype string) {
	st.get(h, rtype)
	delete(st.handles, h)
}

// call is one invocation of a host function.
type call struct {
	ctx context.Context
	st  *state
	h   *hostFunc
	mem api.Memory
}

func (c *call) memory() api.Memory {
	if c.mem != nil {
		return c.mem
	}
	if c.h.memory == nil {
		trap("%s needs the component's memory, which its canonical options do not name", c.h.key())
	}
	m := c.st.insts[c.h.memory.inst]
	if m == nil {
		trap("the component's memory is not instantiated yet")
	}
	c.mem = m.ExportedMemory(c.h.memory.name)
	if c.mem == nil {
		trap("the component's memory export %q is missing", c.h.memory.name)
	}
	return c.mem
}

func (c *call) write(ptr uint32, b []byte) {
	if !c.memory().Write(ptr, b) {
		trap("out of bounds memory access")
	}
}

func (c *call) read(ptr, n uint32) []byte {
	b, ok := c.memory().Read(ptr, n)
	if !ok {
		trap("out of bounds memory access")
	}
	return b
}

func (c *call) u8(ptr uint32, v uint8) {
	if !c.memory().WriteByte(ptr, v) {
		trap("out of bounds memory access")
	}
}

func (c *call) u32(ptr, v uint32) {
	if !c.memory().WriteUint32Le(ptr, v) {
		trap("out of bounds memory access")
	}
}

func (c *call) u64(ptr uint32, v uint64) {
	if !c.memory().WriteUint64Le(ptr, v) {
		trap("out of bounds memory access")
	}
}

func (c *call) readU32(ptr uint32) uint32 {
	v, ok := c.memory().ReadUint32Le(ptr)
	if !ok {
		trap("out of bounds memory access")
	}
	return v
}

// alloc asks the guest's realloc for size bytes, as the canonical ABI does
// for every list and string the host returns. The guest must not call back
// into the host while it allocates.
func (c *call) alloc(align, size uint32) uint32 {
	if c.h.realloc == nil {
		trap("%s needs the component's realloc, which its canonical options do not name", c.h.key())
	}
	m := c.st.insts[c.h.realloc.inst]
	if m == nil {
		trap("the component's realloc is not instantiated yet")
	}
	fn := m.ExportedFunction(c.h.realloc.name)
	if fn == nil {
		trap("the component's realloc export %q is missing", c.h.realloc.name)
	}
	c.st.inGuest = true
	res, err := fn.Call(c.ctx, 0, 0, uint64(align), uint64(size))
	c.st.inGuest = false
	if err != nil {
		panic(err)
	}
	ptr := uint32(res[0])
	if ptr%align != 0 || uint64(ptr)+uint64(size) > uint64(c.memory().Size()) {
		trap("realloc returned an invalid pointer")
	}
	return ptr
}

// list stores n elements of size bytes each and returns the pointer.
func (c *call) list(n, size, align uint32) uint32 {
	total := uint64(n) * uint64(size)
	if total > 1<<31 {
		trap("list too large")
	}
	return c.alloc(align, uint32(total))
}

// str copies s into guest memory and returns its pointer and length.
func (c *call) str(s string) (uint32, uint32) {
	p := c.alloc(1, uint32(len(s)))
	c.write(p, []byte(s))
	return p, uint32(len(s))
}

// hostFunction wraps a host function for wazero. Functions the runner does
// not implement trap with a message that names them.
func (st *state) hostFunction(h *hostFunc, sig coreSig) api.GoModuleFunc {
	impl := implementation(h)
	return func(ctx context.Context, _ api.Module, stack []uint64) {
		if st.inGuest {
			trap("the component called %s while allocating memory for the host", h.key())
		}
		c := &call{ctx: ctx, st: st, h: h}
		if h.drop {
			st.drop(uint32(stack[0]), h.name)
			return
		}
		if impl == nil {
			trap("the component called %s.%s, which TAP does not provide to a primitive; files, commands, network requests and tools go through the broker", h.iface, h.name)
		}
		impl.fn(c, stack)
	}
}

// checkSignature refuses a component whose core import of a WASI function
// does not have the canonical ABI signature the runner implements.
func checkSignature(h *hostFunc, sig coreSig) {
	want := "i>"
	if !h.drop {
		impl := implementation(h)
		if impl == nil {
			return
		}
		want = impl.sig
	}
	if sig.String() != want {
		fail("the component imports %s with core signature %s; WASI 0.2 defines %s", h.key(), sig, want)
	}
}
