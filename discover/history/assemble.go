package history

import (
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

func (UserText) event()   {}
func (ToolCall) event()   {}
func (ToolResult) event() {}
func (TurnUsage) event()  {}

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

// Add applies one event.
func (a *Assembler) Add(e Event) {
	switch e := e.(type) {
	case UserText:
		if !trace.IsRequest(e.Text) {
			return
		}
		role := e.Role
		if role == "" {
			role = "user"
		}
		a.S.AddRequestWithRole(e.Text, role)
	case ToolCall:
		c := e.Call
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
		RecordResult(c, e.Text)
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
	return a.S
}

// RecordResult fills a call's result fields from the result text: outcome,
// the identifiers, paths and collections it returned, a preview, and its
// size.
func RecordResult(c *trace.Call, text string) {
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
