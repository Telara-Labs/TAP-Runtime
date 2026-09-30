package main

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

// collector is a real OTLP endpoint over HTTP: it takes what the exporter
// sends and decodes it.
type collector struct {
	mu      sync.Mutex
	spans   map[string][]map[string]string // span name -> attributes of each
	headers []http.Header
	paths   []string
}

func newCollector(t *testing.T) (*collector, *httptest.Server) {
	c := &collector{spans: map[string][]map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			z, err := gzip.NewReader(r.Body)
			if err != nil {
				t.Errorf("collector: %v", err)
				return
			}
			body = z
		}
		raw, _ := io.ReadAll(body)
		var req coltrace.ExportTraceServiceRequest
		if err := proto.Unmarshal(raw, &req); err != nil {
			t.Errorf("collector: what arrived is not OTLP: %v", err)
			w.WriteHeader(400)
			return
		}
		c.mu.Lock()
		c.headers = append(c.headers, r.Header.Clone())
		c.paths = append(c.paths, r.URL.Path)
		for _, rs := range req.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, s := range ss.Spans {
					attrs := map[string]string{}
					for _, a := range s.Attributes {
						attrs[a.Key] = a.Value.String()
					}
					c.spans[s.Name] = append(c.spans[s.Name], attrs)
				}
			}
		}
		c.mu.Unlock()
		out, _ := proto.Marshal(&coltrace.ExportTraceServiceResponse{})
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return c, srv
}

func (c *collector) everything() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	for name, all := range c.spans {
		for _, attrs := range all {
			b.WriteString(name + " ")
			for k, v := range attrs {
				b.WriteString(k + "=" + v + " ")
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

const telemetryManifest = `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: dev.test, name: observed, version: 0.1.0}
execution: {entrypoint: main.sh}
files:
  - {path: out, access: write}
commands:
  - {command: tee, globals: ["-a"], args: ["*"], effect: write}
  - {command: basename, args: ["*"], effect: read}
`

const telemetryScript = `basename /tmp/SECRET-ARGUMENT-VALUE
echo one | tee -a out/SECRET-FILE-NAME.txt
curl https://example.com/SECRET-PATH || echo refused
`

func runObserved(t *testing.T, payloads bool) (*collector, *Result) {
	t.Helper()
	inDir(t)
	os.MkdirAll("out", 0o755)
	c, srv := newCollector(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-tenant=acme")
	res, err := Run(context.Background(), Options{
		Package: writePackage(t, telemetryManifest, telemetryScript), Approve: yes, Journal: io.Discard,
		InterpDir: interpreterStore(t), RunsDir: t.TempDir(), TelemetryPayloads: payloads,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, res
}

// Rulings 26 and 35.
func TestEventsAreExportedAndPayloadsAreNot(t *testing.T) {
	c, res := runObserved(t, false)
	all := c.everything()
	t.Logf("spans received:\n%s", all)

	if len(c.spans["tap.run"]) != 1 {
		t.Fatalf("%d spans for the run, want 1", len(c.spans["tap.run"]))
	}
	run := c.spans["tap.run"][0]
	if !strings.Contains(run["tap.primitive"], "observed") || !strings.Contains(run["tap.run_id"], res.RunID) || !strings.Contains(run["tap.outcome"], "completed") {
		t.Errorf("the run's span says %v", run)
	}
	// What ran, for a registry to attribute the run to a release, and what
	// the program did (TENG-3042).
	for k, want := range map[string]string{
		"tap.publisher": "dev.test", "tap.version": "0.1.0", "tap.exit": "0",
		"tap.ran": "2", "tap.refused": "1", "tap.package_digest": "",
	} {
		if !strings.Contains(run[k], want) {
			t.Errorf("tap.run %s = %q, want it to contain %q", k, run[k], want)
		}
	}
	if run["tap.package_digest"] == "" {
		t.Errorf("tap.run carries no package digest: %v", run)
	}
	// Two commands ran, one of them after being held for approval, and one
	// was refused: four events. One kind of change was approved.
	if n := len(c.spans["tap.exec"]); n != 4 {
		t.Errorf("%d command events, want 4 (run, held, run, refused)", n)
	}
	if n := len(c.spans["tap.approval"]); n != 1 {
		t.Errorf("%d approval events, want 1", n)
	}
	for _, want := range []string{"tap.command=", "tee", "tap.effect=", "tap.outcome=", "refused_undeclared", "approved"} {
		if !strings.Contains(all, want) {
			t.Errorf("the events do not carry %q", want)
		}
	}
	// What a call was given and what it touched stay on the machine.
	for _, secret := range []string{"SECRET-ARGUMENT-VALUE", "SECRET-FILE-NAME", "SECRET-PATH"} {
		if strings.Contains(all, secret) {
			t.Errorf("a payload was exported: %s", secret)
		}
	}
	// The standard variables are honoured by the exporter itself.
	if len(c.paths) == 0 || c.paths[0] != "/v1/traces" {
		t.Errorf("sent to %v, want /v1/traces under the endpoint", c.paths)
	}
	if len(c.headers) == 0 || c.headers[0].Get("x-tenant") != "acme" {
		t.Errorf("OTEL_EXPORTER_OTLP_HEADERS was not sent")
	}
}

func TestPayloadsAreExportedOnlyWhenEnabled(t *testing.T) {
	c, _ := runObserved(t, true)
	all := c.everything()
	for _, want := range []string{"SECRET-ARGUMENT-VALUE", "SECRET-FILE-NAME"} {
		if !strings.Contains(all, want) {
			t.Errorf("payloads were enabled and %s was not sent:\n%s", want, all)
		}
	}
}

func TestNothingIsExportedUnlessAnEndpointIsSet(t *testing.T) {
	for _, v := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"} {
		t.Setenv(v, "")
	}
	tel, err := startTelemetry(context.Background(), runIdentity{Name: "p"}, "r", "c", false)
	if tel != nil || err != nil {
		t.Fatalf("the exporter started with no endpoint: %v %v", tel, err)
	}
	// The run itself is unaffected.
	inDir(t)
	os.MkdirAll("out", 0o755)
	res, err := Run(context.Background(), Options{
		Package: writePackage(t, telemetryManifest, telemetryScript), Approve: yes, Journal: io.Discard,
		InterpDir: interpreterStore(t), RunsDir: t.TempDir(),
	})
	if err != nil || res.Ran != 2 {
		t.Fatalf("%v %+v", err, res)
	}
	if b, _ := os.ReadFile(filepath.Join("out", "SECRET-FILE-NAME.txt")); strings.TrimSpace(string(b)) != "one" {
		t.Fatalf("the run did not do its work: %q", b)
	}
}

func TestACollectorThatCannotBeReachedDoesNotStopARun(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1") // nothing listens
	inDir(t)
	os.MkdirAll("out", 0o755)
	res, err := Run(context.Background(), Options{
		Package: writePackage(t, telemetryManifest, telemetryScript), Approve: yes, Journal: io.Discard,
		InterpDir: interpreterStore(t), RunsDir: t.TempDir(),
	})
	if err != nil || res.Ran != 2 {
		t.Fatalf("an unreachable collector stopped the run: %v %+v", err, res)
	}
}

func TestAProtocolThisRunnerDoesNotSpeakIsSaidAndNotGuessed(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")
	tel, err := startTelemetry(context.Background(), runIdentity{Name: "p"}, "r", "c", false)
	if tel != nil || err == nil || !strings.Contains(err.Error(), "http/protobuf") {
		t.Fatalf("%v %v", tel, err)
	}
}

// A package the telara CLI pulled carries the registry's ref and artifact
// digest beside it, and the run reports them (TENG-3042).
func TestARegistryPulledRunReportsItsArtifactDigest(t *testing.T) {
	inDir(t)
	os.MkdirAll("out", 0o755)
	c, srv := newCollector(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	pkg := writePackage(t, telemetryManifest, telemetryScript)
	if err := os.WriteFile(filepath.Join(pkg, ".telara-primitive.json"),
		[]byte(`{"ref":"dev.test/observed@0.1.0","artifact_digest":"sha256:abc123"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), Options{
		Package: pkg, Approve: yes, Journal: io.Discard, InterpDir: interpreterStore(t), RunsDir: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	run := c.spans["tap.run"][0]
	if !strings.Contains(run["tap.artifact_digest"], "sha256:abc123") || !strings.Contains(run["tap.ref"], "dev.test/observed@0.1.0") {
		t.Fatalf("the run's span does not name the pulled release: %v", run)
	}
}
