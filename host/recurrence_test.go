package main

import (
	"strings"
	"testing"
	"time"
)

func stubHistory(t *testing.T, past []pastRequest) {
	t.Helper()
	old := readHistory
	readHistory = func(string) []pastRequest { return past }
	t.Cleanup(func() { readHistory = old })
}

func req(session string, daysAgo int, text string) pastRequest {
	return pastRequest{session: session, at: time.Now().AddDate(0, 0, -daysAgo), text: text, words: wordSet(text)}
}

// Unrelated history around the requests under test, as a real history has.
func background() []pastRequest {
	return []pastRequest{
		req("b1", 9, "Can you fix the failing login test in the web app?"),
		req("b2", 8, "Summarize the open Jira tickets for the billing team"),
		req("b3", 7, "Can you check whether the staging deploy finished?"),
		req("b4", 6, "Write a short email to the design team about the launch"),
		req("b5", 5, "Why is the dashboard slow after the last release?"),
	}
}

func TestSameKindOfRequestInTwoSessionsRecurs(t *testing.T) {
	past := append(background(),
		req("s1", 3, "Can you check whether GitLab Runner commit 3c39fcebf73d is ready to release after v19.4.0?"),
		req("s2", 0, "Can you check whether GitLab Runner commit 3378221a4ba8 is ready to release after v19.4.1?"))
	rec := findRecurrence("gitlab runner commit release readiness", past)
	if rec.Sessions != 2 || !strings.Contains(rec.Example, "3c39fcebf73d") {
		t.Fatalf("recurrence = %+v", rec)
	}
}

func TestOneSessionIsNotARecurrence(t *testing.T) {
	past := append(background(), req("s1", 0, "Can you check whether GitLab Runner commit 3c39fcebf73d is ready to release after v19.4.0?"))
	if rec := findRecurrence("gitlab runner release readiness", past); rec.Sessions != 1 {
		t.Fatalf("recurrence = %+v", rec)
	}
}

func TestUnrelatedRequestsDoNotRecur(t *testing.T) {
	past := append(background(), req("s1", 0, "Can you check whether GitLab Runner commit 3c39fcebf73d is ready to release after v19.4.0?"))
	if rec := findRecurrence("summarize billing jira tickets", past); rec.Sessions > 1 {
		t.Fatalf("recurrence = %+v", rec)
	}
}

func TestValuesDoNotCountAsWords(t *testing.T) {
	w := wordSet("commit 3378221a4ba8 after v19.4.1 for TENG-3213")
	for _, v := range []string{"33782", "v19", "3213"} {
		if w[v] {
			t.Fatalf("%q kept: %v", v, w)
		}
	}
	if !w["commi"] || !w["after"] {
		t.Fatalf("words dropped: %v", w)
	}
}

func TestInitializeGivesTheAgentInstructions(t *testing.T) {
	if !strings.Contains(serverInstructions, "tap_search") || !strings.Contains(serverInstructions, "note") {
		t.Fatalf("instructions: %s", serverInstructions)
	}
}

// The current session began after the agent connected, so it never counts
// as an earlier one: the first ask of a kind is not offered for saving.
func TestTheCurrentSessionIsNotAnEarlierOne(t *testing.T) {
	h := &historyLoad{started: time.Now().Add(-time.Minute), done: make(chan struct{})}
	h.past = []pastRequest{req("current", 0, "Can you check whether GitLab Runner commit 3c39fceb is ready to release after v19.4.0?")}
	close(h.done)
	if note := noMatchNote("gitlab runner release readiness", h); strings.Contains(note, "tap-author") {
		t.Fatalf("current session counted: %s", note)
	}
}

func TestASlowHistoryDoesNotHoldTheSearchForever(t *testing.T) {
	h := &historyLoad{started: time.Now(), done: make(chan struct{})}
	if _, ok := h.earlier(10 * time.Millisecond); ok {
		t.Fatal("an unfinished read reported ready")
	}
}

// The query Claude Code sent on the second ask in the clean-machine test: two
// of its words are its own phrasing and appear in no earlier request.
func TestAgentPhrasingDoesNotHideARecurrence(t *testing.T) {
	past := []pastRequest{req("s1", 0, "Can you check whether GitLab Runner commit 3c39fcebf73d01d464db3dee8a5267155273a6c5 is ready to release after v19.4.0?")}
	if rec := findRecurrence("check commit release readiness after tag", past); rec.Sessions != 1 {
		t.Fatalf("recurrence = %+v", rec)
	}
}

// One shared everyday word is not the same kind of request.
func TestOneSharedWordIsNotARecurrence(t *testing.T) {
	past := []pastRequest{req("s1", 0, "Can you check whether GitLab Runner commit 3c39fceb is ready to release after v19.4.0?")}
	if rec := findRecurrence("check staging deploy status", past); rec.Sessions != 0 {
		t.Fatalf("recurrence = %+v", rec)
	}
}
