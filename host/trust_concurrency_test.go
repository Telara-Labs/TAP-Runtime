package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestTrustChild is not a test. The tests below re-run this binary as a child
// process on the same configuration directory, so two processes really do
// share one trusted.json. It does nothing unless it is given arguments after
// "--".
func TestTrustChild(t *testing.T) {
	args := flag.Args()
	if len(args) < 3 || args[0] != "trust-child" {
		t.Skip("run by TestForgetIsNotUndoneByAConcurrentTrustAdd")
	}
	dir, mode := args[1], args[2]
	userConfigDir = func() (string, error) { return dir, nil }
	switch mode {
	case "add":
		store := newTrustStore()
		for i := 0; i < 150; i++ {
			if err := store.add(fmt.Sprintf("a%03d%s", i, strings.Repeat("0", 60)), "pkg-a", "/p/a"); err != nil {
				t.Fatal(err)
			}
		}
	case "forget":
		var out, errOut bytes.Buffer
		if code := trustCommand([]string{"--forget", "bbbb"}, &out, &errOut); code != 0 {
			t.Fatalf("forget: %d %s", code, errOut.String())
		}
	}
}

func runTrustChild(t *testing.T, dir, mode string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestTrustChild$", "-test.count=1", "--", "trust-child", dir, mode)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

// A forget in a terminal must not be undone by a long-lived runner that adds an
// entry after a prompt, and a reader must never see a half-written store
// (TAP-G08).
func TestForgetIsNotUndoneByAConcurrentTrustAdd(t *testing.T) {
	for round := 0; round < 8; round++ {
		dir := t.TempDir()
		old := userConfigDir
		userConfigDir = func() (string, error) { return dir, nil }
		t.Cleanup(func() { userConfigDir = old })
		store := newTrustStore()
		bDigest := "bbbb" + strings.Repeat("0", 60)
		if err := store.add(bDigest, "pkg-b", "/p/b"); err != nil {
			t.Fatal(err)
		}

		stop := make(chan struct{})
		var bad int
		var readers sync.WaitGroup
		readers.Add(1)
		go func() {
			defer readers.Done()
			path := filepath.Join(dir, "tap", "trusted.json")
			for {
				select {
				case <-stop:
					return
				default:
				}
				b, err := os.ReadFile(path)
				var v map[string]trusted
				if err == nil && json.Unmarshal(b, &v) != nil {
					bad++
				}
			}
		}()

		adder := runTrustChild(t, dir, "add")
		forgetter := runTrustChild(t, dir, "forget")
		if err := adder.Wait(); err != nil {
			t.Fatalf("round %d: adder: %v", round, err)
		}
		if err := forgetter.Wait(); err != nil {
			t.Fatalf("round %d: forgetter: %v", round, err)
		}
		close(stop)
		readers.Wait()

		all := store.load()
		if _, resurrected := all[bDigest]; resurrected {
			t.Fatalf("round %d: a forgotten package was trusted again by a concurrent add", round)
		}
		if len(all) != 150 {
			t.Fatalf("round %d: %d entries after 150 adds and a forget of another package", round, len(all))
		}
		if bad > 0 {
			t.Fatalf("round %d: a reader saw an unparsable trusted.json %d times", round, bad)
		}
	}
}
