package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"testing"
)

// Declarations a person could not mean, and where a declared
// origin may actually be reached.

func TestDeclarationsThatCannotBeMeantAreRefused(t *testing.T) {
	cases := []struct {
		name string
		m    manifest
		want string // empty: accepted
	}{
		{"the file system root", manifest{Files: []fileDecl{{Path: "/", Access: "read"}}}, "root of the file system"},
		{"the root, spelled differently", manifest{Files: []fileDecl{{Path: "//", Access: "write"}}}, "root of the file system"},
		{"a path that leaves the directory", manifest{Files: []fileDecl{{Path: "../secrets", Access: "read"}}}, "leaves the directory"},
		{"a directory", manifest{Files: []fileDecl{{Path: "out", Access: "write"}}}, ""},
		{"an absolute directory", manifest{Files: []fileDecl{{Path: "/tmp/reports", Access: "write"}}}, ""},
		{"plain http to a remote host", manifest{Fetch: []fetchDecl{{Origin: "http://api.example.com"}}}, "not https"},
		{"plain http to localhost", manifest{Fetch: []fetchDecl{{Origin: "http://localhost:8080"}}}, ""},
		{"plain http to loopback", manifest{Fetch: []fetchDecl{{Origin: "http://127.0.0.1:9000"}}}, ""},
		{"the cloud metadata address", manifest{Fetch: []fetchDecl{{Origin: "https://169.254.169.254"}}}, "no primitive may reach"},
		{"the unspecified address", manifest{Fetch: []fetchDecl{{Origin: "https://0.0.0.0"}}}, "no primitive may reach"},
		{"an https host", manifest{Fetch: []fetchDecl{{Origin: "https://api.github.com"}}}, ""},
		{"a wildcard host", manifest{Fetch: []fetchDecl{{Origin: "https://*.atlassian.net"}}}, ""},
	}
	if runtime.GOOS == "windows" {
		for _, path := range []string{`C:\`, `C:/`, `\\server\share`, `//server/share`, `\\server\share\`, `\\?\C:\`, `\\.\C:\`} {
			cases = append(cases, struct {
				name string
				m    manifest
				want string
			}{"Windows volume root " + path, manifest{Files: []fileDecl{{Path: path, Access: "read"}}}, "root of the file system"})
		}
		for _, path := range []string{`C:\reports`, `\\server\share\reports`, `\\?\C:\reports`, `C:reports`} {
			cases = append(cases, struct {
				name string
				m    manifest
				want string
			}{"Windows directory " + path, manifest{Files: []fileDecl{{Path: path, Access: "read"}}}, ""})
		}
	}
	for _, c := range cases {
		got := strings.Join(declarationProblems(&c.m), "; ")
		if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestANameThatResolvesToTheMachineItselfIsNotReached(t *testing.T) {
	for _, c := range []struct {
		host string
		ip   string
		ok   bool
	}{
		{"api.example.com", "93.184.216.34", true},
		{"api.example.com", "127.0.0.1", false},       // DNS pointed at this machine
		{"api.example.com", "10.0.0.5", false},        // or at its network
		{"api.example.com", "169.254.169.254", false}, // or at a metadata service
		{"127.0.0.1", "127.0.0.1", true},              // declared as an address, deliberately
		{"localhost", "127.0.0.1", true},
		{"192.168.1.20", "192.168.1.20", true},
		{"169.254.169.254", "169.254.169.254", false}, // never, however it is named
	} {
		err := reachable(c.host, net.ParseIP(c.ip))
		if (err == nil) != c.ok {
			t.Errorf("%s -> %s: err = %v, want ok = %v", c.host, c.ip, err, c.ok)
		}
	}
}

func TestADeclaredLoopbackOriginStillWorks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hello")) }))
	defer srv.Close()
	m := &manifest{Fetch: []fetchDecl{{Origin: srv.URL}}}
	if p := declarationProblems(m); len(p) != 0 {
		t.Fatalf("a loopback origin was refused: %v", p)
	}
	var j strings.Builder
	if r := fetchOp(m, request{URL: srv.URL + "/x"}, true, &j); r.Result != "hello" {
		t.Fatalf("a declared loopback origin was not reached: %+v", r)
	}
}

func TestARunWithAnUnmeantDeclarationIsRefusedBeforeItStarts(t *testing.T) {
	_, err := runLimited(t, "main.sh", "files:\n  - {path: /, access: read}\n", "echo hi\n", Options{})
	if err == nil || !strings.Contains(err.Error(), "root of the file system") {
		t.Fatalf("got %v", err)
	}
}

// Found testing: the guard set the proxy to nil, so a primitive behind a
// corporate proxy could not fetch at all. A proxy from the environment is used,
// and a name that this machine resolves to its own network is still refused.
func TestAFetchGoesThroughAProxyTheEnvironmentNames(t *testing.T) {
	var proxied []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied = append(proxied, r.Method+" "+r.URL.String())
		w.Write([]byte("via proxy"))
	}))
	defer proxy.Close()
	pu, _ := url.Parse(proxy.URL)
	tr := guardedTransportVia(func(*http.Request) (*url.URL, error) { return pu, nil })
	client := &http.Client{Transport: tr}
	resp, err := client.Get("http://origin.invalid/data")
	if err != nil {
		t.Fatalf("a request through a proxy failed: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "via proxy" || len(proxied) != 1 || !strings.Contains(proxied[0], "origin.invalid/data") {
		t.Fatalf("got %q, proxy saw %v", b, proxied)
	}
}

func TestAProxyDoesNotLetAnOriginBeAMetadataAddress(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("the proxy was asked for a metadata address") }))
	defer proxy.Close()
	pu, _ := url.Parse(proxy.URL)
	client := &http.Client{Transport: guardedTransportVia(func(*http.Request) (*url.URL, error) { return pu, nil })}
	if _, err := client.Get("http://169.254.169.254/latest/meta-data/"); err == nil {
		t.Fatal("a metadata address was fetched through a proxy")
	}
}
