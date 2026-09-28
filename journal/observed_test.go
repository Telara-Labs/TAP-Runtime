package journal

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Ruling 27: replayed steps see what they saw before, and steps after the
// resume point see the real clock and real random bytes.
func TestReadingsAreReplayedAndThenReal(t *testing.T) {
	dir := t.TempDir()
	then := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	first, err := OpenObserved(dir)
	if err != nil {
		t.Fatal(err)
	}
	first.now = func() time.Time { return then }
	first.randomSrc = bytes.NewReader(bytes.Repeat([]byte{7}, 64))
	s1, _ := first.Walltime()
	s2, _ := first.Walltime()
	m1 := first.Nanotime()
	r1 := make([]byte, 5)
	first.Read(r1)
	first.Close() // and the run stops

	later := then.Add(72 * time.Hour)
	second, err := OpenObserved(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	second.now = func() time.Time { return later }
	second.randomSrc = bytes.NewReader(bytes.Repeat([]byte{9}, 64))
	if !second.Replaying() {
		t.Fatal("nothing to replay")
	}
	a, _ := second.Walltime()
	b, _ := second.Walltime()
	if a != s1 || b != s2 || a != then.Unix() {
		t.Fatalf("replayed clock readings %d, %d; the program saw %d, %d", a, b, s1, s2)
	}
	if m := second.Nanotime(); m != m1 {
		t.Fatalf("replayed monotonic reading %d, the program saw %d", m, m1)
	}
	got := make([]byte, 8) // asks for more than was recorded
	second.Read(got)
	if !bytes.Equal(got[:5], r1) || !bytes.Equal(got[5:], []byte{9, 9, 9}) {
		t.Fatalf("random bytes %v: want the 5 recorded, then real ones", got)
	}
	// Caught up: the real clock, three days on.
	if second.Replaying() {
		t.Fatal("still replaying after every reading was given")
	}
	if c, _ := second.Walltime(); c != later.Unix() {
		t.Fatalf("after the resume point the clock read %d, want the real %d", c, later.Unix())
	}
	if m := second.Nanotime(); m <= m1 {
		t.Fatalf("the monotonic clock went backwards: %d after %d", m, m1)
	}

	// A third start replays everything the first two were given.
	second.Sync()
	third, _ := OpenObserved(dir)
	defer third.Close()
	for i, want := range []int64{then.Unix(), then.Unix(), later.Unix()} {
		if got, _ := third.Walltime(); got != want {
			t.Fatalf("third start, reading %d: %d, want %d", i, got, want)
		}
	}
}

func TestAReadingCutShortIsIgnored(t *testing.T) {
	dir := t.TempDir()
	o, _ := OpenObserved(dir)
	o.now = func() time.Time { return time.Unix(1000, 0) }
	o.Walltime()
	o.Close()
	f, _ := os.OpenFile(filepath.Join(dir, "observed.bin"), os.O_APPEND|os.O_WRONLY, 0o600)
	f.Write([]byte{'W', 0, 0, 0}) // power lost mid-write
	f.Close()
	o, err := OpenObserved(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	o.now = func() time.Time { return time.Unix(2000, 0) }
	if s, _ := o.Walltime(); s != 1000 {
		t.Fatalf("first reading %d, want the recorded 1000", s)
	}
	if s, _ := o.Walltime(); s != 2000 {
		t.Fatalf("second reading %d, want the real 2000: a half-written reading was taken as one", s)
	}
}
