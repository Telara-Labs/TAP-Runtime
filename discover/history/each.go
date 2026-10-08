package history

import (
	"time"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

// Streamer is a reader that can pass its sessions on as it reads them,
// instead of returning them all at once. It reads exactly what Read reads;
// only how much is held at a time differs, and the order is its own.
type Streamer interface {
	Each(since time.Time, yield func(trace.Session) error) error
}

// Each passes every session r reads to yield: as r reads them when r is a
// Streamer, otherwise from Read. An error from yield stops the read and is
// returned.
func Each(r trace.Reader, since time.Time, yield func(trace.Session) error) error {
	if s, ok := r.(Streamer); ok {
		return s.Each(since, yield)
	}
	ss, err := r.Read(since)
	if err != nil {
		return err
	}
	for _, s := range ss {
		if err := yield(s); err != nil {
			return err
		}
	}
	return nil
}

// Each passes on the sessions of every reader in the set, one reader after
// another.
func (r ReaderSet) Each(since time.Time, yield func(trace.Session) error) error {
	for _, x := range r.List {
		if err := Each(x, since, yield); err != nil {
			return err
		}
	}
	return nil
}
