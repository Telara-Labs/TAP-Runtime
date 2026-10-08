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

// cacheFormat changes when the cache's own layout changes. A change to a
// reader needs no bump: entries are tied to the binary that wrote them.
//
// Format 3 keeps each unit in a file of its own beside a small index, so a
// read loads only the units it reaches. Format 2 kept a reader's whole
// history in one file that every read decoded whole (409 MB for one Codex
// history).
const cacheFormat = 3

// spotChecks is how many cache hits each read checks against a fresh read.
var spotChecks = 2

// activeWindow is how recently a file may have changed and still be cached:
// a file written this recently may be written again within the same
// modification time.
var activeWindow = 2 * time.Second

// unitReadHook, when set, is told of every unit read from its source rather
// than served from the cache. Tests use it to see what a run read.
var unitReadHook func(cache, unit string)

// cacheFile is a reader's index: each unit's fingerprint, not its sessions.
type cacheFile struct {
	Format int
	Binary string
	Units  map[string]cacheEntry
	// Marks are per-store values a reader carries to its next run, such as
	// the highest row ID it saw.
	Marks map[string]int64
	// Dirs are the folders a home-folder search saw (aiderHistories).
	Dirs map[string]walkDir
}

// cacheEntry is one unit in the index, valid while its fingerprint is
// unchanged. Its sessions are in the unit's own file (unitFile).
type cacheEntry struct {
	Fingerprint string
}

type unitCache struct {
	name, path, binary string
	unitDir            string // the units' files

	mu    sync.Mutex
	units map[string]cacheEntry
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
	c := &unitCache{name: name, path: filepath.Join(dir, name+".index.gob"), unitDir: filepath.Join(dir, name), binary: bin,
		units: map[string]cacheEntry{}, marks: map[string]int64{}, dirs: map[string]walkDir{}}
	// A format-2 cache held the whole history in one file; it is not read.
	os.Remove(filepath.Join(dir, name+".gob"))
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

// unitFile is where key's sessions are kept: named by the key's digest, so
// a unit read again replaces its own file.
func (c *unitCache) unitFile(key string) string {
	return filepath.Join(c.unitDir, hexSum(key)[:32]+".gob")
}

// unitData is a unit file's content. Key guards against two keys whose
// digests collide in the file name.
type unitData struct {
	Key      string
	Sessions []trace.Session
}

// get loads key's sessions when they are cached under fp.
func (c *unitCache) get(key, fp string) ([]trace.Session, bool) {
	if !c.has(key, fp) {
		return nil, false
	}
	f, err := os.Open(c.unitFile(key))
	if err != nil {
		return nil, false
	}
	defer f.Close()
	var u unitData
	if gob.NewDecoder(f).Decode(&u) != nil || u.Key != key {
		return nil, false
	}
	return u.Sessions, true
}

// has reports whether key is cached under fp, without loading it.
func (c *unitCache) has(key, fp string) bool {
	if c == nil || fp == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	u, ok := c.units[key]
	return ok && u.Fingerprint == fp
}

// put writes key's sessions to the unit's own file and indexes them under
// fp. A unit whose file cannot be written is left out of the index.
func (c *unitCache) put(key, fp string, ss []trace.Session) {
	if c == nil || fp == "" {
		return
	}
	if err := writePrivate(c.unitDir, c.unitFile(key), unitData{Key: key, Sessions: ss}); err != nil {
		return
	}
	c.mu.Lock()
	c.units[key] = cacheEntry{Fingerprint: fp}
	c.dirty = true
	c.mu.Unlock()
}

// writePrivate writes v as gob to path atomically, in dir, private to the
// user.
func writePrivate(dir, path string, v any) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".unit-*")
	if err != nil {
		return err
	}
	err = gob.NewEncoder(tmp).Encode(v)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
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
			os.Remove(c.unitFile(k))
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
	if writePrivate(filepath.Dir(c.path), c.path, cacheFile{Format: cacheFormat, Binary: c.binary, Units: c.units, Marks: c.marks, Dirs: c.dirs}) != nil {
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
	readFilesEach(files, cache, extra, p, read, func(i int, r unitResult) error {
		out[i] = r
		return nil
	})
	return out
}

// readAheadBytes bounds how far readFilesEach reads ahead of the file it
// passes on next, in bytes of the files read or being read. Results go out
// in file order, so one large file at the head holds the rest back; the
// window has to be wide enough that the parse slots stay busy meanwhile. A
// parse keeps a small part of its file (Codex: 15 GB of files parse to
// about 270 MB), so this much file is far less in memory.
var readAheadBytes int64 = 2 << 30

// readFilesEach is readFiles passing each file's result to emit in file
// order, a chunk of files at a time: cached results are loaded only when
// their chunk is reached, so a reader's history is never held whole. The
// spot checks of cache hits run before anything is passed on.
func readFilesEach(files []string, cache string, extra func(string) string, p trace.Progress, read func(string) ([]trace.Session, error), emit func(int, unitResult) error) error {
	c := openUnitCache(cache)
	fps := make([]string, len(files))
	xs := make([]string, len(files))
	hit := make([]bool, len(files))
	var hits []int
	for i, f := range files {
		if c != nil {
			if extra != nil {
				xs[i] = extra(f)
			}
			fps[i], _ = fileFingerprint(f, xs[i])
			if c.has(f, fps[i]) {
				hit[i] = true
				hits = append(hits, i)
			}
		}
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
	readOne := func(i int) unitResult {
		reportRead(cache, files[i])
		ss, err := read(files[i])
		if err == nil {
			// A file that changed while it was read is read again next time.
			if fp, _ := fileFingerprint(files[i], xs[i]); fp == fps[i] {
				c.put(files[i], fps[i], ss)
			}
		}
		return unitResult{ss, err}
	}
	// Check a few hits against a fresh read before any is used.
	checked := map[int]unitResult{}
	checks := sample(hits, spotChecks)
	for _, i := range checks {
		cached, _ := c.get(files[i], fps[i])
		r := readOne(i)
		checked[i] = r
		if r.Err != nil || !reflect.DeepEqual(r.Sessions, cached) {
			notice("%s: cached history did not match a fresh read; read it all again", cache)
			c.prune(func(string) bool { return true })
			for j := range hit {
				hit[j] = false
			}
			break
		}
	}
	// Files are read ahead of the one passed on next, up to readAheadBytes of
	// files started and not yet passed on, and passed on in file order as
	// soon as each is ready. The largest start first: a parse takes time in
	// proportion to its file, so the longest set the pace, and started last
	// they would leave the other slots idle at the end. The file due next is
	// always started, whatever the budget, so the read never waits on itself.
	results := make([]chan unitResult, len(files))
	cost := make([]int64, len(files))
	order := make([]int, len(files))
	for i, f := range files {
		results[i] = make(chan unitResult, 1)
		order[i] = i
		var size int64
		if info, err := os.Stat(f); err == nil {
			size = info.Size()
		}
		cost[i] = min(max(size, 1), readAheadBytes)
	}
	sort.SliceStable(order, func(a, b int) bool { return cost[order[a]] > cost[order[b]] })
	var smu sync.Mutex
	cond := sync.NewCond(&smu)
	started := make([]bool, len(files))
	avail, head, stopped := readAheadBytes, 0, false
	defer func() {
		smu.Lock()
		stopped = true
		smu.Unlock()
		cond.Broadcast()
	}()
	work := func(i int) {
		if r, ok := checked[i]; ok {
			results[i] <- r
			return
		}
		if hit[i] {
			if ss, ok := c.get(files[i], fps[i]); ok {
				results[i] <- unitResult{Sessions: ss}
				return
			}
		}
		parseSlots <- struct{}{}
		r := readOne(i)
		<-parseSlots
		results[i] <- r
	}
	go func() {
		next := 0 // position in order
		for {
			smu.Lock()
			pick := -1
			for pick < 0 {
				if stopped {
					smu.Unlock()
					return
				}
				for next < len(order) && started[order[next]] {
					next++
				}
				switch {
				case head < len(files) && !started[head]:
					pick = head
				case next >= len(order):
					smu.Unlock()
					return
				case avail >= cost[order[next]]:
					pick = order[next]
				default:
					cond.Wait()
				}
			}
			started[pick] = true
			avail -= cost[pick]
			smu.Unlock()
			go work(pick)
		}
	}()
	for i := range files {
		r := <-results[i]
		smu.Lock()
		head, avail = i+1, avail+cost[i]
		smu.Unlock()
		cond.Broadcast()
		report(1)
		if err := emit(i, r); err != nil {
			return err
		}
	}
	// Files that are gone leave the cache; files outside this run's window
	// stay for a wider one.
	c.prune(func(k string) bool {
		_, err := os.Stat(k)
		return errors.Is(err, os.ErrNotExist)
	})
	c.save()
	return nil
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
	// Batch, when set, splits the units to read into groups that are read
	// one at a time, each group's results passed on before the next is
	// read (each), so a large store is never held whole.
	Batch func(ids []string) [][]string
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

// storeBatchUnits is how many units (sessions, threads) one read of a store
// takes when its reader sets no Batch of its own.
var storeBatchUnits = 200

// each passes every unit's result to emit as it is read, a group of units
// (Batch, or storeBatchUnits) at a time, never holding more than a group at
// once. A store that could not be listed (Units nil) is read whole by run. Cache hits are checked as in run,
// before anything is passed on, so a cache that disagrees with the store is
// dropped before any of it is used.
func (s storeRead) each(emit func(unit string, r unitResult) error) error {
	if s.Batch == nil {
		s.Batch = func(ids []string) [][]string { return batchByRecords(ids, nil, storeBatchUnits) }
	}
	if s.Units == nil {
		res, err := s.run()
		if err != nil {
			return err
		}
		ids := make([]string, 0, len(res))
		for u := range res {
			ids = append(ids, u)
		}
		sort.Strings(ids)
		for _, u := range ids {
			if err := emit(u, res[u]); err != nil {
				return err
			}
		}
		return nil
	}
	name := s.Cache.cacheName()
	ids := make([]string, 0, len(s.Units))
	for u := range s.Units {
		ids = append(ids, u)
	}
	sort.Strings(ids)
	// Hits are found in the index alone; each is loaded only when it is
	// passed on.
	var want, hits []string
	for _, u := range ids {
		if s.Cache.has(s.key(u), s.Units[u]) && !s.Force[u] {
			hits = append(hits, u)
			continue
		}
		want = append(want, u)
	}
	listed := func(u string, r unitResult) error {
		reportRead(name, u)
		if r.Err == nil {
			s.Cache.put(s.key(u), s.Units[u], r.Sessions)
		}
		return emit(u, r)
	}
	if checks := sample(hits, spotChecks); len(checks) > 0 {
		sort.Strings(checks)
		fresh, err := s.Read(checks)
		if err != nil {
			return err
		}
		agree := true
		for _, u := range checks {
			cached, ok := s.Cache.get(s.key(u), s.Units[u])
			if r := fresh[u]; !ok || r.Err != nil || !reflect.DeepEqual(r.Sessions, cached) {
				agree = false
			}
		}
		if !agree {
			notice("%s: cached history did not match a fresh read; read it all again", name)
			s.Cache.prune(func(k string) bool { return strings.HasPrefix(k, s.Scope+"\x00") })
			want, hits = ids, nil
		} else {
			checked := map[string]bool{}
			for _, u := range checks {
				checked[u] = true
				if err := listed(u, fresh[u]); err != nil {
					return err
				}
			}
			var rest []string
			for _, u := range hits {
				if !checked[u] {
					rest = append(rest, u)
				}
			}
			hits = rest
		}
	}
	for _, u := range hits {
		ss, ok := s.Cache.get(s.key(u), s.Units[u])
		if !ok {
			// Its file is gone or unreadable: read it with the rest.
			want = append(want, u)
			continue
		}
		if err := emit(u, unitResult{Sessions: ss}); err != nil {
			return err
		}
	}
	sort.Strings(want)
	for _, group := range s.Batch(want) {
		fresh, err := s.Read(group)
		if err != nil {
			return err
		}
		for _, u := range group {
			if err := listed(u, fresh[u]); err != nil {
				return err
			}
			delete(fresh, u)
		}
		// A unit created after the listing is used, not cached.
		for u, r := range fresh {
			if err := emit(u, r); err != nil {
				return err
			}
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
	return nil
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
// reading no unit's contents. The listing is what lets a store be read a
// batch at a time, so it is made with or without a cache (c). It is nil when
// the listing fails (a store of another version); the store is then read
// whole.
func storeFingerprints(c *unitCache, bin, db, sql string) map[string]string {
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
