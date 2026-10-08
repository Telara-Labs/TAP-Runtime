package history

import (
	"encoding/gob"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

// The history cache keeps what a reader parsed, per unit: the smallest piece
// of an agent's history that can be read on its own (a session file, a
// conversation in a database, a thread). Each unit is stored with a
// fingerprint the reader can compute without reading the unit, so a repeat
// run reads only the units whose fingerprint changed.
//
// Three guards keep a cached read equal to a fresh one:
//   - a unit still being written gets no fingerprint and is never cached;
//   - every run reads a few cache hits again and compares them; a mismatch
//     drops the reader's cached units and is reported (TakeNotices);
//   - every entry is tied to the digest of the program that wrote it.

// cacheDir is where caches live; "" disables them. See UseCache.
var (
	cacheMu  sync.Mutex
	cacheDir string
)

// UseCache keeps each reader's parsed sessions under dir, so a later run
// reads only what changed. Parsed sessions hold the user's messages, so the
// directory and its files are private to the user.
func UseCache(dir string) {
	cacheMu.Lock()
	cacheDir = dir
	cacheMu.Unlock()
}

// cacheFormat changes when the cache file's own layout changes. A change to
// a reader needs no bump: entries are tied to the binary that wrote them.
const cacheFormat = 2

// spotChecks is how many cache hits each read checks against a fresh read.
var spotChecks = 2

// activeWindow is how recently a file may have changed and still be cached:
// a file written this recently may be written again within the same
// modification time.
var activeWindow = 2 * time.Second

// unitReadHook, when set, is told of every unit read from its source rather
// than served from the cache. Tests use it to see what a run read.
var unitReadHook func(cache, unit string)

type cacheFile struct {
	Format int
	Binary string
	Units  map[string]cacheUnit
	// Marks are per-store values a reader carries to its next run, such as
	// the highest row ID it saw.
	Marks map[string]int64
	// Dirs are the folders a home-folder search saw (aiderHistories).
	Dirs map[string]walkDir
}

// cacheUnit is one unit's parse, valid while its fingerprint is unchanged.
type cacheUnit struct {
	Fingerprint string
	Sessions    []trace.Session
}

type unitCache struct {
	name, path, binary string

	mu    sync.Mutex
	units map[string]cacheUnit
	marks map[string]int64
	dirs  map[string]walkDir
	dirty bool
}

// openUnitCache loads the named cache. A missing, unreadable or foreign
// cache starts empty. It is nil when caching is off, and a nil cache misses
// every lookup and saves nothing.
func openUnitCache(name string) *unitCache {
	cacheMu.Lock()
	dir := cacheDir
	cacheMu.Unlock()
	bin := binaryDigest()
	if name == "" || dir == "" || bin == "" {
		return nil
	}
	c := &unitCache{name: name, path: filepath.Join(dir, name+".gob"), binary: bin,
		units: map[string]cacheUnit{}, marks: map[string]int64{}, dirs: map[string]walkDir{}}
	f, err := os.Open(c.path)
	if err != nil {
		return c
	}
	defer f.Close()
	var cf cacheFile
	if gob.NewDecoder(f).Decode(&cf) == nil && cf.Format == cacheFormat && cf.Binary == bin {
		if cf.Units != nil {
			c.units = cf.Units
		}
		if cf.Marks != nil {
			c.marks = cf.Marks
		}
		if cf.Dirs != nil {
			c.dirs = cf.Dirs
		}
	}
	return c
}

func (c *unitCache) cacheName() string {
	if c == nil {
		return ""
	}
	return c.name
}

func (c *unitCache) get(key, fp string) ([]trace.Session, bool) {
	if c == nil || fp == "" {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	u, ok := c.units[key]
	if !ok || u.Fingerprint != fp {
		return nil, false
	}
	return u.Sessions, true
}

func (c *unitCache) put(key, fp string, ss []trace.Session) {
	if c == nil || fp == "" {
		return
	}
	c.mu.Lock()
	c.units[key] = cacheUnit{Fingerprint: fp, Sessions: ss}
	c.dirty = true
	c.mu.Unlock()
}

// prune drops the units drop reports.
func (c *unitCache) prune(drop func(key string) bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.units {
		if drop(k) {
			delete(c.units, k)
			c.dirty = true
		}
	}
}

func (c *unitCache) mark(store string) int64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.marks[store]
}

func (c *unitCache) setMark(store string, v int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.marks[store] != v {
		c.marks[store] = v
		c.dirty = true
	}
	c.mu.Unlock()
}

// save writes the cache back if it changed.
func (c *unitCache) save() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
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
	err = gob.NewEncoder(tmp).Encode(cacheFile{Format: cacheFormat, Binary: c.binary, Units: c.units, Marks: c.marks, Dirs: c.dirs})
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), c.path)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return
	}
	c.dirty = false
}

var (
	noticeMu sync.Mutex
	notices  []string
)

func notice(format string, args ...any) {
	noticeMu.Lock()
	notices = append(notices, fmt.Sprintf(format, args...))
	noticeMu.Unlock()
}

// TakeNotices returns, and forgets, what readers reported about their caches
// since the last call: a cache that disagreed with a fresh read and was
// rebuilt.
func TakeNotices() []string {
	noticeMu.Lock()
	defer noticeMu.Unlock()
	out := notices
	notices = nil
	return out
}

// sample picks up to n of xs at random, so every cached unit is checked
// now and then.
func sample[T any](xs []T, n int) []T {
	if len(xs) <= n {
		return append([]T(nil), xs...)
	}
	out := make([]T, 0, n)
	for _, i := range rand.Perm(len(xs))[:n] {
		out = append(out, xs[i])
	}
	return out
}

func reportRead(cache, unit string) {
	if h := unitReadHook; h != nil {
		h(cache, unit)
	}
}

// unitResult is one unit's sessions, or why it could not be read. A unit
// that could not be read is never cached.
type unitResult struct {
	Sessions []trace.Session
	Err      error
}

// --- Session files ---------------------------------------------------------

// fileFingerprint is a file's size and modification time, with extra (what
// else its parse depends on). It is "" for a file changed within
// activeWindow, which is read but not cached.
func fileFingerprint(path, extra string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if time.Since(info.ModTime()) < activeWindow {
		return "", nil
	}
	return fmt.Sprintf("%d:%d:%s", info.Size(), info.ModTime().UnixNano(), extra), nil
}

// statFingerprint is a file's size and modification time, or "absent". It
// names a file a parse also reads, such as a database's write-ahead log.
func statFingerprint(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "absent"
	}
	return fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
}

// readFiles reads each file with read, at most cap(parseSlots) at once across
// the process, and returns the results in file order. With a cache name, a
// file whose fingerprint (size, modification time and extra(path)) is
// unchanged is served from the cache, and a few such files are read again
// to check it. Only a reader whose result depends on nothing but the file
// and extra may use a cache.
func readFiles(files []string, cache string, extra func(string) string, p trace.Progress, read func(string) ([]trace.Session, error)) []unitResult {
	out := make([]unitResult, len(files))
	c := openUnitCache(cache)
	fps := make([]string, len(files))
	xs := make([]string, len(files))
	var misses, hits []int
	cached := map[int][]trace.Session{}
	for i, f := range files {
		if c != nil {
			if extra != nil {
				xs[i] = extra(f)
			}
			fps[i], _ = fileFingerprint(f, xs[i])
		}
		if ss, ok := c.get(f, fps[i]); ok {
			hits = append(hits, i)
			cached[i] = ss
			continue
		}
		misses = append(misses, i)
	}
	var mu sync.Mutex
	done := 0
	report := func(n int) {
		if p == nil {
			return
		}
		mu.Lock()
		done += n
		p(done, len(files))
		mu.Unlock()
	}
	if p != nil {
		p(0, len(files))
	}
	readOne := func(i int) {
		reportRead(cache, files[i])
		ss, err := read(files[i])
		out[i] = unitResult{ss, err}
		if err != nil {
			return
		}
		// A file that changed while it was read is read again next time.
		if fp, _ := fileFingerprint(files[i], xs[i]); fp == fps[i] {
			c.put(files[i], fps[i], ss)
		}
	}
	// Check a few hits against a fresh read along with the misses.
	checks := sample(hits, spotChecks)
	parallelEach(append(append([]int(nil), misses...), checks...), func(i int) {
		readOne(i)
		report(1)
	})
	for _, i := range checks {
		if out[i].Err != nil || !reflect.DeepEqual(out[i].Sessions, cached[i]) {
			notice("%s: cached history did not match a fresh read; read it all again", cache)
			c.prune(func(string) bool { return true })
			var rest []int
			for _, j := range hits {
				if !containsInt(checks, j) {
					rest = append(rest, j)
				}
			}
			parallelEach(rest, func(j int) {
				readOne(j)
				report(1)
			})
			hits = nil
			break
		}
	}
	for _, i := range hits {
		if !containsInt(checks, i) {
			out[i] = unitResult{Sessions: cached[i]}
		}
	}
	report(len(hits) - countIn(hits, checks))
	// Files that are gone leave the cache; files outside this run's window
	// stay for a wider one.
	c.prune(func(k string) bool {
		_, err := os.Stat(k)
		return errors.Is(err, os.ErrNotExist)
	})
	c.save()
	return out
}

func containsInt(is []int, v int) bool {
	for _, i := range is {
		if i == v {
			return true
		}
	}
	return false
}

func countIn(is, of []int) int {
	n := 0
	for _, i := range is {
		if containsInt(of, i) {
			n++
		}
	}
	return n
}

// parallelEach runs f for each index, at most cap(parseSlots) at once across
// the process.
func parallelEach(idx []int, f func(int)) {
	next := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < min(cap(parseSlots), len(idx)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				parseSlots <- struct{}{}
				f(i)
				<-parseSlots
			}
		}()
	}
	for _, i := range idx {
		next <- i
	}
	close(next)
	wg.Wait()
}

// --- Databases -------------------------------------------------------------

// storeRead serves one database's units: those whose fingerprint is
// unchanged from the cache, the rest from Read.
type storeRead struct {
	Cache *unitCache
	// Scope is the store's path; it keys the store's units within the cache.
	Scope string
	// Units is every unit in the store now, with its fingerprint (from a
	// query that reads no unit's contents). Nil means the store could not be
	// listed: everything is read and nothing cached.
	Units map[string]string
	// Force names units to read even when their fingerprint matches.
	Force map[string]bool
	// Read reads the named units, or every unit when ids is nil. A unit with
	// no sessions may be left out of the result.
	Read func(ids []string) (map[string]unitResult, error)
}

func (s storeRead) key(unit string) string { return s.Scope + "\x00" + unit }

func (s storeRead) run() (map[string]unitResult, error) {
	name := s.Cache.cacheName()
	if s.Cache == nil || s.Units == nil {
		res, err := s.Read(nil)
		for u := range res {
			reportRead(name, u)
		}
		return res, err
	}
	ids := make([]string, 0, len(s.Units))
	for u := range s.Units {
		ids = append(ids, u)
	}
	sort.Strings(ids)
	cached := map[string][]trace.Session{}
	var want, hits []string
	for _, u := range ids {
		if ss, ok := s.Cache.get(s.key(u), s.Units[u]); ok && !s.Force[u] {
			cached[u] = ss
			hits = append(hits, u)
			continue
		}
		want = append(want, u)
	}
	checks := sample(hits, spotChecks)
	want = append(want, checks...)
	sort.Strings(want)
	fresh := map[string]unitResult{}
	full := false
	if len(want) > 0 {
		// A large change set is cheaper read in one pass over the store.
		var err error
		if full = len(want)*2 > len(s.Units); full {
			fresh, err = s.Read(nil)
		} else {
			fresh, err = s.Read(want)
		}
		if err != nil {
			return nil, err
		}
	}
	for _, u := range checks {
		if r := fresh[u]; r.Err != nil || !reflect.DeepEqual(r.Sessions, cached[u]) {
			notice("%s: cached history did not match a fresh read; read it all again", name)
			s.Cache.prune(func(k string) bool { return strings.HasPrefix(k, s.Scope+"\x00") })
			if !full {
				res, err := s.Read(nil)
				if err != nil {
					return nil, err
				}
				fresh, full = res, true
			}
			break
		}
	}
	wanted := map[string]bool{}
	for _, u := range want {
		wanted[u] = true
	}
	out := map[string]unitResult{}
	for _, u := range ids {
		if r, ok := fresh[u]; ok || full || wanted[u] {
			reportRead(name, u)
			out[u] = r
			if r.Err == nil {
				s.Cache.put(s.key(u), s.Units[u], r.Sessions)
			}
			continue
		}
		out[u] = unitResult{Sessions: cached[u]}
	}
	// A unit created after the listing is used, not cached.
	for u, r := range fresh {
		if _, ok := s.Units[u]; !ok {
			out[u] = r
		}
	}
	s.Cache.prune(func(k string) bool {
		scope, unit, ok := strings.Cut(k, "\x00")
		if !ok || scope != s.Scope {
			return false
		}
		_, present := s.Units[unit]
		return !present
	})
	return out, nil
}

// keepStores drops cached units of stores not in keep (a project removed
// from Crush's list, for one).
func (c *unitCache) keepStores(keep map[string]bool) {
	c.prune(func(k string) bool {
		scope, _, ok := strings.Cut(k, "\x00")
		return ok && !keep[scope]
	})
}

// sqlList quotes values for an SQL IN list or VALUES rows.
func sqlList(vals []string, row bool) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		q := "'" + strings.ReplaceAll(v, "'", "''") + "'"
		if row {
			q = "(" + q + ")"
		}
		parts[i] = q
	}
	return strings.Join(parts, ", ")
}

var (
	binaryOnce sync.Once
	binaryHash string
)

// binaryDigest identifies the running program, so a reader change in any
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

// dir is what the last search saw in a folder.
func (c *unitCache) dir(path string) (walkDir, bool) {
	if c == nil {
		return walkDir{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.dirs[path]
	return d, ok
}

func (c *unitCache) setDir(path string, d walkDir) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.dirs[path] = d
	c.dirty = true
	c.mu.Unlock()
}

// keepDirs forgets the folders the last search did not reach.
func (c *unitCache) keepDirs(seen map[string]bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for p := range c.dirs {
		if !seen[p] {
			delete(c.dirs, p)
			c.dirty = true
		}
	}
}

// storeFingerprints runs a listing query that returns each unit's id and fp,
// reading no unit's contents. It is nil when caching is off or the listing
// fails (a store of another version); the store is then read whole.
func storeFingerprints(c *unitCache, bin, db, sql string) map[string]string {
	if c == nil {
		return nil
	}
	rows, err := sqliteRows(bin, db, sql)
	if err != nil {
		return nil
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[rowString(r, "id")] = rowString(r, "fp")
	}
	return out
}
