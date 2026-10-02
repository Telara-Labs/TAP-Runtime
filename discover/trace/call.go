package trace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
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
		// Sorted, so which leaves fit under the bound is the same every run.
		keys := make([]string, 0, len(node))
		for key := range node {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			CollectResultScalars(node[key], path+JqKeyPath(key), leaves, depth+1)
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
	// Skipped counts the records of this session the reader could not
	// parse and left out. Like SourceDigest it describes the read, not the
	// session, so it is not part of the session's encoding.
	Skipped int `json:"-"`
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
// ReadStats is what a read left out: whole files or sessions it could not
// read at all. Records skipped inside a readable session are Session.Skipped.
type ReadStats struct {
	UnreadableFiles int
}

// StatReader is a Reader that also says what it left out, so a vendor
// format change shows as a spike in skipped records, not as no history.
type StatReader interface {
	Reader
	ReadWithStats(since time.Time) ([]Session, ReadStats, error)
}

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
