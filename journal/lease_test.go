package journal

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestASecondResumerIsRefusedAndToldWhoHoldsTheRun(t *testing.T) {
	root := t.TempDir()
	first, err := Create(root, header("run-lease-1"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Open(root, "run-lease-1")
	var held *HeldError
	if !errors.As(err, &held) {
		t.Fatalf("a second process continued a run that is held: %v", err)
	}
	if held.PID != os.Getpid() || !strings.Contains(err.Error(), "held by process") {
		t.Fatalf("the refusal does not name the holder: %v", err)
	}
	// Released, the run is anybody's.
	first.Close()
	second, err := Open(root, "run-lease-1")
	if err != nil {
		t.Fatalf("a released run could not be continued: %v", err)
	}
	second.Close()
	if _, err := os.Stat(filepath.Join(root, "run-lease-1", "lease.json")); err == nil {
		t.Fatal("a released lease was left behind")
	}
}

func writeLease(t *testing.T, root, run string, pid int, expires time.Time) {
	t.Helper()
	b, _ := json.Marshal(leaseRecord{PID: pid, Token: "someone-else", Expires: expires})
	if err := os.WriteFile(filepath.Join(root, run, "lease.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// deadPID is the id of a process that has exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skip("cannot start a process to let it die")
	}
	return cmd.Process.Pid
}

func TestALeaseDoesNotOutliveItsHolder(t *testing.T) {
	root := t.TempDir()
	j, _ := Create(root, header("run-lease-2"))
	j.Close()

	// Held by a live process, not expired: refused.
	writeLease(t, root, "run-lease-2", os.Getpid(), time.Now().Add(time.Minute))
	if _, err := Open(root, "run-lease-2"); err == nil {
		t.Fatal("a run held by a live process was continued")
	}
	// The same holder, expired: taken.
	writeLease(t, root, "run-lease-2", os.Getpid(), time.Now().Add(-time.Second))
	j, err := Open(root, "run-lease-2")
	if err != nil {
		t.Fatalf("an expired lease held the run: %v", err)
	}
	j.Close()
	// Not expired, and its holder has exited: taken.
	writeLease(t, root, "run-lease-2", deadPID(t), time.Now().Add(time.Minute))
	j, err = Open(root, "run-lease-2")
	if err != nil {
		t.Fatalf("a lease whose holder has exited held the run: %v", err)
	}
	j.Close()
	// A lease that cannot be read is nobody's.
	os.WriteFile(filepath.Join(root, "run-lease-2", "lease.json"), []byte("{half"), 0o600)
	j, err = Open(root, "run-lease-2")
	if err != nil {
		t.Fatalf("an unreadable lease held the run: %v", err)
	}
	j.Close()
}

func TestAHolderRenewsAndDoesNotReleaseAnothersLease(t *testing.T) {
	root := t.TempDir()
	j, _ := Create(root, header("run-lease-3"))
	path := filepath.Join(root, "run-lease-3", "lease.json")
	before, _ := readLease(path)
	j.lease.renew(time.Now().Add(time.Hour))
	after, _ := readLease(path)
	if !after.Expires.After(before.Expires) {
		t.Fatal("renewing did not extend the lease")
	}
	// Somebody else took it over. Closing must leave theirs alone.
	writeLease(t, root, "run-lease-3", os.Getpid(), time.Now().Add(time.Minute))
	j.Close()
	if held, err := readLease(path); err != nil || held.Token != "someone-else" {
		t.Fatalf("closing removed a lease that was not this holder's: %v", err)
	}
}

func TestSweep(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	age := func(run string, days int) {
		when := now.Add(-time.Duration(days) * 24 * time.Hour)
		filepath.Walk(filepath.Join(root, run), func(p string, _ os.FileInfo, _ error) error {
			return os.Chtimes(p, when, when)
		})
	}
	for _, run := range []string{"run-old-0001", "run-recent-01", "run-held-0001", "run-edge-0001"} {
		j, err := Create(root, header(run))
		if err != nil {
			t.Fatal(err)
		}
		j.Close()
	}
	age("run-old-0001", 45)
	age("run-recent-01", 3)
	age("run-held-0001", 45)
	age("run-edge-0001", 29)
	writeLease(t, root, "run-held-0001", os.Getpid(), now.Add(time.Minute))
	// Something in the directory that is not a run record.
	os.MkdirAll(filepath.Join(root, "not-a-run-dir"), 0o755)
	os.WriteFile(filepath.Join(root, "not-a-run-dir", "keep.txt"), []byte("x"), 0o644)
	age("not-a-run-dir", 400)

	if n := Sweep(root, 0, now); n != 0 {
		t.Fatalf("0 days keeps everything, and %d were removed", n)
	}
	if n := Sweep(root, 30, now); n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
	for run, want := range map[string]bool{"run-old-0001": false, "run-recent-01": true, "run-held-0001": true, "run-edge-0001": true, "not-a-run-dir": true} {
		_, err := os.Stat(filepath.Join(root, run))
		if (err == nil) != want {
			t.Errorf("%s: kept=%v, want %v", run, err == nil, want)
		}
	}
}
