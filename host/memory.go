package main

import (
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

// A history read or a burst of runs can take hundreds of megabytes that are
// garbage once it ends. Go keeps freed memory for reuse and gives it back to
// the system only slowly, so a long-lived process (the shared runner) looked
// as large after a burst as during it. releaseMemory gives it back shortly
// after: calls that come close together are answered by one release.

var releasePending atomic.Bool

func releaseMemory() {
	if !releasePending.CompareAndSwap(false, true) {
		return
	}
	time.AfterFunc(2*time.Second, func() {
		releasePending.Store(false)
		debug.FreeOSMemory()
	})
}

// activeRuns counts the runs in progress, so memory is given back when the
// last of a burst ends.
var activeRuns struct {
	sync.Mutex
	n int
}

func runStarted() func() {
	activeRuns.Lock()
	activeRuns.n++
	activeRuns.Unlock()
	return func() {
		activeRuns.Lock()
		activeRuns.n--
		idle := activeRuns.n == 0
		activeRuns.Unlock()
		if idle {
			go releaseMemory()
		}
	}
}
