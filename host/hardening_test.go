package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// File modes, prompt text, the relay socket, logging, the install
// script and the VS Code path setting (threat-model gaps G11 to G17).

func TestFilesAPrimitiveWritesAreOwnerOnly(t *testing.T) {
	dir := inDir(t)
	m := &manifest{Files: []fileDecl{{Path: "reports", Access: "write"}}}
	var j bytes.Buffer
	if r := fileOp(m, request{Method: "write", Path: "reports/deep/a.txt", Stdin: "x"}, true, &j); r.Refused != "" || r.Exit != 0 {
		t.Fatalf("%+v", r)
	}
	for path, want := range map[string]os.FileMode{
		filepath.Join(dir, "reports", "deep", "a.txt"): 0o600,
		filepath.Join(dir, "reports", "deep"):          0o700,
	} {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := st.Mode().Perm(); got != want {
			t.Errorf("%s is %v, want %v", path, got, want)
		}
	}
}

func TestTheInterpreterStoreIsPrivate(t *testing.T) {
	body := []byte("an interpreter")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer srv.Close()
	old := interpreters[".sh"]
	defer func() { interpreters[".sh"] = old }()
	interpreters[".sh"] = shInterpreter(srv.URL+"/sh-9.wasm", digest(body), "9")
	store := filepath.Join(t.TempDir(), "interpreters")
	if _, _, _, err := obtain(store, "main.sh"); err != nil {
		t.Fatal(err)
	}
	dst, _ := os.Stat(store)
	fst, _ := os.Stat(filepath.Join(store, "sh-9.wasm"))
	if dst.Mode().Perm() != 0o700 || fst.Mode().Perm() != 0o600 {
		t.Errorf("store %v, file %v", dst.Mode().Perm(), fst.Mode().Perm())
	}
}

func TestTextAProgramWroteIsMadeSafeToShowAPerson(t *testing.T) {
	got := forPrompt("run id\n\nApprove? yes\x1b[2J\x07 and then" + strings.Repeat("x", 400))
	if strings.ContainsAny(got, "\n\x1b\x07") {
		t.Errorf("control characters survived: %q", got)
	}
	if len([]rune(got)) > 360 || !strings.Contains(got, "more characters") {
		t.Errorf("a long text was not cut and marked: %d runes: %.80q", len([]rune(got)), got)
	}
	if forPrompt("write the file out/a.txt") != "write the file out/a.txt" {
		t.Error("an ordinary text was changed")
	}
}

func TestAnAddressIsLoggedWithoutItsQueryButRecordedWhole(t *testing.T) {
	if got := logURL("https://api.example.com/search?q=SECRET&token=T#frag"); got != "https://api.example.com/search" {
		t.Errorf("logged %q", got)
	}
}

func TestALongRelaySocketPathFallsBackToAPrivateDirectory(t *testing.T) {
	deep := filepath.Join(t.TempDir(), strings.Repeat("d", 60), strings.Repeat("e", 60))
	h := newRelayHub(deep, "tap")
	if err := h.listen(); err != nil {
		t.Fatal(err)
	}
	if h.sockDir == "" || filepath.Dir(h.sock) != h.sockDir {
		t.Fatalf("the socket %q is not in a directory of its own (%q)", h.sock, h.sockDir)
	}
	st, err := os.Stat(h.sockDir)
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("the socket directory is %v (%v), want 0700", st, err)
	}
	h.close()
	if _, err := os.Stat(h.sockDir); err == nil {
		t.Error("the socket directory was left behind")
	}
}

func TestServeNoRecordKeepsNoRunRecord(t *testing.T) {
	inDir(t)
	c := startServerArgs(t, true, accept, "--no-record")
	c.run(writePackage(t, writeManifest, writeScript))
	entries, _ := os.ReadDir(c.runs)
	if len(entries) != 0 {
		t.Fatalf("a run left %d record(s) in %s with --no-record", len(entries), c.runs)
	}
	// And with no flag a run is recorded, so the test above can fail.
	c2 := startServerArgs(t, true, accept)
	c2.run(writePackage(t, writeManifest, writeScript))
	if entries, _ := os.ReadDir(c2.runs); len(entries) == 0 {
		t.Fatal("a normal run left no record")
	}
}

func TestTheVSCodePathSettingIsMachineScoped(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot, "vscode", "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pkg struct {
		Contributes struct {
			Configuration struct {
				Properties map[string]struct {
					Scope string `json:"scope"`
				} `json:"properties"`
			} `json:"configuration"`
		} `json:"contributes"`
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		t.Fatal(err)
	}
	if got := pkg.Contributes.Configuration.Properties["tapRuntime.path"].Scope; got != "machine" {
		t.Fatalf("tapRuntime.path has scope %q; a workspace could choose the program the extension runs", got)
	}
}

func TestACommandIsLoggedWithoutItsTrailingArguments(t *testing.T) {
	got := logCommand("kubectl", []string{"get", "secrets", "-o", "jsonpath=TOKEN-VALUE", "--token=SECRET"})
	if strings.Contains(got, "TOKEN-VALUE") || strings.Contains(got, "SECRET") || !strings.Contains(got, "kubectl get secrets") || !strings.Contains(got, "+3 more arguments") {
		t.Fatalf("logged %q", got)
	}
	if logCommand("git", []string{"status"}) != "git status" {
		t.Error("a short command was changed")
	}
}

func TestASourceBuiltInterpreterThatChangesAfterFirstUseIsRefused(t *testing.T) {
	store := t.TempDir()
	path := filepath.Join(store, "sh.wasm")
	os.WriteFile(path, []byte("the interpreter as built"), 0o644)
	if _, _, sum, err := obtain(store, "main.sh"); err != nil || sum != digest([]byte("the interpreter as built")) {
		t.Fatalf("first read: %v", err)
	}
	if _, _, _, err := obtain(store, "main.sh"); err != nil {
		t.Fatalf("an unchanged interpreter was refused: %v", err)
	}
	os.WriteFile(path, []byte("something else"), 0o644)
	if _, _, _, err := obtain(store, "main.sh"); err == nil || !strings.Contains(err.Error(), "first one this runner read") {
		t.Fatalf("a changed interpreter was accepted: %v", err)
	}
	os.Remove(path + ".sha256")
	if _, _, _, err := obtain(store, "main.sh"); err != nil {
		t.Fatalf("removing the remembered digest did not accept a rebuild: %v", err)
	}
}
