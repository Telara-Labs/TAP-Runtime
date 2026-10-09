package author

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestFindBriefsCatchesABriefByNameOrByWhatItSays(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("main.py", "print('ok')\n")
	write("CHANGELOG.md", "## 0.1.0\n\n- First.\n")
	if got, err := FindBriefs(dir); err != nil || len(got) != 0 {
		t.Fatalf("clean package: got %v, %v", got, err)
	}
	write("BRIEF.md", "anything")
	write("notes/context.md", "# Authoring brief x\n\nPrivate: this file holds text from your session history. Do not commit or share it.\n")
	write("data/b.json", `{"kind": "tap.authoring-brief/v1"}`)
	got, err := FindBriefs(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"BRIEF.md", filepath.Join("data", "b.json"), filepath.Join("notes", "context.md")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
