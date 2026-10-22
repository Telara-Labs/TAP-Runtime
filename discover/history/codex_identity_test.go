package history

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/discover/trace"
)

// A sub-rollout that opens with its parent's meta is named by its own file
// whether or not its parent is read with it.
func TestCodexSessionIdentityDoesNotDependOnWhatElseIsRead(t *testing.T) {
	const parentID, childID = "0199bbbb-0000-7000-8000-00000000000a", "0199bbbb-0000-7000-8000-00000000000b"
	parentFile := "rollout-2026-10-01T10-00-00-" + parentID
	childFile := "rollout-2026-10-01T11-00-00-" + childID
	call := map[string]any{"timestamp": "2026-10-01T10:00:01Z", "type": "response_item", "payload": map[string]any{"type": "function_call", "name": "exec_command", "arguments": `{"cmd":"ls"}`, "call_id": "c"}}
	write := func(dir string, files ...string) {
		for _, f := range files {
			writeJSONL(t, filepath.Join(dir, "2026", "10", "01", f+".jsonl"), []map[string]any{
				{"timestamp": "2026-10-01T10:00:00Z", "type": "session_meta", "payload": map[string]any{"id": parentID}}, call,
			})
		}
	}
	read := func(dir string, o *ReadOptions) map[string]trace.Session {
		out := map[string]trace.Session{}
		y := func(s trace.Session) error { out[s.ID] = s; return nil }
		var err error
		if o == nil {
			err = Codex{Dir: dir}.Each(time.Time{}, y)
		} else {
			err = Codex{Dir: dir}.EachWith(time.Time{}, *o, y)
		}
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	ids := func(m map[string]trace.Session) []string {
		var out []string
		for id, s := range m {
			for _, c := range s.Calls {
				if c.Session != id {
					t.Errorf("call names session %q in session %q", c.Session, id)
				}
			}
			out = append(out, id)
		}
		sort.Strings(out)
		return out
	}
	alone, both := t.TempDir(), t.TempDir()
	write(alone, childFile)
	write(both, parentFile, childFile)
	for _, tc := range []struct {
		name string
		dir  string
		want []string
	}{
		{"child alone", alone, []string{childFile}},
		{"parent and child", both, []string{parentID, childFile}},
	} {
		if got := ids(read(tc.dir, nil)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: sessions %v, want %v", tc.name, got, tc.want)
		}
	}
	got := read(alone, &ReadOptions{})
	if p := got[childFile].Parent; p == nil || *p != (trace.SessionLink{Relation: "fork", Session: parentID}) {
		t.Errorf("parent = %+v", p)
	}
	if read(both, &ReadOptions{})[parentID].Parent != nil {
		t.Error("the parent rollout has a parent")
	}
	// A tail of the child alone names it the same way.
	path := filepath.Join(alone, "2026", "10", "01", childFile+".jsonl")
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	s, _, err := TailCodexFile(path, nil, ReadOptions{})
	if err != nil || s.ID != childFile {
		t.Fatalf("tail id %q: %v", s.ID, err)
	}
}
