package trace

import (
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/discover/shellparse"
)

// Slot types. A primitive takes the varying slots as typed inputs; the
// literal skeleton (program, subcommand, flags, argument keys) is its body.
const (
	SlotFlag   = "flag"
	SlotURL    = "url"
	SlotText   = "text"
	SlotNumber = "number"
	SlotID     = "id"
	SlotPath   = "path"
	SlotWord   = "word"
	// SlotSecret is a credential: never written, supplied by the caller.
	SlotSecret = "secret"
)

// Slot is one argument of a step.
type Slot struct {
	Key   string `json:"key"`
	Type  string `json:"type"`
	Value string `json:"value"`
	// Sub marks the shell subcommand (already in the step's label). It is
	// kept as a slot only so the command can be rebuilt in its original
	// order (git -C dir status); templates and parameters skip it.
	Sub bool `json:"-"`
	// Raw marks a tool argument whose recorded value was JSON other than a
	// string (a number, a boolean, an object).
	Raw bool `json:"-"`
}

// Step is one unit of work: a simple shell command or one tool call.
type Step struct {
	// Label is what the miner compares: "sh:git commit", "mcp:telara_task_list", "Read".
	Label string
	// Skeleton adds the flag names (shell) or argument keys (tools) to the label.
	Skeleton string
	Slots    []Slot
	Time     time.Time
	// Tokens is this step's share of the model turn that issued it; Turn
	// names that turn; Measured is false when the client records no usage.
	Tokens   Usage
	Turn     int
	Measured bool
	// Turns counts the model turns this step spans: more than one when
	// repeats of the same call were merged into it.
	Turns int
	// Request is the user message this step answered (normSession.Requests).
	Request int
	// Call is the index of the call this step came from in its session. A
	// shell call can hold several steps (a pipeline, a && chain).
	Call int
	// Raw is the whole recorded command line of a shell call, and Compound
	// says it was more than one plain command: a pipeline, a chain, a
	// redirect, a heredoc, a command substitution or a cd first. Such a call
	// is replayed as its recorded line, never as separate commands.
	Raw      string
	Compound bool
	// Outcome and OutIDs come from the call's recorded result.
	Outcome Outcome
	OutIDs  []string
	OutCtx  []string
	// OutPaths are the ids' JSON paths (see Call.OutPaths).
	OutPaths []string
	// Output is the start of the call's result.
	Output string
	// OutTokens are the result's name-like words (see Call.OutTokens).
	OutTokens []string
	// Session is the client and session the step was recorded in.
	Session string
}

type NormSession struct {
	Client string
	ID     string
	Start  time.Time
	Steps  []Step
	// Skills are the known skills this session loaded; they are ground truth
	// for the recall check and are not themselves steps.
	Skills map[string]bool
	// Requests are the user's messages; RequestSkills the skills each loaded.
	Requests      []string
	RequestSkills map[int]map[string]bool
	Approvals     []Approval
}

var (
	NumericFlag = regexp.MustCompile(`^-\d+$`)
	NumberRe    = regexp.MustCompile(`^[0-9]+([.:][0-9]+)*[a-zA-Z]{0,2}$`)
	IdRe        = regexp.MustCompile(`^([0-9a-fA-F]{7,}|[0-9a-fA-F-]{32,36}|[A-Z][A-Z0-9]+-[0-9]+|.*[0-9].*[a-zA-Z].*[0-9].*)$`)
	ExtRe       = regexp.MustCompile(`\.[A-Za-z0-9]{1,6}$`)
	SkillRe     = regexp.MustCompile(`([A-Za-z0-9_.:-]+)/SKILL\.md`)
)

func TypeOf(w shellparse.Word) string {
	t := w.Text
	switch {
	case !w.Quoted && NumericFlag.MatchString(t):
		// head -10, tail -15: a count, not an option.
		return SlotNumber
	case !w.Quoted && len(t) > 1 && strings.HasPrefix(t, "-"):
		return SlotFlag
	case w.Quoted && strings.ContainsAny(t, " \n\t"):
		// A quoted sentence that mentions a URL is still a sentence.
		return SlotText
	case strings.Contains(t, "://") || strings.HasPrefix(t, "data:"):
		return SlotURL
	case NumberRe.MatchString(t):
		return SlotNumber
	case strings.ContainsAny(t, "/~") || strings.HasPrefix(t, ".") || (ExtRe.MatchString(t) && !strings.Contains(t, " ")):
		return SlotPath
	case IdRe.MatchString(t):
		return SlotID
	case w.Quoted || strings.ContainsAny(t, " \n\t$*?[]"):
		return SlotText
	}
	return SlotWord
}

// SubcommandOf is the first bare word after the program that does not follow
// a flag (which may be that flag's value): "git -C x status" -> "status",
// "kubectl --context k get" -> "get".
func SubcommandOf(ws []shellparse.Word) string {
	for i := 1; i < len(ws); i++ {
		tp := TypeOf(ws[i])
		if tp == SlotFlag {
			if !strings.Contains(ws[i].Text, "=") {
				i++
			}
			continue
		}
		if tp == SlotWord {
			return ws[i].Text
		}
		return ""
	}
	return ""
}

// Normalize turns raw sessions into step sequences. A shell subcommand
// becomes part of the label only when that program+word pair occurs in at
// least two sessions: recurring words are structure, one-off words are data.
func Normalize(sessions []Session) []NormSession {
	pairSessions := map[string]map[string]bool{}
	for _, s := range sessions {
		for _, c := range s.Calls {
			if c.Tool != "shell" {
				continue
			}
			for _, ws := range shellparse.SimpleCommands(c.Command) {
				if sub := SubcommandOf(ws); sub != "" {
					k := ws[0].Text + " " + sub
					if pairSessions[k] == nil {
						pairSessions[k] = map[string]bool{}
					}
					pairSessions[k][s.Client+"/"+s.ID] = true
				}
			}
		}
	}
	out := make([]NormSession, 0, len(sessions))
	for _, s := range sessions {
		ns := NormSession{Client: s.Client, ID: s.ID, Start: s.Start, Skills: map[string]bool{}, Requests: s.Requests, RequestSkills: map[int]map[string]bool{}, Approvals: s.Approvals}
		for ci, c := range s.Calls {
			if sk := SkillOf(c); sk != "" {
				ns.Skills[sk] = true
				if ns.RequestSkills[c.Request] == nil {
					ns.RequestSkills[c.Request] = map[string]bool{}
				}
				ns.RequestSkills[c.Request][sk] = true
				continue
			}
			steps := StepsOf(c, pairSessions)
			for i := range steps {
				// A call opened into several steps shares its turn among them.
				steps[i].Tokens = c.Tokens.Scale(1 / float64(len(steps)))
				steps[i].Turn, steps[i].Measured, steps[i].Turns = c.Turn, c.Measured, 1
				steps[i].Request, steps[i].Call = c.Request, ci
				if c.Tool == "shell" {
					steps[i].Raw, steps[i].Compound = c.Command, shellparse.IsCompound(c.Command)
				}
				steps[i].Outcome, steps[i].OutIDs, steps[i].OutCtx, steps[i].OutPaths = c.Outcome, c.OutIDs, c.OutCtx, c.OutPaths
				steps[i].Output, steps[i].OutTokens = c.Output, c.OutTokens
				steps[i].Session = s.Client + "/" + s.ID
			}
			for _, st := range steps {
				if n := len(ns.Steps); n > 0 && ns.Steps[n-1].Label == st.Label && ns.Steps[n-1].Request == st.Request && SameArgs(ns.Steps[n-1], st) {
					// A retry (the same call with the same arguments) counts
					// once, and so does what it cost. Two calls with the same
					// label but different arguments are two steps.
					ns.Steps[n-1].Tokens = ns.Steps[n-1].Tokens.Add(st.Tokens)
					// The retry's result is what the step finally did.
					ns.Steps[n-1].Outcome, ns.Steps[n-1].OutIDs, ns.Steps[n-1].OutCtx, ns.Steps[n-1].OutPaths, ns.Steps[n-1].Output, ns.Steps[n-1].OutTokens = st.Outcome, st.OutIDs, st.OutCtx, st.OutPaths, st.Output, st.OutTokens
					if st.Turn != ns.Steps[n-1].Turn {
						ns.Steps[n-1].Turns++
					}
					continue
				}
				ns.Steps = append(ns.Steps, st)
			}
		}
		if len(ns.Steps) > 0 {
			out = append(out, ns)
		}
	}
	return out
}

// SkillOf names the skill a call loads: Claude Code's Skill tool, or any call
// that reads a <name>/SKILL.md file (how Codex and Cursor load skills).
func SkillOf(c Call) string {
	if c.Tool == "Skill" {
		return c.Args["skill"]
	}
	// Only a command or a short argument (a path, not a document or script
	// that merely mentions a SKILL.md) counts as loading a skill.
	text := c.Command
	for _, v := range c.Args {
		if len(v) <= 300 {
			text += " " + v
		}
	}
	if m := SkillRe.FindStringSubmatch(text); m != nil {
		return path.Base(m[1])
	}
	return ""
}

func StepsOf(c Call, pairSessions map[string]map[string]bool) []Step {
	if c.Tool == "mcp:js" || c.Tool == "js" {
		if steps := JsSteps(c.Args["code"]); len(steps) > 0 {
			return Stamp(steps, c)
		}
	}
	if in := c.Args["input"]; strings.Contains(in, "*** Begin Patch") {
		if steps := PatchSteps(in); len(steps) > 0 {
			return Stamp(steps, c)
		}
	}
	if c.Tool != "shell" {
		keys := make([]string, 0, len(c.Args))
		for k := range c.Args {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		st := Step{Label: c.Tool, Skeleton: c.Tool + "{" + strings.Join(keys, ",") + "}", Time: c.Time}
		for _, k := range keys {
			ss := ArgSlots(k, c.Args[k])
			ss[0].Raw = c.RawArgs[k]
			st.Slots = append(st.Slots, ss...)
		}
		return []Step{st}
	}
	var out []Step
	for _, ws := range shellparse.SimpleCommands(c.Command) {
		label := "sh:" + ws[0].Text
		sub := SubcommandOf(ws)
		if sub != "" && len(pairSessions[ws[0].Text+" "+sub]) >= 2 {
			label += " " + sub
		} else {
			sub = ""
		}
		// Arguments are keyed by what they are, not where they sit, so
		// "go test -count=1 -run X" and "go test -run X -count=1" line up: a
		// flag by its name, the word after a flag as that flag's value
		// ("-run="), anything else by its order among positionals (p0, p1).
		var flags []string
		st := Step{Label: label, Time: c.Time}
		used := map[string]int{}
		keyOf := func(k string) string {
			used[k]++
			if used[k] > 1 {
				return k + "#" + strconv.Itoa(used[k])
			}
			return k
		}
		positional, pendingFlag := 0, ""
		for _, w := range ws[1:] {
			if sub != "" && w.Text == sub && !w.Quoted {
				st.Slots = append(st.Slots, Slot{Key: "sub", Type: SlotWord, Value: w.Text, Sub: true})
				sub, pendingFlag = "", ""
				continue
			}
			tp := TypeOf(w)
			var key string
			switch {
			case tp == SlotFlag:
				name := w.Text
				if i := strings.Index(name, "="); i > 0 {
					name = name[:i]
					pendingFlag = ""
				} else {
					pendingFlag = name
				}
				flags = append(flags, name)
				key = name
			case pendingFlag != "":
				key, pendingFlag = pendingFlag+"=", ""
			default:
				key = "p" + strconv.Itoa(positional)
				positional++
			}
			st.Slots = append(st.Slots, Slot{Key: keyOf(key), Type: tp, Value: w.Text})
		}
		sort.Strings(flags)
		st.Skeleton = label + " " + strings.Join(flags, " ")
		out = append(out, st)
	}
	return out
}

// Stamp gives steps opened up from inside one call that call's time.
func Stamp(steps []Step, c Call) []Step {
	for i := range steps {
		steps[i].Time = c.Time
	}
	return steps
}

// SameArgs reports two steps with the same arguments: a retry.
func SameArgs(a, b Step) bool {
	if a.Skeleton != b.Skeleton || len(a.Slots) != len(b.Slots) {
		return false
	}
	for i := range a.Slots {
		if a.Slots[i].Key != b.Slots[i].Key || a.Slots[i].Value != b.Slots[i].Value {
			return false
		}
	}
	return true
}
