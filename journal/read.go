package journal

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// InspectState describes whether a journal has reached its terminal record.
// Unknown means inspection was bounded before the terminal record could be
// established.
type InspectState string

const (
	InspectFinished    InspectState = "finished"
	InspectRunning     InspectState = "running"
	InspectInterrupted InspectState = "interrupted"
	InspectUnknown     InspectState = "unknown"
)

// Event is the safe, displayable part of one journal record. It deliberately
// excludes request digests, replies, blob paths, and byte counts.
type Event struct {
	Phase   string    `json:"phase"`
	ID      string    `json:"id,omitempty"`
	Method  string    `json:"method,omitempty"`
	Effect  string    `json:"effect,omitempty"`
	At      time.Time `json:"at"`
	Outcome string    `json:"outcome,omitempty"`
}

// Snapshot is a bounded, read-only view of a TAP run.
type Snapshot struct {
	Header    Header       `json:"header"`
	State     InspectState `json:"state"`
	Outcome   string       `json:"outcome,omitempty"`
	Events    []Event      `json:"events"`
	Truncated bool         `json:"truncated"`
}

const (
	defaultInspectEvents = 100
	maxInspectEvents     = 1000
	maxInspectLineBytes  = 64 << 10
	maxInspectTotalBytes = 4 << 20
)

// Inspect reads a run without opening it for resume or taking its lease. A
// cut-short final line is ignored because it cannot describe a durable event.
// The returned view is bounded; Truncated reports that events were omitted or
// that the journal exceeded the inspection byte limit.
func Inspect(root, runID string, limit int) (Snapshot, error) {
	if !runIDRe.MatchString(runID) {
		return Snapshot{}, fmt.Errorf("run id %q is not usable", runID)
	}
	if limit <= 0 {
		limit = defaultInspectEvents
	}
	if limit > maxInspectEvents {
		limit = maxInspectEvents
	}

	dir, err := inspectRunDir(root, runID)
	if err != nil {
		return Snapshot{}, err
	}
	path := filepath.Join(dir, "index.jsonl")
	info, err := os.Lstat(path)
	if err != nil {
		return Snapshot{}, fmt.Errorf("no run %s: %w", runID, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return Snapshot{}, fmt.Errorf("run %s has an unsafe index", runID)
	}

	f, err := os.Open(path)
	if err != nil {
		return Snapshot{}, fmt.Errorf("no run %s: %w", runID, err)
	}
	defer f.Close()

	s := Snapshot{Events: make([]Event, 0, limit)}
	reader := bufio.NewReaderSize(io.LimitReader(f, maxInspectTotalBytes+1), maxInspectLineBytes+1)
	sawHeader, terminalKnown, finished := false, info.Size() <= maxInspectTotalBytes, false
	if !terminalKnown {
		s.Truncated = true
	}
	for {
		line, readErr := reader.ReadSlice('\n')
		if readErr == bufio.ErrBufferFull {
			s.Truncated = true
			terminalKnown = false
			break
		}
		if len(line) > maxInspectLineBytes {
			s.Truncated = true
			terminalKnown = false
			break
		}
		if readErr == io.EOF {
			if len(line) != 0 { // a crash may have cut this final record short
				s.Truncated = true
			}
			break
		}
		if readErr != nil {
			return Snapshot{}, fmt.Errorf("read run %s: %w", runID, readErr)
		}
		var record Record
		if err := json.Unmarshal(line, &record); err != nil {
			return Snapshot{}, fmt.Errorf("run %s has malformed journal record: %w", runID, err)
		}
		switch record.Phase {
		case "header":
			if record.Header == nil {
				return Snapshot{}, fmt.Errorf("run %s has a header without metadata", runID)
			}
			s.Header = *record.Header
			sawHeader = true
		case "finish":
			finished = true
			s.Outcome = record.Outcome
		}
		if record.Phase != "header" {
			event := Event{Phase: record.Phase, ID: record.ID, Method: record.Method, Effect: record.Effect, At: record.At, Outcome: record.Outcome}
			if len(s.Events) < limit {
				s.Events = append(s.Events, event)
			} else {
				s.Truncated = true
			}
		}
	}
	if !sawHeader {
		return Snapshot{}, fmt.Errorf("run %s has no header", runID)
	}
	if finished {
		s.State = InspectFinished
		return s, nil
	}
	if !terminalKnown {
		s.State = InspectUnknown
		return s, nil
	}
	if inspectLeaseHeld(dir, time.Now()) {
		s.State = InspectRunning
	} else {
		s.State = InspectInterrupted
	}
	return s, nil
}

func inspectRunDir(root, runID string) (string, error) {
	cleanRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("journal root: %w", err)
	}
	dir := filepath.Join(cleanRoot, runID)
	info, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("no run %s: %w", runID, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("run %s has an unsafe directory", runID)
	}
	return dir, nil
}

func inspectLeaseHeld(dir string, now time.Time) bool {
	lease, err := readLease(filepath.Join(dir, "lease.json"))
	return err == nil && now.Before(lease.Expires) && alive(lease.PID)
}
