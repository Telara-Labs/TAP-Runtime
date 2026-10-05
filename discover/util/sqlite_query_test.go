package util

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSQLiteQueryKillsAStalledProcess(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	// A real child process that never produces a result. No shell, environment
	// switch, or live user database is needed to exercise process cancellation.
	if err := os.WriteFile(src, []byte("package main\nimport \"time\"\nfunc main() { time.Sleep(time.Hour) }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "stalled-sqlite.exe")
	if b, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("build child: %v %s", err, b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := SQLiteQuery(ctx, bin, filepath.Join(dir, "store.db"), "SELECT 1")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "snapshot") {
		t.Fatalf("timeout error: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("cancellation waited %s", elapsed)
	}
}

func TestSQLiteQueryReadsRealStoreAndPreservesSchemaErrors(t *testing.T) {
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 unavailable")
	}
	db := filepath.Join(t.TempDir(), "store.db")
	if b, err := exec.Command(bin, db, "CREATE TABLE item(x); INSERT INTO item VALUES(42);").CombinedOutput(); err != nil {
		t.Fatalf("create: %v %s", err, b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), SQLiteReadTimeout)
	defer cancel()
	b, err := SQLiteQuery(ctx, bin, db, "SELECT x FROM item")
	if err != nil || !strings.Contains(string(b), "42") {
		t.Fatalf("read: %s %v", b, err)
	}
	if _, err := SQLiteQuery(ctx, bin, db, "SELECT * FROM missing"); err == nil || !strings.Contains(err.Error(), "no such table") {
		t.Fatalf("schema error: %v", err)
	}
}
