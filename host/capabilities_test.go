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

func inDir(t *testing.T) string {
	t.Helper()
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
	return dir
}

func TestFilesAreBoundedByWhatIsDeclared(t *testing.T) {
	dir := inDir(t)
	outside, _ := filepath.EvalSymlinks(t.TempDir())
	os.MkdirAll(filepath.Join(dir, "reports"), 0o755)
	os.MkdirAll(filepath.Join(dir, "inputs"), 0o755)
	os.WriteFile(filepath.Join(dir, "inputs", "a.txt"), []byte("input"), 0o644)
	os.WriteFile(filepath.Join(dir, "reports-private.txt"), []byte("sibling"), 0o644)
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644)
	os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "inputs", "link-out"))
	os.Symlink(outside, filepath.Join(dir, "reports", "dir-out"))

	m := &manifest{Files: []fileDecl{{Path: "inputs", Access: "read"}, {Path: "reports", Access: "write"}}}
	var j bytes.Buffer
	read := func(p string) reply { return fileOp(m, request{Method: "read", Path: p}, true, &j) }
	write := func(p string, approve bool) reply {
		return fileOp(m, request{Method: "write", Path: p, Stdin: "x"}, approve, &j)
	}

	if r := read("inputs/a.txt"); r.Refused != "" || r.Result != "input" {
		t.Fatalf("a declared read failed: %+v", r)
	}
	for name, p := range map[string]string{
		"parent directory":                "inputs/../reports-private.txt",
		"sibling sharing a name prefix":   "reports-private.txt",
		"absolute path outside":           filepath.Join(outside, "secret.txt"),
		"symbolic link to a file outside": "inputs/link-out",
		"system file":                     "/etc/hosts",
	} {
		if r := read(p); r.Refused == "" {
			t.Errorf("%s: read of %s was allowed and returned %q", name, p, r.Result)
		}
	}
	if r := write("inputs/b.txt", true); r.Refused == "" {
		t.Error("a write was allowed under a read declaration")
	}
	if r := write("reports/dir-out/planted.txt", true); r.Refused == "" {
		t.Error("a write through a symbolic link left the declared directory")
	}
	if _, err := os.Stat(filepath.Join(outside, "planted.txt")); err == nil {
		t.Fatal("a file was created outside the declared directory")
	}
	if r := write("reports/new/out.txt", false); r.Refused == "" {
		t.Error("a write ran without approval")
	}
	if _, err := os.Stat(filepath.Join(dir, "reports", "new", "out.txt")); err == nil {
		t.Fatal("an unapproved write created a file")
	}
	for p, want := range map[string]bool{"reports/new/out.txt": true, "inputs/b.txt": false, "/etc/hosts": false} {
		r := fileOp(m, request{Method: "canwrite", Path: p}, true, &j)
		if (r.Refused == "") != want {
			t.Errorf("canwrite %s: refused=%q, want allowed=%v", p, r.Refused, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "reports", "new")); err == nil {
		t.Fatal("asking whether a write is allowed created something")
	}
	if r := fileOp(m, request{Method: "canwrite", Path: "reports/new/out.txt"}, false, &j); r.Refused == "" {
		t.Error("canwrite said yes to a write nobody approved")
	}
	if r := write("reports/new/out.txt", true); r.Refused != "" || r.Exit != 0 {
		t.Fatalf("an approved declared write failed: %+v", r)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "reports", "new", "out.txt")); string(b) != "x" {
		t.Fatalf("wrote %q", b)
	}
	if r := read("reports/new/out.txt"); r.Result != "x" {
		t.Error("write access does not include read")
	}
	if strings.Contains(j.String(), "secret") && strings.Contains(j.String(), `"bytes":6`) {
		t.Error("a refused read recorded content")
	}
}

func TestFetchIsBoundedByOriginAndMethod(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("other origin")) }))
	defer other.Close()
	posts := 0
	declared := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/leave":
			http.Redirect(w, r, other.URL+"/x", http.StatusFound)
		case r.URL.Path == "/stay":
			http.Redirect(w, r, "/data", http.StatusFound)
		case r.Method == "POST":
			posts++
			w.Write([]byte("posted"))
		default:
			w.Write([]byte("data " + r.Header.Get("X-Test")))
		}
	}))
	defer declared.Close()

	m := &manifest{Fetch: []fetchDecl{{Origin: declared.URL, Methods: []string{"GET", "POST"}}}}
	if err := validateCapabilities(m); err != nil {
		t.Fatal(err)
	}
	var j bytes.Buffer
	if r := fetchOp(m, request{URL: declared.URL + "/data", Headers: map[string]string{"X-Test": "h"}}, false, &j); r.Result != "data h" || r.Status != 200 {
		t.Fatalf("a declared GET failed: %+v", r)
	}
	if r := fetchOp(m, request{URL: other.URL + "/x"}, true, &j); r.Refused == "" {
		t.Error("an undeclared origin was fetched")
	}
	if r := fetchOp(m, request{URL: declared.URL + "/data", HTTPMethod: "DELETE"}, true, &j); r.Refused == "" {
		t.Error("an undeclared method was sent")
	}
	if r := fetchOp(m, request{URL: declared.URL + "/stay"}, false, &j); r.Result != "data " {
		t.Errorf("a redirect within the declared origin was not followed: %+v", r)
	}
	if r := fetchOp(m, request{URL: declared.URL + "/leave"}, false, &j); r.Exit == 0 || strings.Contains(r.Result, "other origin") {
		t.Errorf("a redirect left the declared origin: %+v", r)
	}
	if r := fetchOp(m, request{URL: declared.URL + "/data", HTTPMethod: "POST", Stdin: "b"}, false, &j); r.Refused == "" || posts != 0 {
		t.Errorf("a POST ran without approval (posts=%d)", posts)
	}
	if r := fetchOp(m, request{URL: declared.URL + "/data", HTTPMethod: "POST", Stdin: "b"}, true, &j); r.Result != "posted" {
		t.Errorf("an approved POST failed: %+v", r)
	}
	getOnly := &manifest{Fetch: []fetchDecl{{Origin: declared.URL}}}
	if r := fetchOp(getOnly, request{URL: declared.URL + "/data", HTTPMethod: "POST"}, true, &j); r.Refused == "" {
		t.Error("methods did not default to GET only")
	}
}

func TestCapabilityDeclarationsAreValidated(t *testing.T) {
	bad := []manifest{
		{Files: []fileDecl{{Path: "x", Access: "append"}}},
		{Files: []fileDecl{{Access: "read"}}},
		{Fetch: []fetchDecl{{Origin: "api.github.com"}}},
		{Fetch: []fetchDecl{{Origin: "https://*.github.com"}}},
		{Fetch: []fetchDecl{{Origin: "https://api.github.com/repos"}}},
		{Fetch: []fetchDecl{{Origin: "ftp://example.com"}}},
		{Fetch: []fetchDecl{{Origin: "https://user:pw@example.com"}}},
	}
	for i := range bad {
		if err := validateCapabilities(&bad[i]); err == nil {
			t.Errorf("case %d was accepted: %+v", i, bad[i])
		}
	}
}
