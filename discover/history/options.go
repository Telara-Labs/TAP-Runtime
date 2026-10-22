package history

import (
	"bytes"
	"encoding/json"

	"github.com/Telara-Labs/TAP-Runtime/discover/redact"
	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

// ReadOptions asks a reader for more of a session than discover uses, for a
// caller that keeps a record of the conversation itself.
//
// A read made with options, even the zero value, differs from a plain read
// in two more ways. Every text it keeps (requests, the model's prose, tool
// arguments, commands, results, settings) is redacted with discover/redact
// before anything bounds it, so a credential cut at a size limit cannot
// leave half of itself unrecognised. And it never uses the parse cache, so
// nothing it reads is written to disk.
//
// Only the Claude Code and Codex readers take options so far.
type ReadOptions struct {
	// Conversation keeps the model's prose (Session.Assistant) and the
	// session's settings as they change (Session.Meta).
	Conversation bool
	// KeepEmpty keeps sessions that made no tool call: a question and its
	// answer. Discover leaves them out, as they hold no procedure.
	KeepEmpty bool
	// Full keeps each text whole where a session otherwise bounds it:
	// requests (Session.FullRequests), tool arguments (Call.FullArgs) and
	// results (Call.FullOutput). They are redacted like everything else.
	Full bool
}

// redactArgs is, for a read with options, each argument redacted whole
// (redact.Argument, by its name and its structure) and keeping its JSON
// type, before anything flattens or bounds it. A plain read keeps in as it
// is.
func redactArgs(in map[string]json.RawMessage, o *ReadOptions) map[string]json.RawMessage {
	if o == nil || in == nil {
		return in
	}
	out := make(map[string]json.RawMessage, len(in))
	for k, v := range in {
		s := RawString(v)
		r := redact.Argument(k, s)
		if r == s {
			out[k] = v
			continue
		}
		if t := bytes.TrimSpace(v); len(t) > 0 && t[0] != '"' && json.Valid([]byte(r)) {
			out[k] = json.RawMessage(r)
			continue
		}
		out[k], _ = json.Marshal(r)
	}
	return out
}

// keepFullArgs sets c.FullArgs, for a read with ReadOptions.Full, from the
// already redacted arguments in: every value whole.
func keepFullArgs(c *trace.Call, in map[string]json.RawMessage, o *ReadOptions) {
	if o == nil || !o.Full || c.Args == nil {
		return
	}
	c.FullArgs = make(map[string]string, len(in))
	for k, v := range in {
		c.FullArgs[k] = RawString(v)
	}
}

// keepSession reports whether a read keeps a session: one that made a
// call, or with KeepEmpty one where anything was said.
func keepSession(s trace.Session, o *ReadOptions) bool {
	if len(s.Calls) > 0 {
		return true
	}
	if o == nil || !o.KeepEmpty {
		return false
	}
	for _, r := range s.Requests {
		if r != "" {
			return true
		}
	}
	return len(s.Assistant) > 0
}

// redactText is what a read with options keeps of a text: the text with
// every credential replaced.
func redactText(s string) string {
	if s == "" {
		return s
	}
	return redact.Redact(s)
}
