package main

import (
	"bytes"
	"github.com/Telara-Labs/TAP-Runtime/bridge"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	read := func(p string) reply { return fileOp(bridge.Proc{}, m, request{Method: "read", Path: p}, true, &j) }
	write := func(p string, approve bool) reply {
		return fileOp(bridge.Proc{}, m, request{Method: "write", Path: p, Stdin: "x"}, approve, &j)
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
		r := fileOp(bridge.Proc{}, m, request{Method: "canwrite", Path: p}, true, &j)
		if (r.Refused == "") != want {
			t.Errorf("canwrite %s: refused=%q, want allowed=%v", p, r.Refused, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "reports", "new")); err == nil {
		t.Fatal("asking whether a write is allowed created something")
	}
	if r := fileOp(bridge.Proc{}, m, request{Method: "canwrite", Path: "reports/new/out.txt"}, false, &j); r.Refused == "" {
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
	var j bytes.Buffer
	if r := fetchOp(m, request{URL: declared.URL + "/data", Headers: map[string]string{"X-Test": "h"}}, true, &j); r.Result != "data h" || r.Status != 200 {
		t.Fatalf("a declared GET failed: %+v", r)
	}
	if r := fetchOp(m, request{URL: other.URL + "/x"}, true, &j); r.Refused == "" {
		t.Error("an undeclared origin was fetched")
	}
	if r := fetchOp(m, request{URL: declared.URL + "/data", HTTPMethod: "DELETE"}, true, &j); r.Refused == "" {
		t.Error("an undeclared method was sent")
	}
	if r := fetchOp(m, request{URL: declared.URL + "/stay"}, true, &j); r.Result != "data " {
		t.Errorf("a redirect within the declared origin was not followed: %+v", r)
	}
	if r := fetchOp(m, request{URL: declared.URL + "/leave"}, true, &j); r.Exit == 0 || strings.Contains(r.Result, "other origin") {
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

// A declared path may be a pattern.
func TestFilePatterns(t *testing.T) {
	dir := inDir(t)
	outside, _ := filepath.EvalSymlinks(t.TempDir())
	for _, d := range []string{"reports/2026/q3", "reports-private", "data"} {
		os.MkdirAll(filepath.Join(dir, d), 0o755)
	}
	for _, f := range []string{"reports/a.txt", "reports/a.md", "reports/2026/q3/b.txt", "reports-private/c.txt", "data/x.csv", "data/xy.csv"} {
		os.WriteFile(filepath.Join(dir, f), []byte(f), 0o644)
	}
	os.WriteFile(filepath.Join(outside, "o.txt"), []byte("outside"), 0o644)
	os.Symlink(outside, filepath.Join(dir, "reports", "link"))

	m := &manifest{Files: []fileDecl{
		{Path: "reports/**/*.txt", Access: "read"},
		{Path: "data/?.csv", Access: "write"},
	}}
	var j bytes.Buffer
	for path, want := range map[string]bool{
		"reports/a.txt":         true,
		"reports/2026/q3/b.txt": true,
		"reports/a.md":          false, // the wrong ending
		"reports-private/c.txt": false, // a sibling sharing a prefix
		"reports/../data/x.csv": true,  // resolves to data/x.csv, which is declared
		"data/x.csv":            true,
		"data/xy.csv":           false, // ? is one character
		"reports/link/o.txt":    false, // resolves outside
		"/etc/hosts":            false,
	} {
		r := fileOp(bridge.Proc{}, m, request{Method: "read", Path: path}, true, &j)
		if (r.Refused == "") != want {
			t.Errorf("read %s: refused=%q, want allowed=%v", path, r.Refused, want)
		}
	}
	if r := fileOp(bridge.Proc{}, m, request{Method: "write", Path: "reports/new.txt", Stdin: "x"}, true, &j); r.Refused == "" {
		t.Error("a write was allowed under a read pattern")
	}
	if r := fileOp(bridge.Proc{}, m, request{Method: "write", Path: "data/z.csv", Stdin: "x"}, true, &j); r.Refused != "" {
		t.Errorf("a write under a write pattern was refused: %s", r.Refused)
	}
	if declaredRoot(bridge.Proc{}, m, "data/z.csv") != "data/?.csv" {
		t.Errorf("an approval would name %q", declaredRoot(bridge.Proc{}, m, "data/z.csv"))
	}
}

// One subdomain level may be a wildcard.
func TestFetchSubdomainWildcard(t *testing.T) {
	decls := []fetchDecl{{Origin: "https://*.atlassian.net"}, {Origin: "https://api.github.com:8443", Methods: []string{"GET", "POST"}}}
	for raw, want := range map[string]bool{
		"https://telara.atlassian.net/rest/api/3/issue": true,
		"https://TELARA.atlassian.net/x":                true,
		"https://atlassian.net/x":                       false, // no label
		"https://a.b.atlassian.net/x":                   false, // two levels
		"https://evilatlassian.net/x":                   false,
		"https://telara.atlassian.net.evil.com/x":       false,
		"http://telara.atlassian.net/x":                 false, // the scheme is exact
		"https://telara.atlassian.net:8443/x":           false, // and so is the port
		"https://api.github.com:8443/x":                 true,
		"https://api.github.com/x":                      false,
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := fetchAllowed(decls, "GET", u); got != want {
			t.Errorf("GET %s: allowed=%v, want %v", raw, got, want)
		}
	}
}

// A GET is a read, but its address and headers can carry what the
// program read off this machine. It is gated like a change, and the full
// address is recorded whether it ran or not.
func TestAReadFetchThatCarriesDataOutIsGatedAndRecorded(t *testing.T) {
	seen := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen++; w.Write([]byte("ok")) }))
	defer srv.Close()
	m := &manifest{Fetch: []fetchDecl{{Origin: srv.URL}}}
	var j bytes.Buffer
	leak := srv.URL + "/search?q=SECRET-FILE-CONTENTS"
	r := fetchOp(m, request{URL: leak}, false, &j)
	if !r.Gated || r.Refused == "" || seen != 0 {
		t.Fatalf("a GET carrying data out ran without approval: %+v (server saw %d)", r, seen)
	}
	if !strings.Contains(j.String(), "SECRET-FILE-CONTENTS") || !strings.Contains(j.String(), "gated") {
		t.Errorf("the refused request's address is not in the record: %s", j.String())
	}
	if r := fetchOp(m, request{URL: leak}, true, &j); r.Result != "ok" || seen != 1 {
		t.Errorf("an approved GET did not run: %+v", r)
	}
}

func TestThePersonIsAskedOncePerOriginForReads(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	defer srv.Close()
	var asks []Ask
	res, err := runLimited(t, "main.py", "fetch:\n  - {origin: \""+srv.URL+"\"}\n", `
a = tap.fetch("`+srv.URL+`/one?x=1")
b = tap.fetch("`+srv.URL+`/two?x=2")
print(a["status"], b["status"])
`, Options{Approve: func(a Ask) Grant {
		asks = append(asks, a)
		return Grant{OK: true, Limit: Unlimited}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(asks) != 1 || asks[0].Effect != "read" || !strings.Contains(asks[0].Kind, srv.URL) {
		t.Fatalf("want one question about the origin, got %+v", asks)
	}
	if strings.TrimSpace(res.Stdout) != "200 200" {
		t.Fatalf("got %q (stderr %q)", res.Stdout, res.Stderr)
	}
}

func TestADeclinedOriginIsNotFetched(t *testing.T) {
	seen := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen++; w.Write([]byte("ok")) }))
	defer srv.Close()
	res, err := runLimited(t, "main.py", "fetch:\n  - {origin: \""+srv.URL+"\"}\n", `
try:
    tap.fetch("`+srv.URL+`/x")
    print("fetched")
except PermissionError:
    print("refused")
`, Options{Approve: func(Ask) Grant { return Grant{} }})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Stdout) != "refused" || seen != 0 {
		t.Fatalf("a declined origin was reached: %q, server saw %d", res.Stdout, seen)
	}
}
