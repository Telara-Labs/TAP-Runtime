package history

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"bytes"
	"github.com/Telara-Labs/TAP-Runtime/discover/redact"
	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

// FileParser parses one session file. It reads the file from r, which is
// already open; path names the file for the session ID and errors.
type FileParser func(path string, r io.Reader) (trace.Session, error)

// Parsed is one file's result: the session, or why it could not be read.
type Parsed struct {
	Session trace.Session
	Err     error
}

// parseSlots bounds how many files are parsed at once across every reader in
// the process, so reading all agents together leaves a CPU for the host.
var parseSlots = make(chan struct{}, max(1, runtime.NumCPU()-1))

// ParseFiles parses files concurrently and returns their results in the
// order given, so readers keep their ordering rules. Each file is read once:
// the parse and its SourceDigest come from the same bytes. cache names the
// reader's cache (see UseCache); "" parses every file. Only a parser whose
// result depends on nothing but the file's bytes may be cached.
func ParseFiles(files []string, cache string, p trace.Progress, parse FileParser) []Parsed {
	return ParseFilesWith(files, cache, nil, p, parse)
}

// ParseFilesWith is ParseFiles for a parser that also depends on something
// beside the file: extra(path) names it (a size and time, a configuration),
// and a cached parse is used only while extra is unchanged too.
func ParseFilesWith(files []string, cache string, extra func(string) string, p trace.Progress, parse FileParser) []Parsed {
	out := make([]Parsed, len(files))
	ParseFilesEach(files, cache, extra, p, parse, func(i int, r Parsed) error {
		out[i] = r
		return nil
	})
	return out
}

// ParseFilesEach is ParseFilesWith passing each file's result to emit, in
// file order, a chunk of files at a time, so a long history is never held
// whole. An error from emit stops the read and is returned.
func ParseFilesEach(files []string, cache string, extra func(string) string, p trace.Progress, parse FileParser, emit func(int, Parsed) error) error {
	return parseFilesEach(files, cache, extra, p, parse, emit, true)
}

func parseFilesEach(files []string, cache string, extra func(string) string, p trace.Progress, parse FileParser, emit func(int, Parsed) error, digest bool) error {
	return readFilesEach(files, cache, extra, p, func(path string) ([]trace.Session, error) {
		s, err := parseFile(path, parse, digest)
		if err != nil {
			return nil, err
		}
		return []trace.Session{s}, nil
	}, func(i int, r unitResult) error {
		var out Parsed
		out.Err = r.Err
		if len(r.Sessions) == 1 {
			out.Session = r.Sessions[0]
		}
		return emit(i, out)
	})
}

// ParseFile parses one file, setting SourceDigest from the bytes parsed.
func ParseFile(path string, parse FileParser) (trace.Session, error) {
	return parseFile(path, parse, true)
}

func parseFile(path string, parse FileParser, digest bool) (trace.Session, error) {
	fh, err := os.Open(path)
	if err != nil {
		return trace.Session{}, err
	}
	defer fh.Close()
	if !digest {
		return parse(path, fh)
	}
	h := sha256.New()
	audit := &sourceAudit{disabled: filepath.Ext(path) == ".db" || filepath.Ext(path) == ".vscdb"}
	s, err := parse(path, io.TeeReader(fh, io.MultiWriter(h, audit)))
	if err != nil {
		return s, err
	}
	// The digest covers the whole file, including what the parser left
	// unread; like FileDigest, a failed read leaves it empty.
	if _, err := io.Copy(io.MultiWriter(h, audit), fh); err == nil {
		s.SourceDigest = hex.EncodeToString(h.Sum(nil))
	}
	audit.check()
	s.CacheUnsafe = audit.unsafe
	return s, nil
}

// sourceAudit inspects complete source lines before parser truncation. An
// oversized line is conservatively non-cacheable and never buffered whole.
type sourceAudit struct {
	pending  []byte
	unsafe   bool
	disabled bool
}

func (a *sourceAudit) Write(b []byte) (int, error) {
	n := len(b)
	for len(b) > 0 && !a.unsafe && !a.disabled {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			i = len(b) - 1
		}
		part := b[:i+1]
		if len(a.pending)+len(part) > 128<<20 {
			a.unsafe = true
			break
		}
		a.pending = append(a.pending, part...)
		b = b[i+1:]
		if part[len(part)-1] == '\n' {
			a.check()
		}
	}
	return n, nil
}

func (a *sourceAudit) check() {
	if len(a.pending) > 0 {
		s := string(a.pending)
		a.unsafe = a.unsafe || redact.Redact(s) != s
		a.pending = nil
	}
}
