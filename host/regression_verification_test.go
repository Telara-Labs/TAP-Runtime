package main

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var verificationProxyRunner = flag.String("verification-proxy-runner", "", "opt in to a real HTTPS proxy test of this runner binary")
var verificationInterpreterDir = flag.String("verification-interpreter-dir", "", "verified interpreter store for the external-runner proxy test")
var verificationCacheDir = flag.String("verification-cache-dir", "", "warm compiled-interpreter cache for external-runner verification")

func TestVerificationExternalBinaryHonorsHTTPSProxy(t *testing.T) {
	if *verificationProxyRunner == "" || *verificationInterpreterDir == "" {
		t.Skip("pass verification runner and interpreter paths")
	}
	var tunnels atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "example.com:443" {
			http.Error(w, "unexpected target", 400)
			return
		}
		upstream, err := net.DialTimeout("tcp", r.Host, 10*time.Second)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		client, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		tunnels.Add(1)
		fmt.Fprint(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go func() { defer client.Close(); defer upstream.Close(); io.Copy(upstream, client) }()
		go func() { defer client.Close(); defer upstream.Close(); io.Copy(client, upstream) }()
	}))
	defer proxy.Close()
	pkg := t.TempDir()
	os.WriteFile(filepath.Join(pkg, "primitive.yaml"), []byte("apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.verify, name: binary-proxy, version: 1.0.0}\nexecution: {entrypoint: main.py}\nfetch:\n  - {origin: \"https://example.com\"}\n"), 0o600)
	os.WriteFile(filepath.Join(pkg, "main.py"), []byte("print(tap.fetch('https://example.com/')['status'])\n"), 0o600)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cache := *verificationCacheDir
	if cache == "" {
		cache = t.TempDir()
	}
	cmd := exec.CommandContext(ctx, *verificationProxyRunner, "--approve", "--no-record", "--interpreters", *verificationInterpreterDir, "--cache", cache, pkg)
	// Standard proxy variables already read by net/http; isolated to this
	// child process. No product configuration or environment option is added.
	for _, v := range os.Environ() {
		key := strings.ToUpper(strings.SplitN(v, "=", 2)[0])
		if key != "HTTP_PROXY" && key != "HTTPS_PROXY" && key != "NO_PROXY" && key != "ALL_PROXY" {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Env = append(cmd.Env, "HTTPS_PROXY="+proxy.URL, "NO_PROXY=")
	b, err := cmd.CombinedOutput()
	if err != nil || tunnels.Load() != 1 || !strings.Contains(string(b), "200") {
		t.Fatalf("binary=%s CONNECT tunnels=%d exit=%v\n%s", *verificationProxyRunner, tunnels.Load(), err, b)
	}
	t.Logf("binary=%s fetched example.com through one real CONNECT proxy", *verificationProxyRunner)
}

// Exercise the corporate-proxy path with a real HTTPS origin and CONNECT
// tunnel, rather than a proxy that manufactures an HTTP response.
func TestVerificationHTTPSOriginThroughCONNECTProxy(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "allowed-origin:"+r.URL.Path)
	}))
	defer origin.Close()
	var tunnels atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		upstream, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		client, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		tunnels.Add(1)
		fmt.Fprint(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go func() { defer client.Close(); defer upstream.Close(); io.Copy(upstream, client) }()
		go func() { defer client.Close(); defer upstream.Close(); io.Copy(client, upstream) }()
	}))
	defer proxy.Close()
	pu, _ := url.Parse(proxy.URL)
	tr := guardedTransportVia(func(*http.Request) (*url.URL, error) { return pu, nil })
	defer tr.CloseIdleConnections()
	pool := x509.NewCertPool()
	pool.AddCert(origin.Certificate())
	tr.TLSClientConfig = origin.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	tr.TLSClientConfig.RootCAs = pool
	resp, err := (&http.Client{Transport: tr}).Get(origin.URL + "/data")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil || string(b) != "allowed-origin:/data" || tunnels.Load() != 1 {
		t.Fatalf("body=%q tunnels=%d err=%v", b, tunnels.Load(), err)
	}
}

func TestVerificationStatusEvidenceAfterUnknownOutcomeResume(t *testing.T) {
	dir := inDir(t)
	c := startServer(t, true, accept)
	pkg := writePackage(t, appendManifest, appendScript)
	o := opts(t, pkg, c.runs)
	o.stopDuring = "r2"
	if _, err := Run(context.Background(), o); err == nil {
		t.Fatal("interruption did not occur")
	}
	id := onlyRun(t, c.runs)
	inspect := func(tool string) map[string]any {
		return toolObject(t, c.callDetail("tools/call", map[string]any{"name": tool, "arguments": map[string]any{"run_id": id}}))
	}
	if got := inspect("tap_status"); got["state"] != "interrupted" {
		t.Fatalf("interrupted status: %#v", got)
	}
	o = opts(t, pkg, c.runs)
	o.Resume = id
	res, err := Run(context.Background(), o)
	if err != nil || res.Unknown != 1 {
		t.Fatalf("resumed result=%+v err=%v", res, err)
	}
	if got := strings.Join(lines(t, filepath.Join(dir, "counter.txt")), ","); got != "one,three" {
		t.Fatalf("unknown write was repeated or another write lost: %s", got)
	}
	status, evidence := inspect("tap_status"), inspect("tap_evidence")
	if status["state"] != "finished" || evidence["state"] != "finished" {
		t.Fatalf("resumed state: status=%#v evidence=%#v", status, evidence)
	}
	if status["outcome"] != "completed_with_unknown" {
		t.Fatalf("resumed unknown outcome was reported as complete: %#v", status)
	}
	found := false
	for _, item := range evidence["events"].([]any) {
		e := item.(map[string]any)
		if e["outcome"] == "outcome_unknown" || e["outcome"] == "unknown" {
			found = true
		}
	}
	if !found {
		t.Fatalf("unknown outcome disappeared from MCP evidence: %#v", evidence)
	}
	t.Logf("status after unknown outcome: %#v", status)
}

func TestVerificationStatusEvidenceAfterFailedGuest(t *testing.T) {
	inDir(t)
	c := startServer(t, true, accept)
	pkg := writePackage(t, "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.verify, name: fails, version: 1.0.0}\nexecution: {entrypoint: main.sh}\n", "echo failed-output; exit 7\n")
	res, err := Run(context.Background(), opts(t, pkg, c.runs))
	if err != nil || res.Exit != 7 {
		t.Fatalf("failed guest: result=%+v err=%v", res, err)
	}
	for _, tool := range []string{"tap_status", "tap_evidence"} {
		got := toolObject(t, c.callDetail("tools/call", map[string]any{"name": tool, "arguments": map[string]any{"run_id": res.RunID}}))
		if got["state"] != "finished" {
			t.Fatalf("%s failed state: %#v", tool, got)
		}
		if tool == "tap_status" && got["outcome"] != "failed" {
			t.Fatalf("failed guest reported a successful outcome: %#v", got)
		}
		if tool == "tap_evidence" {
			events := got["events"].([]any)
			if len(events) == 0 || events[len(events)-1].(map[string]any)["outcome"] != "failed" {
				t.Fatalf("failed finish missing from evidence: %#v", got)
			}
		}
		t.Logf("%s after exit 7: %#v", tool, got)
	}
}

func TestVerificationJavaScriptMemoryPressure(t *testing.T) {
	t.Run("ordinary allocation", func(t *testing.T) {
		res, err := runLimited(t, "main.js", "", `const x = "x".repeat(32 << 20); print(x.length);`, Options{})
		if err != nil || res == nil || strings.TrimSpace(res.Stdout) != "33554432" {
			t.Fatalf("ordinary JS workload: result=%+v err=%v", res, err)
		}
	})
	t.Run("beyond ceiling", func(t *testing.T) {
		res, err := runLimited(t, "main.js", "", `const blocks = []; for (let i = 0; i < 128; i++) blocks.push(String(i) + "x".repeat(8 << 20)); print("allocated-1GiB");`, Options{})
		if res != nil && strings.Contains(res.Stdout, "allocated-1GiB") {
			t.Fatal("JS allocated beyond the 512 MiB ceiling")
		}
		if err == nil && (res == nil || res.Exit == 0 || !strings.Contains(res.Stderr, "out of memory")) {
			t.Fatalf("JS memory exhaustion was not reported: %+v", res)
		}
		if res != nil {
			t.Logf("JS memory exhaustion: exit=%d stderr=%q err=%v", res.Exit, res.Stderr, err)
		}
	})
}

func TestVerificationRealGuestLineBeyond16MiB(t *testing.T) {
	_, err := runLimited(t, "main.js", "", `print("x".repeat((16 << 20) + 1));`, Options{})
	if err == nil || !strings.Contains(err.Error(), "larger than the runner accepts") {
		t.Fatalf("real guest over the line cap: %v", err)
	}
}

func TestVerificationDefaultDispatchBoundary(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "input.txt"), []byte("small"), 0o600); err != nil {
		t.Fatal(err)
	}
	extra := fmt.Sprintf("files:\n  - {path: %q, access: read}\n", filepath.ToSlash(dir))
	program := fmt.Sprintf(`ok = 0
refused = 0
for i in range(1001):
    try:
        tap.read(%q)
        ok += 1
    except PermissionError:
        refused += 1
print(str(ok) + "," + str(refused))
`, filepath.ToSlash(filepath.Join(dir, "input.txt")))
	res, err := runLimited(t, "main.py", extra, program, Options{})
	if err != nil || res == nil || strings.TrimSpace(res.Stdout) != "1000,1" {
		t.Fatalf("1001 requests with default limit: result=%+v err=%v", res, err)
	}
}

func TestVerificationSearchBeyondTwentyPackages(t *testing.T) {
	c := startServer(t, true, accept)
	for i := 0; i < 25; i++ {
		pkg := writePackage(t, fmt.Sprintf("apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.verify, name: probe-%02d, version: 1.0.0}\nexecution: {entrypoint: main.sh}\n", i), "echo unused\n")
		c.stagePackage(pkg)
	}
	search := func(args map[string]any) map[string]any {
		return c.call("tools/call", map[string]any{"name": "tap_search", "arguments": args})
	}
	all := toolObject(t, search(map[string]any{"query": "dev.verify", "detail": true}))
	if matches, _ := all["matches"].([]any); len(matches) != 20 {
		t.Fatalf("default search beyond 20 entries: %#v", all)
	}
	last := toolObject(t, search(map[string]any{"query": "probe-24", "detail": true}))
	if matches, _ := last["matches"].([]any); len(matches) != 1 {
		t.Fatalf("last primitive is undiscoverable by specific query: %#v", last)
	}
	if bad := search(map[string]any{"limit": 21}); bad["isError"] != true {
		t.Fatalf("oversized result request accepted: %#v", bad)
	}
	t.Log("25 packages are scanned; 20 results returned without pagination; a specific query reaches package 25")
}

func TestVerificationLegacyTrustSurvivesRelocationButNotManifestChange(t *testing.T) {
	pkg := writePackage(t, "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.verify, name: trust-migration, version: 1.0.0}\nexecution: {entrypoint: main.sh}\n", "echo unchanged\n")
	raw, _ := os.ReadFile(filepath.Join(pkg, "primitive.yaml"))
	program, _ := os.ReadFile(filepath.Join(pkg, "main.sh"))
	// This is packageDigest's exact v0.1.3 algorithm, before RunDigest.
	sum := sha256.Sum256(append(append([]byte{}, raw...), program...))
	legacy := hex.EncodeToString(sum[:])
	store := &trustStore{path: filepath.Join(t.TempDir(), "trusted.json")}
	if err := store.add(legacy, "trust-migration", pkg); err != nil {
		t.Fatal(err)
	}
	relocated := t.TempDir()
	os.WriteFile(filepath.Join(relocated, "primitive.yaml"), raw, 0o600)
	os.WriteFile(filepath.Join(relocated, "main.sh"), program, 0o600)
	os.WriteFile(filepath.Join(relocated, "SKILL.md"), []byte("pointer changed after save or pull"), 0o600)
	called := 0
	ask := func(string, string, string, string, string, string) bool { called++; return false }
	if refusal := admitPackage(store, ask, relocated); refusal != "" || called != 0 {
		t.Fatalf("legacy trust was lost after relocation/pointer creation: refusal=%q prompts=%d", refusal, called)
	}
	os.WriteFile(filepath.Join(relocated, "primitive.yaml"), append(raw, []byte("# byte-level change\n")...), 0o600)
	if refusal := admitPackage(store, ask, relocated); refusal == "" || called != 1 {
		t.Fatalf("changed manifest reused old approval: refusal=%q prompts=%d", refusal, called)
	}
	t.Log("v0.1.3 approval digest survives relocation and SKILL.md changes; a manifest byte change prompts again")
}

func TestVerificationBashMemoryPressure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		doublings int
		exhaust   bool
	}{
		{"ordinary32MiB", 25, false}, {"beyondCeiling", 30, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			program := fmt.Sprintf("blob=x\nfor ((i=0; i<%d; i++)); do blob=\"$blob$blob\"; done\nprintf 'allocated:%%s\\n' \"${#blob}\"\n", tc.doublings)
			res, err := runLimited(t, "main.sh", "", program, Options{})
			if !tc.exhaust && (err != nil || res == nil || res.Exit != 0 || !strings.Contains(res.Stdout, "allocated:33554432")) {
				t.Fatalf("ordinary bash: result=%+v err=%v", res, err)
			}
			if tc.exhaust && err == nil && (res == nil || res.Exit == 0) {
				t.Fatalf("bash exceeded ceiling without failure: %+v", res)
			}
			if res != nil {
				t.Logf("bash memory workload: exit=%d stdout=%q stderr=%q err=%v", res.Exit, res.Stdout, res.Stderr, err)
			} else {
				t.Logf("bash memory workload: %v", err)
			}
		})
	}
}

var verificationCrossUser = flag.Bool("verification-cross-user", false, "opt in to a distinct-UID Docker output-sharing check")

func TestVerificationPrivateOutputAcrossUsers(t *testing.T) {
	if !*verificationCrossUser {
		t.Skip("pass -verification-cross-user for the Docker UID check")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker unavailable for a distinct-UID read")
	}
	dir, err := os.MkdirTemp("/tmp", "tap-output-modes-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "output.txt")
	res, err := runLimited(t, "main.py", fmt.Sprintf("files:\n  - {path: %q, access: write}\n", filepath.ToSlash(dir)), fmt.Sprintf("tap.write(%q, 'runner-output')\n", filepath.ToSlash(out)), Options{Approve: func(Ask) Grant { return Grant{OK: true, Limit: Unlimited} }})
	if err != nil || res == nil || res.Exit != 0 {
		t.Fatalf("write primitive: result=%+v err=%v", res, err)
	}
	info, err := os.Stat(out)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("output permissions: %v, %v", info, err)
	}
	if b, err := os.ReadFile(out); err != nil || string(b) != "runner-output" {
		t.Fatalf("same-user stage: %q %v", b, err)
	}
	// Docker Desktop's macOS shared mount translates permissions. Copy into
	// the container's native filesystem before testing the Unix UID boundary.
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	name := filepath.Base(dir)
	defer func() {
		clean, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		exec.CommandContext(clean, "docker", "rm", "-f", name).Run()
	}()
	cmd := exec.CommandContext(ctx, "docker", "run", "--name", name, "--rm", "--network", "none", "--platform", "linux/amd64", "--mount", "type=bind,source="+dir+",target=/outputs,readonly", "golang:1.26", "/bin/sh", "-c", `cp --preserve=mode /outputs/output.txt /tmp/runner-output; chown 0:0 /tmp/runner-output; su -s /bin/sh nobody -c 'cat /tmp/runner-output'`)
	cmd.WaitDelay = time.Second
	b, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(b), "Permission denied") {
		t.Fatalf("other-user stage should be refused: err=%v output=%s", err, b)
	}
	t.Logf("same-user output read passes; a different UID is denied: %s", b)
}
