package history

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestAiderDirectoryCacheSeesAliasedCreatesAndDeletes(t *testing.T) {
	for _, initialHistory := range []bool{false, true} {
		name := "create"
		if initialHistory {
			name = "delete"
		}
		t.Run(name, func(t *testing.T) {
			useTempCache(t)
			home := t.TempDir()
			dir := filepath.Join(home, "project")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(dir, ".aider.chat.history.md")
			if initialHistory {
				putAiderHistory(t, file)
			}
			// A future stamp keeps this alias test inside the recent window
			// even if a loaded runner pauses. Clock-skewed directories also
			// must not supply reusable snapshots.
			stamp := setAiderDirectoryTime(t, dir, time.Now().Add(time.Hour))
			c := openUnitCache("aider-walk")
			assertAiderWalkEqual(t, home, c)
			if initialHistory {
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
			} else {
				putAiderHistory(t, file)
			}
			if got := setAiderDirectoryTime(t, dir, stamp); !got.Equal(stamp) {
				t.Fatalf("fixture did not preserve the timestamp: %v -> %v", stamp, got)
			}
			assertAiderWalkEqual(t, home, c)
		})
	}
}

func TestAiderRecentSnapshotIsInvalidAfterAging(t *testing.T) {
	for _, initialHistory := range []bool{false, true} {
		t.Run(map[bool]string{false: "create", true: "delete"}[initialHistory], func(t *testing.T) {
			useTempCache(t)
			home := t.TempDir()
			dir := filepath.Join(home, "project")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(dir, ".aider.chat.history.md")
			if initialHistory {
				putAiderHistory(t, file)
			}
			stamp := setAiderDirectoryTime(t, dir, time.Unix(1700000000, 0))
			setAiderDirectoryTime(t, home, stamp)
			c := openUnitCache("aider-walk")
			aiderHistoriesCachedAt(home, 6, c, stamp.Add(time.Second))
			if entry, ok := c.dir(dir); !ok || entry.Stable {
				t.Fatalf("recent directory snapshot is reusable: %+v", entry)
			}
			// Persist the active snapshot, alias its mutation, then reopen
			// after the window. Passing time cannot legitimize stale data.
			c.save()
			if initialHistory {
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
			} else {
				putAiderHistory(t, file)
			}
			setAiderDirectoryTime(t, dir, stamp)
			c = openUnitCache("aider-walk")
			if got, want := aiderHistoriesCachedAt(home, 6, c, stamp.Add(3*time.Second)), aiderHistories(home, 6); !reflect.DeepEqual(got, want) {
				t.Fatalf("aged aliased snapshot reused: cached %v, plain %v", got, want)
			}
			if entry, ok := c.dir(dir); !ok || !entry.Stable {
				t.Fatalf("fresh stable directory snapshot was not retained: %+v", entry)
			}
		})
	}
}

func TestAiderStableDirectoryCacheReusesPersistedListings(t *testing.T) {
	useTempCache(t)
	reads := readsOf(t)
	home := t.TempDir()
	dir := filepath.Join(home, "project")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, ".aider.chat.history.md")
	putAiderHistory(t, file)
	stamp := setAiderDirectoryTime(t, dir, time.Unix(1700000000, 0))
	setAiderDirectoryTime(t, home, stamp)
	c := openUnitCache("aider-walk")
	if got := aiderHistoriesCached(home, 6, c); !reflect.DeepEqual(got, []string{file}) {
		t.Fatalf("cold search %v", got)
	}
	if got := reads("aider-walk"); !reflect.DeepEqual(got, []string{home, dir}) {
		t.Fatalf("cold search did not enumerate the actual directories: %v", got)
	}
	if got := aiderHistoriesCached(home, 6, c); !reflect.DeepEqual(got, []string{file}) {
		t.Fatalf("warm search changed: %v", got)
	}
	if got := reads("aider-walk"); len(got) != 0 {
		t.Fatalf("warm cache enumerated directories again: %v", got)
	}
	c.save()
	c = openUnitCache("aider-walk")
	if entry, ok := c.dir(dir); !ok || !entry.Stable {
		t.Fatalf("stable status did not survive cache reopen: %+v", entry)
	}
	if got := aiderHistoriesCached(home, 6, c); !reflect.DeepEqual(got, []string{file}) {
		t.Fatalf("persisted search changed: %v", got)
	}
	if got := reads("aider-walk"); len(got) != 0 {
		t.Fatalf("persisted stable cache enumerated directories: %v", got)
	}
}

func putAiderHistory(t *testing.T, file string) {
	t.Helper()
	if err := os.WriteFile(file, []byte("synthetic history"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func setAiderDirectoryTime(t *testing.T, dir string, stamp time.Time) time.Time {
	t.Helper()
	if err := os.Chtimes(dir, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime()
}

func assertAiderWalkEqual(t *testing.T, home string, c *unitCache) {
	t.Helper()
	if got, want := aiderHistoriesCached(home, 6, c), aiderHistories(home, 6); !reflect.DeepEqual(got, want) {
		t.Fatalf("cached search %v, plain walk %v", got, want)
	}
}
