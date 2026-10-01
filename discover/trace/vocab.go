package trace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"gitlab.com/telara-labs/tap-runtime/discover/shellparse"
)

// SelectorSlot reports an argument whose value is a short plain word: an
// enumerated choice such as an operation, state or context name, rather than
// an identifier, path, URL, number or free text. It is decided by the value's
// type, never by the argument's name or the tool's name.
func SelectorSlot(sl Slot) bool {
	return !sl.Sub && sl.Type == SlotWord && sl.Value != "" && len(sl.Value) <= 40
}

// IsScopeSlot reports a flag or argument that selects where a call acts (a
// context, namespace or environment name): a selector by its value type.
func IsScopeSlot(st Step, sl Slot) bool {
	return SelectorSlot(sl)
}

// Effect evidence.
var (
	ReadPrograms = map[string]bool{"cat": true, "head": true, "tail": true, "grep": true, "rg": true, "find": true, "ls": true, "wc": true, "awk": true, "jq": true, "sort": true, "uniq": true,
		"diff": true, "stat": true, "file": true, "du": true, "df": true, "date": true, "pwd": true, "shasum": true, "sha256sum": true, "md5": true, "md5sum": true, "tree": true, "which": true,
		"nl": true, "cut": true, "tr": true, "column": true, "less": true, "realpath": true, "basename": true, "dirname": true, "[": true, "test": true}
	WritePrograms = map[string]bool{"rm": true, "mv": true, "cp": true, "mkdir": true, "touch": true, "tee": true, "chmod": true, "chown": true, "ln": true, "rsync": true, "scp": true}
)

// StepEffect is what one recorded step does by declared evidence: "read",
// "write" or "unknown".
func StepEffect(st Step) string {
	switch {
	case ReadTools[st.Label] || FetchTools[st.Label]:
		return "read"
	case EditTools[st.Label] || strings.HasPrefix(st.Label, "patch:"):
		return "write"
	case strings.HasPrefix(st.Label, "sh:"):
		if st.Compound && HasFileRedirect(st.Raw) {
			return "write"
		}
		f := strings.Fields(strings.TrimPrefix(st.Label, "sh:"))
		prog := f[0]
		if prog == "sed" {
			for _, sl := range st.Slots {
				if strings.HasPrefix(sl.Value, "-i") {
					return "write"
				}
			}
			return "read"
		}
		if prog == "curl" {
			for _, sl := range st.Slots {
				v := strings.ToUpper(sl.Value)
				k := strings.SplitN(sl.Key, "#", 2)[0]
				if (k == "-X=" || k == "--request=") && v != "GET" && v != "HEAD" {
					return "write"
				}
				if k == "-d" || k == "--data" || k == "-d=" || k == "--data=" || k == "-F=" || k == "--form=" || k == "--data-raw=" || k == "--data-binary=" {
					return "write"
				}
			}
			return "read"
		}
		// Only single-purpose programs, whose effect never depends on their
		// arguments, are classified. A program with subcommands (git,
		// kubectl, ...) is unknown: its effect is not inferred from a table.
		if len(f) == 1 && WritePrograms[prog] {
			return "write"
		}
		if len(f) == 1 && ReadPrograms[prog] {
			return "read"
		}
		return "unknown"
	case strings.HasPrefix(st.Label, "mcp:"):
		// An MCP tool's effect is whatever the tool declares; a session log
		// does not record that, and a verb in its name is not evidence.
		return "unknown"
	}
	return "unknown"
}

var FileRedirectRe = regexp.MustCompile(`(^|[^0-9&<>])>>?\s*([^\s&|;]+)`)

// HasFileRedirect reports a > or >> redirect to something other than
// /dev/null or a file descriptor.
func HasFileRedirect(raw string) bool {
	for _, m := range FileRedirectRe.FindAllStringSubmatch(raw, -1) {
		if m[2] != "/dev/null" && !strings.HasPrefix(m[2], "&") {
			return true
		}
	}
	return false
}

// DropCopiedCalls removes calls a session file copied from another: the same
// client call ID read twice. The first file read keeps it.
func DropCopiedCalls(ss []Session) {
	seen := map[string]bool{}
	for i := range ss {
		kept := ss[i].Calls[:0]
		for _, c := range ss[i].Calls {
			if c.ID != "" {
				k := ss[i].Client + "\x00" + c.ID
				if seen[k] {
					continue
				}
				seen[k] = true
			}
			kept = append(kept, c)
		}
		ss[i].Calls = kept
	}
}

// InResult reports whether a value appears in a step's recorded result.
func InResult(v string, st Step) bool {
	if len(v) < 4 || strings.ContainsAny(v, " \n") {
		return false
	}
	for _, id := range st.OutIDs {
		if id == v {
			return true
		}
	}
	for _, t := range st.OutTokens {
		if t == v {
			return true
		}
	}
	return strings.Contains(st.Output, v)
}

// Derived reports a slot computed from another (a URL's host, a path's base
// name). Derived slots help spot what is fixed; they are never arguments.
func Derived(key string) bool {
	return strings.Contains(key, ".") && !strings.HasPrefix(key, "-")
}

// Agent-builtin tools have no TAP equivalent to bind. Reading a file and
// fetching a page have one-line replays; editing is judgement (a human
// step); the rest is the agent's own bookkeeping.
// Replayable reports whether a primitive can run a step with this label
// itself: a host command, an MCP tool, a browser call, a file read or a page
// fetch. Edits decided per run and the agent's bookkeeping cannot.
func Replayable(label string) bool {
	for _, p := range []string{"sh:", "mcp:", "js:"} {
		if strings.HasPrefix(label, p) {
			return true
		}
	}
	return ReadTools[label] || FetchTools[label]
}

var (
	ReadTools  = map[string]bool{"Read": true, "read_file": true, "read_file_v2": true}
	EditTools  = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "edit_file": true, "edit_file_v2": true, "search_replace": true, "apply_patch": true}
	FetchTools = map[string]bool{"WebFetch": true}
)

func OneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return TruncateUTF8(s, n) + "…"
	}
	return s
}

// SearchPrograms read what the agent chose to look for: an unexplained
// pattern or target on them is a choice made during the run.
var SearchPrograms = map[string]bool{"grep": true, "rg": true, "find": true, "ag": true}

// EpisodeID is the opaque identifier of a session's request.
func EpisodeID(client, session string, request int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d", client, session, request)))
	return "ep_" + hex.EncodeToString(h[:6])
}

// MatchAt returns the first gapped occurrence of items in seq (positions), or nil.
func MatchAt(seq, items []int, window int) []int {
	for start := range seq {
		if seq[start] != items[0] {
			continue
		}
		if idx := MatchFrom(seq, items, window, start); idx != nil {
			return idx
		}
	}
	return nil
}

func MatchFrom(seq, items []int, window, start int) []int {
	idx := []int{start}
	if len(items) == 1 {
		return idx
	}
	for j := start + 1; j < len(seq) && j <= start+window; j++ {
		if seq[j] == items[1] {
			if rest := MatchFrom(seq, items[1:], window, j); rest != nil {
				return append(idx, rest...)
			}
		}
	}
	return nil
}

type ObservedField struct {
	Path       []string
	Value      string
	TypeName   string
	JsonString bool
}

func ObservedArgs(c Call) map[string]ObservedField {
	out := map[string]ObservedField{}
	if c.Tool == "shell" {
		commands, err := shellparse.ProgramShellCommands(c.Command)
		if err != nil {
			return out
		}
		for stage, words := range commands {
			for i, value := range words[1:] {
				key := fmt.Sprintf("argv_%d", i)
				if len(commands) > 1 {
					key = fmt.Sprintf("pipe_%d_argv_%d", stage, i)
				}
				out[key] = ObservedField{Path: []string{key}, Value: value, TypeName: "string"}
			}
		}
		return out
	}
	var add func(path []string, v any, jsonString bool)
	add = func(path []string, v any, jsonString bool) {
		if m, ok := v.(map[string]any); ok {
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				add(append(append([]string(nil), path...), k), m[k], jsonString)
			}
			return
		}
		value := fmt.Sprint(v)
		typ := "string"
		switch number := v.(type) {
		case bool:
			typ = "boolean"
		case json.Number:
			if strings.ContainsAny(number.String(), ".eE") {
				typ = "number"
			} else {
				typ = "integer"
			}
		case float64:
			typ = "number"
		case []any:
			typ = "array"
		}
		out[strings.Join(path, "/")] = ObservedField{Path: path, Value: value, TypeName: typ, JsonString: jsonString}
	}
	for key, raw := range c.Args {
		var v any = raw
		jsonString := false
		if strings.HasPrefix(strings.TrimSpace(raw), "{") {
			var obj map[string]any
			dec := json.NewDecoder(strings.NewReader(raw))
			dec.UseNumber()
			if json.Valid([]byte(raw)) && dec.Decode(&obj) == nil {
				v, jsonString = obj, !c.RawArgs[key]
			}
		} else if c.RawArgs[key] && json.Valid([]byte(raw)) {
			dec := json.NewDecoder(strings.NewReader(raw))
			dec.UseNumber()
			_ = dec.Decode(&v)
		}
		add([]string{key}, v, jsonString)
	}
	return out
}

// unexplained names the first input whose values were mostly not in the
// request, or "".
// InRequest reports whether a value was given in the request: the value
// itself, or for a path its base name, or for a URL its path, appears in the
// text (case-insensitive).
func InRequest(v, text string) bool {
	if text == "" {
		return false
	}
	t := strings.ToLower(text)
	lv := strings.ToLower(strings.TrimSpace(v))
	if lv == "" {
		return false
	}
	if strings.Contains(t, lv) {
		return true
	}
	if b := path.Base(lv); len(b) >= 3 && b != "." && strings.Contains(t, b) {
		return true
	}
	return false
}

var Digits = regexp.MustCompile(`\d+`)

// TextKey is a request's text with digits and spacing normalized.
func TextKey(t string) string {
	t = strings.Join(strings.Fields(strings.ToLower(Digits.ReplaceAllString(t, "#"))), " ")
	if len(t) < 8 {
		return ""
	}
	return t
}
