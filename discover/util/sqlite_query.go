package util

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// InProcessQuery, when set, reads a store without the sqlite3 program. A
// clean Linux machine has no sqlite3 (Debian, Ubuntu desktop), and every
// SQLite history reader then read nothing; the runner sets this to a pure-Go
// SQLite. It opens uri (SQLiteURI) and returns sqlite3 -json's shape: a JSON
// array of row objects, or nothing for no rows.
var InProcessQuery func(ctx context.Context, uri, sql string) ([]byte, error)

// InProcess is the bin that tells SQLiteQuery to use InProcessQuery.
const InProcess = "(in-process sqlite)"

// SQLiteBin is the sqlite3 program, or InProcess when it is not installed
// and InProcessQuery is set.
func SQLiteBin() (string, error) {
	if p, err := exec.LookPath("sqlite3"); err == nil {
		return p, nil
	}
	if InProcessQuery != nil {
		return InProcess, nil
	}
	return "", errors.New("sqlite3 is not installed")
}

// SQLiteReadTimeout bounds a store read even when sqlite3 or its pipes stall.
// Keep this in code: it is part of Discover's local-reader contract.
const SQLiteReadTimeout = 30 * time.Second

// SQLiteReadPerGB is added to SQLiteReadTimeout for each gigabyte of the
// store, so a large store is slow, never failed: Cursor keeps 15 GB stores
// whose relevant rows alone take tens of seconds to read.
const SQLiteReadPerGB = 30 * time.Second

// SQLiteDeadline bounds one query of db: SQLiteReadTimeout, plus
// SQLiteReadPerGB for each gigabyte the store and its WAL hold. It still
// ends a read that has stalled.
func SQLiteDeadline(db string) time.Duration {
	var size int64
	for _, p := range []string{db, db + "-wal"} {
		if info, err := os.Stat(p); err == nil {
			size += info.Size()
		}
	}
	return SQLiteReadTimeout + time.Duration(float64(SQLiteReadPerGB)*float64(size)/(1<<30))
}

// SQLiteQuery reads a store with the same WAL-aware URI used by every reader.
// WaitDelay also bounds inherited output pipes after the process is killed.
func SQLiteQuery(ctx context.Context, bin, db, sql string) ([]byte, error) {
	if bin == InProcess && InProcessQuery != nil {
		out, err := InProcessQuery(ctx, SQLiteURI(db), sql)
		if ctx.Err() != nil {
			return nil, fmt.Errorf("sqlite read of %s exceeded its deadline; close the agent or read a snapshot of its store: %w", db, ctx.Err())
		}
		if err != nil {
			return nil, fmt.Errorf("sqlite: %w", err)
		}
		return out, nil
	}
	cmd := exec.CommandContext(ctx, bin, "-readonly", "-json", SQLiteURI(db), sql)
	cmd.WaitDelay = time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("sqlite3 read of %s exceeded its deadline; close the agent or read a snapshot of its store: %w", db, ctx.Err())
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite3: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// SQLiteEach runs sql like SQLiteQuery and passes each result row to row as
// it arrives, so a large result is never held whole: Cursor's tool-call query
// returns hundreds of megabytes. An error from row stops the read and is
// returned.
func SQLiteEach(ctx context.Context, bin, db, sql string, row func(json.RawMessage) error) error {
	if bin == InProcess && InProcessQuery != nil {
		out, err := SQLiteQuery(ctx, bin, db, sql)
		if err != nil {
			return err
		}
		return eachRow(bytes.NewReader(out), row)
	}
	cmd := exec.CommandContext(ctx, bin, "-readonly", "-json", SQLiteURI(db), sql)
	cmd.WaitDelay = time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("sqlite3: %w", err)
	}
	rerr := eachRow(stdout, row)
	if rerr != nil {
		// Stop sqlite3 and drain what it already wrote so Wait returns.
		cmd.Process.Kill()
		io.Copy(io.Discard, stdout)
	}
	werr := cmd.Wait()
	if ctx.Err() != nil {
		return fmt.Errorf("sqlite3 read of %s exceeded its deadline; close the agent or read a snapshot of its store: %w", db, ctx.Err())
	}
	if rerr != nil {
		return rerr
	}
	if werr != nil {
		return fmt.Errorf("sqlite3: %w: %s", werr, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// eachRow decodes sqlite3 -json's output, a JSON array of row objects or
// nothing for no rows, one row at a time.
func eachRow(r io.Reader, row func(json.RawMessage) error) error {
	dec := json.NewDecoder(r)
	tok, err := dec.Token()
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return fmt.Errorf("sqlite3 output: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return fmt.Errorf("sqlite3 output: expected an array of rows")
	}
	for dec.More() {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return fmt.Errorf("sqlite3 output: %w", err)
		}
		if err := row(raw); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil {
		return fmt.Errorf("sqlite3 output: %w", err)
	}
	return nil
}
