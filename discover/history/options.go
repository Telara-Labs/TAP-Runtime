package history

import "github.com/Telara-Labs/TAP-Runtime/discover/redact"

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
}

// redactText is what a read with options keeps of a text: the text with
// every credential replaced.
func redactText(s string) string {
	if s == "" {
		return s
	}
	return redact.Redact(s)
}
