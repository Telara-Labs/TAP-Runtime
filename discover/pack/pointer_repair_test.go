package pack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExplicitMigrationRepairsStalePointerWithoutChangingPackage(t *testing.T) {
	home, coll := t.TempDir(), t.TempDir()
	pkg := savePrimitive(t, coll, "close-stale", "1.0.0")
	rs, err := WritePointers(pkg, []Target{target(t, "claude-code", false)}, false, home, "")
	if err != nil {
		t.Fatal(err)
	}
	oldID, _ := ReadIdentity(pkg)
	program := []byte("echo updated\n")
	if err := os.WriteFile(filepath.Join(pkg, "main.sh"), program, 0o600); err != nil {
		t.Fatal(err)
	}
	id, _ := ReadIdentity(pkg)
	if id.Digest == oldID.Digest {
		t.Fatal("program edit did not change digest")
	}
	repaired, err := MigrateSaved(home, "", coll)
	if err != nil || len(repaired) != 1 || repaired[0].Mode != PointerWritten {
		t.Fatalf("repair: %+v %v", repaired, err)
	}
	b, _ := os.ReadFile(filepath.Join(rs[0].Path, "SKILL.md"))
	if !strings.Contains(string(b), id.Digest) || strings.Contains(string(b), oldID.Digest) {
		t.Fatalf("pointer did not refresh: %s", b)
	}
	after, _ := os.ReadFile(filepath.Join(pkg, "main.sh"))
	if string(after) != string(program) {
		t.Fatal("repair changed the package")
	}
	again, err := MigrateSaved(home, "", coll)
	if err != nil || len(again) != 0 {
		t.Fatalf("second repair: %+v %v", again, err)
	}
}

func TestPointerRepairPreservesOtherVersionsAndAdditionalFiles(t *testing.T) {
	for _, extra := range []bool{false, true} {
		t.Run(map[bool]string{false: "different-version", true: "extra-file"}[extra], func(t *testing.T) {
			home, coll := t.TempDir(), t.TempDir()
			pkg := savePrimitive(t, coll, "close-stale", "1.0.0")
			rs, _ := WritePointers(pkg, []Target{target(t, "claude-code", false)}, false, home, "")
			before, _ := os.ReadFile(filepath.Join(rs[0].Path, "SKILL.md"))
			if extra {
				os.WriteFile(filepath.Join(rs[0].Path, "notes.txt"), []byte("keep"), 0o600)
			} else {
				savePrimitive(t, coll, "close-stale", "2.0.0")
			}
			repaired, err := MigrateSaved(home, "", coll)
			if err != nil || len(repaired) != 1 || repaired[0].Mode != PointerSkipped {
				t.Fatalf("repair: %+v %v", repaired, err)
			}
			after, _ := os.ReadFile(filepath.Join(rs[0].Path, "SKILL.md"))
			if string(after) != string(before) {
				t.Fatal("conflicting pointer overwritten")
			}
		})
	}
}
