package history

import (
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"

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
// reader's parse cache (see UseCache); "" parses every file. Only a parser
// whose result depends on nothing but the file's bytes may be cached.
func ParseFiles(files []string, cache string, p trace.Progress, parse FileParser) []Parsed {
	out := make([]Parsed, len(files))
	c := openCache(cache)
	var mu sync.Mutex
	done := 0
	report := func() {
		if p == nil {
			return
		}
		mu.Lock()
		done++
		p(done, len(files))
		mu.Unlock()
	}
	if p != nil {
		p(0, len(files))
	}
	next := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < min(cap(parseSlots), len(files)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				parseSlots <- struct{}{}
				out[i] = c.parse(files[i], parse)
				<-parseSlots
				report()
			}
		}()
	}
	for i := range files {
		next <- i
	}
	close(next)
	wg.Wait()
	c.save()
	return out
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

// cacheDir is where parse caches live; "" disables them. See UseCache.
var (
	cacheMu  sync.Mutex
	cacheDir string
)

// UseCache keeps each reader's parsed sessions under dir, so a later run
// parses only the files that changed. Parsed sessions hold the user's
// messages, so the directory and its files are private to the user.
func UseCache(dir string) {
	cacheMu.Lock()
	cacheDir = dir
	cacheMu.Unlock()
}

// cacheFormat changes when the cache file's own layout changes. A change to
// a parser needs no bump: entries are tied to the binary that wrote them.
const cacheFormat = 1

type cacheFile struct {
	Format  int
	Binary  string
	Entries map[string]cacheEntry
}

// cacheEntry is one file's parse, valid while the file keeps this size and
// modification time.
type cacheEntry struct {
	Size    int64
	ModTime int64
	Session trace.Session
}

type parseCache struct {
	path    string
	binary  string
	mu      sync.Mutex
	entries map[string]cacheEntry
	dirty   bool
}

// openCache loads the named cache; a missing, unreadable or foreign cache
// starts empty. A nil cache parses every file and saves nothing.
func openCache(name string) *parseCache {
	cacheMu.Lock()
	dir := cacheDir
	cacheMu.Unlock()
	bin := binaryDigest()
	if name == "" || dir == "" || bin == "" {
		return nil
	}
	c := &parseCache{path: filepath.Join(dir, name+".gob"), binary: bin, entries: map[string]cacheEntry{}}
	f, err := os.Open(c.path)
	if err != nil {
		return c
	}
	defer f.Close()
	var cf cacheFile
	if gob.NewDecoder(f).Decode(&cf) == nil && cf.Format == cacheFormat && cf.Binary == bin {
		c.entries = cf.Entries
	}
	return c
}

func (c *parseCache) parse(path string, parse FileParser) Parsed {
	if c == nil {
		s, err := ParseFile(path, parse)
		return Parsed{s, err}
	}
	before, err := os.Stat(path)
	if err != nil {
		return Parsed{Err: err}
	}
	c.mu.Lock()
	e, ok := c.entries[path]
	c.mu.Unlock()
	if ok && e.Size == before.Size() && e.ModTime == before.ModTime().UnixNano() {
		return Parsed{Session: e.Session}
	}
	s, err := ParseFile(path, parse)
	if err != nil {
		return Parsed{s, err}
	}
	// A file still being written is parsed again next time.
	if after, err := os.Stat(path); err == nil && after.Size() == before.Size() && after.ModTime().Equal(before.ModTime()) {
		c.mu.Lock()
		c.entries[path] = cacheEntry{Size: before.Size(), ModTime: before.ModTime().UnixNano(), Session: s}
		c.dirty = true
		c.mu.Unlock()
	}
	return Parsed{Session: s}
}

// save writes the cache back, dropping entries whose file is gone. Entries
// for files outside this run's window are kept for a wider one.
func (c *parseCache) save() {
	if c == nil {
		return
	}
	for p := range c.entries {
		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			delete(c.entries, p)
			c.dirty = true
		}
	}
	if !c.dirty {
		return
	}
	if os.MkdirAll(filepath.Dir(c.path), 0o700) != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.path), ".parse-*")
	if err != nil {
		return
	}
	err = gob.NewEncoder(tmp).Encode(cacheFile{Format: cacheFormat, Binary: c.binary, Entries: c.entries})
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), c.path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
}

var (
	binaryOnce sync.Once
	binaryHash string
)

// binaryDigest identifies the running program, so a parser change in any
// build, released or not, invalidates what an older build cached.
func binaryDigest() string {
	binaryOnce.Do(func() {
		exe, err := os.Executable()
		if err != nil {
			return
		}
		binaryHash = FileDigest(exe)
	})
	return binaryHash
}
