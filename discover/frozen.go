package discover

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"gitlab.com/telara-labs/tap-runtime/discover/trace"
)

// A frozen corpus lets two runs be compared on identical inputs. Client
// stores keep changing (a session in progress, a resumed one), so a run
// reads only the sessions a manifest lists and stops if any of them now
// reads differently. The manifest holds session IDs and digests, never the
// sessions themselves: the history stays in the clients' own files.

// ManifestEntry is one frozen session.
type ManifestEntry struct {
	Client string `json:"client"`
	ID     string `json:"id"`
	Digest string `json:"digest"`
	Calls  int    `json:"calls"`
}

// Manifest lists the sessions of a frozen corpus.
type Manifest struct {
	Cutoff   time.Time       `json:"cutoff"`
	Sessions []ManifestEntry `json:"sessions"`
	// Digest covers every entry, so two manifests can be compared at a glance.
	Digest string `json:"digest"`
}

// ErrCorpusChanged is returned when a frozen session is missing or differs.
var ErrCorpusChanged = errors.New("frozen corpus changed")

// SessionDigest identifies a session's input: the digest of what its reader
// read (SourceDigest) when there is one, so a parser change never reads as
// changed input; otherwise the sha256 of the session's JSON encoding.
func SessionDigest(s trace.Session) string {
	if s.SourceDigest != "" {
		return s.SourceDigest
	}
	b, _ := json.Marshal(s)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// lastActivity is the latest time a session records.
func lastActivity(s trace.Session) time.Time {
	t := s.Start
	for _, c := range s.Calls {
		if c.Time.After(t) {
			t = c.Time
		}
	}
	return t
}

// BuildManifest freezes the sessions whose last recorded activity is before
// cutoff; sessions still in progress are left out.
func BuildManifest(ss []trace.Session, cutoff time.Time) Manifest {
	m := Manifest{Cutoff: cutoff.UTC()}
	for _, s := range ss {
		if len(s.Calls) == 0 || !lastActivity(s).Before(cutoff) {
			continue
		}
		m.Sessions = append(m.Sessions, ManifestEntry{Client: s.Client, ID: s.ID, Digest: SessionDigest(s), Calls: len(s.Calls)})
	}
	sort.Slice(m.Sessions, func(i, j int) bool {
		if m.Sessions[i].Client != m.Sessions[j].Client {
			return m.Sessions[i].Client < m.Sessions[j].Client
		}
		return m.Sessions[i].ID < m.Sessions[j].ID
	})
	h := sha256.New()
	for _, e := range m.Sessions {
		fmt.Fprintf(h, "%s\x00%s\x00%s\n", e.Client, e.ID, e.Digest)
	}
	m.Digest = hex.EncodeToString(h.Sum(nil))
	return m
}

// FrozenReader returns only the sessions a manifest lists for its client,
// and fails if one is missing or reads differently than when frozen.
type FrozenReader struct {
	Inner    trace.Reader
	Manifest Manifest
	// DropChanged leaves out a session that changed or disappeared since
	// the freeze, instead of failing, and lists it in Dropped. A live
	// client store keeps appending to and deleting sessions; an evaluation
	// that uses this must report what was dropped.
	DropChanged bool
	Dropped     *[]string
}

func (f FrozenReader) Client() string { return f.Inner.Client() }

func (f FrozenReader) Read(since time.Time) ([]trace.Session, error) {
	want := map[string]string{}
	for _, e := range f.Manifest.Sessions {
		if e.Client == f.Inner.Client() {
			if _, dup := want[e.ID]; dup {
				return nil, fmt.Errorf("%w: %s: session id %s is listed twice; the reader must give each session a unique id", ErrCorpusChanged, e.Client, e.ID)
			}
			want[e.ID] = e.Digest
		}
	}
	ss, err := f.Inner.Read(time.Time{})
	if err != nil {
		return nil, err
	}
	var out []trace.Session
	var changed []string
	for _, s := range ss {
		d, ok := want[s.ID]
		if !ok {
			continue
		}
		delete(want, s.ID)
		if SessionDigest(s) != d {
			changed = append(changed, s.ID)
			continue
		}
		if !s.Start.Before(since) || since.IsZero() {
			out = append(out, s)
		}
	}
	for id := range want {
		changed = append(changed, id+" (missing)")
	}
	if len(changed) > 0 && f.DropChanged {
		sort.Strings(changed)
		if f.Dropped != nil {
			for _, id := range changed {
				*f.Dropped = append(*f.Dropped, f.Inner.Client()+"/"+id)
			}
		}
		return out, nil
	}
	if len(changed) > 0 {
		sort.Strings(changed)
		return nil, fmt.Errorf("%w: %s: %d session(s): %v", ErrCorpusChanged, f.Inner.Client(), len(changed), firstN(changed, 5))
	}
	return out, nil
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// fileDigest is the sha256 of a file's bytes, "" if it cannot be read.
func fileDigest(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}
