package util

import (
	"context"
	"strings"
	"testing"
)

// A clean Linux machine has no sqlite3; with InProcessQuery set, reads use
// it, opening the same WAL-aware URI.
func TestReadsUseTheInProcessReaderWithoutSqlite3(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	old := InProcessQuery
	t.Cleanup(func() { InProcessQuery = old })
	if _, err := SQLiteBin(); err == nil {
		t.Fatal("found sqlite3 on an empty PATH with no in-process reader")
	}
	var gotURI string
	InProcessQuery = func(_ context.Context, uri, sql string) ([]byte, error) {
		gotURI = uri
		return []byte(`[{"n":1}]`), nil
	}
	bin, err := SQLiteBin()
	if err != nil || bin != InProcess {
		t.Fatalf("bin %q %v", bin, err)
	}
	out, err := SQLiteQuery(context.Background(), bin, "/tmp/store.db", "SELECT 1 AS n")
	if err != nil || string(out) != `[{"n":1}]` || !strings.HasPrefix(gotURI, "file:/tmp/store.db?") {
		t.Fatalf("out %s uri %s err %v", out, gotURI, err)
	}
}
