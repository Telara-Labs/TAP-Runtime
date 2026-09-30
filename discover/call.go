// Package discover finds recurring, templated work in the session history
// that agent clients already keep on this machine (TENG-3054, TENG-3059).
//
// It is the client-local discovery path of TAP concept 2 (telara-documentation
// architecture/tap/24-what-we-can-observe.md section 1): it reads the history
// a client retains, keeps no store of its own (doc 30 section 13: TAP creates
// no local pattern ledger), and sends nothing anywhere. A candidate it reports
// is a nomination for a human or tap-creator to look at, not a primitive.
//
// Every rule here is mechanical. Nothing asks a model what a call means.
package discover

import (
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Call is one tool call an agent made, in any client.
type Call struct {
	Client  string
	Session string
	// ID is the client's own identifier for the call, when it records one. A
	// resumed or forked session can copy earlier calls into a new file with
	// their IDs; a call already read from another file is not read again.
	ID   string
	Time time.Time
	// Tool is the client-neutral tool name: "shell" for a shell command,
	// "mcp:<tool>" for an MCP tool, otherwise the client's own tool name.
	Tool string
	// Command is the raw command line when Tool is "shell".
	Command string
	// Args are the structured arguments of a non-shell call, values
	// flattened to strings.
	Args map[string]string
	// RawArgs marks arguments whose value was JSON other than a string (a
	// number, a boolean, an object), so a replay sends the same type back.
	RawArgs map[string]bool
	// Tokens is this call's share of the model turn that issued it, and Turn
	// identifies that turn within the session. Measured is false when the
	// client does not record usage for it.
	Tokens   Usage
	Turn     int
	Measured bool
	// Request is the index in Session.Requests of the user message this
	// call answered.
	Request int
	// Outcome is what the client recorded about the result: unknown when it
	// recorded nothing. OutIDs are identifiers the result contained (ticket
	// keys, UUIDs, hashes, URLs), used to tell a value an earlier step
	// produced from one the caller supplied.
	Outcome Outcome
	OutIDs  []string
	// OutCtx is, for each of OutIDs, where it sat in the result: up to 32
	// bytes before it on its line, a NUL, then the character after it ("" at
	// the end of a line). A draft uses it to pull the value back out.
	OutCtx []string
}

// Outcome of a call, as the client recorded it.
type Outcome int8

const (
	OutcomeUnknown Outcome = iota
	OutcomeOK
	OutcomeFailed
)

var (
	outIDRe = regexp.MustCompile(`https?://[^\s"'<>)\]]+|\b[A-Z][A-Z0-9]{1,9}-\d+\b|\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b|\b[0-9a-f]{8,40}\b|\b\d{6,}\b`)
	exitRe  = regexp.MustCompile(`(?i)(?:"exit_code"\s*:\s*|exit code:?\s*|exited with code\s*)(-?\d+)`)
)

// outputIDs are the identifiers in a call's result: at most 64, from its
// first 64 KB.
func outputIDs(text string) []string {
	ids, _ := outputRefs(text)
	return ids
}

// outputRefs returns outputIDs and, for each, the context of its first
// occurrence (see Call.OutCtx).
func outputRefs(text string) (ids, ctx []string) {
	text = truncateUTF8(text, 64<<10)
	seen := map[string]bool{}
	for _, loc := range outIDRe.FindAllStringIndex(text, -1) {
		m := strings.TrimRight(text[loc[0]:loc[1]], ".,;:")
		if seen[m] {
			continue
		}
		seen[m] = true
		start, end := loc[0], loc[0]+len(m)
		from := max(start-32, 0)
		// A line starts after a newline, or after an escaped one ("\\n"):
		// clients that store a result JSON-encoded keep the escape.
		if nl := strings.LastIndexByte(text[from:start], '\n'); nl >= 0 {
			from += nl + 1
		}
		if nl := strings.LastIndex(text[from:start], `\n`); nl >= 0 {
			from += nl + 2
		}
		for from < start && !utf8.RuneStart(text[from]) {
			from++
		}
		after := ""
		if end < len(text) && text[end] != '\n' && text[end] != '\r' {
			r, _ := utf8.DecodeRuneInString(text[end:])
			after = string(r)
		}
		ids = append(ids, m)
		ctx = append(ctx, text[from:start]+"\x00"+after)
		if len(ids) == 64 {
			break
		}
	}
	return ids, ctx
}

// exitOutcome reads a shell result's exit status: failed when it names a
// non-zero exit code, OK otherwise.
func exitOutcome(text string) Outcome {
	for _, m := range exitRe.FindAllStringSubmatch(text, -1) {
		if m[1] != "0" {
			return OutcomeFailed
		}
	}
	return OutcomeOK
}

// Usage is model token use: Fresh input (new or cache-written), Cached input
// (read from the prompt cache, far cheaper) and Output.
type Usage struct {
	Fresh  float64 `json:"fresh"`
	Cached float64 `json:"cached"`
	Output float64 `json:"output"`
}

func (u Usage) Total() float64 { return u.Fresh + u.Cached + u.Output }

func (u Usage) add(v Usage) Usage {
	return Usage{u.Fresh + v.Fresh, u.Cached + v.Cached, u.Output + v.Output}
}

func (u Usage) scale(f float64) Usage {
	return Usage{u.Fresh * f, u.Cached * f, u.Output * f}
}

// spread gives each of calls[from:] an equal share of one turn's usage.
func spread(calls []Call, from, turn int, u Usage) {
	n := len(calls) - from
	if n <= 0 {
		return
	}
	for i := from; i < len(calls); i++ {
		calls[i].Tokens, calls[i].Turn, calls[i].Measured = u.scale(1/float64(n)), turn, true
	}
}

// Session is one conversation in one client, calls in the order they ran.
type Session struct {
	Client string
	ID     string
	Start  time.Time
	Calls  []Call
	// Requests are the user's messages in order; each call belongs to the
	// last one before it. Request 0 is empty when calls came before any
	// message. The text stays on this machine: it is used only to check
	// whether a routine's inputs were given in the request.
	Requests []string
}

// isRequest reports a user message that asks for work. Harness wrappers
// (<environment_context>, <command-name>/clear, <local-command-stdout>) are
// written in angle brackets and are not requests.
func isRequest(text string) bool {
	t := strings.TrimSpace(text)
	return t != "" && !strings.HasPrefix(t, "<")
}

// addRequest records a user message as a new request, unless it continues
// the one before it in this session: the same text sent again (a retry), or
// an acknowledgement ("yes, file", "continue", "already approved."), whose
// calls belong to the request it answers.
func (s *Session) addRequest(text string) {
	text = truncateUTF8(requestText(text), 4000)
	t := strings.TrimSpace(text)
	if n := len(s.Requests); n > 0 && strings.TrimSpace(s.Requests[n-1]) != "" {
		if t == strings.TrimSpace(s.Requests[n-1]) || isAcknowledgement(t) {
			return
		}
	}
	s.Requests = append(s.Requests, text)
}

// requestText is what the user asked. Codex wraps it in context blocks
// ("# In app browser:", "# Files mentioned by the user:") and puts the
// request under "## My request for Codex:".
func requestText(text string) string {
	const marker = "## My request for Codex:"
	if i := strings.LastIndex(text, marker); i >= 0 {
		return strings.TrimSpace(text[i+len(marker):])
	}
	return text
}

var ackWords = map[string]bool{"yes": true, "yeah": true, "yep": true, "yup": true, "y": true, "ok": true, "okay": true, "k": true, "sure": true,
	"continue": true, "proceed": true, "go": true, "done": true, "approved": true, "already": true, "lgtm": true, "perfect": true,
	"great": true, "thanks": true, "thank": true, "fine": true, "correct": true, "agreed": true, "good": true, "right": true}

// isAcknowledgement is a short reply that answers the agent rather than
// asking for new work: at most six words, starting with an acknowledgement
// word, naming nothing (no path, URL, ticket or id).
func isAcknowledgement(t string) bool {
	words := strings.Fields(strings.ToLower(t))
	if len(words) == 0 || len(words) > 6 || outIDRe.MatchString(t) || strings.Contains(t, "/") {
		return false
	}
	return ackWords[strings.Trim(words[0], ".,!?:;")]
}

// truncateUTF8 cuts s to at most n bytes without splitting a character.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// request is the index calls made now belong to.
func (s *Session) request() int {
	if len(s.Requests) == 0 {
		s.Requests = append(s.Requests, "")
	}
	return len(s.Requests) - 1
}

// Reader reads one client's retained history.
type Reader interface {
	// Client names the client, e.g. "claude-code".
	Client() string
	// Read returns every session that started at or after since. A client
	// whose store is absent returns (nil, nil).
	Read(since time.Time) ([]Session, error)
}
