package history

import (
	"sort"
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/discover/client"
)

// The registry's History flag and the reader map are two lists of one fact;
// this keeps them equal.
func TestEveryHistoryClientHasAReaderAndViceVersa(t *testing.T) {
	want := client.IDs(client.HasHistory)
	var got []string
	for id := range Readers {
		got = append(got, id)
		if _, ok := client.Lookup(id); !ok {
			t.Errorf("reader %q has no registry entry", id)
		}
	}
	sort.Strings(want)
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("readers %v, registry clients with history %v", got, want)
	}
	for id, r := range Readers {
		if c := r(t.TempDir()).Client(); c != id {
			t.Errorf("reader under %q reports client %q", id, c)
		}
	}
}

func TestDefaultReadersFollowTheRegistry(t *testing.T) {
	home := t.TempDir()
	rs, err := DefaultReaders(nil, home)
	if err != nil || len(rs) != 0 {
		t.Fatalf("empty home: %d readers, %v", len(rs), err)
	}
	rs, err = DefaultReaders([]string{"claude"}, home)
	if err != nil || len(rs) != 1 || rs[0].Client() != "claude-code" {
		t.Fatalf("alias: %v, %v", rs, err)
	}
	// Every registered agent has a reader now (TENG-3117 to TENG-3121).
	if rs, err := DefaultReaders([]string{"all"}, home); err != nil || len(rs) != len(client.All()) {
		t.Fatalf("all: %d readers for %d agents, %v", len(rs), len(client.All()), err)
	}
	if _, err := DefaultReaders([]string{"nope"}, home); err == nil || !strings.Contains(err.Error(), "unknown client") {
		t.Fatalf("an unknown name: %v", err)
	}
}
