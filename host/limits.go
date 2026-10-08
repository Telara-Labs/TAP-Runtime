package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// What a run is held to when its manifest says nothing. A
// primitive that declares execution.timeoutSeconds or limits.max_dispatches
// is held to that instead.
const (
	defaultTimeout     = 10 * time.Minute
	defaultDispatches  = 1000
	maxGuestLine       = 16 << 20 // one protocol line from the guest
	maxGuestStderr     = 16 << 20 // cumulative native diagnostics from one guest
	maxCommandOutput   = 16 << 20 // stdout, and separately stderr, of one host program
	commandTimeout     = 10 * time.Minute
	guestMemoryPages   = 8192 // 512 MiB of 64 KiB pages
	maxDispatchesLimit = "max_dispatches"
)

// unenforcedLimits are limits the manifest format accepts that this runner
// has no way to apply. They are named in the log so a bound that is not held
// never reads as one that is.
var unenforcedLimits = []string{"max_steps", "max_input_bytes", "max_checkpoint_bytes"}

// budget is the wall-clock time a run has. It stops while a person is being
// asked, because their thinking is not the program's running.
type budget struct {
	mu        sync.Mutex
	timer     *time.Timer
	remaining time.Duration
	since     time.Time
	paused    int
	fired     bool
	stopped   bool
	onExpire  func()
}

func newBudget(d time.Duration, onExpire func()) *budget {
	b := &budget{remaining: d, onExpire: onExpire, since: time.Now()}
	b.timer = time.AfterFunc(d, b.expire)
	return b
}

func (b *budget) expire() {
	b.mu.Lock()
	if b.stopped || b.paused > 0 {
		b.mu.Unlock()
		return
	}
	b.fired = true
	b.mu.Unlock()
	b.onExpire()
}

// pause stops the clock until the matching resume.
func (b *budget) pause() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped {
		return
	}
	if b.paused == 0 {
		if b.timer.Stop() {
			b.remaining -= time.Since(b.since)
			if b.remaining < 0 {
				b.remaining = 0
			}
		}
	}
	b.paused++
}

func (b *budget) resume() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped || b.paused == 0 {
		return
	}
	b.paused--
	if b.paused == 0 {
		b.since = time.Now()
		b.timer = time.AfterFunc(b.remaining, b.expire)
	}
}

func (b *budget) stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopped = true
	b.timer.Stop()
}

func (b *budget) expired() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fired
}

// timeoutFor is how long the manifest lets a run take.
func timeoutFor(seconds int) time.Duration {
	if seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return defaultTimeout
}

// dispatchesFor is how many requests the manifest lets a run make.
func dispatchesFor(limits map[string]int) int {
	if n, ok := limits[maxDispatchesLimit]; ok {
		return n
	}
	return defaultDispatches
}

var errLineTooLong = errors.New("the program's output is larger than the runner accepts; return less of it")

// readLine reads one line, refusing one longer than max. A guest that never
// writes a newline would otherwise grow the runner's memory without bound.
func readLine(rd *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := rd.ReadSlice('\n')
		if len(line)+len(chunk) > max {
			return nil, fmt.Errorf("%w (%d bytes)", errLineTooLong, max)
		}
		line = append(line, chunk...)
		if err == bufio.ErrBufferFull {
			continue
		}
		return line, err
	}
}

// cappedBuffer keeps the first max bytes written to it and drops the rest, so
// a host program that prints without end cannot fill the runner's memory.
type cappedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := c.max - c.buf.Len()
	if room <= 0 {
		if len(p) > 0 {
			c.truncated = true
		}
		return len(p), nil
	}
	if len(p) > room {
		c.buf.Write(p[:room])
		c.truncated = true
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *cappedBuffer) Len() int       { return c.buf.Len() }
func (c *cappedBuffer) String() string { return c.buf.String() }

// WriteString adds the runner's own note, which the cap does not drop.
func (c *cappedBuffer) WriteString(s string) { c.buf.WriteString(s) }

// guestStderr bounds cumulative diagnostics independently of protocol output.
// Keep recognizing memory failures even after the retained prefix is full,
// including a diagnostic split between writes.
type guestStderr struct {
	cappedBuffer
	memoryFailure bool
	tail          [32]byte
	tailLen       int
}

func (g *guestStderr) Write(p []byte) (int, error) {
	if !g.memoryFailure {
		var boundary [64]byte
		n := copy(boundary[:], g.tail[:g.tailLen])
		n += copy(boundary[n:], p[:min(len(p), 32)])
		g.memoryFailure = outOfMemory(string(boundary[:n])) || bytes.Contains(p, []byte("out of memory")) || bytes.Contains(p, []byte("MemoryError"))
	}
	if len(p) >= 32 {
		g.tailLen = copy(g.tail[:], p[len(p)-32:])
	} else {
		keep := min(g.tailLen, 32-len(p))
		copy(g.tail[:], g.tail[g.tailLen-keep:g.tailLen])
		g.tailLen = keep + copy(g.tail[keep:], p)
	}
	return g.cappedBuffer.Write(p)
}

func (g *guestStderr) String() string {
	s := g.cappedBuffer.String()
	if g.truncated {
		s += "\n... (guest stderr past the runner's limit was dropped)"
	}
	return s
}

// clipLines keeps the first n lines of a text and says how many it left out.
// A guest that runs out of memory writes its whole runtime state, which is no
// help to anyone reading a log.
func clipLines(s string, n int) string {
	lines := strings.Count(s, "\n") + 1
	if lines <= n {
		return s
	}
	end := 0
	for i := 0; i < n; i++ {
		end += strings.IndexByte(s[end:], '\n')
		if i+1 < n {
			end++
		}
	}
	return s[:end] + fmt.Sprintf("\n... (%d more lines)", lines-n)
}

// outOfMemory reports whether a guest's error output says it ran out of memory
// (the Go runtime of the bash interpreter and CPython both say so).
func outOfMemory(stderr string) bool {
	return strings.Contains(stderr, "out of memory") || strings.Contains(stderr, "MemoryError")
}
