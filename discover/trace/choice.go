package trace

import (
	"fmt"
	"strings"

	"github.com/Telara-Labs/TAP-Runtime/discover/shellparse"
)

// ChoiceStats counts the plain-word values one argument took across a corpus.
type ChoiceStats struct {
	Values map[string]int
	Seen   int
	// Supplied counts observations whose value the request text or an
	// earlier result of the same request gave.
	Supplied int
}

// Choice reports whether an argument's plain-word values choose the
// operation (they are part of what the step does) rather than carry data
// (they become a caller's input). It is decided from the corpus alone,
// never from the argument's or the tool's name:
//   - a small set of values, each reused more than twice on average, is an
//     enumerated choice;
//   - values the caller or an earlier result always supplied are data;
//   - a new value nearly every time is data;
//   - otherwise the evidence is too thin, and the conservative answer is a
//     choice: different values stay different procedures.
func (cs *ChoiceStats) Choice() bool {
	if cs == nil || cs.Seen == 0 {
		return true
	}
	d := len(cs.Values)
	reused := false
	for _, n := range cs.Values {
		if n >= 2 {
			reused = true
		}
	}
	switch {
	case reused && 2*d < cs.Seen:
		return true
	case cs.Supplied == cs.Seen:
		return false
	case d >= 3 && 2*d > cs.Seen:
		return false
	}
	return true
}

// Choices is corpus evidence about which arguments choose an operation.
// A nil *Choices has no evidence: every plain-word argument is a choice.
type Choices struct {
	args map[string]*ChoiceStats
	// shellWords holds, per program and first word, the sessions that
	// used it: the evidence Normalize uses to make a word a subcommand.
	shellWords map[string]map[string]bool
}

// PlainChoiceValue reports a value that can be an enumerated choice: a short
// plain word (letters, digits and _ . : -), by its type.
func PlainChoiceValue(v string) bool {
	if v == "" || len(v) > 40 || TypeOf(shellparse.Word{Text: v}) != SlotWord {
		return false
	}
	for _, r := range v {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_.:-", r)) {
			return false
		}
	}
	return true
}

// NewChoices observes every recorded argument of every call.
func NewChoices(ss []Session) *Choices {
	ch := &Choices{args: map[string]*ChoiceStats{}, shellWords: map[string]map[string]bool{}}
	for _, s := range ss {
		for i, c := range s.Calls {
			text := ""
			if c.Request >= 0 && c.Request < len(s.Requests) {
				text = s.Requests[c.Request]
			}
			for path, f := range ObservedArgs(c) {
				id, ok := ArgIdentity(c, path)
				if !ok || !PlainChoiceValue(f.Value) {
					continue
				}
				if c.Tool == "shell" {
					k := id + "=" + f.Value
					if ch.shellWords[k] == nil {
						ch.shellWords[k] = map[string]bool{}
					}
					ch.shellWords[k][s.Client+"\x00"+s.ID] = true
					continue
				}
				supplied := InRequest(f.Value, text)
				for j := i - 1; j >= 0 && !supplied; j-- {
					prev := s.Calls[j]
					if prev.Request != c.Request {
						break
					}
					supplied = len(f.Value) >= 4 && strings.Contains(prev.Output, f.Value)
				}
				ch.Observe(id, f.Value, supplied)
			}
		}
	}
	return ch
}

// Observe records one value of the argument named id.
func (ch *Choices) Observe(id, value string, supplied bool) {
	st := ch.args[id]
	if st == nil {
		st = &ChoiceStats{Values: map[string]int{}}
		ch.args[id] = st
	}
	st.Seen++
	st.Values[value]++
	if supplied {
		st.Supplied++
	}
}

// Choice reports whether the argument named id is a choice.
func (ch *Choices) Choice(id string) bool {
	if ch == nil {
		return true
	}
	return ch.args[id].Choice()
}

// ArgIdentity names an argument across calls: an MCP tool and its argument
// path, or a shell program and the position of a word after it. A shell
// word is named only in the first position, where a subcommand would be.
func ArgIdentity(c Call, path string) (string, bool) {
	if c.Tool == "shell" {
		commands, err := shellparse.ProgramShellCommands(c.Command)
		if err != nil {
			return "", false
		}
		stage, i := ShellArgPosition(path, len(commands))
		if stage < 0 || stage >= len(commands) || i != 0 || len(commands[stage]) < 2 {
			return "", false
		}
		return "sh:" + commands[stage][0] + "|argv_0", true
	}
	if !strings.HasPrefix(c.Tool, "mcp:") {
		return "", false
	}
	return c.Tool + "|" + path, true
}

// ShellArgPosition parses an ObservedArgs shell path into its pipeline stage
// and argument index, or (-1, -1).
func ShellArgPosition(path string, stages int) (int, int) {
	stage, i := 0, -1
	if stages > 1 {
		if _, err := fmt.Sscanf(path, "pipe_%d_argv_%d", &stage, &i); err != nil || fmt.Sprintf("pipe_%d_argv_%d", stage, i) != path {
			return -1, -1
		}
		return stage, i
	}
	if _, err := fmt.Sscanf(path, "argv_%d", &i); err != nil || fmt.Sprintf("argv_%d", i) != path {
		return -1, -1
	}
	return 0, i
}

// Selector reports an argument that chooses the call's operation: a shell
// flag that takes no value, or a plain-word value the corpus evidence calls
// a choice (see ChoiceStats.Choice).
func (ch *Choices) Selector(c Call, path string) bool {
	if c.Tool == "shell" {
		commands, err := shellparse.ProgramShellCommands(c.Command)
		if err != nil {
			return false
		}
		stage, i := ShellArgPosition(path, len(commands))
		if stage < 0 || stage >= len(commands) || i < 0 || i+1 >= len(commands[stage]) {
			return false
		}
		value := commands[stage][i+1]
		if strings.HasPrefix(value, "-") && !strings.Contains(value, "=") {
			return true
		}
		// A first word is a subcommand when that program used it in two
		// or more sessions (as Normalize decides labels); a word used once
		// is data, such as a search pattern.
		id, ok := ArgIdentity(c, path)
		if !ok || !PlainChoiceValue(value) {
			return false
		}
		return ch == nil || len(ch.shellWords[id+"="+value]) >= 2
	}
	field, ok := ObservedArgs(c)[path]
	if !ok || field.TypeName != "string" || !PlainChoiceValue(field.Value) {
		return false
	}
	id, ok := ArgIdentity(c, path)
	return ok && ch.Choice(id)
}
