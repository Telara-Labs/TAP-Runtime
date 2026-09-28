package journal

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Observed is what a program saw besides the answers to its requests: the
// clock and random bytes.
//
// Ruling 27 (doc 34 section 13.16): replayed steps see what they saw before,
// and steps after the resume point see the real clock and real random bytes.
// So every reading is recorded as it is given, a resumed program is given
// the recorded readings in the order it took them, and once they run out it
// is given the real thing, which is recorded in turn.
//
// A program is one thread, so the order of its readings is its own and is
// the same when it is started again.
type Observed struct {
	mu   sync.Mutex
	f    *os.File
	wall []int64 // unix nanoseconds
	mono []int64
	rnd  []byte

	lastMono  int64
	monoBase  time.Time
	now       func() time.Time
	randomSrc io.Reader
}

const (
	kindWall   = 'W'
	kindMono   = 'N'
	kindRandom = 'R'
)

// OpenObserved opens the record of readings for the run in dir, reading what
// an earlier run of it took.
func OpenObserved(dir string) (*Observed, error) {
	path := filepath.Join(dir, "observed.bin")
	o := &Observed{now: time.Now, randomSrc: rand.Reader, monoBase: time.Now()}
	if old, err := os.Open(path); err == nil {
		r := bufio.NewReader(old)
		for {
			kind, err := r.ReadByte()
			if err != nil {
				break
			}
			var n int64
			if binary.Read(r, binary.BigEndian, &n) != nil {
				break // a reading cut short by a crash recorded nothing
			}
			switch kind {
			case kindWall:
				o.wall = append(o.wall, n)
			case kindMono:
				o.mono = append(o.mono, n)
			case kindRandom:
				b := make([]byte, n)
				if _, err := io.ReadFull(r, b); err != nil {
					n = -1
				} else {
					o.rnd = append(o.rnd, b...)
				}
			}
			if n < 0 {
				break
			}
		}
		old.Close()
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	o.f = f
	return o, nil
}

func (o *Observed) record(kind byte, n int64, extra []byte) {
	buf := make([]byte, 9, 9+len(extra))
	buf[0] = kind
	binary.BigEndian.PutUint64(buf[1:], uint64(n))
	o.f.Write(append(buf, extra...))
}

// Walltime is the clock a program reads.
func (o *Observed) Walltime() (int64, int32) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var t int64
	if len(o.wall) > 0 {
		t, o.wall = o.wall[0], o.wall[1:]
	} else {
		t = o.now().UnixNano()
		o.record(kindWall, t, nil)
	}
	return t / 1e9, int32(t % 1e9)
}

// Nanotime is the monotonic clock a program reads. After the recorded
// readings run out it carries on from the last of them, so it never goes
// backwards.
func (o *Observed) Nanotime() int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.mono) > 0 {
		o.lastMono, o.mono = o.mono[0], o.mono[1:]
		o.monoBase = o.now()
		return o.lastMono
	}
	t := o.lastMono + int64(o.now().Sub(o.monoBase))
	if t <= o.lastMono {
		t = o.lastMono + 1
	}
	o.lastMono, o.monoBase = t, o.now()
	o.record(kindMono, t, nil)
	return t
}

// Read gives random bytes.
func (o *Observed) Read(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := copy(p, o.rnd)
	o.rnd = o.rnd[n:]
	if n < len(p) {
		if _, err := io.ReadFull(o.randomSrc, p[n:]); err != nil {
			return n, err
		}
		o.record(kindRandom, int64(len(p)-n), p[n:])
	}
	return len(p), nil
}

// Replaying reports whether recorded readings remain to be given.
func (o *Observed) Replaying() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.wall) > 0 || len(o.mono) > 0 || len(o.rnd) > 0
}

// Sync puts the readings on disk. It is called before a request is recorded
// as begun, so a request never outlives the readings it was computed from.
func (o *Observed) Sync() error { return o.f.Sync() }

func (o *Observed) Close() error { return o.f.Close() }
