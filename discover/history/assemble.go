package history

import (
	"strings"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

// A reader is a decoder plus this assembler. The decoder knows
// one agent's format: where its sessions are, how a record is laid out, how
// it names MCP tools. It turns records into the events below, in the order
// they happened. The assembler does what every reader used to repeat: it
// attributes calls to the request they serve, pairs results with their calls,
// fills the result fields, and spreads each model turn's token usage over the
// calls it made.
type Event interface{ event() }

// UserText is a message the person typed (or the agent injected, with Role
// "synthetic_context" or "scheduled"). Text that is not a request (empty,
// harness noise) is ignored. Role "" means "user".
type UserText struct {
	Text, Role string
}

// ToolCall is one call. Call holds the decoded tool, arguments and time;
// Client, Session and Request are filled here. Key pairs it with its result
// (several calls may share one key: a script that made several calls). Turn,
// when set, names the model response that made it, for TurnUsage.
type ToolCall struct {
	Key, Turn string
	Call      trace.Call
}

// ToolResult is the result of the calls under Key: the Nth of them, or the
// latest when Nth is -1. A failed result is IsError, or text that reads as a
// failure (trace.ResultOutcome). A store that records the outcome itself sets
// HasOutcome and Outcome, which are then used as given.
type ToolResult struct {
	// Line and Record place the result in its store (trace.CallSource).
	Line   int
	Record string

	Key        string
	Nth        int
	Text       string
	IsError    bool
	HasOutcome bool
	Outcome    trace.Outcome
}

// TurnUsage is one model response's token usage. With a Turn, it belongs to
// the calls whose ToolCall named that turn; the first TurnUsage of a turn
// fixes its place in the order (later ones for the same turn are ignored, as
// an agent repeats a response's usage on each of its records). With no Turn,
// it belongs to every call since the previous unnamed TurnUsage.
type TurnUsage struct {
	Turn  string
	Usage trace.Usage
}

// AssistantText is prose the model wrote to the person; Turn, when set,
// names the model response it belongs to. Only an assembler made with
// ReadOptions.Conversation keeps it (Session.Assistant).
type AssistantText struct {
	Text, Turn string
}

// SessionMeta is the session's settings as one record states them. An empty
// field is one the record does not state, not one it cleared. Only an
// assembler made with ReadOptions.Conversation keeps it (Session.Meta), and
// only when something changed.
type SessionMeta struct {
	Cwd, Branch, Version, Model string
}

func (UserText) event()      {}
func (ToolCall) event()      {}
func (ToolResult) event()    {}
func (TurnUsage) event()     {}
func (AssistantText) event() {}
func (SessionMeta) event()   {}

// Assembler builds one session from events. S is the session so far; a
// decoder may read it (for example to see how many requests there are) and
// set fields the events do not carry (Start, SourceDigest).
type Assembler struct {
	S trace.Session

	skipped int
	byKey   map[string][]int
	turns   map[string]*namedTurn
	order   []string
	spread  struct{ from, turn int }
	// opts is set for a read with options (NewAssemblerWith): texts are
	// redacted before they are bounded, and conversation events are kept.
	opts *ReadOptions
}

type namedTurn struct {
	usage trace.Usage
	set   bool // a TurnUsage was seen; later ones are repeats
	calls []int
}

// NewAssembler starts a session of client with id.
func NewAssembler(client, id string) *Assembler {
	return &Assembler{S: trace.Session{Client: client, ID: id}, byKey: map[string][]int{}, turns: map[string]*namedTurn{}}
}

// NewAssemblerWith starts a session for a read with options: every text
// is redacted before it is bounded, and the options say what else is kept.
func NewAssemblerWith(client, id string, o ReadOptions) *Assembler {
	if !safeIdentifier(id) {
		id = ""
	}
	a := NewAssembler(client, id)
	a.opts = &o
	return a
}

// clean is the text an assembler keeps: redacted for a read with options,
// as given otherwise.
func (a *Assembler) clean(s string) string {
	if a.opts == nil {
		a.S.CacheUnsafe = a.S.CacheUnsafe || redactText(s) != s
		return s
	}
	return redactText(s)
}

func (a *Assembler) conversation() bool { return a.opts != nil && a.opts.Conversation }

func (a *Assembler) full() bool { return a.opts != nil && a.opts.Full }

// padFullRequests lines FullRequests up with the first n Requests: a
// request the session added itself (the empty one before any message) is
// whole as it is.
func (a *Assembler) padFullRequests(n int) {
	for len(a.S.FullRequests) < n {
		a.S.FullRequests = append(a.S.FullRequests, a.S.Requests[len(a.S.FullRequests)])
	}
}

// Add applies one event.
func (a *Assembler) Add(e Event) {
	if a.opts != nil {
		switch v := e.(type) {
		case ToolCall:
			if !safeIdentifier(v.Key) || !safeIdentifier(v.Turn) || !safeIdentifier(v.Call.ID) || !safeIdentifier(v.Call.Tool) || !safeIdentifier(v.Call.MCPServer) || !safeIdentifier(v.Call.MCPTool) {
				a.Skip()
				return
			}
		case ToolResult:
			if !safeIdentifier(v.Key) || !safeIdentifier(v.Record) {
				a.Skip()
				return
			}
		case TurnUsage:
			if !safeIdentifier(v.Turn) {
				a.Skip()
				return
			}
		case AssistantText:
			if !safeIdentifier(v.Turn) {
				a.Skip()
				return
			}
		}
	}
	switch e := e.(type) {
	case UserText:
		if !trace.IsRequest(e.Text) {
			return
		}
		role := e.Role
		if role == "" {
			role = "user"
		}
		text := a.clean(e.Text)
		n := len(a.S.Requests)
		a.S.AddRequestWithRole(text, role)
		if a.full() && len(a.S.Requests) > n {
			a.padFullRequests(len(a.S.Requests) - 1)
			a.S.FullRequests = append(a.S.FullRequests, trace.RequestText(text))
		}
	case AssistantText:
		if !a.conversation() || strings.TrimSpace(e.Text) == "" {
			return
		}
		a.S.Assistant = append(a.S.Assistant, trace.AssistantMessage{Request: len(a.S.Requests) - 1, AfterCall: len(a.S.Calls), Turn: e.Turn, Text: a.clean(e.Text)})
	case SessionMeta:
		if !a.conversation() {
			return
		}
		var cur trace.SessionMeta
		if n := len(a.S.Meta); n > 0 {
			cur = a.S.Meta[n-1]
		}
		next := cur
		for _, f := range []struct {
			to   *string
			from string
		}{{&next.Cwd, e.Cwd}, {&next.Branch, e.Branch}, {&next.Version, e.Version}, {&next.Model, e.Model}} {
			if f.from != "" {
				*f.to = a.clean(f.from)
			}
		}
		if next == cur {
			return // unchanged, or nothing stated before anything was
		}
		next.Request, next.AfterCall = len(a.S.Requests)-1, len(a.S.Calls)
		if n := len(a.S.Meta); n > 0 && cur.Request == next.Request && cur.AfterCall == next.AfterCall {
			a.S.Meta[n-1] = next // two records stating settings at one point
			return
		}
		a.S.Meta = append(a.S.Meta, next)
	case ToolCall:
		c := e.Call
		a.S.CacheUnsafe = a.S.CacheUnsafe || c.CacheUnsafe
		c.Client = a.S.Client
		if c.Session == "" {
			c.Session = a.S.ID
		}
		c.Request = a.S.Request()
		i := len(a.S.Calls)
		if e.Key != "" {
			a.byKey[e.Key] = append(a.byKey[e.Key], i)
		}
		if e.Turn != "" {
			t := a.turn(e.Turn)
			t.calls = append(t.calls, i)
		}
		a.S.Calls = append(a.S.Calls, c)
	case ToolResult:
		idx := a.byKey[e.Key]
		n := e.Nth
		if n < 0 {
			n = len(idx) - 1
		}
		if n < 0 || n >= len(idx) {
			return
		}
		c := &a.S.Calls[idx[n]]
		text := a.clean(e.Text)
		RecordResult(c, text)
		if a.full() {
			c.FullOutput = text
		}
		if e.Line > 0 {
			c.Src.ResultLine = e.Line
		}
		if e.Record != "" {
			c.Src.ResultRecord = e.Record
		}
		switch {
		case e.HasOutcome:
			c.Outcome = e.Outcome
		case e.IsError:
			c.Outcome = trace.OutcomeFailed
		}
	case TurnUsage:
		if e.Turn == "" {
			trace.Spread(a.S.Calls, a.spread.from, a.spread.turn, e.Usage)
			a.spread.turn++
			a.spread.from = len(a.S.Calls)
			return
		}
		if t := a.turn(e.Turn); !t.set {
			t.usage, t.set = e.Usage, true
		}
	}
}

func (a *Assembler) turn(id string) *namedTurn {
	t := a.turns[id]
	if t == nil {
		t = &namedTurn{}
		a.turns[id] = t
		a.order = append(a.order, id)
	}
	return t
}

// Skip counts one record the decoder could not parse.
func (a *Assembler) Skip() { a.skipped++ }

// CallsUnder is how many calls share key.
func (a *Assembler) CallsUnder(key string) int { return len(a.byKey[key]) }

// Finish spreads named turns' usage over their calls and returns the session.
// A turn is numbered by its place among all named turns.
func (a *Assembler) Finish() trace.Session {
	for ti, id := range a.order {
		t := a.turns[id]
		if t.usage.Total() == 0 || len(t.calls) == 0 {
			continue
		}
		for _, ci := range t.calls {
			a.S.Calls[ci].Tokens = t.usage.Scale(1 / float64(len(t.calls)))
			a.S.Calls[ci].Turn, a.S.Calls[ci].Measured = ti, true
		}
	}
	a.S.Skipped = a.skipped
	if a.full() {
		a.padFullRequests(len(a.S.Requests))
	}
	for i := range a.S.Assistant {
		m := &a.S.Assistant[i]
		m.Final = i == len(a.S.Assistant)-1 || a.S.Assistant[i+1].Request != m.Request
	}
	return a.S
}

// RecordResult fills a call's result fields from the result text: outcome,
// the identifiers, paths and collections it returned, a preview, and its
// size.
func RecordResult(c *trace.Call, text string) {
	c.CacheUnsafe = c.CacheUnsafe || redactText(text) != text
	c.Outcome = trace.ResultOutcome(text)
	c.OutIDs, c.OutCtx, c.OutPaths = trace.OutputRefsPaths(text)
	c.OutCollections = trace.ResultCollections(text)
	c.Output = trace.TruncateUTF8(text, 600)
	c.OutTokens = trace.OutputTokens(text)
}

// FirstTime keeps the earliest time seen as a session's start.
func FirstTime(s *trace.Session, t time.Time) {
	if s.Start.IsZero() && !t.IsZero() {
		s.Start = t
	}
}
