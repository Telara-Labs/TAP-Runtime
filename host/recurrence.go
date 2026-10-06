package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	agents "github.com/Telara-Labs/TAP-Runtime/discover/client"
	"github.com/Telara-Labs/TAP-Runtime/discover/history"
)

// When a search finds no saved primitive, the agent is told whether the task
// recurs: whether the person asked for something like it in other sessions.
// Only then is it told to offer saving it. The comparison reads this agent's
// own history on this machine and sends nothing anywhere.

const (
	recurrenceDays = 30
	// recurrenceWait is how long a search waits for the history read that
	// started when the agent connected. A long history takes seconds.
	recurrenceWait = 10 * time.Second
	// recurrenceScore is the share of the query's weight a past request must
	// carry to count as the same kind of request.
	recurrenceScore = 0.6
)

// pastRequest is one user request from the agent's history.
type pastRequest struct {
	ref     string // client/session/request, as tap discover brief --task takes it
	session string
	at      time.Time
	text    string
	words   map[string]bool
}

// recurrence is what the search says about earlier requests like this one.
type recurrence struct {
	Sessions int       // distinct sessions with a similar request
	Latest   time.Time // the most recent of them
	Example  string    // the earliest similar request, shortened
	Ref      string    // the most recent similar request, for tap discover brief --task
}

// readHistory returns the user requests of the named agent from the last
// recurrenceDays. A full read of a long history takes seconds (12.5 s for 283
// sessions measured), so what was read is kept in a private cache and each
// later read parses only the session files changed since.
var readHistory = func(clientName string) []pastRequest {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	c, ok := historyClient(clientName)
	if !ok {
		return nil
	}
	r, err := history.ReaderFor(c.ID, home)
	if err != nil {
		return nil
	}
	path := requestCachePath(c.ID)
	cache := loadRequestCache(path)
	now := time.Now()
	window := now.AddDate(0, 0, -recurrenceDays)
	since := window
	if cache.ReadAt.After(window) {
		// A session file written during the last read may have been read
		// half-written; read anything touched shortly before it again.
		since = cache.ReadAt.Add(-time.Hour)
	}
	sessions, err := r.Read(since)
	if err != nil && len(sessions) == 0 {
		return cache.requests(c.ID, window)
	}
	for _, s := range sessions {
		cs := cachedSession{Start: s.Start}
		for i, text := range s.Requests {
			if i < len(s.RequestRoles) && s.RequestRoles[i] != "" && s.RequestRoles[i] != "user" {
				continue
			}
			if strings.TrimSpace(text) == "" {
				continue
			}
			words := make([]string, 0, 8)
			for w := range wordSet(text) {
				words = append(words, w)
			}
			sort.Strings(words)
			// A task someone repeats is what they open a session with;
			// the later messages are replies within that task.
			cs.Requests = append(cs.Requests, cachedRequest{Index: i, Words: words, Example: shorten(text, 120)})
			break
		}
		cache.Sessions[s.ID] = cs
	}
	cache.ReadAt = now
	for id, cs := range cache.Sessions {
		if cs.Start.Before(window) {
			delete(cache.Sessions, id)
		}
	}
	saveRequestCache(path, cache)
	return cache.requests(c.ID, window)
}

// requestCache is what earlier reads found: per session, each user
// request's words and a short excerpt, never the whole text.
type requestCache struct {
	Version  int                      `json:"version"`
	ReadAt   time.Time                `json:"read_at"`
	Sessions map[string]cachedSession `json:"sessions"`
}

type cachedSession struct {
	Start    time.Time       `json:"start"`
	Requests []cachedRequest `json:"requests"`
}

type cachedRequest struct {
	Index   int      `json:"index"`
	Words   []string `json:"words"`
	Example string   `json:"example"`
}

const requestCacheVersion = 2

func requestCachePath(client string) string {
	base, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "tap-runtime", "requests-"+client+".json")
}

func loadRequestCache(path string) requestCache {
	empty := requestCache{Version: requestCacheVersion, Sessions: map[string]cachedSession{}}
	if path == "" {
		return empty
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return empty
	}
	var c requestCache
	if json.Unmarshal(b, &c) != nil || c.Version != requestCacheVersion || c.Sessions == nil {
		return empty
	}
	return c
}

// saveRequestCache writes the cache privately and atomically: several agent
// sessions, each with its own tap serve, may write it at once.
func saveRequestCache(path string, c requestCache) {
	if path == "" {
		return
	}
	b, err := json.Marshal(c)
	if err != nil || os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".requests-*")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil || tmp.Chmod(0o600) != nil || tmp.Close() != nil {
		tmp.Close()
		return
	}
	os.Rename(tmp.Name(), path)
}

func (c requestCache) requests(client string, window time.Time) []pastRequest {
	var out []pastRequest
	for id, cs := range c.Sessions {
		if cs.Start.Before(window) {
			continue
		}
		for _, rq := range cs.Requests {
			words := make(map[string]bool, len(rq.Words))
			for _, w := range rq.Words {
				words[w] = true
			}
			out = append(out, pastRequest{ref: fmt.Sprintf("%s/%s/%d", client, id, rq.Index), session: id, at: cs.Start, text: rq.Example, words: words})
		}
	}
	return out
}

// historyClient is the agent whose history to read, from the name it gave
// when it connected: Claude Code says "claude-code", but Codex says
// "codex-mcp-client" and Gemini CLI "gemini-cli-mcp-client" (clientFor).
func historyClient(name string) (agents.Client, bool) {
	if c, ok := agents.Lookup(name); ok {
		return c, true
	}
	return agents.Lookup(clientFor(name))
}

// historyLoad is a history read started when the agent connected. Its
// sessions all began before then, so none of them is the current one.
type historyLoad struct {
	started time.Time
	done    chan struct{}
	past    []pastRequest
}

func loadHistory(clientName string) *historyLoad {
	h := &historyLoad{started: time.Now(), done: make(chan struct{})}
	go func() {
		h.past = readHistory(clientName)
		close(h.done)
	}()
	return h
}

// earlier returns the requests from sessions that began before the agent
// connected, or ok false when the read has not finished within wait.
func (h *historyLoad) earlier(wait time.Duration) (past []pastRequest, ok bool) {
	if h == nil {
		return nil, false
	}
	select {
	case <-h.done:
	case <-time.After(wait):
		return nil, false
	}
	for _, p := range h.past {
		if p.at.Before(h.started) {
			past = append(past, p)
		}
	}
	return past, true
}

// wordSet is the words of a text that name what is asked, not the values it
// is asked for: identifiers with digits (commits, versions, ticket numbers)
// change between requests of the same kind and are left out. Words are cut
// to five letters so that "release" and "releases" agree.
func wordSet(text string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len([]rune(w)) < 3 || strings.IndexFunc(w, unicode.IsDigit) >= 0 {
			continue
		}
		if r := []rune(w); len(r) > 5 {
			w = string(r[:5])
		}
		out[w] = true
	}
	return out
}

// findRecurrence compares a search query with past requests. A word's weight
// is its rarity across the history itself, so words every request uses count
// for little and no list of common words is needed.
func findRecurrence(query string, past []pastRequest) recurrence {
	q := wordSet(query)
	if len(q) == 0 || len(past) == 0 {
		return recurrence{}
	}
	df := map[string]int{}
	for _, p := range past {
		for w := range p.words {
			df[w]++
		}
	}
	// Two shared words must be rare in this history (in at most a tenth of
	// its requests, or two on a short history): words most requests use,
	// such as "check" or "what", do not make two requests the same task.
	rareLimit := len(past) / 10
	if rareLimit < 2 {
		rareLimit = 2
	}
	// Smoothed, so that on a short history, where every word is in every
	// request, the weights fall back to plain overlap instead of zero.
	weight := func(w string) float64 { return 1 + math.Log(float64(len(past)+1)/float64(df[w]+1)) }
	// Words the history never uses are the agent's phrasing of the search,
	// not evidence either way, so they carry no weight. A past request must
	// still share at least half of the query's words.
	total := 0.0
	for w := range q {
		if df[w] > 0 {
			total += weight(w)
		}
	}
	if total <= 0 {
		return recurrence{}
	}
	sessions := map[string]bool{}
	var rec recurrence
	var earliest time.Time
	for _, p := range past {
		shared, count, rare := 0.0, 0, 0
		for w := range q {
			if p.words[w] {
				shared, count = shared+weight(w), count+1
				if df[w] <= rareLimit {
					rare++
				}
			}
		}
		// The overlap must also be a real part of the earlier request: a long
		// message shares some words with almost anything.
		if shared/total < recurrenceScore || 2*count < len(q) || rare < 2 || 10*count < 3*len(p.words) {
			continue
		}
		sessions[p.session] = true
		if p.at.After(rec.Latest) || rec.Ref == "" {
			rec.Latest, rec.Ref = p.at, p.ref
		}
		if earliest.IsZero() || p.at.Before(earliest) {
			earliest, rec.Example = p.at, shorten(p.text, 120)
		}
	}
	rec.Sessions = len(sessions)
	return rec
}

func shorten(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// noMatchNote is what an empty search tells the agent. Only a task the person
// asked for in an earlier session is offered for saving.
func noMatchNote(query string, h *historyLoad) string {
	if strings.TrimSpace(query) == "" {
		return "No saved primitive fits. Search again with a few words describing the task to learn whether the person has asked for it before."
	}
	past, ok := h.earlier(recurrenceWait)
	if !ok {
		return "No saved primitive fits. Do the task as usual."
	}
	rec := findRecurrence(query, past)
	if rec.Sessions == 0 {
		return "No saved primitive fits, and nothing like this was asked in earlier sessions. Do the task as usual."
	}
	return fmt.Sprintf("No saved primitive fits, but the person asked for this kind of task in %d earlier session(s), starting with %q. Do the task, then offer in one line to save it as a TAP primitive so it runs as one step next time. Do not save it unless they agree. If they do, the tap-author skill says how; start from the earlier request with: %s discover brief --task %s --out <folder>",
		rec.Sessions, rec.Example, runnerCommand(), rec.Ref)
}

// runnerCommand is how to start this runner from a shell. An agent's shell
// may not have the npm bin folder on PATH (Codex runs commands in a login
// shell), so the note names the program this server is running from.
func runnerCommand() string {
	self, err := os.Executable()
	if err != nil {
		return "tap"
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	if strings.ContainsAny(self, " '\"") {
		return fmt.Sprintf("%q", self)
	}
	return self
}
