package main

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/Telara-Labs/TAP-Runtime/discover/util"
	_ "modernc.org/sqlite"
)

// Agents keep their history in SQLite (OpenCode, Kilo, Crush, Goose,
// Cursor), and discover reads it with the sqlite3 program. A clean Debian or
// Ubuntu desktop has none, so the repeat check found no earlier session and
// tap discover read nothing. The runner reads those stores in process.
func init() { util.InProcessQuery = readSQLite }

// readSQLite runs one query on uri (util.SQLiteURI: read-only, WAL-aware)
// and returns rows as sqlite3 -json does: a JSON array of objects, or
// nothing when there are no rows.
func readSQLite(ctx context.Context, uri, query string) ([]byte, error) {
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			if b, ok := vals[i].([]byte); ok {
				row[c] = string(b)
			} else {
				row[c] = vals[i]
			}
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return json.Marshal(out)
}
