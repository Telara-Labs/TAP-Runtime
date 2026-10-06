package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// A client waits only so long for one tool call: Codex's code cell stopped
// waiting for tap_run at about 30 seconds and the model answered by hand
// while the primitive was still running. A run that has not finished by
// handoffAfter keeps running here, and tap_run answers with a handle that
// tap_result waits on, again for at most handoffAfter at a time.
var handoffAfter = 20 * time.Second

// handoffPromptWait bounds a question a run asks after its tap_run call has
// returned: a client may not show a prompt outside a tool call, and an
// unanswered question is a no.
var handoffPromptWait = 2 * time.Minute

type heldRun struct {
	done chan struct{}
	res  *Result
	err  error
}

type heldRuns struct {
	mu   sync.Mutex
	n    int
	runs map[string]*heldRun
}

func (h *heldRuns) add(r *heldRun) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.runs == nil {
		h.runs = map[string]*heldRun{}
	}
	h.n++
	key := fmt.Sprintf("held-%d-%d", time.Now().Unix(), h.n)
	h.runs[key] = r
	return key
}

func (h *heldRuns) get(key string) *heldRun {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.runs[key]
}

func (h *heldRuns) forget(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.runs, key)
}

// runWithHandoff runs o and answers the tap_run call inline when the run
// ends within handoffAfter, or with a handle when it does not.
func (s *server) runWithHandoff(id *json.RawMessage, ctx context.Context, o Options, canElicit bool) {
	var handed atomic.Bool
	o.Approve = boundedAfterHandoff(o.Approve, &handed)
	o.Choose = boundedChooseAfterHandoff(o.Choose, &handed)
	r := &heldRun{done: make(chan struct{})}
	// The run outlives this request once it is handed off.
	runCtx := context.WithoutCancel(ctx)
	go func() {
		r.res, r.err = Run(runCtx, o)
		close(r.done)
	}()
	select {
	case <-r.done:
		s.replyRun(id, r.res, r.err, canElicit)
		return
	case <-time.After(handoffAfter):
	}
	handed.Store(true)
	key := s.held.add(r)
	s.reply(id, map[string]any{"content": []any{map[string]any{"type": "text", "text": handoffText(key)}}})
}

func handoffText(key string) string {
	return fmt.Sprintf("The primitive is still running (it has taken over %d seconds) and keeps running. Call tap_result with run %q to get its result; do not run it again or do its steps by hand.", int(handoffAfter.Seconds()), key)
}

// replyHeld answers tap_result for a handed-off run: its result if it ends
// within handoffAfter, else another handle to wait on.
func (s *server) replyHeld(id *json.RawMessage, key string, r *heldRun, canElicit bool) {
	select {
	case <-r.done:
		s.held.forget(key)
		s.replyRun(id, r.res, r.err, canElicit)
	case <-time.After(handoffAfter):
		s.reply(id, map[string]any{"content": []any{map[string]any{"type": "text", "text": handoffText(key)}}})
	}
}

func boundedAfterHandoff(inner Approver, handed *atomic.Bool) Approver {
	if inner == nil {
		return nil
	}
	return func(a Ask) Grant {
		if !handed.Load() {
			return inner(a)
		}
		ch := make(chan Grant, 1)
		go func() { ch <- inner(a) }()
		select {
		case g := <-ch:
			return g
		case <-time.After(handoffPromptWait):
			return Grant{}
		}
	}
}

func boundedChooseAfterHandoff(inner Chooser, handed *atomic.Bool) Chooser {
	if inner == nil {
		return nil
	}
	return func(p Pick) (string, bool) {
		if !handed.Load() {
			return inner(p)
		}
		type answer struct {
			s  string
			ok bool
		}
		ch := make(chan answer, 1)
		go func() { s, ok := inner(p); ch <- answer{s, ok} }()
		select {
		case a := <-ch:
			return a.s, a.ok
		case <-time.After(handoffPromptWait):
			return "", false
		}
	}
}
