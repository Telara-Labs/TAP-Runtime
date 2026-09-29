package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestObtainRefusesAlteredInterpreter(t *testing.T) {
	store := t.TempDir()
	in := interpreters[".js"]
	if err := os.WriteFile(filepath.Join(store, in.File), []byte("not the pinned file"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := obtain(store, "main.js")
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("altered interpreter was not refused: %v", err)
	}
}

func TestObtainNamesTheBuildForAnUnpublishedInterpreter(t *testing.T) {
	_, _, _, err := obtain(t.TempDir(), "main.sh")
	if err == nil || !strings.Contains(err.Error(), "go build") {
		t.Fatalf("want an error naming the build command, got: %v", err)
	}
}

// A released runner knows where its bash-compatible interpreter is and what
// it must be. It fetches it once, and refuses one that is not the pinned file.
func TestAReleasedRunnerFetchesItsShInterpreter(t *testing.T) {
	body := []byte("the interpreter of this release")
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write(body)
	}))
	defer srv.Close()
	old := interpreters[".sh"]
	defer func() { interpreters[".sh"] = old }()

	interpreters[".sh"] = shInterpreter(srv.URL+"/sh-1.2.3.wasm", digest(body), "1.2.3")
	store := t.TempDir()
	for i := 0; i < 2; i++ {
		got, in, _, err := obtain(store, "main.sh")
		if err != nil || string(got) != string(body) || in.File != "sh-1.2.3.wasm" {
			t.Fatalf("read %d: %q %q %v", i, got, in.File, err)
		}
	}
	if hits != 1 {
		t.Errorf("the interpreter was fetched %d times; the second read should come from the store", hits)
	}

	interpreters[".sh"] = shInterpreter(srv.URL+"/sh-1.2.3.wasm", digest([]byte("something else")), "1.2.3")
	empty := t.TempDir()
	if _, _, _, err := obtain(empty, "main.sh"); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("a download that is not the pinned file was not refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(empty, "sh-1.2.3.wasm")); err == nil {
		t.Error("a refused download was kept in the store")
	}
}

func TestObtainRefusesUnknownEntrypoint(t *testing.T) {
	if _, _, _, err := obtain(t.TempDir(), "main.rb"); err == nil {
		t.Fatal("an entrypoint with no listed interpreter was accepted")
	}
}

func TestEveryPublishedInterpreterIsPinned(t *testing.T) {
	for ext, in := range interpreters {
		if in.URL != "" && len(in.SHA256) != 64 {
			t.Errorf("%s has a URL and no sha256", ext)
		}
		if in.URL == "" && in.Build == "" {
			t.Errorf("%s has neither a URL nor a build command", ext)
		}
	}
}

// fetch fills the store, so that a run with no network downloads nothing.
func TestFetchFillsTheStore(t *testing.T) {
	files := map[string][]byte{"/py.wasm": []byte("python"), "/js.wasm": []byte("javascript"), "/sh.wasm": []byte("shell")}
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write(files[r.URL.Path])
	}))
	old := interpreters
	defer func() { interpreters = old }()
	js := interpreter{Kind: "js", File: "js.wasm", URL: srv.URL + "/js.wasm", SHA256: digest(files["/js.wasm"])}
	ts := js
	ts.Kind = "ts"
	interpreters = map[string]interpreter{
		".py": {Kind: "py", File: "py.wasm", URL: srv.URL + "/py.wasm", SHA256: digest(files["/py.wasm"])},
		".js": js, ".ts": ts,
		".sh": shInterpreter(srv.URL+"/sh.wasm", digest(files["/sh.wasm"]), "9.9.9"),
	}
	store := t.TempDir()
	var out, errb bytes.Buffer
	if code := fetchCommand([]string{"--interpreters", store}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if hits != 3 {
		t.Errorf("%d downloads for three files", hits)
	}
	srv.Close() // the network is gone
	for _, entry := range []string{"main.py", "main.js", "main.ts", "main.sh"} {
		if _, _, _, err := obtain(store, entry); err != nil {
			t.Errorf("%s needed the network after fetch: %v", entry, err)
		}
	}
}

func TestADownloadRefusedByCodexSaysSo(t *testing.T) {
	old := interpreters[".js"]
	defer func() { interpreters[".js"] = old }()
	in := old
	in.URL = "http://127.0.0.1:1/nothing-listens-here"
	interpreters[".js"] = in

	_, _, _, err := obtain(t.TempDir(), "main.js")
	if err == nil || strings.Contains(err.Error(), "Codex") {
		t.Fatalf("outside Codex the error should not name it: %v", err)
	}
	t.Setenv("CODEX_SANDBOX_NETWORK_DISABLED", "1")
	_, _, _, err = obtain(t.TempDir(), "main.js")
	if err == nil || !strings.Contains(err.Error(), "tap fetch") {
		t.Fatalf("inside Codex's shell the error should say what to do: %v", err)
	}
}
