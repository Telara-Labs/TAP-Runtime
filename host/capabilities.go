package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/Telara-Labs/TAP-Runtime/contract/glob"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// fileDecl bounds the files a primitive may touch.
//
//	files:
//	  - {path: reports, access: write}
//	  - {path: /etc/hosts, access: read}
//
// path is a file, or a directory and everything under it. A relative path is
// taken from the directory the runner was started in. access write includes
// read.

// fetchDecl bounds the URLs a primitive may fetch.
//
//	fetch:
//	  - {origin: "https://api.github.com", methods: [GET]}
//
// origin is a scheme, a host and an optional port, and nothing else. methods
// defaults to GET. GET and HEAD are reads; every other method is a write.

const maxBody = 10 << 20

// resolvePath turns a path into the real location it names, following
// symbolic links, so that neither ../ nor a link can leave a declared
// directory. A path that does not exist yet is resolved through its nearest
// existing parent.
func resolvePath(p, cwd string) (string, error) {
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	p = filepath.Clean(p)
	rest := ""
	cur := p
	for {
		real, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(real, rest), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// under reports whether the real path falls under one declared path. A
// declaration with no wildcard is a file, or a directory and everything
// under it. One with a wildcard is a pattern: its leading part,
// up to the first wildcard, is resolved through symbolic links like any
// other path, and what follows is matched segment by segment.
func under(declared, real, cwd string) bool {
	if !glob.HasMeta(declared) {
		root, err := resolvePath(declared, cwd)
		if err != nil {
			return false
		}
		return real == root || strings.HasPrefix(real, root+string(filepath.Separator))
	}
	parts := strings.Split(filepath.ToSlash(declared), "/")
	fixed := 0
	for fixed < len(parts) && !glob.HasMeta(parts[fixed]) {
		fixed++
	}
	prefix := strings.Join(parts[:fixed], "/")
	if prefix == "" {
		if strings.HasPrefix(declared, "/") {
			prefix = "/"
		} else {
			prefix = "."
		}
	}
	root, err := resolvePath(filepath.FromSlash(prefix), cwd)
	if err != nil {
		return false
	}
	if real != root && !strings.HasPrefix(real, strings.TrimSuffix(root, string(filepath.Separator))+string(filepath.Separator)) {
		return false
	}
	rel, err := filepath.Rel(root, real)
	if err != nil {
		return false
	}
	if rel == "." {
		rel = ""
	}
	return glob.Path(strings.Join(parts[fixed:], "/"), filepath.ToSlash(rel))
}

// fileAllowed reports whether the real path falls under a declaration that
// grants the access asked for.
func fileAllowed(decls []fileDecl, real, want, cwd string) bool {
	for _, d := range decls {
		if want == "write" && d.Access != "write" {
			continue
		}
		if under(d.Path, real, cwd) {
			return true
		}
	}
	return false
}

// fetchAllowed reports whether a declaration admits this method on this URL.
func fetchAllowed(decls []fetchDecl, method string, u *url.URL) bool {
	for _, d := range decls {
		// A declared origin may wildcard its first label. The
		// scheme and the port are always exact.
		scheme, rest, ok := strings.Cut(d.Origin, "://")
		if !ok || !strings.EqualFold(scheme, u.Scheme) {
			continue
		}
		rest = strings.TrimSuffix(rest, "/")
		host, port := rest, ""
		if i := strings.LastIndexByte(rest, ':'); i >= 0 {
			host, port = rest[:i], rest[i+1:]
		}
		if port != u.Port() || !glob.Host(host, u.Hostname()) {
			continue
		}
		methods := d.Methods
		if len(methods) == 0 {
			methods = []string{"GET"}
		}
		for _, m := range methods {
			if strings.EqualFold(m, method) {
				return true
			}
		}
	}
	return false
}

type capRecorder struct {
	journal io.Writer
	entry   map[string]any
}

func newCapRecorder(journal io.Writer, kind string) *capRecorder {
	return &capRecorder{journal, map[string]any{"ts": time.Now().UTC().Format(time.RFC3339Nano), "capability": kind}}
}

func (r *capRecorder) done(outcome string, extra map[string]any) {
	r.entry["outcome"] = outcome
	for k, v := range extra {
		r.entry[k] = v
	}
	b, _ := json.Marshal(r.entry)
	r.journal.Write(append(b, '\n'))
}

// declaredRoot is the declared path a file falls under, as the manifest
// writes it, so an approval can name a directory instead of one file.
func declaredRoot(m *manifest, path string) string {
	cwd, _ := os.Getwd()
	real, err := resolvePath(path, cwd)
	if err != nil {
		return path
	}
	for _, d := range m.Files {
		if under(d.Path, real, cwd) {
			return d.Path
		}
	}
	return path
}

func fileOp(m *manifest, rq request, approve bool, journal io.Writer) reply {
	rec := newCapRecorder(journal, "file."+rq.Method)
	rec.entry["path"] = rq.Path
	cwd, _ := os.Getwd()
	real, err := resolvePath(rq.Path, cwd)
	if err != nil {
		rec.done("failed", map[string]any{"error": err.Error()})
		return reply{Exit: 1, Stderr: err.Error()}
	}
	rec.entry["resolved"] = real
	want := "read"
	if rq.Method == "write" || rq.Method == "canwrite" {
		want = "write"
	}
	if !fileAllowed(m.Files, real, want, cwd) {
		logf("  REFUSED  %s %s  (outside the declared files)", rq.Method, rq.Path)
		rec.done("refused_undeclared", nil)
		return reply{Refused: "path is outside the files this primitive declares for " + want}
	}
	if want == "write" {
		if !approve {
			logf("  GATED    write %s  (write, no approval)", rq.Path)
			rec.done("gated", nil)
			return reply{Refused: "write needs approval", Gated: true}
		}
		if rq.Method == "canwrite" {
			// Asked at open, so a script learns of a refusal where it
			// happens and not at close. Nothing is written or recorded.
			return reply{}
		}
		if err := os.MkdirAll(filepath.Dir(real), 0o700); err != nil {
			rec.done("failed", map[string]any{"error": err.Error()})
			return reply{Exit: 1, Stderr: err.Error()}
		}
		if err := os.WriteFile(real, []byte(rq.Stdin), 0o600); err != nil {
			rec.done("failed", map[string]any{"error": err.Error()})
			return reply{Exit: 1, Stderr: err.Error()}
		}
		logf("  write    %s  [write] %dB", real, len(rq.Stdin))
		rec.done("ran", map[string]any{"bytes": len(rq.Stdin)})
		return reply{}
	}
	f, err := os.Open(real)
	if err != nil {
		rec.done("failed", map[string]any{"error": err.Error()})
		return reply{Exit: 1, Stderr: err.Error()}
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxBody+1))
	if err != nil {
		rec.done("failed", map[string]any{"error": err.Error()})
		return reply{Exit: 1, Stderr: err.Error()}
	}
	if len(b) > maxBody {
		rec.done("failed", map[string]any{"error": "file is larger than the limit"})
		return reply{Exit: 1, Stderr: "file is larger than 10 MB"}
	}
	logf("  read     %s  [read] %dB", real, len(b))
	rec.done("ran", map[string]any{"bytes": len(b)})
	return reply{Result: string(b)}
}

func fetchOp(m *manifest, rq request, approve bool, journal io.Writer) reply {
	rec := newCapRecorder(journal, "fetch")
	method := strings.ToUpper(rq.HTTPMethod)
	if method == "" {
		method = "GET"
	}
	rec.entry["method"], rec.entry["url"] = method, rq.URL
	u, err := url.Parse(rq.URL)
	if err != nil || u.Host == "" {
		rec.done("refused_undeclared", nil)
		return reply{Refused: "not a URL"}
	}
	if !fetchAllowed(m.Fetch, method, u) {
		logf("  REFUSED  fetch %s %s  (outside the declared origins)", method, logURL(rq.URL))
		rec.done("refused_undeclared", nil)
		return reply{Refused: "origin or method is outside what this primitive declares"}
	}
	effect := "write"
	if method == "GET" || method == "HEAD" {
		effect = "read"
	}
	rec.entry["effect"] = effect
	// Every fetch is gated, a read included: the address, the headers and the
	// body can carry what the program has read out of this machine.
	// The person is asked once per origin and kind of request, and the full
	// address is in the record either way.
	if !approve {
		logf("  GATED    fetch %s %s  (%s, no approval)", method, logURL(rq.URL), effect)
		rec.done("gated", nil)
		if effect == "read" {
			return reply{Refused: "a request that leaves this machine needs approval, even a read: its address can carry data out", Gated: true}
		}
		return reply{Refused: "write needs approval", Gated: true}
	}
	req, err := http.NewRequest(method, rq.URL, bytes.NewReader([]byte(rq.Stdin)))
	if err != nil {
		rec.done("failed", map[string]any{"error": err.Error()})
		return reply{Exit: 1, Stderr: err.Error()}
	}
	for k, v := range rq.Headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{
		Timeout:   60 * time.Second,
		Transport: guardedTransport(),
		// A redirect is a new request. It must be one the primitive declared.
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			if !fetchAllowed(m.Fetch, next.Method, next.URL) {
				return fmt.Errorf("redirected to %s, which this primitive does not declare", next.URL.Host)
			}
			return nil
		},
	}
	t0 := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		logf("  FAILED   fetch %s %s: %v", method, logURL(rq.URL), err)
		rec.done("failed", map[string]any{"error": err.Error()})
		return reply{Exit: 1, Stderr: err.Error()}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil || len(b) > maxBody {
		rec.done("failed", map[string]any{"error": "response could not be read or is larger than the limit"})
		return reply{Exit: 1, Stderr: "response could not be read or is larger than 10 MB"}
	}
	logf("  fetch    %s %s  [%s] status=%d %dB %s", method, logURL(rq.URL), effect, resp.StatusCode, len(b), time.Since(t0).Round(time.Millisecond))
	rec.done("ran", map[string]any{"status": resp.StatusCode, "bytes": len(b), "ms": time.Since(t0).Milliseconds()})
	return reply{Result: string(b), Status: resp.StatusCode}
}
