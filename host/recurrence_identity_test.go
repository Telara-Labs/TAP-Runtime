package main

import (
	"strings"
	"testing"
	"time"
)

func TestRecentDistinctSessionsCountWithoutCountingTheCurrentOne(t *testing.T) {
	for _, gap := range []time.Duration{time.Second, 9 * time.Second, 16 * time.Second, 45 * time.Second} {
		t.Run(gap.String(), func(t *testing.T) {
			now := time.Now()
			text := "Can you check whether GitLab Runner commit abc123 is ready to release after v19.4.0?"
			query := "gitlab runner commit release readiness"
			current := pastRequest{session: "current", ref: "opencode/current/0", at: now.Add(-500 * time.Millisecond), text: text, words: wordSet(text), searches: []pendingSearch{{Query: searchDigest(query), At: now.Add(-100 * time.Millisecond)}}}
			previous := pastRequest{session: "previous", ref: "opencode/previous/0", at: now.Add(-gap), text: text, words: wordSet(text)}
			h := &historyLoad{started: now, done: make(chan struct{}), past: []pastRequest{current, previous}}
			close(h.done)
			if note := noMatchNote(query, h); !strings.Contains(note, "1 earlier session") || !strings.Contains(note, "opencode/previous/0") {
				t.Fatalf("distinct %s session lost: %s", gap, note)
			}
			h.past = []pastRequest{current}
			if note := noMatchNote(query, h); strings.Contains(note, "Want me to save") {
				t.Fatalf("current-only first ask offered: %s", note)
			}
		})
	}
}

func TestPendingSearchIdentifiesResumedSessionAndDoesNotGuessOnConcurrency(t *testing.T) {
	now := time.Now()
	query := "gitlab runner commit release readiness"
	text := "Can you check GitLab Runner commit abc123 release readiness?"
	current := pastRequest{session: "resumed", at: now.Add(-time.Hour), text: text, words: wordSet(text), searches: []pendingSearch{{Query: searchDigest(query), At: now.Add(-time.Second)}}}
	previous := pastRequest{session: "previous", at: now.Add(-9 * time.Second), text: text, words: wordSet(text)}
	h := &historyLoad{started: now, done: make(chan struct{}), past: []pastRequest{current, previous}}
	close(h.done)
	past, ok := h.earlierForSearch(time.Second, query)
	if !ok || len(past) != 1 || past[0].session != "previous" {
		t.Fatalf("resumed session was not excluded by identity: %+v %v", past, ok)
	}
	previous.searches = []pendingSearch{{Query: searchDigest(query), At: now.Add(-2 * time.Second)}}
	h.past = []pastRequest{current, previous}
	if _, ok := h.earlierForSearch(time.Second, query); ok {
		t.Fatal("concurrent identical pending searches guessed a current session")
	}
}

func TestRecentFallbackExcludesOnlyOneSessionAndRefusesTiedCandidates(t *testing.T) {
	now := time.Now()
	h := &historyLoad{started: now, done: make(chan struct{}), past: []pastRequest{{session: "previous", at: now.Add(-9 * time.Second)}, {session: "current", at: now.Add(-time.Second)}}}
	close(h.done)
	past, ok := h.earlier(time.Second)
	if !ok || len(past) != 1 || past[0].session != "previous" {
		t.Fatalf("recent fallback dropped distinct session: %+v %v", past, ok)
	}
	h.past[0].at = h.past[1].at
	if _, ok := h.earlier(time.Second); ok {
		t.Fatal("tied current-session candidates guessed")
	}
}
