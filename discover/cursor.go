package discover

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Cursor reads Cursor's chat store, globalStorage/state.vscdb, a SQLite
// database. No SQLite driver is a dependency of this module, so it runs the
// system sqlite3 read-only with immutable=1: the file is never written or
// locked, and a missing sqlite3 makes this reader unavailable, not the run.
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

// cursorArgs keeps each argument's JSON type: text stays text (capped),
// numbers, booleans, objects and arrays pass through as JSON.
const cursorArgs = `CASE WHEN json_valid(%[1]s) THEN (SELECT json_group_object(a.key, CASE WHEN a.type = 'text' THEN substr(a.value, 1, 32768) ELSE json(a.value) END) FROM json_each(%[1]s) a) ELSE '{}' END`

var (
	// The table is aliased c throughout: inside the json_each subquery a bare
	// "value" would name json_each's own column, not the row's.
	cursorBubbleSQL = `SELECT substr(c.key, 10, 36) AS composer, substr(c.key, 47) AS bubble,
  json_extract(c.value, '$.toolFormerData.name') AS name,
  ` + fmt.Sprintf(cursorArgs, `coalesce(json_extract(c.value, '$.toolFormerData.rawArgs'), json_extract(c.value, '$.toolFormerData.params'))`) + ` AS args,
  json_extract(c.value, '$.createdAt') AS created,
  json_extract(c.value, '$.toolFormerData.status') AS status,
  substr(CAST(json_extract(c.value, '$.toolFormerData.result') AS TEXT), 1, 2048) AS result
FROM cursorDiskKV c WHERE c.key LIKE 'bubbleId:%' AND json_extract(c.value, '$.toolFormerData.name') IS NOT NULL;`

	cursorComposerSQL = `SELECT substr(c.key, 14) AS composer, json_extract(c.value, '$.createdAt') AS created,
  (SELECT json_group_array(json_extract(h.value, '$.bubbleId')) FROM json_each(c.value, '$.fullConversationHeadersOnly') h) AS headers
FROM cursorDiskKV c WHERE c.key LIKE 'composerData:%';`

	// The user's messages (bubble type 1), in both storage forms.
	cursorUserSQL = `SELECT substr(c.key, 10, 36) AS composer, substr(c.key, 47) AS bubble,
  substr(json_extract(c.value, '$.text'), 1, 4000) AS text
FROM cursorDiskKV c WHERE c.key LIKE 'bubbleId:%' AND json_extract(c.value, '$.type') = 1;`

	cursorInlineUserSQL = `SELECT substr(c.key, 14) AS composer, CAST(j.key AS TEXT) AS bubble,
  substr(json_extract(j.value, '$.text'), 1, 4000) AS text
FROM cursorDiskKV c, json_each(c.value, '$.conversation') j
WHERE c.key LIKE 'composerData:%' AND json_extract(j.value, '$.type') = 1;`

	cursorInlineSQL = `SELECT substr(c.key, 14) AS composer, CAST(j.key AS TEXT) AS bubble,
  json_extract(j.value, '$.toolFormerData.name') AS name,
  ` + fmt.Sprintf(cursorArgs, `coalesce(json_extract(j.value, '$.toolFormerData.rawArgs'), json_extract(j.value, '$.toolFormerData.params'))`) + ` AS args,
  NULL AS created,
  json_extract(j.value, '$.toolFormerData.status') AS status,
  substr(CAST(json_extract(j.value, '$.toolFormerData.result') AS TEXT), 1, 2048) AS result
FROM cursorDiskKV c, json_each(c.value, '$.conversation') j
WHERE c.key LIKE 'composerData:%' AND json_extract(j.value, '$.toolFormerData.name') IS NOT NULL;`
)

type cursorRow struct {
	Composer string          `json:"composer"`
	Bubble   string          `json:"bubble"`
	Name     string          `json:"name"`
	Args     string          `json:"args"`
	Created  json.RawMessage `json:"created"`
	Status   string          `json:"status"`
	Result   string          `json:"result"`
	Headers  string          `json:"headers"`
	Text     string          `json:"text"`
}

func (r Cursor) Read(since time.Time) ([]Session, error) {
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
	query := func(sql string) ([]cursorRow, error) {
		cmd := exec.Command(bin, "-readonly", "-json", "file:"+r.DB+"?immutable=1", sql)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("cursor: sqlite3: %v: %s", err, strings.TrimSpace(stderr.String()))
		}
		var rows []cursorRow
		if len(bytes.TrimSpace(out)) == 0 {
			return nil, nil
		}
		return rows, json.Unmarshal(out, &rows)
	}
	composers, err := query(cursorComposerSQL)
	if err != nil {
		return nil, err
	}
	bubbles, err := query(cursorBubbleSQL)
	if err != nil {
		return nil, err
	}
	inline, err := query(cursorInlineSQL)
	if err != nil {
		return nil, err
	}
	users, err := query(cursorUserSQL)
	if err != nil {
		return nil, err
	}
	inlineUsers, err := query(cursorInlineUserSQL)
	if err != nil {
		return nil, err
	}

	convs := map[string]*cursorConv{}
	for _, c := range composers {
		cv := &cursorConv{start: cursorTime(c.Created), order: map[string]int{}}
		var hs []string
		_ = json.Unmarshal([]byte(c.Headers), &hs)
		for i, h := range hs {
			cv.order[h] = i
		}
		convs[c.Composer] = cv
	}
	add := func(row cursorRow, pos int) {
		cv := convs[row.Composer]
		if cv == nil {
			return
		}
		call := cursorCall(row)
		call.Session = row.Composer
		cv.calls = append(cv.calls, cursorPlaced{pos, call})
	}
	for _, b := range bubbles {
		pos, ok := convs[b.Composer].orderOf(b.Bubble)
		if !ok {
			// A bubble the header list does not name: order it by time after the named ones.
			pos = 1<<30 + int(cursorTime(b.Created).Unix()%(1<<30))
		}
		add(b, pos)
	}
	for _, b := range inline {
		i, _ := strconv.Atoi(b.Bubble)
		add(b, i)
	}
	addUser := func(row cursorRow, pos int) {
		if cv := convs[row.Composer]; cv != nil {
			cv.users = append(cv.users, cursorUser{pos, row.Text})
		}
	}
	for _, u := range users {
		if pos, ok := convs[u.Composer].orderOf(u.Bubble); ok {
			addUser(u, pos)
		}
	}
	for _, u := range inlineUsers {
		i, _ := strconv.Atoi(u.Bubble)
		addUser(u, i)
	}

	var out []Session
	for id, cv := range convs {
		if len(cv.calls) == 0 || cv.start.Before(since) {
			continue
		}
		sort.SliceStable(cv.calls, func(i, j int) bool { return cv.calls[i].pos < cv.calls[j].pos })
		sort.SliceStable(cv.users, func(i, j int) bool { return cv.users[i].pos < cv.users[j].pos })
		s := Session{Client: "cursor", ID: id, Start: cv.start}
		u := 0
		for _, c := range cv.calls {
			for u < len(cv.users) && cv.users[u].pos < c.pos {
				if isRequest(cv.users[u].text) {
					s.addRequest(cv.users[u].text)
				}
				u++
			}
			c.call.Request = s.request()
			s.Calls = append(s.Calls, c.call)
		}
		out = append(out, s)
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

type cursorPlaced struct {
	pos  int
	call Call
}

type cursorConv struct {
	start time.Time
	order map[string]int
	calls []cursorPlaced
	users []cursorUser
}

type cursorUser struct {
	pos  int
	text string
}

// orderOf is the bubble's position in its conversation's header list.
func (c *cursorConv) orderOf(bubble string) (int, bool) {
	if c == nil {
		return 0, false
	}
	i, ok := c.order[bubble]
	return i, ok
}

func cursorCall(row cursorRow) Call {
	c := Call{Client: "cursor", Time: cursorTime(row.Created)}
	c.OutIDs, c.OutCtx = outputRefs(row.Result)
	c.Output = truncateUTF8(row.Result, 600)
	switch row.Status {
	case "completed":
		c.Outcome = OutcomeOK
	case "error", "cancelled":
		c.Outcome = OutcomeFailed
	}
	var args map[string]json.RawMessage
	_ = json.Unmarshal([]byte(row.Args), &args)
	switch {
	case strings.HasPrefix(row.Name, "run_terminal"):
		c.Tool, c.Command = "shell", rawString(args["command"])
	case strings.HasPrefix(row.Name, "mcp-"):
		parts := strings.Split(row.Name, "-")
		args = cursorMCPArgs(args)
		c.Tool, c.Args, c.RawArgs = "mcp:"+parts[len(parts)-1], flatten(args), rawKeys(args)
	default:
		c.Tool, c.Args, c.RawArgs = row.Name, flatten(args), rawKeys(args)
	}
	return c
}

// cursorMCPArgs returns the arguments the MCP tool received. Cursor records
// an MCP call as an envelope, {name, args, toolCallId, providerIdentifier,
// serverIdentifier, ...} in rawArgs, or {tools: [{name, parameters}]} with
// the arguments as a JSON string in params. The envelope is Cursor's
// bookkeeping, not the tool's input.
func cursorMCPArgs(args map[string]json.RawMessage) map[string]json.RawMessage {
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

// cursorTime reads createdAt, which Cursor has written both as epoch
// milliseconds and as an RFC 3339 string.
func cursorTime(raw json.RawMessage) time.Time {
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
