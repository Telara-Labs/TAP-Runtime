package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/discover/util"
)

// The in-process reader returns what sqlite3 -json returns, through the
// read-only URI the history readers use, including rows still in the WAL.
func TestInProcessSQLiteReadsLikeSqlite3Json(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	w, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, q := range []string{"PRAGMA journal_mode=WAL", "PRAGMA wal_autocheckpoint=0", "CREATE TABLE s (id TEXT, n INTEGER, body TEXT)", "INSERT INTO s VALUES ('a', 1, 'first'), ('b', 2, NULL)"} {
		if _, err := w.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	out, err := readSQLite(context.Background(), util.SQLiteURI(path), "SELECT id, n, body FROM s ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(out, &rows); err != nil || len(rows) != 2 || rows[0]["id"] != "a" || rows[0]["n"] != float64(1) || rows[1]["body"] != nil {
		t.Fatalf("rows %s %v", out, err)
	}
	if out, err := readSQLite(context.Background(), util.SQLiteURI(path), "SELECT id FROM s WHERE n > 5"); err != nil || out != nil {
		t.Fatalf("no rows = %s %v", out, err)
	}
}
