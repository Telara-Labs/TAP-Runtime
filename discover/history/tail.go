package history

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

// Checkpoint is an assembler's whole state: the session so far, the calls
// still awaiting a result or a turn's usage, and the options it was made
// with. RestoreAssembler continues from it exactly where Checkpoint was
// taken. It holds what the session holds (requests, arguments, results), so
// a caller that keeps it keeps it as privately as the session. Encode it
// with encoding/gob: JSON leaves out Call.Src and Session.SourceDigest.
type Checkpoint struct {
	Session    trace.Session
	Skipped    int
	ByKey      map[string][]int
	Turns      []CheckpointTurn
	SpreadFrom int
	SpreadTurn int
	Options    *ReadOptions
}

// CheckpointTurn is one named model turn: its usage, once seen, and the
// calls it made.
type CheckpointTurn struct {
	ID    string
	Usage trace.Usage
	Set   bool
	Calls []int
}

// Checkpoint copies the assembler's state; later events do not change it.
func (a *Assembler) Checkpoint() Checkpoint {
	c := Checkpoint{Session: copySession(a.S), Skipped: a.skipped, ByKey: map[string][]int{},
		SpreadFrom: a.spread.from, SpreadTurn: a.spread.turn}
	for k, v := range a.byKey {
		c.ByKey[k] = append([]int(nil), v...)
	}
	for _, id := range a.order {
		t := a.turns[id]
		c.Turns = append(c.Turns, CheckpointTurn{ID: id, Usage: t.usage, Set: t.set, Calls: append([]int(nil), t.calls...)})
	}
	if a.opts != nil {
		o := *a.opts
		c.Options = &o
	}
	return c
}

// RestoreAssembler is an assembler in the state c was taken in.
func RestoreAssembler(c Checkpoint) *Assembler {
	a := &Assembler{S: copySession(c.Session), skipped: c.Skipped, byKey: map[string][]int{}, turns: map[string]*namedTurn{}}
	for k, v := range c.ByKey {
		a.byKey[k] = append([]int(nil), v...)
	}
	for _, t := range c.Turns {
		a.turns[t.ID] = &namedTurn{usage: t.Usage, set: t.Set, calls: append([]int(nil), t.Calls...)}
		a.order = append(a.order, t.ID)
	}
	a.spread.from, a.spread.turn = c.SpreadFrom, c.SpreadTurn
	if c.Options != nil {
		o := *c.Options
		a.opts = &o
	}
	return a
}

// Snapshot is the session as Finish would return it now, without ending
// the assembly: more events may follow.
func (a *Assembler) Snapshot() trace.Session {
	return RestoreAssembler(a.Checkpoint()).Finish()
}

// copySession copies the parts of a session an assembler appends to or
// changes in place. Calls' maps are shared: nothing changes them once made.
func copySession(s trace.Session) trace.Session {
	s.Calls = append([]trace.Call(nil), s.Calls...)
	s.Requests = append([]string(nil), s.Requests...)
	s.RequestRoles = append([]string(nil), s.RequestRoles...)
	s.Approvals = append([]trace.Approval(nil), s.Approvals...)
	s.Assistant = append([]trace.AssistantMessage(nil), s.Assistant...)
	s.Meta = append([]trace.SessionMeta(nil), s.Meta...)
	s.FullRequests = append([]string(nil), s.FullRequests...)
	if s.Parent != nil {
		p := *s.Parent
		s.Parent = &p
	}
	return s
}

// TailState is where a tail read of one transcript stopped: the end of the
// last complete line read, and the state to continue from. A line still
// being written is left for the next read.
type TailState struct {
	// Offset is the byte just past the last complete line read.
	Offset int64
	// FirstLine is the length of the file's first line, with its newline.
	// A file whose first line differs, or that is shorter than Offset, was
	// replaced or cut, and is read again from its start. Nothing is
	// hashed: a digest of unredacted text would be a guessing oracle.
	FirstLine int64
	Assembler Checkpoint
	// Codex is the Codex decoder's state; nil for other clients.
	Codex *CodexState
}

// tailDecoder is one client's decoder for a tail read: its assembler, how
// it applies a line, the session so far, and what it adds to a TailState.
type tailDecoder struct {
	a        *Assembler
	line     func([]byte)
	snapshot func() trace.Session
	save     func(*TailState)
}

// TailClaudeFile reads a Claude Code transcript from where from stopped (the
// start when from is nil, or when the file was replaced since) and returns
// the session so far and where this read stopped. Tailing a file to its end
// gives the session a whole read with the same options gives, except
// SourceDigest, which a tail does not compute.
func TailClaudeFile(path string, from *TailState, o ReadOptions) (trace.Session, TailState, error) {
	dec := func(a *Assembler) tailDecoder {
		return tailDecoder{a: a, line: func(b []byte) { claudeLine(a, b, a.opts) }, snapshot: a.Snapshot}
	}
	return tailFile(path, from, func() tailDecoder { return dec(newClaudeAssembler(path, &o)) },
		func(st *TailState) (tailDecoder, bool) { return dec(RestoreAssembler(st.Assembler)), true })
}

// TailCodexFile is TailClaudeFile for a Codex rollout.
func TailCodexFile(path string, from *TailState, o ReadOptions) (trace.Session, TailState, error) {
	dec := func(d *codexDecoder) tailDecoder {
		return tailDecoder{a: d.a, line: d.line,
			snapshot: func() trace.Session {
				c := RestoreAssembler(d.a.Checkpoint())
				d.fix(&c.S)
				return c.Finish()
			},
			save: func(st *TailState) {
				cs := copyCodexState(d.st)
				st.Codex = &cs
			}}
	}
	return tailFile(path, from, func() tailDecoder { return dec(newCodexDecoder(path, &o)) },
		func(st *TailState) (tailDecoder, bool) {
			if st.Codex == nil {
				return tailDecoder{}, false
			}
			a := RestoreAssembler(st.Assembler)
			file := strings.TrimSuffix(filepath.Base(path), ".jsonl")
			return dec(&codexDecoder{a: a, o: a.opts, file: file, st: copyCodexState(*st.Codex)}), true
		})
}

func copyCodexState(s CodexState) CodexState {
	out := CodexState{MetaSeen: s.MetaSeen, Automation: s.Automation, Exec: map[string]CodexExecShape{}, MCPEnds: map[string]codexMCPResult{}}
	for k, v := range s.Exec {
		out.Exec[k] = v
	}
	for k, v := range s.MCPEnds {
		out.MCPEnds[k] = v
	}
	return out
}

// tailFile reads path's complete lines from from.Offset with the decoder
// resume gives, or from the start with a fresh one, and records where it
// stopped.
func tailFile(path string, from *TailState, fresh func() tailDecoder, resume func(*TailState) (tailDecoder, bool)) (trace.Session, TailState, error) {
	fh, err := os.Open(path)
	if err != nil {
		return trace.Session{}, TailState{}, err
	}
	defer fh.Close()
	first, err := firstLineLength(fh)
	if err != nil {
		return trace.Session{}, TailState{}, err
	}
	var d tailDecoder
	offset := int64(0)
	if from != nil && from.Offset > 0 && from.FirstLine == first {
		if info, err := fh.Stat(); err == nil && info.Size() >= from.Offset {
			var ok bool
			if d, ok = resume(from); ok {
				offset = from.Offset
			}
		}
	}
	if offset == 0 {
		d = fresh()
	}
	if _, err := fh.Seek(offset, io.SeekStart); err != nil {
		return trace.Session{}, TailState{}, err
	}
	r := bufio.NewReaderSize(fh, 1<<20)
	for {
		b, err := r.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			break // a line without its newline is still being written
		}
		if err != nil {
			return trace.Session{}, TailState{}, err
		}
		offset += int64(len(b))
		d.line(bytes.TrimSuffix(b, []byte("\n")))
	}
	st := TailState{Offset: offset, FirstLine: first, Assembler: d.a.Checkpoint()}
	if d.save != nil {
		d.save(&st)
	}
	return d.snapshot(), st, nil
}

// firstLineLength is the length of fh's first complete line with its
// newline, 0 when it has none yet. fh is left anywhere.
func firstLineLength(fh *os.File) (int64, error) {
	if _, err := fh.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	buf := make([]byte, 64<<10)
	var n int64
	for {
		k, err := fh.Read(buf)
		if i := bytes.IndexByte(buf[:k], '\n'); i >= 0 {
			return n + int64(i) + 1, nil
		}
		n += int64(k)
		if errors.Is(err, io.EOF) {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
	}
}
