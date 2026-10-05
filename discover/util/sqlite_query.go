package util

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// SQLiteReadTimeout bounds a store read even when sqlite3 or its pipes stall.
// Keep this in code: it is part of Discover's local-reader contract.
const SQLiteReadTimeout = 30 * time.Second

// SQLiteQuery reads a store with the same WAL-aware URI used by every reader.
// WaitDelay also bounds inherited output pipes after the process is killed.
func SQLiteQuery(ctx context.Context, bin, db, sql string) ([]byte, error) {
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
