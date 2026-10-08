package history

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"runtime"

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
	return readFilesEach(files, cache, extra, p, func(path string) ([]trace.Session, error) {
		s, err := ParseFile(path, parse)
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
	fh, err := os.Open(path)
	if err != nil {
		return trace.Session{}, err
	}
	defer fh.Close()
	h := sha256.New()
	s, err := parse(path, io.TeeReader(fh, h))
	if err != nil {
		return s, err
	}
	// The digest covers the whole file, including what the parser left
	// unread; like FileDigest, a failed read leaves it empty.
	if _, err := io.Copy(h, fh); err == nil {
		s.SourceDigest = hex.EncodeToString(h.Sum(nil))
	}
	return s, nil
}
