package history

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
	"github.com/Telara-Labs/TAP-Runtime/discover/util"
)

// CursorCLI reads the Cursor CLI's (cursor-agent's) sessions:
// <Dir>/<workspace>/<session>/store.db, one SQLite database per session
// Like Cursor, it runs the system sqlite3 read-only
// (util.SQLiteURI), so the store is never written.
//
// The store is content-addressed: table blobs holds each message as AI SDK
// JSON ({role, content: [text | tool-call | tool-result]}) under its sha256,
// plus protobuf tree nodes. Table meta holds one hex-encoded JSON record
// whose latestRootBlobId names the root node; the root's field 1 lists the
// conversation's message blob ids in order.
type CursorCLI struct {
	Dir     string
	SQLite3 string // path to sqlite3; "" looks it up on PATH
}

func (CursorCLI) Client() string { return "cursor-cli" }

func (r CursorCLI) Read(since time.Time) ([]trace.Session, error) {
	ss, _, err := r.ReadWithStats(since)
	return ss, err
}

func (r CursorCLI) ReadWithStats(since time.Time) ([]trace.Session, trace.ReadStats, error) {
	var st trace.ReadStats
	stores, err := filepath.Glob(filepath.Join(r.Dir, "*", "*", "store.db"))
	if err != nil || len(stores) == 0 {
		return nil, st, err
	}
	bin := r.SQLite3
	if bin == "" {
		p, err := util.SQLiteBin()
		if err != nil {
			return nil, st, fmt.Errorf("cursor-cli: %w: sqlite3 is not installed", ErrUnavailable)
		}
		bin = p
	}
	var out []trace.Session
	for _, db := range stores {
		if info, err := os.Stat(db); err != nil || info.ModTime().Before(since) {
			continue
		}
		s, err := ReadCursorCLIStore(bin, db)
		if err != nil {
			st.UnreadableFiles++ // one store that cannot be read is skipped, not the run
			continue
		}
		if len(s.Calls) == 0 || s.Start.Before(since) {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Start.Equal(out[j].Start) {
			return out[i].Start.Before(out[j].Start)
		}
		return out[i].ID < out[j].ID
	})
	return out, st, nil
}

// CursorCLIMessage is one AI SDK message.
type CursorCLIMessage struct {
	Blob    string          `json:"-"` // the blob id it was read from (sha256 of it)
	Role    string          `json:"role"`
	ID      string          `json:"id"`
	Content json.RawMessage `json:"content"`
}

type cursorCLIPart struct {
	Type       string                     `json:"type"`
	Text       string                     `json:"text"`
	ToolCallID string                     `json:"toolCallId"`
	ToolName   string                     `json:"toolName"`
	Args       map[string]json.RawMessage `json:"args"`
	Result     json.RawMessage            `json:"result"`
	IsError    bool                       `json:"isError"`
}

// ReadCursorCLIStore reads one session's store.db with the sqlite3 at bin.
func ReadCursorCLIStore(bin, db string) (trace.Session, error) {
	ctx, cancel := context.WithTimeout(context.Background(), util.SQLiteReadTimeout)
	defer cancel()
	query := func(sql string) ([]map[string]string, error) {
		b, err := util.SQLiteQuery(ctx, bin, db, sql)
		if err != nil {
			return nil, fmt.Errorf("cursor-cli: %w", err)
		}
		var rows []map[string]string
		if len(bytes.TrimSpace(b)) == 0 {
			return nil, nil
		}
		return rows, json.Unmarshal(b, &rows)
	}
	metaRows, err := query(`SELECT value FROM meta`)
	if err != nil {
		return trace.Session{}, err
	}
	var meta struct {
		Root      string `json:"latestRootBlobId"`
		CreatedAt int64  `json:"createdAt"`
	}
	for _, row := range metaRows {
		raw, err := hex.DecodeString(row["value"])
		if err != nil {
			raw = []byte(row["value"])
		}
		if json.Unmarshal(raw, &meta) == nil && meta.Root != "" {
			break
		}
	}
	if meta.Root == "" {
		return trace.Session{}, fmt.Errorf("%s: no root", db)
	}
	blobRows, err := query(`SELECT id, hex(data) AS data FROM blobs`)
	if err != nil {
		return trace.Session{}, err
	}
	blobs := make(map[string][]byte, len(blobRows))
	for _, row := range blobRows {
		if b, err := hex.DecodeString(row["data"]); err == nil {
			blobs[row["id"]] = b
		}
	}
	var msgs []CursorCLIMessage
	skipped := 0
	h := sha256.New()
	for _, id := range CursorCLIOrder(blobs[meta.Root]) {
		var m CursorCLIMessage
		if json.Unmarshal(blobs[id], &m) != nil || m.Role == "" {
			skipped++ // the root lists only messages: this one is missing or unreadable
			continue
		}
		h.Write([]byte(id))
		m.Blob = id
		msgs = append(msgs, m)
	}
	id := filepath.Base(filepath.Dir(db))
	start := time.UnixMilli(meta.CreatedAt).UTC()
	if meta.CreatedAt == 0 {
		if info, err := os.Stat(db); err == nil {
			start = info.ModTime().UTC()
		}
	}
	s := CursorCLISession(id, start, msgs)
	s.SourceDigest = hex.EncodeToString(h.Sum(nil))
	s.Skipped = skipped
	return s, nil
}

// CursorCLIOrder reads the message ids a root tree node lists, in order:
// every 32-byte length-delimited field 1 of the protobuf message.
func CursorCLIOrder(root []byte) []string {
	var ids []string
	for i := 0; i < len(root); {
		key, n := uvarint(root[i:])
		if n <= 0 {
			break
		}
		i += n
		field, wire := key>>3, key&7
		switch wire {
		case 0:
			_, n = uvarint(root[i:])
			if n <= 0 {
				return ids
			}
			i += n
		case 2:
			l, n := uvarint(root[i:])
			if n <= 0 || i+n+int(l) > len(root) {
				return ids
			}
			i += n
			if field == 1 && l == 32 {
				ids = append(ids, hex.EncodeToString(root[i:i+int(l)]))
			}
			i += int(l)
		case 1:
			i += 8
		case 5:
			i += 4
		default:
			return ids
		}
	}
	return ids
}

func uvarint(b []byte) (uint64, int) {
	var x uint64
	for i, c := range b {
		if i == 10 {
			return 0, -1
		}
		x |= uint64(c&0x7f) << (7 * i)
		if c < 0x80 {
			return x, i + 1
		}
	}
	return 0, 0
}

// CursorCLISession assembles one session from its messages. The store keeps
// no time per message; the CLI writes each user turn's time into the turn
// (<timestamp>Friday, Aug 28, 2026, 4:32 PM (UTC-4)</timestamp>), so a call
// carries the time of the turn it answers (to the minute), or the session's
// start before any.
func CursorCLISession(id string, start time.Time, msgs []CursorCLIMessage) trace.Session {
	a := NewAssembler("cursor-cli", id)
	a.S.Start = start
	at := start
	for _, m := range msgs {
		if m.Role == "user" {
			if t, ok := cursorCLITurnTime(m.Content); ok && !t.Before(start.Truncate(time.Minute)) {
				at = t
			}
		}
		for _, e := range CursorCLIEvents(m, id, at) {
			a.Add(e)
		}
	}
	return a.Finish()
}

var cursorCLITimestamp = regexp.MustCompile(`<timestamp>([^<]+?) \(UTC([+-]\d{1,2})(?::?(\d{2}))?\)</timestamp>`)

// cursorCLITurnTime reads the time a user turn was sent, from its
// <timestamp> envelope.
func cursorCLITurnTime(content json.RawMessage) (time.Time, bool) {
	m := cursorCLITimestamp.FindSubmatch(content)
	if m == nil {
		return time.Time{}, false
	}
	hours, _ := strconv.Atoi(string(m[2]))
	mins, _ := strconv.Atoi(string(m[3]))
	off := hours*3600 + sign(hours)*mins*60
	t, err := time.ParseInLocation("Monday, Jan 2, 2006, 3:04 PM", string(m[1]), time.FixedZone("", off))
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

func sign(n int) int {
	if n < 0 {
		return -1
	}
	return 1
}

// CursorCLIEvents decodes one message. The person's words are inside
// <user_query>; the other user parts (<user_info>, <system_reminder>) are
// context the CLI adds.
func CursorCLIEvents(m CursorCLIMessage, session string, at time.Time) []Event {
	var parts []cursorCLIPart
	var str string
	if json.Unmarshal(m.Content, &str) == nil {
		parts = []cursorCLIPart{{Type: "text", Text: str}}
	} else if json.Unmarshal(m.Content, &parts) != nil {
		return nil
	}
	var out []Event
	for _, p := range parts {
		switch {
		case m.Role == "user" && p.Type == "text":
			if q, ok := Envelope(p.Text, "user_query"); ok {
				out = append(out, UserText{Text: q})
			} else if trace.IsRequest(p.Text) {
				out = append(out, UserText{Text: p.Text})
			}
		case p.Type == "tool-call":
			c := trace.Call{Session: session, ID: p.ToolCallID, Time: at, Src: trace.CallSource{CallRecord: m.Blob}}
			CursorCLITool(&c, p.ToolName, p.Args)
			out = append(out, ToolCall{Key: p.ToolCallID, Call: c})
		case p.Type == "tool-result":
			out = append(out, ToolResult{Key: p.ToolCallID, Nth: -1, Text: ResultText(p.Result), IsError: p.IsError, Record: m.Blob})
		}
	}
	return out
}

// CursorCLITool decodes the CLI's tool names: Shell is the shell, and
// CallMcpTool / CallDynamicTool are dispatchers naming the MCP server (or
// namespace) and tool.
func CursorCLITool(c *trace.Call, name string, args map[string]json.RawMessage) {
	switch name {
	case "Shell":
		c.Tool, c.Command = "shell", RawString(args["command"])
		return
	case "CallMcpTool":
		if Dispatcher(c, args, "server", "toolName", "arguments") {
			return
		}
	case "CallDynamicTool":
		if Dispatcher(c, Without(args, "mcpDetails"), "namespace", "toolName", "arguments") {
			return
		}
	}
	c.Tool, c.Args, c.RawArgs = name, Flatten(args), RawKeys(args)
}
