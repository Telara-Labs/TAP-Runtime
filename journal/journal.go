// Package journal is the durable record of one run, kept so that a run which
// stops can continue without doing anything twice.
//
// A run is resumed by starting the program again from the top and answering
// every request it already made from this record. So the record is keyed by
// the request's own id, which the program assigns, and never by the order
// requests arrived in: with several requests in flight, arrival order is not
// the program's order (doc 34 sections 11.7 and 11.9).
//
// It is tiered. The index holds one line for the start and one for the end of
// each request. A result too large for a line is stored once, by its digest,
// beside the index. Nothing is ever rewritten or compacted.
//
// PROPOSED, not ruled: the design this implements belongs to TENG-3037. The
// journal in telara-agents/tap-runtime/journal carries leases, fencing tokens
// and idempotency keys built for manifest v2's policy store. This package
// does not replace those; it is what the v3 runner needs to resume. The two
// are reconciled when that code moves here (ruling 10).
package journal

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// InlineLimit is the largest result kept in the index. Anything larger goes
// to a blob.
const InlineLimit = 8 << 10

// Header identifies the run. A run is resumed only against the package it
// was started with.
type Header struct {
	RunID         string    `json:"run_id"`
	Package       string    `json:"package"`
	PackageDigest string    `json:"package_digest"`
	Args          []string  `json:"args"`
	Started       time.Time `json:"started"`
}

// Record is one line of the index.
type Record struct {
	Phase   string          `json:"phase"` // header, begin, end, finish
	ID      string          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Digest  string          `json:"digest,omitempty"` // of the request, so a changed request is noticed
	Effect  string          `json:"effect,omitempty"`
	Reply   json.RawMessage `json:"reply,omitempty"`
	Blob    string          `json:"blob,omitempty"`
	Bytes   int             `json:"bytes,omitempty"`
	At      time.Time       `json:"at"`
	Header  *Header         `json:"header,omitempty"`
	Outcome string          `json:"outcome,omitempty"`
}

// State is what is known about one request when a run is resumed.
type State int

const (
	Unseen      State = iota // never asked
	Finished                 // asked and answered: reply from the record
	Interrupted              // asked, and the run stopped before an answer was recorded
)

var ErrChanged = errors.New("the program made a different request under an id it used before")

type Journal struct {
	mu     sync.Mutex
	dir    string
	f      *os.File
	Header Header
	begun  map[string]Record
	ended  map[string]Record
	closed bool
}

var runIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{5,80}$`)

// NewRunID is sortable by start time and unique on one machine.
func NewRunID(now time.Time) string {
	var b [4]byte
	rand.Read(b[:])
	return now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

// Create starts the record of a new run.
func Create(root string, h Header) (*Journal, error) {
	if !runIDRe.MatchString(h.RunID) {
		return nil, fmt.Errorf("run id %q is not usable", h.RunID)
	}
	dir := filepath.Join(root, h.RunID)
	if _, err := os.Stat(dir); err == nil {
		return nil, fmt.Errorf("run %s already exists", h.RunID)
	}
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "index.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	j := &Journal{dir: dir, f: f, Header: h, begun: map[string]Record{}, ended: map[string]Record{}}
	if err := j.append(Record{Phase: "header", Header: &h, At: h.Started}); err != nil {
		f.Close()
		return nil, err
	}
	return j, nil
}

// Open reads the record of an earlier run so that it can continue. A last
// line cut short by a crash is ignored: it recorded nothing that finished.
func Open(root, runID string) (*Journal, error) {
	if !runIDRe.MatchString(runID) {
		return nil, fmt.Errorf("run id %q is not usable", runID)
	}
	dir := filepath.Join(root, runID)
	path := filepath.Join(dir, "index.jsonl")
	raw, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("no run %s: %w", runID, err)
	}
	j := &Journal{dir: dir, begun: map[string]Record{}, ended: map[string]Record{}}
	sc := bufio.NewScanner(raw)
	sc.Buffer(make([]byte, 0, 64<<10), 64<<20)
	sawHeader, finished := false, false
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		switch r.Phase {
		case "header":
			if r.Header != nil {
				j.Header, sawHeader = *r.Header, true
			}
		case "begin":
			j.begun[r.ID] = r
		case "end":
			j.ended[r.ID] = r
		case "finish":
			finished = true
		}
	}
	raw.Close()
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !sawHeader {
		return nil, fmt.Errorf("run %s has no header", runID)
	}
	if finished {
		return nil, fmt.Errorf("run %s finished; there is nothing to resume", runID)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	j.f = f
	return j, nil
}

func (j *Journal) append(r Record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err := j.f.Write(append(b, '\n')); err != nil {
		return err
	}
	// The record must be on disk before the effect it describes happens, or
	// a crash between the two loses the fact that the effect was started.
	return j.f.Sync()
}

// Digest identifies the content of a request, whatever order its fields are
// written in.
func Digest(v any) string {
	b, _ := json.Marshal(v)
	var generic any
	json.Unmarshal(b, &generic)
	b, _ = json.Marshal(generic) // maps are written with sorted keys
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Lookup reports what is known of a request. It returns ErrChanged when the
// id is known and the request is not the one recorded: the program is not
// following the path it followed before, and replaying its record would
// hand it answers to questions it did not ask.
func (j *Journal) Lookup(id, digest string) (State, []byte, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	b, begun := j.begun[id]
	if !begun {
		return Unseen, nil, nil
	}
	if b.Digest != digest {
		return Unseen, nil, fmt.Errorf("%w: %s", ErrChanged, id)
	}
	e, ended := j.ended[id]
	if !ended {
		return Interrupted, nil, nil
	}
	if e.Blob == "" {
		return Finished, e.Reply, nil
	}
	reply, err := os.ReadFile(filepath.Join(j.dir, "blobs", e.Blob))
	if err != nil {
		return Finished, nil, fmt.Errorf("the recorded result of %s is missing: %w", id, err)
	}
	if got := sha256.Sum256(reply); hex.EncodeToString(got[:]) != e.Blob {
		return Finished, nil, fmt.Errorf("the recorded result of %s has been altered", id)
	}
	return Finished, reply, nil
}

// Effect is the effect recorded when the request was begun.
func (j *Journal) Effect(id string) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.begun[id].Effect
}

// Begin records that a request is about to be acted on.
func (j *Journal) Begin(id, method, digest, effect string, at time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	r := Record{Phase: "begin", ID: id, Method: method, Digest: digest, Effect: effect, At: at}
	if err := j.append(r); err != nil {
		return err
	}
	j.begun[id] = r
	return nil
}

// End records the answer to a request.
func (j *Journal) End(id, outcome string, reply []byte, at time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	r := Record{Phase: "end", ID: id, Outcome: outcome, Bytes: len(reply), At: at}
	if len(reply) <= InlineLimit {
		r.Reply = reply
	} else {
		sum := sha256.Sum256(reply)
		r.Blob = hex.EncodeToString(sum[:])
		path := filepath.Join(j.dir, "blobs", r.Blob)
		if _, err := os.Stat(path); err != nil {
			if err := writeDurable(path, reply); err != nil {
				return err
			}
		}
	}
	if err := j.append(r); err != nil {
		return err
	}
	j.ended[id] = r
	return nil
}

// Finish records that the run ended. A finished run cannot be resumed.
func (j *Journal) Finish(outcome string, at time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.append(Record{Phase: "finish", Outcome: outcome, At: at})
}

func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	return j.f.Close()
}

// Counts reports how many requests were answered and how many were left
// unanswered.
func (j *Journal) Counts() (finished, interrupted int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for id := range j.begun {
		if _, ok := j.ended[id]; ok {
			finished++
		} else {
			interrupted++
		}
	}
	return
}

func writeDurable(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
