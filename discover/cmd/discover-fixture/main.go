// Command discover-fixture captures a reader test fixture from a real
// session on this machine (plan §6.4, TENG-3112). It keeps a slice of the
// session, redacts every string with discover/redact, replaces the home
// directory, and writes the fixture as text: a store.db becomes the SQL that
// rebuilds it, so no binary is checked in.
//
//	discover-fixture cursor-cli  <store.db> <out.sql> [max-calls] [must-keep-call-id...]
//	discover-fixture antigravity <transcript_full.jsonl> <out.jsonl> <step-index>...
//	discover-fixture mcp   (a stdio MCP server for the scripted fixture task)
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"strconv"
	"strings"

	"gitlab.com/telara-labs/tap-runtime/discover/history"
	"gitlab.com/telara-labs/tap-runtime/discover/redact"
	"gitlab.com/telara-labs/tap-runtime/discover/util"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "mcp" {
		fixtureMCPMain()
		return
	}
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: discover-fixture cursor-cli <store.db> <out.sql> [max-calls] [call-id...] | antigravity <transcript.jsonl> <out.jsonl> <step>...")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "cursor-cli":
		err = cursorCLI(os.Args[2], os.Args[3], os.Args[4:])
	case "antigravity":
		err = antigravity(os.Args[2], os.Args[3], os.Args[4:])
	case "sqlite":
		err = sqliteCapture(os.Args[2], os.Args[3], os.Args[4:])
	case "json":
		err = jsonCapture(os.Args[2], os.Args[3])
	case "text":
		err = textCapture(os.Args[2], os.Args[3])
	default:
		err = fmt.Errorf("unknown client %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "discover-fixture:", err)
		os.Exit(1)
	}
}

const maxString = 2000

// scrub redacts credentials and the home directory in every string of a
// JSON value, and shortens long strings.
func scrub(v any, home string) any {
	switch x := v.(type) {
	case string:
		s := redact.Redact(x)
		if home != "" {
			s = strings.ReplaceAll(s, home, "/home/user")
			s = strings.ReplaceAll(s, filepath.Base(home), "user")
		}
		if len(s) > maxString {
			s = s[:maxString] + "…"
		}
		return s
	case map[string]any:
		for k, e := range x {
			x[k] = scrub(e, home)
		}
	case []any:
		for i, e := range x {
			x[i] = scrub(e, home)
		}
	}
	return v
}

func marshal(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimRight(b.Bytes(), "\n")
}

func sqlite(db, sql string) ([]map[string]string, error) {
	out, err := exec.Command("sqlite3", "-readonly", "-json", util.SQLiteURI(db), sql).Output()
	if err != nil {
		return nil, err
	}
	var rows []map[string]string
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil
	}
	return rows, json.Unmarshal(out, &rows)
}

// cursorCLI keeps user messages, and assistant messages up to max calls
// (plus any assistant message making one of the named calls) with every
// result they got. Reasoning parts and provider options are dropped.
func cursorCLI(db, out string, rest []string) error {
	home, _ := os.UserHomeDir()
	max := 12
	if len(rest) > 0 {
		if n, err := strconv.Atoi(rest[0]); err == nil {
			max, rest = n, rest[1:]
		}
	}
	must := map[string]bool{}
	for _, id := range rest {
		must[id] = true
	}
	metaRows, err := sqlite(db, `SELECT value FROM meta`)
	if err != nil || len(metaRows) == 0 {
		return fmt.Errorf("meta: %v", err)
	}
	metaRaw, _ := hex.DecodeString(metaRows[0]["value"])
	var meta map[string]any
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		return err
	}
	rows, err := sqlite(db, `SELECT id, hex(data) AS data FROM blobs`)
	if err != nil {
		return err
	}
	blobs := map[string][]byte{}
	for _, r := range rows {
		blobs[r["id"]], _ = hex.DecodeString(r["data"])
	}
	type msg struct {
		Role    string           `json:"role"`
		ID      string           `json:"id,omitempty"`
		Content []map[string]any `json:"content"`
	}
	var all []msg
	for _, id := range history.CursorCLIOrder(blobs[meta["latestRootBlobId"].(string)]) {
		var raw map[string]any
		if json.Unmarshal(blobs[id], &raw) != nil {
			continue
		}
		m := msg{Role: fmt.Sprint(raw["role"])}
		if id, ok := raw["id"].(string); ok {
			m.ID = id
		}
		switch c := raw["content"].(type) {
		case string:
			m.Content = []map[string]any{{"type": "text", "text": c}}
		case []any:
			for _, p := range c {
				if pm, ok := p.(map[string]any); ok && pm["type"] != "reasoning" {
					delete(pm, "providerOptions")
					delete(pm, "experimental_content")
					m.Content = append(m.Content, pm)
				}
			}
		}
		if m.Role != "system" {
			all = append(all, m)
		}
	}
	keepCall := map[string]bool{}
	calls := 0
	var kept []msg
	for _, m := range all {
		switch m.Role {
		case "user":
			kept = append(kept, m)
		case "assistant":
			var ids []string
			named := false
			for _, p := range m.Content {
				if p["type"] == "tool-call" {
					id := fmt.Sprint(p["toolCallId"])
					ids = append(ids, id)
					named = named || must[id]
				}
			}
			if len(ids) == 0 || (calls >= max && !named) {
				continue
			}
			calls += len(ids)
			for _, id := range ids {
				keepCall[id] = true
			}
			kept = append(kept, m)
		case "tool":
			for _, p := range m.Content {
				if keepCall[fmt.Sprint(p["toolCallId"])] {
					kept = append(kept, m)
					break
				}
			}
		}
	}
	// Rebuild the content-addressed store: each message under its sha256,
	// a root node listing them in field 1, and meta naming the root.
	var sql strings.Builder
	sql.WriteString("CREATE TABLE blobs (id TEXT PRIMARY KEY, data BLOB);\nCREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT);\n")
	var root []byte
	seen := map[string]bool{}
	for _, m := range kept {
		b := marshal(scrub(toAny(m), home))
		sum := sha256.Sum256(b)
		id := hex.EncodeToString(sum[:])
		root = append(root, 0x0a, 32)
		root = append(root, sum[:]...)
		if !seen[id] {
			seen[id] = true
			fmt.Fprintf(&sql, "INSERT INTO blobs VALUES ('%s', '%s');\n", id, strings.ReplaceAll(string(b), "'", "''"))
		}
	}
	rs := sha256.Sum256(root)
	rootID := hex.EncodeToString(rs[:])
	fmt.Fprintf(&sql, "INSERT INTO blobs VALUES ('%s', X'%s');\n", rootID, hex.EncodeToString(root))
	newMeta := map[string]any{"agentId": meta["agentId"], "latestRootBlobId": rootID, "createdAt": meta["createdAt"], "mode": meta["mode"]}
	fmt.Fprintf(&sql, "INSERT INTO meta VALUES ('0', '%s');\n", hex.EncodeToString(marshal(newMeta)))
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "kept %d of %d messages, %d calls\n", len(kept), len(all), calls)
	return os.WriteFile(out, []byte(sql.String()), 0o644)
}

func toAny(v any) any {
	var out any
	_ = json.Unmarshal(marshal(v), &out)
	return out
}

// antigravity keeps the named steps, in file order, redacted.
func antigravity(in, out string, steps []string) error {
	home, _ := os.UserHomeDir()
	want := map[int]bool{}
	for _, s := range steps {
		if a, b, ok := strings.Cut(s, "-"); ok {
			lo, _ := strconv.Atoi(a)
			hi, _ := strconv.Atoi(b)
			for i := lo; i <= hi; i++ {
				want[i] = true
			}
			continue
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return err
		}
		want[n] = true
	}
	f, err := os.Open(in)
	if err != nil {
		return err
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var step map[string]any
		if json.Unmarshal(sc.Bytes(), &step) != nil {
			continue
		}
		idx, _ := step["step_index"].(float64)
		if !want[int(idx)] {
			continue
		}
		delete(step, "thinking")
		lines = append(lines, string(marshal(scrub(step, home))))
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "kept %d steps\n", len(lines))
	if err := os.WriteFile(out, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	return antigravityUsage(in, out, want)
}

// antigravityUsage writes, beside the fixture, the SQL that rebuilds the
// conversation state database's gen_metadata rows for the kept steps. Each
// row is cut down to what the reader uses (token counts and
// last_step_index), so no prompt, id or header leaves the machine.
func antigravityUsage(in, out string, kept map[int]bool) error {
	conv := filepath.Dir(filepath.Dir(filepath.Dir(in)))
	id := filepath.Base(conv)
	src := filepath.Join(filepath.Dir(filepath.Dir(conv)), "conversations", id+".db")
	if _, err := os.Stat(src); err != nil {
		return nil // no state database: the fixture has no usage
	}
	// Copy it with its write-ahead log, so the newest generations are read.
	tmp, err := os.MkdirTemp("", "discover-fixture")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	for _, ext := range []string{"", "-wal", "-shm"} {
		if b, err := os.ReadFile(src + ext); err == nil {
			os.WriteFile(filepath.Join(tmp, "s.db"+ext), b, 0o600)
		}
	}
	rows, err := exec.Command("sqlite3", "-json", "file:"+filepath.Join(tmp, "s.db")+"?immutable=1", `SELECT hex(data) AS data FROM gen_metadata`).Output()
	if err != nil {
		return err
	}
	var recs []struct{ Data string }
	json.Unmarshal(rows, &recs)
	var sql strings.Builder
	sql.WriteString("CREATE TABLE gen_metadata (idx integer PRIMARY KEY, data blob, size integer NOT NULL DEFAULT 0);\n")
	n := 0
	for _, r := range recs {
		b, _ := hex.DecodeString(r.Data)
		last, u, ok := history.AntigravityGeneration(b)
		if !ok || !kept[last+1] {
			continue
		}
		stats := history.ProtoAppend(nil, 2, uint64(u.Fresh))
		stats = history.ProtoAppend(stats, 3, uint64(u.Output))
		if u.Cached > 0 {
			stats = history.ProtoAppend(stats, 5, uint64(u.Cached))
		}
		meta := history.ProtoAppend(nil, 1, "last_step_index")
		meta = history.ProtoAppend(meta, 2, strconv.Itoa(last))
		inner := history.ProtoAppend(history.ProtoAppend(nil, 4, stats), 20, meta)
		row := history.ProtoAppend(nil, 1, inner)
		fmt.Fprintf(&sql, "INSERT INTO gen_metadata VALUES (%d, X'%s', %d);\n", n, hex.EncodeToString(row), len(row))
		n++
	}
	// Laid out as Antigravity does: <root>/brain/<id>/... and <root>/conversations/<id>.
	outConv := filepath.Dir(filepath.Dir(filepath.Dir(out)))
	dest := filepath.Join(filepath.Dir(filepath.Dir(outConv)), "conversations", id+".sql")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "kept usage for %d generations\n", n)
	return os.WriteFile(dest, []byte(sql.String()), 0o644)
}
