//go:build !windows

package rebuild

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCanceledBuildTerminatesCompilerDescendants(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := (Here{}).Run(ctx, dir, "(sleep 1; touch child-survived) & wait")
	if err == nil {
		t.Fatal("canceled build succeeded")
	}
	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "child-survived")); !os.IsNotExist(err) {
		t.Fatalf("compiler descendant survived: %v", err)
	}
}
