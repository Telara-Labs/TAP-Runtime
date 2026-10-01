// Package trace is the shared vocabulary of recorded agent sessions: calls, sessions,
// normalized steps and slots, and the step-effect helpers every later stage uses.
package trace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"gitlab.com/telara-labs/tap-runtime/discover/shellparse"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
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
	// MCPServer and MCPTool retain the exact inventory identity where the
	// client records it. A normalized tool label alone cannot pin a TAP call.
	MCPServer string
	MCPTool   string
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
	// OutPaths is, for each of OutIDs, its jq path when the result is JSON
	// and the value sits at exactly one path; "*" when it sits at several;
	// "" when the result is not JSON or the value is not a JSON value.
	OutPaths []string `json:",omitempty"`
	// OutTokens are the distinct name-like words of the result (a repo, a
	// pod, a file name): at most 128, from its first 8 KB. They show that a
	// later value, or a list a later step loops over, came from this result.
	OutTokens []string `json:",omitempty"`
	// OutCollections summarizes complete structured result lists while the
	// original result is in the reader. Only paths, types, counts, and value
	// digests are retained; incomplete or oversized results yield no proof.
	OutCollections []ResultCollection `json:",omitempty"`
	// Output is the start of the result as the client recorded it (at most
	// 600 bytes), kept for reviewing a task's evidence on this machine.
	Output string `json:",omitempty"`
}

type ResultCollection struct {
	Path   string                           `json:"path"`
	Count  int                              `json:"count"`
	Fields map[string]ResultCollectionField `json:"fields"`
}

type ResultCollectionField struct {
	Type    string   `json:"type"`
	Digests []string `json:"digests"`
}

// ResultCollections retains bounded equality evidence for complete JSON
// arrays. It never stores the raw list items in a Discover report or graph.
func ResultCollections(text string) []ResultCollection {
	text = strings.TrimSpace(text)
	if len(text) == 0 || len(text) > 64<<10 || !strings.Contains(text, "[") || !json.Valid([]byte(text)) {
		return nil
	}
	var value any
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	if dec.Decode(&value) != nil {
		return nil
	}
	var out []ResultCollection
	var walk func(any, string, int)
	walk = func(v any, path string, depth int) {
		if depth > 12 || len(out) >= 64 {
			return
		}
		switch node := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(node))
			for key := range node {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				walk(node[key], path+JqKeyPath(key), depth+1)
			}
		case []any:
			if len(node) > 0 && len(node) <= 512 {
				fields := ResultItemFields(node)
				if len(fields) > 0 {
					out = append(out, ResultCollection{Path: path, Count: len(node), Fields: fields})
				}
			}
			for i, child := range node {
				if len(out) >= 64 {
					break
				}
				walk(child, fmt.Sprintf("%s[%d]", path, i), depth+1)
			}
		}
	}
	walk(value, "", 0)
	return out
}

func ResultItemFields(items []any) map[string]ResultCollectionField {
	first := map[string]ResultScalar{}
	CollectResultScalars(items[0], "", first, 0)
	if len(first) == 0 || len(first) > 64 {
		return nil
	}
	// Bound retained equality evidence independently of collection length.
	// A 200-item tool result is common, while a large cross-product of item
	// fields and values should remain unresolved instead of bloating reports.
	if len(items)*len(first) > 8192 {
		return nil
	}
	fields := map[string]ResultCollectionField{}
	for path, scalar := range first {
		fields[path] = ResultCollectionField{Type: scalar.Typ, Digests: []string{ResultValueDigest(scalar.Value)}}
	}
	for _, item := range items[1:] {
		leaves := map[string]ResultScalar{}
		CollectResultScalars(item, "", leaves, 0)
		for path, field := range fields {
			scalar, ok := leaves[path]
			if !ok || scalar.Typ != field.Type {
				delete(fields, path)
				continue
			}
			field.Digests = append(field.Digests, ResultValueDigest(scalar.Value))
			fields[path] = field
		}
	}
	return fields
}

type ResultScalar struct{ Typ, Value string }

func CollectResultScalars(value any, path string, leaves map[string]ResultScalar, depth int) {
	if depth > 12 || len(leaves) > 64 {
		return
	}
	switch node := value.(type) {
	case map[string]any:
		for key, child := range node {
			CollectResultScalars(child, path+JqKeyPath(key), leaves, depth+1)
		}
	case []any:
		for i, child := range node {
			CollectResultScalars(child, fmt.Sprintf("%s[%d]", path, i), leaves, depth+1)
		}
	case string:
		leaves[path] = ResultScalar{"string", node}
	case json.Number:
		typ := "integer"
		if strings.ContainsAny(node.String(), ".eE") {
			typ = "number"
		}
		leaves[path] = ResultScalar{typ, node.String()}
	case bool:
		leaves[path] = ResultScalar{"boolean", fmt.Sprint(node)}
	}
}

func ResultValueDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// Outcome of a call, as the client recorded it.
type Outcome int8

const (
	OutcomeUnknown Outcome = iota
	OutcomeOK
	OutcomeFailed
)

var (
	OutIDRe = regexp.MustCompile(`https?://[^\s"'<>)\]]+|\b[A-Z][A-Z0-9]{1,9}-\d+\b|\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b|\b[0-9a-f]{8,40}\b|\b\d{6,}\b`)
	ExitRe  = regexp.MustCompile(`(?i)(?:"exit_code"\s*:\s*|exit code:?\s*|exited with code\s*)(-?\d+)`)
)

// OutputIDs are the identifiers in a call's result: at most 64, from its
// first 64 KB.
func OutputIDs(text string) []string {
	ids, _ := OutputRefs(text)
	return ids
}

// OutputRefs returns outputIDs and, for each, the context of its first
// occurrence (see Call.OutCtx).
func OutputRefs(text string) (ids, ctx []string) {
	ids, ctx, _ = OutputRefsPaths(text)
	return ids, ctx
}

// OutputRefsPaths is outputRefs plus each identifier's JSON path.
func OutputRefsPaths(text string) (ids, ctx, paths []string) {
	defer func() {
		paths = JsonPaths(text, ids)
	}()
	text = TruncateUTF8(text, 64<<10)
	seen := map[string]bool{}
	for _, loc := range OutIDRe.FindAllStringIndex(text, -1) {
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
	return ids, ctx, nil
}

// ExitOutcome reads a shell result's exit status: failed when it names a
// non-zero exit code, OK otherwise.
func ExitOutcome(text string) Outcome {
	for _, m := range ExitRe.FindAllStringSubmatch(text, -1) {
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

func (u Usage) Add(v Usage) Usage {
	return Usage{u.Fresh + v.Fresh, u.Cached + v.Cached, u.Output + v.Output}
}

func (u Usage) Scale(f float64) Usage {
	return Usage{u.Fresh * f, u.Cached * f, u.Output * f}
}

// Spread gives each of calls[from:] an equal share of one turn's usage.
func Spread(calls []Call, from, turn int, u Usage) {
	n := len(calls) - from
	if n <= 0 {
		return
	}
	for i := from; i < len(calls); i++ {
		calls[i].Tokens, calls[i].Turn, calls[i].Measured = u.Scale(1/float64(n)), turn, true
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
	// RequestRoles distinguishes real user turns from client-injected context.
	// An absent entry means user, for readers that have no synthetic turns.
	RequestRoles []string `json:",omitempty"`
	// Approvals are acknowledgements the user gave in the middle of a
	// request ("yes", "approved"): a human decision between two of its
	// calls. None of them carries over to a new run.
	Approvals []Approval `json:",omitempty"`
	// SourceDigest is the sha256 of what the reader read for this session
	// (its transcript file, or its rows in a client database), so a frozen
	// corpus can prove its inputs unchanged whatever the parser does.
	SourceDigest string `json:"-"`
}

// Approval is a user acknowledgement given after AfterCall calls of the
// session, while Request was in progress.
type Approval struct {
	Request   int
	AfterCall int
}

// IsRequest reports a user message that asks for work. Harness wrappers
// (<environment_context>, <command-name>/clear, <local-command-stdout>) are
// written in angle brackets and are not requests.
func IsRequest(text string) bool {
	t := strings.TrimSpace(text)
	return t != "" && !strings.HasPrefix(t, "<")
}

// HarnessPrefixes are the envelopes clients and harnesses inject as a user
// message: instructions files, browser context without a request, and
// interruption markers. They are recorded (so their calls stay attributed)
// but they are not a person's request.
var HarnessPrefixes = []string{"# AGENTS.md instructions", "# In app browser:", "[Request interrupted", "# Context from my IDE setup:"}

// IsHarness reports text injected by a client or harness rather than typed
// by a person.
func IsHarness(text string) bool {
	t := strings.TrimSpace(text)
	for _, p := range HarnessPrefixes {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

// addRequest records a user message as a new request, unless it continues
// the one before it in this session: the same text sent again (a retry), or
// an acknowledgement ("yes, file", "continue", "already approved."), whose
// calls belong to the request it answers.
func (s *Session) AddRequest(text string) {
	s.AddRequestWithRole(text, "user")
}

func (s *Session) AddRequestWithRole(text, role string) {
	text = TruncateUTF8(RequestText(text), 4000)
	t := strings.TrimSpace(text)
	if n := len(s.Requests); n > 0 && strings.TrimSpace(s.Requests[n-1]) != "" {
		if t == strings.TrimSpace(s.Requests[n-1]) {
			return
		}
		if IsAcknowledgement(t) {
			// An answer to the agent partway through the request is a human
			// decision point, recorded so it is never replayed.
			if k := len(s.Calls); k > 0 && s.Calls[k-1].Request == n-1 {
				s.Approvals = append(s.Approvals, Approval{Request: n - 1, AfterCall: k})
			}
			return
		}
	}
	for len(s.RequestRoles) < len(s.Requests) {
		s.RequestRoles = append(s.RequestRoles, "user")
	}
	s.Requests = append(s.Requests, text)
	s.RequestRoles = append(s.RequestRoles, role)
}

// RequestText is what the user asked. Codex wraps it in context blocks
// ("# In app browser:", "# Files mentioned by the user:") and puts the
// request under "## My request for Codex:".
func RequestText(text string) string {
	const marker = "## My request for Codex:"
	if i := strings.LastIndex(text, marker); i >= 0 {
		return strings.TrimSpace(text[i+len(marker):])
	}
	return text
}

var AckWords = map[string]bool{"yes": true, "yeah": true, "yep": true, "yup": true, "y": true, "ok": true, "okay": true, "k": true, "sure": true,
	"continue": true, "proceed": true, "go": true, "done": true, "approved": true, "already": true, "lgtm": true, "perfect": true,
	"great": true, "thanks": true, "thank": true, "fine": true, "correct": true, "agreed": true, "good": true, "right": true}

// IsAcknowledgement is a short reply that answers the agent rather than
// asking for new work: at most six words, starting with an acknowledgement
// word, naming nothing (no path, URL, ticket or id).
func IsAcknowledgement(t string) bool {
	words := strings.Fields(strings.ToLower(t))
	if len(words) == 0 || len(words) > 6 || OutIDRe.MatchString(t) || strings.Contains(t, "/") {
		return false
	}
	return AckWords[strings.Trim(words[0], ".,!?:;")]
}

// TruncateUTF8 cuts s to at most n bytes without splitting a character.
func TruncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// request is the index calls made now belong to.
func (s *Session) Request() int {
	if len(s.Requests) == 0 {
		s.Requests = append(s.Requests, "")
		s.RequestRoles = append(s.RequestRoles, "unknown")
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

// JsonPaths locates each id in a JSON result: its jq path when it is a
// string or number value at exactly one place, "*" when at several, "".
func JsonPaths(text string, ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	t := strings.TrimSpace(text)
	if len(t) > 64<<10 || (!strings.HasPrefix(t, "{") && !strings.HasPrefix(t, "[")) {
		return make([]string, len(ids))
	}
	var v any
	dec := json.NewDecoder(strings.NewReader(t))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		return make([]string, len(ids))
	}
	at := map[string][]string{}
	var walk func(x any, p string)
	walk = func(x any, p string) {
		switch y := x.(type) {
		case map[string]any:
			for k, z := range y {
				walk(z, p+JqKeyPath(k))
			}
		case []any:
			for i, z := range y {
				walk(z, fmt.Sprintf("%s[%d]", p, i))
			}
		case string:
			at[y] = append(at[y], p)
		case json.Number:
			at[y.String()] = append(at[y.String()], p)
		}
	}
	walk(v, "")
	out := make([]string, len(ids))
	for k, id := range ids {
		switch ps := at[id]; len(ps) {
		case 0:
		case 1:
			out[k] = ps[0]
		default:
			out[k] = "*"
		}
	}
	return out
}

var JqPlainKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func JqKeyPath(k string) string {
	if JqPlainKey.MatchString(k) {
		return "." + k
	}
	b, _ := json.Marshal(k)
	return ".[" + string(b) + "]"
}

var OutTokenRe = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9._/:@+-]{2,}`)

// OutputTokens returns the distinct name-like words of a result that carry
// a letter and are at least 4 bytes long.
func OutputTokens(text string) []string {
	text = TruncateUTF8(text, 8<<10)
	seen := map[string]bool{}
	var out []string
	for _, m := range OutTokenRe.FindAllString(text, -1) {
		m = strings.TrimRight(m, ".,:;/")
		if len(m) < 4 || len(m) > 200 || seen[m] || !strings.ContainsAny(strings.ToLower(m), "abcdefghijklmnopqrstuvwxyz") {
			continue
		}
		seen[m] = true
		out = append(out, m)
		if len(out) == 128 {
			break
		}
	}
	return out
}

// RefusedRe matches a result saying the call never ran: the user or the
// client cancelled or rejected it.
var RefusedRe = regexp.MustCompile(`(?i)user cancelled (mcp )?tool call|tool call was cancelled|the user doesn't want to proceed with this tool use|the tool use was rejected`)

// ResultOutcome reads a tool result's outcome from its text: failed when it
// names a non-zero exit code or says the call was cancelled or rejected.
func ResultOutcome(text string) Outcome {
	if RefusedRe.MatchString(TruncateUTF8(text, 4096)) {
		return OutcomeFailed
	}
	return ExitOutcome(text)
}

// ReadJSString reads the string literal opening at rs[i] and returns its
// unescaped text and the index after its closing quote.
func ReadJSString(rs []rune, i int) (string, int) {
	q := rs[i]
	var b strings.Builder
	j := i + 1
	for ; j < len(rs) && rs[j] != q; j++ {
		if rs[j] == '\\' && j+1 < len(rs) {
			j++
			switch rs[j] {
			case 'n':
				b.WriteRune('\n')
			case 't':
				b.WriteRune('\t')
			default:
				b.WriteRune(rs[j])
			}
			continue
		}
		b.WriteRune(rs[j])
	}
	return b.String(), min(j+1, len(rs))
}

// ScanJSValue returns the index of the ',' or '}' that ends the value
// starting at rs[i], skipping nested brackets and strings.
func ScanJSValue(rs []rune, i int) int {
	depth := 0
	for i < len(rs) {
		switch c := rs[i]; {
		case IsQuote(c):
			_, i = ReadJSString(rs, i)
			continue
		case c == '{' || c == '[' || c == '(':
			depth++
		case c == '}' || c == ']' || c == ')':
			if depth == 0 {
				return i
			}
			depth--
		case c == ',' && depth == 0:
			return i
		}
		i++
	}
	return i
}

func IsQuote(c rune) bool { return c == '"' || c == '\'' || c == '`' }

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

var AwaitedCallRe = regexp.MustCompile(`await\s+([A-Za-z_$][\w$]*)((?:\s*\.\s*[A-Za-z_$][\w$]*)+)\s*\(`)

// JsSteps returns one step per awaited method call in a script, in order.
// Awaiting is what separates an action (a navigation, a click, a snapshot)
// from a utility (JSON.stringify, console.log). The method path (goto,
// playwright.domSnapshot) is the label. The receiver is a slot: it is fixed
// when every run used the same object (agent, chrome) and an input when it
// was a variable the author named each time (liTab, gmailTab). The first
// argument is a slot too: a string literal as its value, anything else (a
// function, an object) as its source, marked Raw.
func JsSteps(code string) []Step {
	var out []Step
	for _, m := range AwaitedCallRe.FindAllStringSubmatchIndex(code, -1) {
		method := strings.Join(strings.Fields(strings.ReplaceAll(code[m[4]:m[5]], ".", " ")), ".")
		if IsWait(method) {
			continue
		}
		st := Step{Label: "js:" + method, Skeleton: "js:" + method}
		st.Slots = append(st.Slots, Slot{Key: "recv", Type: SlotWord, Value: code[m[2]:m[3]]})
		rs := []rune(strings.TrimSpace(code[m[1]:]))
		switch {
		case len(rs) == 0 || rs[0] == ')':
		case IsQuote(rs[0]):
			v, _ := ReadJSString(rs, 0)
			st.Slots = append(st.Slots, ArgSlots("0", v)...)
		default:
			src := strings.TrimSpace(string(rs[:ScanJSValue(rs, 0)]))
			src = TruncateUTF8(src, 2000)
			st.Slots = append(st.Slots, Slot{Key: "0", Type: SlotText, Value: src, Raw: true})
		}
		out = append(out, st)
	}
	return out
}

var PatchFileRe = regexp.MustCompile(`(?m)^\*\*\* (Update|Add|Delete) File: (.+?)\s*$`)

// PatchSteps returns one step per file a patch touches: "patch:update",
// "patch:add" or "patch:delete", with the file as a path slot and its base
// name as a separate slot, so a ledger updated every time (activity.log)
// shows as fixed even when it is reached by different paths.
func PatchSteps(input string) []Step {
	var out []Step
	for _, m := range PatchFileRe.FindAllStringSubmatch(input, -1) {
		label := "patch:" + strings.ToLower(m[1])
		out = append(out, Step{Label: label, Skeleton: label, Slots: []Slot{
			{Key: "file", Type: SlotPath, Value: m[2]},
			{Key: "name", Type: SlotWord, Value: path.Base(m[2])},
		}})
	}
	return out
}

// ArgSlots types one argument value. A URL also yields its host as a slot of
// its own: the site is usually the fixed part (the same page family every
// time) and the path the parameter. A path also yields its base name.
func ArgSlots(key, v string) []Slot {
	v = TruncateUTF8(v, 200)
	tp := TypeOf(shellparse.Word{Text: v, Quoted: strings.ContainsAny(v, " \n")})
	out := []Slot{{Key: key, Type: tp, Value: v}}
	switch tp {
	case SlotURL:
		if u, err := url.Parse(v); err == nil && u.Host != "" {
			out = append(out, Slot{Key: key + ".host", Type: SlotWord, Value: u.Host})
		}
	case SlotPath:
		out = append(out, Slot{Key: key + ".name", Type: SlotWord, Value: path.Base(v)})
	}
	return out
}

// IsWait reports a call that only waits (waitForTimeout, waitForLoadState,
// sleep). Waits sit between nearly every browser action; they are timing,
// not work, and would otherwise multiply every browser pattern.
func IsWait(method string) bool {
	last := method[strings.LastIndex(method, ".")+1:]
	return strings.HasPrefix(last, "waitFor") || last == "sleep" || last == "delay" || last == "wait"
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
