package main

import (
	"testing"
	"time"
)

// Found testing Claude Code against the shared runner: the second ask of a
// task, three minutes after the first, was told the task was new. Its session
// was given a history read made before the first ask was written. A pooled
// read is reused only within currentSessionSlack, and each session counts as
// earlier what began before its own start.
func TestASharedHistoryReadDoesNotHideAnEarlierAsk(t *testing.T) {
	now := time.Now()
	done := make(chan struct{})
	close(done)
	text := "Can you check whether GitLab Runner commit 3c39fceb is ready to release after v19.4.0?"
	first := pastRequest{ref: "claude-code/first/0", session: "first", at: now.Add(-45 * time.Second), text: text, words: wordSet(text)}

	// A read from 20 seconds ago is shared, but the new session's own start
	// decides what is earlier: the ask 45 seconds ago is.
	p := &historyPool{loads: map[string]*historyLoad{"claude-code": {started: now.Add(-20 * time.Second), done: done, past: []pastRequest{first}}}}
	h := p.load("claude-code")
	past, ok := h.earlier(time.Second)
	if !ok || len(past) != 1 {
		t.Fatalf("shared read hid the earlier ask: %v %v", past, ok)
	}

	// A read older than the slack is not given to a new session at all.
	stale := &historyLoad{started: now.Add(-40 * time.Second), done: done}
	p.loads["claude-code"] = stale
	stubHistory(t, []pastRequest{first})
	if h := p.load("claude-code"); h == stale || h.from == stale {
		t.Fatal("a read older than the slack was reused")
	}
}
