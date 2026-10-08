package main

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"sync"

	"github.com/tetratelabs/wazero"
)

// Compiling an interpreter takes seconds of CPU and a good deal of memory,
// and every run needs one. Without a cache each run compiled its own, and a
// burst of sessions compiled the same interpreter dozens of times at once.
// A process keeps one compilation cache per directory for its whole life,
// shared by all its runs, and the same module is compiled by one run at a
// time: the next finds it in the cache.

var compileCaches = struct {
	sync.Mutex
	byDir map[string]wazero.CompilationCache
}{byDir: map[string]wazero.CompilationCache{}}

// compilationCache is the process's cache for dir, kept on disk so the next
// process finds the compiled interpreter too. An empty dir is the user cache
// directory's.
func compilationCache(dir string) (wazero.CompilationCache, error) {
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(base, "tap-runtime", "compiled")
	}
	compileCaches.Lock()
	defer compileCaches.Unlock()
	if c := compileCaches.byDir[dir]; c != nil {
		return c, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	c, err := wazero.NewCompilationCacheWithDir(dir)
	if err != nil {
		return nil, err
	}
	compileCaches.byDir[dir] = c
	return c, nil
}

var compiling = struct {
	sync.Mutex
	byModule map[[32]byte]*sync.Mutex
}{byModule: map[[32]byte]*sync.Mutex{}}

// compileLock holds off other compiles of the same module in this process
// until this one is done.
func compileLock(module []byte) (unlock func()) {
	key := sha256.Sum256(module)
	compiling.Lock()
	m := compiling.byModule[key]
	if m == nil {
		m = &sync.Mutex{}
		compiling.byModule[key] = m
	}
	compiling.Unlock()
	m.Lock()
	return m.Unlock
}
