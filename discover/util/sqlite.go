package util

import (
	"os"
	"strings"
)

// SQLiteURI is the URI that opens an agent's SQLite store read-only for the
// system sqlite3. Agents keep their stores in WAL mode, where
// recent writes, and on a fresh store the whole schema, live only in the
// -wal file until a checkpoint. immutable=1 reads the main file alone and
// misses them, so a store with a non-empty -wal is opened mode=ro, which
// reads the WAL and never writes the store. A store without one keeps
// immutable=1: nothing to miss, and no lock is taken.
func SQLiteURI(db string) string {
	path := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(db)
	if fi, err := os.Stat(db + "-wal"); err == nil && fi.Size() > 0 {
		return "file:" + path + "?mode=ro"
	}
	return "file:" + path + "?immutable=1"
}
