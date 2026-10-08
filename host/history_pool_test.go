package main

import (
	"strings"
	"testing"
	"time"
)

// A connection retains identity only; searches see sessions written after
// initialization and after an earlier search, even less than a second later.
func TestASharedHistoryReadDoesNotHideAnEarlierAsk(t *testing.T) {
	p := &historyPool{}
	h := p.load("claude-code")
	if h.client != "claude-code" || h.past != nil || h.done != nil {
		t.Fatalf("connection retained a history snapshot: %+v", h)
	}
	text := "Can you check whether GitLab Runner commit abc123 is ready to release after v19.4.0?"
	now := time.Now()
	first := pastRequest{ref: "claude-code/first/0", session: "first", at: now.Add(-2 * time.Second), text: text, words: wordSet(text)}
	current := pastRequest{ref: "claude-code/current/0", session: "current", at: now.Add(-time.Second), text: text, words: wordSet(text)}
	stubHistory(t, []pastRequest{first})
	if note := noMatchNote("gitlab runner commit release readiness", h); strings.Contains(note, "Want me to save") {
		t.Fatal("first ask was offered")
	}
	stubHistory(t, []pastRequest{first, current})
	if note := noMatchNote("gitlab runner commit release readiness", h); !strings.Contains(note, "1 earlier session") {
		t.Fatalf("second search reused stale history: %s", note)
	}
	if h.past != nil {
		t.Fatal("connection retained completed search history")
	}
}
