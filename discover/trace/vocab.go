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
	"unicode"

	"gitlab.com/telara-labs/tap-runtime/discover/shellparse"
)

// SelectorKeys are tool arguments whose value names the operation to run.
// A routine whose runs used different operations is not one procedure.
var SelectorKeys = map[string]bool{"action": true, "operation": true, "op": true, "method": true, "verb": true, "tool": true, "tool_name": true, "command": true}

// ScopeFlags and scopeArgs carry authority: which cluster, namespace,
// project or environment a call acts on.
var (
	ScopeFlags = map[string]bool{"--context=": true, "--kube-context=": true, "-n=": true, "--namespace=": true, "--project=": true, "--profile=": true, "--region=": true, "--cluster=": true, "--env=": true, "--environment=": true, "-C=": true}
	ScopeArgs  = map[string]bool{"integration": true, "project": true, "project_id": true, "project_key": true, "namespace": true, "context": true, "cluster": true, "environment": true, "env": true}
)

func IsScopeSlot(st Step, sl Slot) bool {
	if strings.HasPrefix(st.Label, "sh:") {
		return ScopeFlags[strings.SplitN(sl.Key, "#", 2)[0]]
	}
	return ScopeArgs[sl.Key]
}

// Effect evidence.
var (
	ReadPrograms = map[string]bool{"cat": true, "head": true, "tail": true, "grep": true, "rg": true, "find": true, "ls": true, "wc": true, "awk": true, "jq": true, "sort": true, "uniq": true,
		"diff": true, "stat": true, "file": true, "du": true, "df": true, "date": true, "pwd": true, "shasum": true, "sha256sum": true, "md5": true, "md5sum": true, "tree": true, "which": true,
		"nl": true, "cut": true, "tr": true, "column": true, "less": true, "realpath": true, "basename": true, "dirname": true, "[": true, "test": true}
	ReadSub = map[string]map[string]bool{
		"git":     {"status": true, "diff": true, "log": true, "show": true, "rev-parse": true, "ls-files": true, "blame": true, "describe": true, "shortlog": true, "grep": true, "cat-file": true, "ls-remote": true, "rev-list": true, "merge-base": true},
		"kubectl": {"get": true, "describe": true, "logs": true, "top": true, "explain": true, "version": true, "api-resources": true, "auth": true},
		"gh":      {"view": true, "list": true, "status": true, "diff": true, "checks": true},
		"go":      {"test": true, "vet": true, "build": true, "list": true, "version": true, "env": true},
		"helm":    {"list": true, "status": true, "get": true, "history": true, "template": true, "show": true},
		"docker":  {"ps": true, "images": true, "logs": true, "inspect": true},
		"npm":     {"test": true, "ls": true, "view": true},
	}
	WriteSub = map[string]map[string]bool{
		"git":     {"push": true, "commit": true, "tag": true, "merge": true, "rebase": true, "reset": true, "checkout": true, "add": true, "rm": true, "mv": true, "stash": true, "cherry-pick": true, "revert": true, "switch": true, "restore": true, "clean": true},
		"kubectl": {"apply": true, "create": true, "delete": true, "set": true, "patch": true, "scale": true, "rollout": true, "edit": true, "label": true, "annotate": true, "replace": true, "cordon": true, "drain": true},
		"helm":    {"install": true, "upgrade": true, "uninstall": true, "rollback": true},
		"docker":  {"push": true, "rm": true, "rmi": true, "run": true, "build": true},
		"npm":     {"publish": true, "install": true},
	}
	WritePrograms = map[string]bool{"rm": true, "mv": true, "cp": true, "mkdir": true, "touch": true, "tee": true, "chmod": true, "chown": true, "ln": true, "rsync": true, "scp": true}
	ToolWord      = regexp.MustCompile(`[a-z]+`)
	ReadVerbs     = map[string]bool{"get": true, "list": true, "search": true, "read": true, "describe": true, "fetch": true, "show": true, "view": true, "count": true, "query": true, "find": true, "lookup": true, "download": true, "status": true}
	WriteVerbs    = map[string]bool{"create": true, "update": true, "delete": true, "add": true, "send": true, "post": true, "set": true, "transition": true, "merge": true, "complete": true, "checkpoint": true,
		"write": true, "remove": true, "publish": true, "upload": true, "edit": true, "assign": true, "move": true, "archive": true, "close": true, "reply": true, "forward": true, "trash": true, "share": true,
		"comment": true, "approve": true, "trigger": true, "run": true, "execute": true, "retry": true, "cancel": true, "restart": true, "deploy": true, "push": true, "apply": true, "patch": true, "store": true, "save": true, "insert": true}
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
		sub := ""
		if len(f) > 1 {
			sub = f[1]
		}
		if prog == "git" && sub == "branch" {
			for _, sl := range st.Slots {
				if sl.Value == "-d" || sl.Value == "-D" || sl.Value == "--delete" || sl.Value == "-m" {
					return "write"
				}
			}
			return "read"
		}
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
		if WritePrograms[prog] || WriteSub[prog][sub] {
			return "write"
		}
		if ReadPrograms[prog] || ReadSub[prog][sub] {
			return "read"
		}
		// A subcommand the label does not carry (a one-off word): look at the
		// recorded words for a known subcommand.
		for _, sl := range st.Slots {
			if WriteSub[prog][sl.Value] {
				return "write"
			}
			if ReadSub[prog][sl.Value] && sl.Key == "p0" {
				return "read"
			}
		}
		return "unknown"
	case strings.HasPrefix(st.Label, "mcp:"):
		name := strings.TrimPrefix(st.Label, "mcp:")
		if strings.HasPrefix(name, "browser_") || name == "js" {
			return "unknown"
		}
		var action string
		for _, sl := range st.Slots {
			if SelectorKeys[sl.Key] {
				action = sl.Value
			}
		}
		if action != "" {
			if effect, found := OperationNameEffect(action); found {
				return effect
			}
			// The gateway's own name describes dispatch, not the selected
			// operation. An opaque action cannot inherit its wrapper's effect.
			return "unknown"
		}
		if effect, found := OperationNameEffect(name); found {
			return effect
		}
		return "unknown"
	}
	return "unknown"
}

// An operation's leading verb determines its effect. A later noun may also be
// a verb in another context (get_comment, list_updates), so scanning for any
// write word first mislabels reads. Explicit compound operation names with
// different effects remain unknown rather than guessed.
func OperationNameEffect(name string) (string, bool) {
	var separated strings.Builder
	runes := []rune(name)
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1]) ||
			unicode.IsUpper(runes[i-1]) && i+1 < len(runes) && unicode.IsLower(runes[i+1])) {
			separated.WriteByte('_')
		}
		separated.WriteRune(r)
	}
	words := ToolWord.FindAllString(strings.ToLower(separated.String()), -1)
	effect := ""
	connected := false
	for _, w := range words {
		if w == "and" || w == "or" || w == "then" {
			connected = true
			continue
		}
		current := ""
		if ReadVerbs[w] {
			current = "read"
		} else if WriteVerbs[w] {
			current = "write"
		}
		if current == "" {
			continue
		}
		if effect == "" {
			effect = current
		} else if connected && current != effect {
			return "unknown", true
		}
		connected = false
	}
	return effect, effect != ""
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

func OperationSelector(c Call, path string) bool {
	if c.Tool == "shell" {
		commands, err := shellparse.ProgramShellCommands(c.Command)
		if err != nil {
			return false
		}
		stage, i := 0, -1
		if len(commands) > 1 {
			if _, err := fmt.Sscanf(path, "pipe_%d_argv_%d", &stage, &i); err != nil || fmt.Sprintf("pipe_%d_argv_%d", stage, i) != path {
				return false
			}
		} else if _, err := fmt.Sscanf(path, "argv_%d", &i); err != nil || fmt.Sprintf("argv_%d", i) != path {
			return false
		}
		if stage < 0 || stage >= len(commands) || i < 0 || i+1 >= len(commands[stage]) {
			return false
		}
		words := commands[stage]
		value := words[i+1]
		if strings.HasPrefix(value, "-") && !strings.Contains(value, "=") {
			return true
		}
		if i != 0 || TypeOf(shellparse.Word{Text: value}) != SlotWord {
			return false
		}
		// The first word is structural only for command families whose
		// first argument selects the operation. Otherwise a stable word
		// may still be a user's value and must not be embedded in code.
		switch words[0] {
		case "git", "gh", "kubectl", "docker", "helm", "tap":
			return true
		}
		return false
	}
	if !strings.HasPrefix(c.Tool, "mcp:") || strings.Contains(path, "/") {
		return false
	}
	switch strings.ToLower(path) {
	case "action", "operation", "integration", "provider", "method", "tool", "service":
		return true
	}
	return false
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

// BookkeepingTools are Telara's own recording and tool-discovery calls,
// which agent instructions make every agent run around its work.
var BookkeepingTools = map[string]bool{
	"mcp:telara_task_list": true, "mcp:telara_task_create": true, "mcp:telara_task_resume": true,
	"mcp:telara_task_checkpoint": true, "mcp:telara_task_complete": true, "mcp:telara_task_pause": true,
	"mcp:telara_tool_search": true, "mcp:telara_tool_describe": true, "mcp:telara_annotate": true,
	"mcp:telara_link": true, "get_mcp_tools": true, "ToolSearch": true,
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
