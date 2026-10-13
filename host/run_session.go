package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/journal"
)

// runSession owns the guest's request/reply protocol, including request
// replay and the once-only result accounting shared by concurrent requests.
type runSession struct {
	dispatcher *requestDispatcher
	options    Options
	result     *Result
	resultMu   sync.Mutex
	encodeMu   sync.Mutex
	stopped    atomic.Bool

	guestCtx  context.Context
	stopGuest context.CancelFunc
	toGuest   *os.File
	fromGuest *os.File
	encoder   *json.Encoder
	done      <-chan error
	guestErr  *guestStderr
	budget    *budget
	limit     time.Duration
	started   time.Time
}

func (s *runSession) afterRequest(rq request) error {
	if rq.info == nil {
		return nil
	}
	if rq.info.err != nil {
		return rq.info.err
	}
	s.resultMu.Lock()
	s.result.Unknown += rq.info.unknown
	if rq.info.run != nil {
		s.result.Children = append(s.result.Children, *rq.info.run)
	}
	s.resultMu.Unlock()
	return nil
}

// answer recovers a recorded response when possible and otherwise dispatches
// once, retaining the same call identity and child-run journal semantics.
func (s *runSession) answer(rq request) (rp reply, replayed bool, err error) {
	d := s.dispatcher
	assess := func() {
		if rq.Method == "call" && d.admission != nil {
			if bd := d.admission.byAlias[rq.Alias]; bd != nil {
				effect, refused, nested := callEffect(bd, d.admission.inv, rq.Arguments, d.bridge)
				rq.callAssessment = &callAssessment{effect: effect, refused: refused, nested: nested}
			}
		}
	}
	// Asking whether a write is allowed changes nothing and is not recorded.
	if rq.Method == "run" {
		rq.info = &childInfo{}
	}
	if d.run == nil || rq.ID == "" || rq.Method == "canwrite" {
		assess()
		rp = d.dispatch(rq)
		return rp, false, s.afterRequest(rq)
	}
	id := rq.ID
	content := rq
	content.ID = ""
	digest := journal.Digest(content)
	state, recorded, err := d.run.Lookup(id, digest)
	if err != nil {
		return reply{}, false, fmt.Errorf("%w; the record cannot be replayed into a program that is not following it", err)
	}
	switch state {
	case journal.Finished:
		var old reply
		if err := json.Unmarshal(recorded, &old); err != nil {
			return reply{}, false, fmt.Errorf("the record of %s cannot be read: %w", id, err)
		}
		s.resultMu.Lock()
		s.result.Replayed++
		s.resultMu.Unlock()
		return old, true, nil
	case journal.Interrupted:
		// Nothing was recorded after it began. A read is asked again. A change
		// may or may not have happened, so repeating it could act twice.
		if effect := d.run.Effect(id); effect != "read" && rq.Method != "run" {
			logf("  UNKNOWN  %s %s  (%s, in progress when the earlier run stopped)", rq.Method, id, effect)
			rp = reply{Unknown: true, Refused: "the outcome of this " + effect + " is unknown: an earlier run stopped while it was in progress"}
			b, _ := json.Marshal(rp)
			if err := d.run.End(id, "unknown", b, time.Now().UTC()); err != nil {
				return reply{}, false, err
			}
			s.resultMu.Lock()
			s.result.Unknown++
			s.resultMu.Unlock()
			return rp, false, nil
		}
	}
	assess()
	var identity *journal.CallIdentity
	if rq.Method == "call" {
		identity = &journal.CallIdentity{Alias: rq.Alias}
		if d.admission != nil {
			if bd := d.admission.byAlias[rq.Alias]; bd != nil {
				identity.Server, identity.Tool, identity.Annotated, identity.Schema = bd.Server, bd.Tool, bd.Annotated, bd.Schema
				identity.Operation = bd.Operation
				if rq.callAssessment != nil && rq.callAssessment.nested != "" {
					identity.Operation = rq.callAssessment.nested
				}
			}
		}
	}
	if rq.Method == "run" {
		// Name the child before it starts so an interrupted parent can resume it.
		child := ""
		if state == journal.Interrupted {
			child = d.run.Child(id)
		}
		rq.resumeChild = child != ""
		if child == "" {
			child = journal.NewRunID(time.Now().UTC())
		}
		rq.childRun = child
		if err := d.run.BeginChild(id, rq.Method, digest, d.effectOf(rq), child, time.Now().UTC()); err != nil {
			return reply{}, false, err
		}
	} else if err := d.run.BeginCall(id, rq.Method, digest, d.effectOf(rq), identity, time.Now().UTC()); err != nil {
		return reply{}, false, err
	}
	if s.options.stopDuring == id {
		s.stopped.Store(true)
		return reply{}, false, errInterrupted
	}
	rp = d.dispatch(rq)
	if err := s.afterRequest(rq); err != nil {
		return reply{}, false, err
	}
	outcome := "ran"
	if rp.Refused != "" {
		outcome = "refused"
	}
	b, _ := json.Marshal(rp)
	if err := d.run.End(id, outcome, b, time.Now().UTC()); err != nil {
		return reply{}, false, err
	}
	return rp, false, nil
}

func (s *runSession) serve() (*Result, string, error) {
	d := s.dispatcher
	var firstErr error
	reader := bufio.NewReaderSize(s.fromGuest, 1<<20)
	var final *request
	var wg sync.WaitGroup
	inFlight := make(chan struct{}, 8)
	answered := 0
	for final == nil && !s.stopped.Load() {
		line, rerr := readLine(reader, maxGuestLine)
		if errors.Is(rerr, errLineTooLong) {
			s.resultMu.Lock()
			if firstErr == nil {
				firstErr = rerr
			}
			s.resultMu.Unlock()
			s.stopped.Store(true)
			break
		}
		if len(line) > 0 {
			var rq request
			if err := json.Unmarshal(line, &rq); err != nil {
				logf("guest      non-protocol output: %s", strings.TrimSpace(string(line)))
			} else if s.stopped.Load() {
				// Once stopped, a newly arriving request is never acted on.
				break
			} else if rq.Method == "return" {
				final = &rq
			} else {
				wg.Add(1)
				inFlight <- struct{}{}
				go func(rq request) {
					defer wg.Done()
					defer func() { <-inFlight }()
					rp, replayed, err := s.answer(rq)
					s.resultMu.Lock()
					defer s.resultMu.Unlock()
					if err != nil {
						if firstErr == nil {
							firstErr = err
						}
						s.stopped.Store(true)
						s.toGuest.Close()
						return
					}
					if s.stopped.Load() || s.guestCtx.Err() != nil {
						return
					}
					answered++
					if s.options.stopAfter > 0 && answered >= s.options.stopAfter {
						// The answer is on record and the program never heard it.
						s.stopped.Store(true)
						if firstErr == nil {
							firstErr = errInterrupted
						}
						s.toGuest.Close()
						return
					}
					if rp.Refused != "" {
						s.result.Refused++
					} else if !replayed && rq.Method != "tools" && rq.Method != "canwrite" {
						s.result.Ran++
						if rq.Method == "call" {
							s.result.Calls++
							if rp.Exit != 0 {
								s.result.FailedCalls++
							}
						}
					}
					rp.ID = rq.ID
					s.encodeMu.Lock()
					err = s.encoder.Encode(rp)
					s.encodeMu.Unlock()
					if err != nil && firstErr == nil {
						firstErr = err
					}
				}(rq)
			}
		}
		if rerr != nil {
			break
		}
	}
	s.stopGuest() // stop dispatched programs before waiting for their answers
	wg.Wait()
	outcome := "failed"
	if s.budget.expired() {
		outcome = "timed_out"
		s.toGuest.Close()
		go io.Copy(io.Discard, s.fromGuest)
		s.stopGuest()
		<-s.done
		msg := fmt.Sprintf("the primitive ran past its time limit of %s and was stopped", s.limit)
		d.approval.audit("timed_out", "run", "", map[string]any{"limit_seconds": int(s.limit / time.Second)})
		logf("%s", msg)
		if d.run != nil {
			d.run.Finish(outcome, time.Now().UTC())
		}
		return nil, outcome, errors.New(msg)
	}
	if firstErr != nil {
		outcome = "interrupted"
		s.toGuest.Close()
		go io.Copy(io.Discard, s.fromGuest)
		s.stopGuest()
		<-s.done
		return nil, outcome, firstErr
	}
	s.toGuest.Close()
	go io.Copy(io.Discard, s.fromGuest)
	if err := <-s.done; err != nil {
		logf("guest exit %v", err)
	}
	if s.guestErr.Len() > 0 {
		logf("guest stderr:\n%s", clipLines(strings.TrimRight(s.guestErr.String(), "\n"), 12))
		if s.guestErr.truncated {
			logf("guest stderr past the runner's limit was dropped")
		}
	}
	logf("%d call(s) and command(s) run, %d refused, in %s", s.result.Ran, s.result.Refused, time.Since(s.started).Round(time.Millisecond))
	if final == nil {
		if s.guestErr.memoryFailure {
			return nil, outcome, fmt.Errorf("the primitive ran out of memory (the limit is %d MiB)", guestMemoryPages*64/1024)
		}
		return nil, outcome, fmt.Errorf("the primitive ended without a result")
	}
	s.result.Exit, s.result.Stdout, s.result.Stderr = final.Exit, final.Stdout, final.Stderr
	s.result.Elapsed, s.result.Inputs = time.Since(s.started), inputItems(s.options.Args)
	outcome = "completed"
	if s.result.Exit != 0 {
		outcome = "failed"
	} else if s.result.Unknown > 0 {
		outcome = "completed_with_unknown"
	} else if s.result.Refused > 0 {
		outcome = "completed_with_refusals"
	}
	if d.run != nil {
		var finishErr error
		if d.tree.parentRun != "" {
			// A child run keeps its output if its parent stops before hearing it.
			b, _ := json.Marshal(storedResult{Exit: s.result.Exit, Stdout: s.result.Stdout, Stderr: s.result.Stderr, Unknown: s.result.Unknown})
			finishErr = d.run.FinishWith(outcome, b, time.Now().UTC())
		} else {
			finishErr = d.run.Finish(outcome, time.Now().UTC())
		}
		if finishErr != nil {
			return nil, outcome, finishErr
		}
		if s.result.Replayed > 0 {
			logf("%d request(s) were answered from the record and not made again", s.result.Replayed)
		}
	}
	return s.result, outcome, nil
}
