package main

import (
	"fmt"
	"os"
	"path/filepath"
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
	return pastRequest{ref: "claude-code/" + session + "/0", session: session, at: time.Now().AddDate(0, 0, -daysAgo), text: text, words: wordSet(text)}
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

// writeClaudeSession writes a Claude Code transcript under home: a session
// that opens with first, makes one tool call, then gets a follow-up.
func writeClaudeSession(t *testing.T, home, id string, at time.Time, first, followUp string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", "work")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ts := func(d time.Duration) string { return at.Add(d).UTC().Format(time.RFC3339) }
	lines := []string{
		fmt.Sprintf(`{"type":"user","sessionId":%q,"timestamp":%q,"message":{"role":"user","content":%q}}`, id, ts(0), first),
		fmt.Sprintf(`{"type":"assistant","sessionId":%q,"timestamp":%q,"message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"git status"}}]}}`, id, ts(time.Second)),
		fmt.Sprintf(`{"type":"user","sessionId":%q,"timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`, id, ts(2*time.Second)),
		fmt.Sprintf(`{"type":"user","sessionId":%q,"timestamp":%q,"message":{"role":"user","content":%q}}`, id, ts(3*time.Second), followUp),
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Only the request that opens a session is compared; the cache is private,
// and a later read finds a session added after the first read.
func TestHistoryReadsOpeningRequestsAndCachesThem(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	writeClaudeSession(t, home, "s1", time.Now().Add(-48*time.Hour), "Can you check whether GitLab Runner commit 3c39fceb is ready to release after v19.4.0?", "ok go one by one")
	past := readHistory("claude-code")
	if len(past) != 1 || !strings.Contains(past[0].text, "GitLab Runner") || past[0].ref != "claude-code/s1/0" {
		t.Fatalf("past = %+v", past)
	}
	info, err := os.Stat(requestCachePath("claude-code"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("cache %v %v", info, err)
	}
	writeClaudeSession(t, home, "s2", time.Now().Add(-time.Minute), "Summarize the billing tickets", "thanks")
	if past = readHistory("claude-code"); len(past) != 2 {
		t.Fatalf("second read = %+v", past)
	}
}

// A long message shares a few words with almost anything; that is not the
// same task.
func TestALongUnrelatedMessageDoesNotRecur(t *testing.T) {
	past := append(background(), req("s1", 1, "I am wondering how big of an ask it would be to remove projects from our scopes, currently we check every project before we release anything and the session only shows a few"))
	if rec := findRecurrence("check commit release readiness after tag", past); rec.Sessions != 0 {
		t.Fatalf("recurrence = %+v", rec)
	}
}
