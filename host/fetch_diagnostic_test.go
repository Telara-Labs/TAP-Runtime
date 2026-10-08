package main

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// Run the real HTTP transport against owned loopback servers. Only the
// rendered diagnostics lose URL credentials; the private journal retains the
// requested address and original failure for troubleshooting and replay.
func captureFetchDiagnostic(t *testing.T, f func() reply) (reply, string) {
	t.Helper()
	log, err := os.CreateTemp(t.TempDir(), "fetch-log")
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	previous := os.Stderr
	os.Stderr = log
	defer func() { os.Stderr = previous }()
	r := f()
	b, err := os.ReadFile(log.Name())
	if err != nil {
		t.Fatal(err)
	}
	return r, string(b)
}

func assertNoURLSecrets(t *testing.T, diagnostic string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if strings.Contains(diagnostic, secret) {
			t.Errorf("URL secret %q reached diagnostics: %q", secret, diagnostic)
		}
	}
}

func TestHTTPFailureDiagnosticsKeepCauseWithoutURLSecrets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		c.Close()
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL + "/failure?token=QUERY_CANARY#FRAGMENT_CANARY")
	u.User = url.UserPassword("USER_CANARY", "PASSWORD_CANARY")
	m := &manifest{Fetch: []fetchDecl{{Origin: srv.URL}}}
	var journal bytes.Buffer
	r, diagnostic := captureFetchDiagnostic(t, func() reply {
		return fetchOp(m, request{URL: u.String()}, true, &journal)
	})
	if r.Exit != 1 || !strings.Contains(r.Stderr, "EOF") || !strings.Contains(diagnostic, "EOF") || !strings.Contains(diagnostic, srv.URL+"/failure") {
		t.Fatalf("failure cause/path lost: reply=%+v diagnostic=%q", r, diagnostic)
	}
	assertNoURLSecrets(t, diagnostic+r.Stderr, "QUERY_CANARY", "FRAGMENT_CANARY", "USER_CANARY", "PASSWORD_CANARY")
	if !strings.Contains(journal.String(), "QUERY_CANARY") || !strings.Contains(journal.String(), "FRAGMENT_CANARY") || !strings.Contains(journal.String(), "EOF") {
		t.Fatalf("private failure evidence lost: %s", &journal)
	}
}

func TestHTTPRedirectFailureDiagnosticsKeepApprovalBoundary(t *testing.T) {
	var reached atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		fmt.Fprint(w, "unexpected destination")
	}))
	defer other.Close()
	target, _ := url.Parse(other.URL + "/redirect?token=REDIRECT_CANARY#REDIRECT_FRAGMENT")
	target.User = url.UserPassword("REDIRECT_USER", "REDIRECT_PASSWORD")
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.String())
		w.WriteHeader(http.StatusFound)
	}))
	defer first.Close()
	m := &manifest{Fetch: []fetchDecl{{Origin: first.URL}, {Origin: other.URL}}}
	var journal bytes.Buffer
	r, diagnostic := captureFetchDiagnostic(t, func() reply {
		return fetchOp(m, request{URL: first.URL + "/start?token=INITIAL_CANARY#INITIAL_FRAGMENT"}, true, &journal)
	})
	if r.Exit != 1 || reached.Load() != 0 || !strings.Contains(r.Stderr, "approval") || !strings.Contains(diagnostic, "different origin") {
		t.Fatalf("redirect boundary/cause changed: reply=%+v reached=%d diagnostic=%q", r, reached.Load(), diagnostic)
	}
	assertNoURLSecrets(t, diagnostic+r.Stderr, "REDIRECT_CANARY", "REDIRECT_FRAGMENT", "REDIRECT_USER", "REDIRECT_PASSWORD", "INITIAL_CANARY", "INITIAL_FRAGMENT")
	if !strings.Contains(journal.String(), "REDIRECT_CANARY") || !strings.Contains(journal.String(), "INITIAL_CANARY") {
		t.Fatalf("private redirect evidence lost: %s", &journal)
	}
}

func TestMalformedRedirectDiagnosticsDoNotRepeatLocationSecrets(t *testing.T) {
	var target string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target)
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()
	target = srv.URL + "/bad%zz?token=LOCATION_CANARY#LOCATION_FRAGMENT"
	m := &manifest{Fetch: []fetchDecl{{Origin: srv.URL}}}
	var journal bytes.Buffer
	r, diagnostic := captureFetchDiagnostic(t, func() reply {
		return fetchOp(m, request{URL: srv.URL + "/start?token=REQUEST_CANARY"}, true, &journal)
	})
	if r.Exit != 1 || !strings.Contains(diagnostic, "failed to parse Location header") || !strings.Contains(r.Stderr, "invalid URL escape") {
		t.Fatalf("parse cause lost: reply=%+v diagnostic=%q", r, diagnostic)
	}
	assertNoURLSecrets(t, diagnostic+r.Stderr, "LOCATION_CANARY", "LOCATION_FRAGMENT", "REQUEST_CANARY")
	if !strings.Contains(journal.String(), "LOCATION_CANARY") || !strings.Contains(journal.String(), "REQUEST_CANARY") {
		t.Fatalf("private parse evidence lost: %s", &journal)
	}
}

func TestSuccessfulHTTPFetchKeepsRequestAndResult(t *testing.T) {
	seen := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, _ := r.BasicAuth()
		seen <- r.URL.RawQuery + " " + user + " " + password
		fmt.Fprint(w, "successful payload with RESULT_CANARY")
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL + "/success?token=QUERY_CANARY#FRAGMENT_CANARY")
	u.User = url.UserPassword("USER_CANARY", "PASSWORD_CANARY")
	m := &manifest{Fetch: []fetchDecl{{Origin: srv.URL}}}
	var journal bytes.Buffer
	r, diagnostic := captureFetchDiagnostic(t, func() reply {
		return fetchOp(m, request{URL: u.String()}, true, &journal)
	})
	requestSeen := <-seen
	if r.Exit != 0 || r.Status != http.StatusOK || r.Result != "successful payload with RESULT_CANARY" || requestSeen != "token=QUERY_CANARY USER_CANARY PASSWORD_CANARY" {
		t.Fatalf("successful fetch changed: reply=%+v request=%q", r, requestSeen)
	}
	assertNoURLSecrets(t, diagnostic, "QUERY_CANARY", "FRAGMENT_CANARY", "USER_CANARY", "PASSWORD_CANARY", "RESULT_CANARY")
	if !strings.Contains(journal.String(), "QUERY_CANARY") {
		t.Fatalf("private request record lost: %s", &journal)
	}
}

func TestHTTPDiagnosticRenderingPreservesErrorContext(t *testing.T) {
	for _, raw := range []string{
		`https://user:pass@example.com/path?token=SECRET#FRAGMENT`,
		`https://user:pass@example.com/path?token=SECRET with spaces#FRAGMENT`,
		`https://user:pass@example.com/path?token=SECRET"quoted#FRAGMENT`,
	} {
		err := fmt.Errorf("request failed: %w", &url.Error{Op: "Get", URL: raw, Err: errors.New("connection refused")})
		original := err.Error()
		got := logHTTPError(err)
		assertNoURLSecrets(t, got, "user", "pass", "SECRET", "FRAGMENT", "with spaces", "quoted")
		if !strings.Contains(got, "request failed: Get") || !strings.Contains(got, "connection refused") || err.Error() != original {
			t.Fatalf("error context/original changed: got=%q original=%q", got, original)
		}
	}
	if got := logURL("https://example.com/path?"); got != "https://example.com/path" {
		t.Fatalf("bare query retained: %q", got)
	}
	err := fmt.Errorf("failed to parse Location header %q: parse %q: invalid URL escape", "/relative%zz?token=RELATIVE_CANARY#RELATIVE_FRAGMENT", "/relative%zz?token=RELATIVE_CANARY#RELATIVE_FRAGMENT")
	got := logHTTPError(err)
	assertNoURLSecrets(t, got, "RELATIVE_CANARY", "RELATIVE_FRAGMENT")
	if !strings.Contains(got, "failed to parse Location header") || !strings.Contains(got, "invalid URL escape") {
		t.Fatalf("relative Location cause lost: %q", got)
	}
}

func TestShellFetchFailureDoesNotRepeatURLSecretsInGuestSummary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		c.Close()
	}))
	defer srv.Close()
	var result *Result
	var runErr error
	_, diagnostic := captureFetchDiagnostic(t, func() reply {
		result, runErr = runLimited(t, "main.sh", "fetch:\n  - {origin: "+srv.URL+"}\n",
			"tap fetch '"+srv.URL+"/failure?token=SHELL_QUERY_CANARY#SHELL_FRAGMENT_CANARY'\n",
			Options{Approve: func(Ask) Grant { return Grant{OK: true, Limit: 1} }})
		return reply{}
	})
	if runErr != nil || result == nil || result.Exit != 1 || !strings.Contains(result.Stderr, "EOF") || !strings.Contains(diagnostic, "EOF") {
		t.Fatalf("real shell failure changed: result=%+v err=%v diagnostic=%q", result, runErr, diagnostic)
	}
	assertNoURLSecrets(t, diagnostic+result.Stderr, "SHELL_QUERY_CANARY", "SHELL_FRAGMENT_CANARY")
}
