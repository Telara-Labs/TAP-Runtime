package journal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// A run has a lease (ruling 25, doc 34 section 13.16). The first process to
// start or resume a run holds it. A second is refused and told which process
// holds it. The lease expires, so a process that died without releasing it
// does not hold the run for ever.

// LeaseTTL is how long a lease lasts without being renewed. The holder
// renews it at a third of that.
const LeaseTTL = 30 * time.Second

// HeldError is returned when another process holds the run.
type HeldError struct {
	RunID   string
	PID     int
	Expires time.Time
}

func (e *HeldError) Error() string {
	return fmt.Sprintf("run %s is held by process %d until %s; two processes may not continue one run",
		e.RunID, e.PID, e.Expires.UTC().Format(time.RFC3339))
}

type leaseRecord struct {
	PID     int       `json:"pid"`
	Token   string    `json:"token"`
	Expires time.Time `json:"expires"`
}

// Lease is a held lease. Release it when the run ends.
type Lease struct {
	path  string
	token string
	stop  chan struct{}
	done  sync.WaitGroup
	once  sync.Once
}

// alive reports whether a process exists. Where that cannot be asked, the
// answer is yes: the lease then lasts until it expires, which is the safe
// direction.
func alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH)
}

func readLease(path string) (*leaseRecord, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r leaseRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// acquire takes the lease of the run in dir, or says who holds it.
func acquire(dir, runID string, now time.Time) (*Lease, error) {
	path := filepath.Join(dir, "lease.json")
	token := NewRunID(now)
	mine := leaseRecord{PID: os.Getpid(), Token: token, Expires: now.Add(LeaseTTL)}
	body, _ := json.Marshal(mine)

	for attempt := 0; attempt < 3; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, werr := f.Write(body)
			f.Close()
			if werr != nil {
				os.Remove(path)
				return nil, werr
			}
			return start(path, token), nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		held, rerr := readLease(path)
		if rerr == nil && now.Before(held.Expires) && alive(held.PID) {
			return nil, &HeldError{RunID: runID, PID: held.PID, Expires: held.Expires}
		}
		// Expired, unreadable, or its holder is gone. It is removed and
		// taken afresh; if two processes do this at once, O_EXCL lets one
		// through and the other reads the winner's lease on its next pass.
		os.Remove(path)
	}
	return nil, fmt.Errorf("run %s: the lease could not be taken", runID)
}

func start(path, token string) *Lease {
	l := &Lease{path: path, token: token, stop: make(chan struct{})}
	l.done.Add(1)
	go func() {
		defer l.done.Done()
		t := time.NewTicker(LeaseTTL / 3)
		defer t.Stop()
		for {
			select {
			case <-l.stop:
				return
			case now := <-t.C:
				l.renew(now)
			}
		}
	}()
	return l
}

func (l *Lease) renew(now time.Time) {
	held, err := readLease(l.path)
	if err != nil || held.Token != l.token {
		return // it is no longer ours to renew
	}
	held.Expires = now.Add(LeaseTTL)
	body, _ := json.Marshal(held)
	tmp := l.path + ".tmp"
	if os.WriteFile(tmp, body, 0o600) == nil {
		os.Rename(tmp, l.path)
	}
}

// Release gives the lease up. It removes the lease only if it is still this
// holder's.
func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		close(l.stop)
		l.done.Wait()
		if held, err := readLease(l.path); err == nil && held.Token == l.token {
			os.Remove(l.path)
		}
	})
}

// Sweep removes the records of runs that ended more than days ago (ruling
// 26). A run that is held is never removed. days of 0 or less keeps
// everything. It returns how many it removed.
func Sweep(root string, days int, now time.Time) int {
	if days <= 0 {
		return 0
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	removed := 0
	for _, e := range entries {
		if !e.IsDir() || !runIDRe.MatchString(e.Name()) {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "index.jsonl")); err != nil {
			continue // not a run record; not ours to remove
		}
		if held, err := readLease(filepath.Join(dir, "lease.json")); err == nil && now.Before(held.Expires) && alive(held.PID) {
			continue
		}
		newest := time.Time{}
		filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
			if err == nil && info.ModTime().After(newest) {
				newest = info.ModTime()
			}
			return nil
		})
		if newest.Before(cutoff) && os.RemoveAll(dir) == nil {
			removed++
		}
	}
	return removed
}
