package history

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
	"github.com/Telara-Labs/TAP-Runtime/discover/util"
)

// Cursor reads Cursor's chat store, globalStorage/state.vscdb, a SQLite
// database. No SQLite driver is a dependency of this module, so it runs the
// system sqlite3 read-only, WAL included (util.SQLiteURI): the file is never
// written, and a missing sqlite3 makes this reader unavailable, not the run.
//
// Each message ("bubble") is a row bubbleId:<composer>:<bubble>; a tool call
// is a bubble with toolFormerData. A conversation ("composer") lists its
// bubbles in order in fullConversationHeadersOnly. Early 2025 conversations
// instead hold their messages inline in composerData.conversation.
type Cursor struct {
	DB      string
	SQLite3 string // path to sqlite3; "" looks it up on PATH
}

func (Cursor) Client() string { return "cursor" }

// ErrUnavailable reports that a store exists but cannot be read here.
var ErrUnavailable = errors.New("unavailable")

// CursorArgs keeps each argument's JSON type: text stays text (capped),
// numbers, booleans, objects and arrays pass through as JSON.
const CursorArgs = `CASE WHEN json_valid(%[1]s) THEN (SELECT json_group_object(a.key, CASE WHEN a.type = 'text' THEN substr(a.value, 1, 32768) ELSE json(a.value) END) FROM json_each(%[1]s) a) ELSE '{}' END`

var (
	// The table is aliased c throughout: inside the json_each subquery a bare
	// "value" would name json_each's own column, not the row's.
	CursorBubbleSQL = `SELECT substr(c.key, 10, 36) AS composer, substr(c.key, 47) AS bubble, length(c.value) AS size,
  json_extract(c.value, '$.toolFormerData.name') AS name,
  ` + fmt.Sprintf(CursorArgs, `coalesce(nullif(json_extract(c.value, '$.toolFormerData.rawArgs'), ''), json_extract(c.value, '$.toolFormerData.params'))`) + ` AS args,
  json_extract(c.value, '$.createdAt') AS created,
  CASE WHEN json_valid(json_extract(c.value, '$.toolFormerData.params')) THEN json_extract(json_extract(c.value, '$.toolFormerData.params'), '$.tools[0].serverName') END AS server,
  json_extract(c.value, '$.toolFormerData.status') AS status,
  substr(CAST(json_extract(c.value, '$.toolFormerData.result') AS TEXT), 1, 65536) AS result
FROM cursorDiskKV c WHERE c.key LIKE 'bubbleId:%' AND json_extract(c.value, '$.toolFormerData.name') IS NOT NULL;`

	CursorComposerSQL = `SELECT substr(c.key, 14) AS composer, length(c.value) AS size, json_extract(c.value, '$.createdAt') AS created,
  (SELECT json_group_array(json_extract(h.value, '$.bubbleId')) FROM json_each(c.value, '$.fullConversationHeadersOnly') h) AS headers
FROM cursorDiskKV c WHERE c.key LIKE 'composerData:%';`

	// The user's messages (bubble type 1), in both storage forms.
	CursorUserSQL = `SELECT substr(c.key, 10, 36) AS composer, substr(c.key, 47) AS bubble, length(c.value) AS size,
  substr(json_extract(c.value, '$.text'), 1, 4000) AS text
FROM cursorDiskKV c WHERE c.key LIKE 'bubbleId:%' AND json_extract(c.value, '$.type') = 1;`

	CursorInlineUserSQL = `SELECT substr(c.key, 14) AS composer, CAST(j.key AS TEXT) AS bubble,
  substr(json_extract(j.value, '$.text'), 1, 4000) AS text
FROM cursorDiskKV c, json_each(c.value, '$.conversation') j
WHERE c.key LIKE 'composerData:%' AND json_extract(j.value, '$.type') = 1;`

	CursorInlineSQL = `SELECT substr(c.key, 14) AS composer, CAST(j.key AS TEXT) AS bubble,
  json_extract(j.value, '$.toolFormerData.name') AS name,
  ` + fmt.Sprintf(CursorArgs, `coalesce(nullif(json_extract(j.value, '$.toolFormerData.rawArgs'), ''), json_extract(j.value, '$.toolFormerData.params'))`) + ` AS args,
  NULL AS created,
  CASE WHEN json_valid(json_extract(j.value, '$.toolFormerData.params')) THEN json_extract(json_extract(j.value, '$.toolFormerData.params'), '$.tools[0].serverName') END AS server,
  json_extract(j.value, '$.toolFormerData.status') AS status,
  substr(CAST(json_extract(j.value, '$.toolFormerData.result') AS TEXT), 1, 65536) AS result
FROM cursorDiskKV c, json_each(c.value, '$.conversation') j
WHERE c.key LIKE 'composerData:%' AND json_extract(j.value, '$.toolFormerData.name') IS NOT NULL;`
)

type CursorRow struct {
	Composer string          `json:"composer"`
	Bubble   string          `json:"bubble"`
	Name     string          `json:"name"`
	Args     string          `json:"args"`
	Created  json.RawMessage `json:"created"`
	Status   string          `json:"status"`
	Server   string          `json:"server"` // an MCP call's server, from params.tools[0].serverName
	Result   string          `json:"result"`
	Headers  string          `json:"headers"`
	Text     string          `json:"text"`
	// Size is the stored record's length in bytes: with its key, the
	// record's identity for the source digest (TENG-3168).
	Size int64 `json:"size"`
}

func (r Cursor) Read(since time.Time) ([]trace.Session, error) {
	if _, err := os.Stat(r.DB); os.IsNotExist(err) {
		return nil, nil
	}
	bin := r.SQLite3
	if bin == "" {
		p, err := exec.LookPath("sqlite3")
		if err != nil {
			return nil, fmt.Errorf("cursor: %w: sqlite3 is not installed", ErrUnavailable)
		}
		bin = p
	}
	query := func(sql string) ([]CursorRow, error) {
		cmd := exec.Command(bin, "-readonly", "-json", util.SQLiteURI(r.DB), sql)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("cursor: sqlite3: %v: %s", err, strings.TrimSpace(stderr.String()))
		}
		var rows []CursorRow
		if len(bytes.TrimSpace(out)) == 0 {
			return nil, nil
		}
		return rows, json.Unmarshal(out, &rows)
	}
	composers, err := query(CursorComposerSQL)
	if err != nil {
		return nil, err
	}
	bubbles, err := query(CursorBubbleSQL)
	if err != nil {
		return nil, err
	}
	inline, err := query(CursorInlineSQL)
	if err != nil {
		return nil, err
	}
	users, err := query(CursorUserSQL)
	if err != nil {
		return nil, err
	}
	inlineUsers, err := query(CursorInlineUserSQL)
	if err != nil {
		return nil, err
	}

	convs := map[string]*CursorConv{}
	for _, c := range composers {
		cv := &CursorConv{Start: CursorTime(c.Created), Order: map[string]int{}, Records: map[string]int64{"composerData:" + c.Composer: c.Size}}
		var hs []string
		_ = json.Unmarshal([]byte(c.Headers), &hs)
		for i, h := range hs {
			cv.Order[h] = i
		}
		convs[c.Composer] = cv
	}
	add := func(row CursorRow, pos int) {
		cv := convs[row.Composer]
		if cv == nil {
			return
		}
		cv.Calls = append(cv.Calls, CursorPlaced{Pos: pos, Row: row})
		if row.Size > 0 {
			cv.Records["bubbleId:"+row.Composer+":"+row.Bubble] = row.Size
		}
	}
	for _, b := range bubbles {
		pos, ok := convs[b.Composer].OrderOf(b.Bubble)
		if !ok {
			// A bubble the header list does not name: order it by time after the named ones.
			pos = 1<<30 + int(CursorTime(b.Created).Unix()%(1<<30))
		}
		add(b, pos)
	}
	for _, b := range inline {
		i, _ := strconv.Atoi(b.Bubble)
		add(b, i)
	}
	addUser := func(row CursorRow, pos int) {
		if cv := convs[row.Composer]; cv != nil {
			cv.Users = append(cv.Users, CursorUser{pos, row.Text})
			if row.Size > 0 {
				cv.Records["bubbleId:"+row.Composer+":"+row.Bubble] = row.Size
			}
		}
	}
	for _, u := range users {
		if pos, ok := convs[u.Composer].OrderOf(u.Bubble); ok {
			addUser(u, pos)
		}
	}
	for _, u := range inlineUsers {
		i, _ := strconv.Atoi(u.Bubble)
		addUser(u, i)
	}

	var out []trace.Session
	for id, cv := range convs {
		if len(cv.Calls) == 0 || cv.Start.Before(since) {
			continue
		}
		sort.SliceStable(cv.Calls, func(i, j int) bool { return cv.Calls[i].Pos < cv.Calls[j].Pos })
		sort.SliceStable(cv.Users, func(i, j int) bool { return cv.Users[i].Pos < cv.Users[j].Pos })
		a := NewAssembler("cursor", id)
		a.S.Start = cv.Start
		a.S.SourceDigest = cv.SourceDigest()
		u := 0
		for i, c := range cv.Calls {
			for u < len(cv.Users) && cv.Users[u].Pos < c.Pos {
				a.Add(UserText{Text: cv.Users[u].Text})
				u++
			}
			for _, e := range CursorEvents(c.Row, strconv.Itoa(i)) {
				a.Add(e)
			}
		}
		out = append(out, a.Finish())
	}
	// Conversations come out of a map; later passes take sessions in order.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Start.Equal(out[j].Start) {
			return out[i].Start.Before(out[j].Start)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

type CursorPlaced struct {
	Pos int
	Row CursorRow
}

type CursorConv struct {
	// Records are the store records the conversation was read from, key to
	// stored length: its source identity, whatever the reader extracts.
	Records map[string]int64
	Start   time.Time
	Order   map[string]int
	Calls   []CursorPlaced
	Users   []CursorUser
}

type CursorUser struct {
	Pos  int
	Text string
}

// SourceDigest identifies the conversation's input: the key and stored
// length of each record it was read from (the conversation and its tool and
// user bubbles; inline bubbles are inside the conversation record). It does
// not depend on what the reader's queries extract, so changing the reader
// never reads as changed input to a frozen corpus, while an edited record
// does (TENG-3168).
func (c *CursorConv) SourceDigest() string {
	keys := make([]string, 0, len(c.Records))
	for k := range c.Records {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s\x00%d\n", k, c.Records[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// orderOf is the bubble's position in its conversation's header list.
func (c *CursorConv) OrderOf(bubble string) (int, bool) {
	if c == nil {
		return 0, false
	}
	i, ok := c.Order[bubble]
	return i, ok
}

// CursorCall decodes one tool bubble into a call with its result.
func CursorCall(row CursorRow) trace.Call {
	a := NewAssembler("cursor", "")
	for _, e := range CursorEvents(row, "c") {
		a.Add(e)
	}
	return a.Finish().Calls[0]
}

// CursorEvents decodes one tool bubble: the call, then its result, whose
// outcome is the bubble's own status (completed, error, cancelled).
func CursorEvents(row CursorRow, key string) []Event {
	c := trace.Call{Session: row.Composer, Time: CursorTime(row.Created)}
	var args map[string]json.RawMessage
	_ = json.Unmarshal([]byte(row.Args), &args)
	switch {
	case strings.HasPrefix(row.Name, "run_terminal"):
		c.Tool, c.Command = "shell", RawString(args["command"])
	case strings.HasPrefix(row.Name, "mcp-"):
		// mcp-<server>-<tool>; the server is also params.tools[0].serverName
		// (TENG-3163).
		parts := strings.Split(row.Name, "-")
		server := row.Server
		if server == "" && len(parts) > 2 {
			server = strings.Join(parts[1:len(parts)-1], "-")
		}
		args = CursorMCPArgs(args)
		MCPCall(&c, server, parts[len(parts)-1], args)
	default:
		c.Tool, c.Args, c.RawArgs = row.Name, Flatten(args), RawKeys(args)
	}
	var outcome trace.Outcome
	switch row.Status {
	case "completed":
		outcome = trace.OutcomeOK
	case "error", "cancelled":
		outcome = trace.OutcomeFailed
	}
	return []Event{ToolCall{Key: key, Call: c}, ToolResult{Key: key, Text: CursorResult(row.Result), HasOutcome: true, Outcome: outcome}}
}

// CursorResult is a tool's result text. Cursor records an MCP result as
// {"result": "<the MCP {content: [...]} object, as a JSON string>"}; the
// tool's result is the text of that content (TENG-3163). Anything else is
// the result as recorded.
func CursorResult(raw string) string {
	if !strings.HasPrefix(raw, `{"result":`) {
		return raw
	}
	var outer map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &outer) != nil || len(outer) != 1 {
		return raw
	}
	inner := RawString(outer["result"])
	var mcp struct {
		Content           json.RawMessage `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
	}
	if json.Unmarshal([]byte(inner), &mcp) != nil || len(mcp.Content) == 0 {
		if inner != "" {
			return inner
		}
		return raw
	}
	if text := strings.TrimSuffix(CodexOutputText(mcp.Content), "\n"); strings.TrimSpace(text) != "" {
		return text
	}
	if len(mcp.StructuredContent) > 0 && string(mcp.StructuredContent) != "null" {
		return string(mcp.StructuredContent)
	}
	return inner
}

// CursorMCPArgs returns the arguments the MCP tool received. Cursor records
// an MCP call as an envelope, {name, args, toolCallId, providerIdentifier,
// serverIdentifier, ...} in rawArgs, or {tools: [{name, parameters}]} with
// the arguments as a JSON string in params. The envelope is Cursor's
// bookkeeping, not the tool's input.
func CursorMCPArgs(args map[string]json.RawMessage) map[string]json.RawMessage {
	inner := args["args"]
	if inner == nil {
		var tools []struct {
			Parameters json.RawMessage `json:"parameters"`
		}
		if json.Unmarshal(args["tools"], &tools) != nil || len(tools) != 1 {
			return args
		}
		inner = tools[0].Parameters
	} else if args["toolCallId"] == nil && args["providerIdentifier"] == nil && args["serverIdentifier"] == nil && args["toolName"] == nil {
		// A tool whose own argument is named "args".
		return args
	}
	var s string
	if json.Unmarshal(inner, &s) == nil {
		inner = json.RawMessage(s)
	}
	var out map[string]json.RawMessage
	if json.Unmarshal(inner, &out) != nil {
		return map[string]json.RawMessage{}
	}
	return out
}

// CursorTime reads createdAt, which Cursor has written both as epoch
// milliseconds and as an RFC 3339 string.
func CursorTime(raw json.RawMessage) time.Time {
	var ms int64
	if json.Unmarshal(raw, &ms) == nil && ms > 0 {
		return time.UnixMilli(ms).UTC()
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
