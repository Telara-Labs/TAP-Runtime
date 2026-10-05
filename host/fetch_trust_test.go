package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDigestScopedFetchGrantRunsWithoutElicitation(t *testing.T) {
	inDir(t)
	c := startServer(t, false, nil)
	var seen atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen.Add(1); fmt.Fprint(w, "approved-fetch") }))
	defer srv.Close()
	pkg := writePackage(t, "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.test, name: fetch-trust, version: 1.0.0}\nexecution: {entrypoint: main.sh}\nfetch:\n  - {origin: "+srv.URL+"}\n", "tap fetch "+srv.URL+"/probe\n")
	if _, err := readCatalogEntry(pkg, "test"); err != nil {
		t.Fatalf("package: %v", err)
	}
	var out, errOut bytes.Buffer
	if code := trustCommand([]string{pkg}, &out, &errOut); code != 0 {
		t.Fatalf("trust: %d %s", code, &errOut)
	}
	c.run(pkg)
	if seen.Load() != 0 {
		t.Fatal("plain package trust granted network permission")
	}
	if code := trustCommand([]string{"--fetch-origin", srv.URL, pkg}, &out, &errOut); code != 0 {
		t.Fatalf("grant: %d %s", code, &errOut)
	}
	result := c.run(pkg)
	if seen.Load() != 1 || !strings.Contains(result, "approved-fetch") || len(c.asked) != 0 {
		t.Fatalf("headless fetch: seen=%d result=%s asks=%v", seen.Load(), result, c.asked)
	}
	if err := os.WriteFile(filepath.Join(pkg, "main.sh"), []byte("tap fetch "+srv.URL+"/probe\n# changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.run(pkg)
	if seen.Load() != 1 {
		t.Fatal("edited package inherited the fetch grant")
	}
}

func TestApprovedOriginCannotRedirectToAnotherDeclaredOrigin(t *testing.T) {
	var seen atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen.Add(1); fmt.Fprint(w, "leaked") }))
	defer other.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/secret", http.StatusFound)
	}))
	defer first.Close()
	m := &manifest{Fetch: []fetchDecl{{Origin: first.URL}, {Origin: other.URL}}}
	r := fetchOp(m, request{URL: first.URL + "/redirect", HTTPMethod: "GET"}, true, io.Discard)
	if r.Exit == 0 || !strings.Contains(r.Stderr, "approval") || seen.Load() != 0 {
		t.Fatalf("cross-origin approval escaped: %+v seen=%d", r, seen.Load())
	}
}

func TestFetchTrustRejectsUndeclaredOrBroadOriginsAndRevokesGrants(t *testing.T) {
	inDir(t)
	startServer(t, false, nil)
	pkg := writePackage(t, "apiVersion: tap/v3\nkind: Primitive\nmetadata: {publisher: dev.test, name: fetch-trust, version: 1.0.0}\nexecution: {entrypoint: main.sh}\nfetch:\n  - {origin: 'https://*.example.com'}\n", "echo ok\n")
	for _, origin := range []string{"https://other.net", "https://*.example.com", "https://a.example.com/path", "https://a.example.com?", "https://user:pass@a.example.com"} {
		var out, errOut bytes.Buffer
		if trustCommand([]string{"--fetch-origin", origin, pkg}, &out, &errOut) == 0 {
			t.Fatalf("accepted %q", origin)
		}
	}
	var out, errOut bytes.Buffer
	if code := trustCommand([]string{"--fetch-origin", "https://a.example.com", pkg}, &out, &errOut); code != 0 {
		t.Fatalf("exact grant: %d %s", code, &errOut)
	}
	digest, _, _ := packageDigest(pkg)
	if got := newTrustStore().fetchOrigins(digest); len(got) != 1 || got[0] != "https://a.example.com" {
		t.Fatalf("grants: %v", got)
	}
	if code := trustCommand([]string{"--forget", digest}, &out, &errOut); code != 0 {
		t.Fatalf("forget: %d %s", code, &errOut)
	}
	if len(newTrustStore().fetchOrigins(digest)) != 0 {
		t.Fatal("forget retained network permission")
	}
}

func TestStaleCatalogDigestGivesRepairGuidanceAndNeverResolves(t *testing.T) {
	entries := []catalogEntry{{Ref: "local/test@1", Digest: strings.Repeat("a", 64)}}
	got, err := resolveCatalog(entries, "local/test@1", strings.Repeat("b", 64))
	if err == nil || got.Path != "" || !strings.Contains(err.Error(), "tap_load") || !strings.Contains(err.Error(), "migrate-saved") {
		t.Fatalf("stale identity: %+v %v", got, err)
	}
}
