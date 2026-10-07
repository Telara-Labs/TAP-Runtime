package discover

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

type fakeReader struct {
	id   string
	read func() ([]trace.Session, error)
}

func (f fakeReader) Client() string                          { return f.id }
func (f fakeReader) Read(time.Time) ([]trace.Session, error) { return f.read() }

// An agent whose store cannot be read, or whose reader panics, is skipped
// with its reason; every other agent is still read, in reader order.
func TestReadAllSkipsAFailingAgentAndKeepsTheRest(t *testing.T) {
	ok := func(id string) fakeReader {
		return fakeReader{id, func() ([]trace.Session, error) { return []trace.Session{{Client: id, ID: id}}, nil }}
	}
	readers := []trace.Reader{
		ok("claude-code"),
		fakeReader{"cursor", func() ([]trace.Session, error) { return nil, errors.New("sqlite3 read exceeded its deadline") }},
		fakeReader{"zed", func() ([]trace.Session, error) { panic("bad record") }},
		ok("codex"),
	}
	sessions, skipped := readAll(readers, time.Time{}, nil)
	if len(sessions) != 2 || sessions[0].ID != "claude-code" || sessions[1].ID != "codex" {
		t.Errorf("sessions %+v, want claude-code then codex", sessions)
	}
	if len(skipped) != 2 || !strings.HasPrefix(skipped[0], "cursor: sqlite3 read exceeded") || !strings.HasPrefix(skipped[1], "zed: reader failed: bad record") {
		t.Errorf("skipped %q", skipped)
	}
}
