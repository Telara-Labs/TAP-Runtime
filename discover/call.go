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
	"strings"
	"time"
)

// Call is one tool call an agent made, in any client.
type Call struct {
	Client  string
	Session string
	Time    time.Time
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

// addRequest records a user message and returns its index.
func (s *Session) addRequest(text string) {
	if len(text) > 4000 {
		text = text[:4000]
	}
	s.Requests = append(s.Requests, text)
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
