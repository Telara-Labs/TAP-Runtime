package component

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"time"

	"github.com/tetratelabs/wazero/sys"
)

// This file is the whole of what a component can reach on the host. Each
// entry is one WASI 0.2 function with its canonical ABI core signature
// (parameters > results; i = i32, I = i64). A function not listed here traps
// when called. Resource names are unique across the WASI 0.2 packages the
// runner accepts, so a handle's type is its resource name.

type impl struct {
	sig string
	fn  func(c *call, s []uint64)
}

type inStream struct{ r io.Reader }
type outStream struct{ w io.Writer }
type ioError struct{ msg string }
type pollable struct{ deadline time.Time } // zero: always ready
type network struct{}

const (
	maxRead  = 64 << 10 // bytes returned by one stream read
	maxWrite = 64 << 10 // bytes check-write permits
	maxBytes = 16 << 20 // bytes one random or zero-write request may ask for
)

// Sockets error-code access-denied: every socket and lookup is refused.
const socketAccessDenied = 1

func implementation(h *hostFunc) *impl {
	if h.drop {
		return nil
	}
	if i, ok := impls[h.iface+"#"+h.name]; ok {
		return &i
	}
	return nil
}

var impls = map[string]impl{
	// wasi:cli: arguments only; no environment variables, no working directory.
	"wasi:cli/environment#get-environment": {"i>", func(c *call, s []uint64) {
		c.u32(arg(s, 0), c.list(0, 16, 4))
		c.u32(arg(s, 0)+4, 0)
	}},
	"wasi:cli/environment#get-arguments": {"i>", func(c *call, s []uint64) {
		args := c.st.cfg.Args
		p := c.list(uint32(len(args)), 8, 4)
		for i, a := range args {
			sp, sl := c.str(a)
			c.u32(p+uint32(i)*8, sp)
			c.u32(p+uint32(i)*8+4, sl)
		}
		c.u32(arg(s, 0), p)
		c.u32(arg(s, 0)+4, uint32(len(args)))
	}},
	"wasi:cli/environment#initial-cwd": {"i>", func(c *call, s []uint64) { c.u8(arg(s, 0), 0) }},
	"wasi:cli/exit#exit": {"i>", func(c *call, s []uint64) {
		code := uint32(0)
		if arg(s, 0) != 0 {
			code = 1
		}
		panic(sys.NewExitError(code))
	}},
	"wasi:cli/exit#exit-with-code": {"i>", func(c *call, s []uint64) {
		panic(sys.NewExitError(uint32(uint8(s[0]))))
	}},
	"wasi:cli/stdin#get-stdin": {">i", func(c *call, s []uint64) {
		s[0] = uint64(c.st.newHandle("input-stream", &inStream{r: c.st.cfg.Stdin}))
	}},
	"wasi:cli/stdout#get-stdout": {">i", func(c *call, s []uint64) {
		s[0] = uint64(c.st.newHandle("output-stream", &outStream{w: c.st.cfg.Stdout}))
	}},
	"wasi:cli/stderr#get-stderr": {">i", func(c *call, s []uint64) {
		s[0] = uint64(c.st.newHandle("output-stream", &outStream{w: c.st.cfg.Stderr}))
	}},
	// No terminal is attached to a primitive's streams.
	"wasi:cli/terminal-stdin#get-terminal-stdin":   {"i>", func(c *call, s []uint64) { c.u8(arg(s, 0), 0) }},
	"wasi:cli/terminal-stdout#get-terminal-stdout": {"i>", func(c *call, s []uint64) { c.u8(arg(s, 0), 0) }},
	"wasi:cli/terminal-stderr#get-terminal-stderr": {"i>", func(c *call, s []uint64) { c.u8(arg(s, 0), 0) }},

	// wasi:clocks: the same clock readings a Preview 1 guest gets.
	"wasi:clocks/monotonic-clock#now":        {">I", func(c *call, s []uint64) { s[0] = uint64(c.st.cfg.Nanotime()) }},
	"wasi:clocks/monotonic-clock#resolution": {">I", func(c *call, s []uint64) { s[0] = 1 }},
	"wasi:clocks/monotonic-clock#subscribe-instant": {"I>i", func(c *call, s []uint64) {
		wait := time.Duration(int64(s[0]) - c.st.cfg.Nanotime())
		s[0] = uint64(c.st.newHandle("pollable", timer(wait)))
	}},
	"wasi:clocks/monotonic-clock#subscribe-duration": {"I>i", func(c *call, s []uint64) {
		s[0] = uint64(c.st.newHandle("pollable", timer(time.Duration(min(s[0], 1<<62)))))
	}},
	"wasi:clocks/wall-clock#now": {"i>", func(c *call, s []uint64) {
		sec, nsec := c.st.cfg.Walltime()
		c.u64(arg(s, 0), uint64(sec))
		c.u32(arg(s, 0)+8, uint32(nsec))
	}},
	"wasi:clocks/wall-clock#resolution": {"i>", func(c *call, s []uint64) {
		c.u64(arg(s, 0), 0)
		c.u32(arg(s, 0)+8, 1)
	}},

	// wasi:io
	"wasi:io/error#[method]error.to-debug-string": {"ii>", func(c *call, s []uint64) {
		e := c.st.get(arg(s, 0), "error").(*ioError)
		p, n := c.str(e.msg)
		c.u32(arg(s, 1), p)
		c.u32(arg(s, 1)+4, n)
	}},
	"wasi:io/poll#[method]pollable.ready": {"i>i", func(c *call, s []uint64) {
		p := c.st.get(arg(s, 0), "pollable").(*pollable)
		s[0] = 0
		if !time.Now().Before(p.deadline) {
			s[0] = 1
		}
	}},
	"wasi:io/poll#[method]pollable.block": {"i>", func(c *call, s []uint64) {
		c.wait(c.st.get(arg(s, 0), "pollable").(*pollable).deadline)
	}},
	"wasi:io/poll#poll": {"iii>", func(c *call, s []uint64) {
		ptr, n, ret := arg(s, 0), arg(s, 1), arg(s, 2)
		if n == 0 {
			trap("poll called with no pollables")
		}
		if n > maxHandles {
			trap("poll called with too many pollables")
		}
		ps := make([]*pollable, n)
		earliest := time.Time{}
		for i := range ps {
			ps[i] = c.st.get(c.readU32(ptr+uint32(i)*4), "pollable").(*pollable)
			if i == 0 || ps[i].deadline.Before(earliest) {
				earliest = ps[i].deadline
			}
		}
		c.wait(earliest)
		now := time.Now()
		var ready []uint32
		for i, p := range ps {
			if !now.Before(p.deadline) {
				ready = append(ready, uint32(i))
			}
		}
		out := c.list(uint32(len(ready)), 4, 4)
		for i, v := range ready {
			c.u32(out+uint32(i)*4, v)
		}
		c.u32(ret, out)
		c.u32(ret+4, uint32(len(ready)))
	}},
	"wasi:io/streams#[method]input-stream.read":          {"iIi>", streamRead},
	"wasi:io/streams#[method]input-stream.blocking-read": {"iIi>", streamRead},
	"wasi:io/streams#[method]input-stream.skip":          {"iIi>", streamSkip},
	"wasi:io/streams#[method]input-stream.blocking-skip": {"iIi>", streamSkip},
	"wasi:io/streams#[method]input-stream.subscribe": {"i>i", func(c *call, s []uint64) {
		c.st.get(arg(s, 0), "input-stream")
		s[0] = uint64(c.st.newHandle("pollable", &pollable{}))
	}},
	"wasi:io/streams#[method]output-stream.check-write": {"ii>", func(c *call, s []uint64) {
		c.st.get(arg(s, 0), "output-stream")
		c.u8(arg(s, 1), 0)
		c.u64(arg(s, 1)+8, maxWrite)
	}},
	"wasi:io/streams#[method]output-stream.write":                    {"iiii>", streamWrite},
	"wasi:io/streams#[method]output-stream.blocking-write-and-flush": {"iiii>", streamWrite},
	"wasi:io/streams#[method]output-stream.flush":                    {"ii>", streamFlush},
	"wasi:io/streams#[method]output-stream.blocking-flush":           {"ii>", streamFlush},
	"wasi:io/streams#[method]output-stream.subscribe": {"i>i", func(c *call, s []uint64) {
		c.st.get(arg(s, 0), "output-stream")
		s[0] = uint64(c.st.newHandle("pollable", &pollable{}))
	}},
	"wasi:io/streams#[method]output-stream.write-zeroes":                    {"iIi>", streamZeroes},
	"wasi:io/streams#[method]output-stream.blocking-write-zeroes-and-flush": {"iIi>", streamZeroes},

	// wasi:filesystem: no preopened directory, so no descriptor can exist.
	"wasi:filesystem/preopens#get-directories": {"i>", func(c *call, s []uint64) {
		c.u32(arg(s, 0), c.list(0, 12, 4))
		c.u32(arg(s, 0)+4, 0)
	}},
	"wasi:filesystem/types#filesystem-error-code": {"ii>", func(c *call, s []uint64) {
		c.st.get(arg(s, 0), "error")
		c.u8(arg(s, 1), 0)
	}},

	// wasi:random: the same random source a Preview 1 guest gets.
	"wasi:random/random#get-random-bytes":            {"Ii>", randomBytes},
	"wasi:random/random#get-random-u64":              {">I", randomU64},
	"wasi:random/insecure#get-insecure-random-bytes": {"Ii>", randomBytes},
	"wasi:random/insecure#get-insecure-random-u64":   {">I", randomU64},
	"wasi:random/insecure-seed#insecure-seed": {"i>", func(c *call, s []uint64) {
		b := c.random(16)
		c.u64(arg(s, 0), binary.LittleEndian.Uint64(b))
		c.u64(arg(s, 0)+8, binary.LittleEndian.Uint64(b[8:]))
	}},

	// wasi:sockets: a network handle exists so standard libraries can ask,
	// and every socket or name lookup they ask for is refused.
	"wasi:sockets/instance-network#instance-network": {">i", func(c *call, s []uint64) {
		s[0] = uint64(c.st.newHandle("network", network{}))
	}},
	"wasi:sockets/tcp-create-socket#create-tcp-socket": {"ii>", socketRefused},
	"wasi:sockets/udp-create-socket#create-udp-socket": {"ii>", socketRefused},
	"wasi:sockets/ip-name-lookup#resolve-addresses": {"iiii>", func(c *call, s []uint64) {
		c.st.get(arg(s, 0), "network")
		c.u8(arg(s, 3), 1)
		c.u8(arg(s, 3)+4, socketAccessDenied)
	}},
}

func arg(s []uint64, i int) uint32 { return uint32(s[i]) }

func timer(d time.Duration) *pollable {
	if d <= 0 {
		return &pollable{}
	}
	return &pollable{deadline: time.Now().Add(d)}
}

// wait sleeps until the deadline or until the run is stopped.
func (c *call) wait(deadline time.Time) {
	d := time.Until(deadline)
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-c.ctx.Done():
		panic(c.ctx.Err())
	}
}

// streamError stores stream-error at ptr: closed, or last-operation-failed
// with an error resource carrying the message.
func (c *call) streamError(ptr uint32, err error) {
	if err == nil || errors.Is(err, io.EOF) {
		c.u8(ptr, 1)
		return
	}
	c.u8(ptr, 0)
	c.u32(ptr+4, c.st.newHandle("error", &ioError{msg: err.Error()}))
}

func (c *call) readSome(in *inStream, n uint64) ([]byte, error) {
	if in.r == nil {
		return nil, io.EOF
	}
	buf := make([]byte, min(n, maxRead))
	for {
		k, err := in.r.Read(buf)
		if k > 0 {
			return buf[:k], nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// read and blocking-read: result<list<u8>, stream-error>, payload at 4.
func streamRead(c *call, s []uint64) {
	in := c.st.get(arg(s, 0), "input-stream").(*inStream)
	ret := arg(s, 2)
	var b []byte
	if s[1] > 0 {
		var err error
		if b, err = c.readSome(in, s[1]); err != nil {
			c.u8(ret, 1)
			c.streamError(ret+4, err)
			return
		}
	}
	p := c.list(uint32(len(b)), 1, 1)
	c.write(p, b)
	c.u8(ret, 0)
	c.u32(ret+4, p)
	c.u32(ret+8, uint32(len(b)))
}

// skip and blocking-skip: result<u64, stream-error>, payload at 8.
func streamSkip(c *call, s []uint64) {
	in := c.st.get(arg(s, 0), "input-stream").(*inStream)
	ret := arg(s, 2)
	var n int
	if s[1] > 0 {
		b, err := c.readSome(in, s[1])
		if err != nil {
			c.u8(ret, 1)
			c.streamError(ret+8, err)
			return
		}
		n = len(b)
	}
	c.u8(ret, 0)
	c.u64(ret+8, uint64(n))
}

// write and blocking-write-and-flush: result<_, stream-error>, payload at 4.
func streamWrite(c *call, s []uint64) {
	out := c.st.get(arg(s, 0), "output-stream").(*outStream)
	b := c.read(arg(s, 1), arg(s, 2))
	ret := arg(s, 3)
	if err := writeAll(out, b); err != nil {
		c.u8(ret, 1)
		c.streamError(ret+4, err)
		return
	}
	c.u8(ret, 0)
}

func streamFlush(c *call, s []uint64) {
	c.st.get(arg(s, 0), "output-stream")
	c.u8(arg(s, 1), 0)
}

func streamZeroes(c *call, s []uint64) {
	out := c.st.get(arg(s, 0), "output-stream").(*outStream)
	if s[1] > maxBytes {
		trap("write-zeroes asked for %d bytes", s[1])
	}
	ret := arg(s, 2)
	if err := writeAll(out, make([]byte, s[1])); err != nil {
		c.u8(ret, 1)
		c.streamError(ret+4, err)
		return
	}
	c.u8(ret, 0)
}

func writeAll(out *outStream, b []byte) error {
	if out.w == nil || len(b) == 0 {
		return nil
	}
	_, err := out.w.Write(b)
	if errors.Is(err, io.EOF) {
		err = io.ErrClosedPipe
	}
	return err
}

func (c *call) random(n uint64) []byte {
	if n > maxBytes {
		trap("asked for %d random bytes", n)
	}
	b := make([]byte, n)
	src := c.st.cfg.Rand
	if src == nil {
		src = rand.Reader
	}
	if _, err := io.ReadFull(src, b); err != nil {
		trap("random source: %v", err)
	}
	return b
}

func randomBytes(c *call, s []uint64) {
	b := c.random(s[0])
	p := c.list(uint32(len(b)), 1, 1)
	c.write(p, b)
	c.u32(arg(s, 1), p)
	c.u32(arg(s, 1)+4, uint32(len(b)))
}

func randomU64(c *call, s []uint64) { s[0] = binary.LittleEndian.Uint64(c.random(8)) }

// create-tcp-socket and create-udp-socket: result<own<socket>, error-code>.
func socketRefused(c *call, s []uint64) {
	c.u8(arg(s, 1), 1)
	c.u8(arg(s, 1)+4, socketAccessDenied)
}
