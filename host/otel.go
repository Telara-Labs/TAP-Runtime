package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// An OpenTelemetry exporter for the events of a run: what tools and commands
// ran, what was approved, how each ended.
//
// It is OFF unless an endpoint is set. It is configured by the standard
// OpenTelemetry environment variables, which the exporter library reads:
// OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_EXPORTER_OTLP_TRACES_ENDPOINT,
// OTEL_EXPORTER_OTLP_HEADERS, OTEL_EXPORTER_OTLP_PROTOCOL, and the rest of
// that family. Luis approved these names, and no others, on 2026-09-28. This
// file reads two of them itself, only to decide whether to start.
//
// It sends events, not payloads. What a call was given and what it answered
// stay on the machine unless payloads are enabled.

// otelEndpointSet reports whether the standard variables name an endpoint.
func otelEndpointSet() bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
}

type telemetry struct {
	provider *sdktrace.TracerProvider
	tracer   trace.Tracer
	payloads bool

	mu   sync.Mutex
	ctx  context.Context
	root trace.Span
	buf  []byte
}

// runIdentity names what a run ran, for the collector. ArtifactDigest is the
// registry's digest of the package, read from the marker the telara CLI writes
// beside a pulled primitive (.telara-primitive.json); it is empty for a
// package that was not pulled from a registry. PackageDigest is the runner's
// own digest of the manifest and entrypoint.
type runIdentity struct {
	Publisher, Name, Version           string
	PackageDigest, ArtifactDigest, Ref string
}

// readRegistryMarker returns the ref and artifact digest a telara CLI pull
// recorded in dir, or empty strings.
func readRegistryMarker(dir string) (ref, digest string) {
	b, err := os.ReadFile(filepath.Join(dir, ".telara-primitive.json"))
	if err != nil {
		return "", ""
	}
	var m struct {
		Ref            string `json:"ref"`
		ArtifactDigest string `json:"artifact_digest"`
	}
	if json.Unmarshal(b, &m) != nil {
		return "", ""
	}
	return m.Ref, m.ArtifactDigest
}

// startTelemetry starts the exporter, or returns nil when none is configured.
func startTelemetry(ctx context.Context, id runIdentity, runID, client string, payloads bool) (*telemetry, error) {
	if !otelEndpointSet() {
		return nil, nil
	}
	switch p := strings.ToLower(os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")); p {
	case "", "http/protobuf":
	default:
		// The exporter built in speaks OTLP over HTTP. Saying so is better
		// than sending to an endpoint in a protocol it does not expect.
		return nil, fmt.Errorf("OTEL_EXPORTER_OTLP_PROTOCOL is %q; this runner exports http/protobuf only", p)
	}
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	res := resource.NewSchemaless(
		attribute.String("service.name", "tap-runtime"),
		attribute.String("service.version", version),
	)
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	t := &telemetry{provider: tp, tracer: tp.Tracer("tap-runtime"), payloads: payloads}
	t.ctx, t.root = t.tracer.Start(ctx, "tap.run", trace.WithAttributes(
		attribute.String("tap.primitive", id.Name),
		attribute.String("tap.publisher", id.Publisher),
		attribute.String("tap.version", id.Version),
		attribute.String("tap.ref", id.Ref),
		attribute.String("tap.package_digest", id.PackageDigest),
		attribute.String("tap.artifact_digest", id.ArtifactDigest),
		attribute.String("tap.run_id", runID),
		attribute.String("tap.client", client),
	))
	return t, nil
}

// Write takes the audit lines the runner writes, one JSON object to a line,
// and turns each into a span.
func (t *telemetry) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	for {
		i := strings.IndexByte(string(t.buf), '\n')
		if i < 0 {
			return len(p), nil
		}
		line := t.buf[:i]
		t.buf = t.buf[i+1:]
		var e map[string]any
		if json.Unmarshal(line, &e) == nil {
			t.span(e)
		}
	}
}

// payload lists the fields of an event that say what a call was given or
// what it touched. They are left out unless payloads are enabled.
var payload = map[string]bool{"args": true, "arguments": true, "url": true, "path": true, "resolved": true,
	"example": true, "action": true, "error": true, "cwd": true}

func (t *telemetry) span(e map[string]any) {
	name := "tap.event"
	switch {
	case e["alias"] != nil:
		name = "tap.call"
	case e["command"] != nil:
		name = "tap.exec"
	case e["capability"] != nil:
		name = fmt.Sprintf("tap.%v", e["capability"])
	case e["kind"] != nil:
		name = "tap.approval"
	}
	end := time.Now()
	if s, ok := e["ts"].(string); ok {
		if at, err := time.Parse(time.RFC3339Nano, s); err == nil {
			end = at
		}
	}
	start := end
	if ms, ok := e["ms"].(float64); ok {
		start = end.Add(-time.Duration(ms) * time.Millisecond)
	}
	var attrs []attribute.KeyValue
	for k, v := range e {
		if k == "ts" || k == "ms" {
			continue
		}
		if payload[k] && !t.payloads {
			// What is safe to keep of a URL is where it went.
			if k == "url" {
				if u, err := url.Parse(fmt.Sprint(v)); err == nil && u.Host != "" {
					attrs = append(attrs, attribute.String("tap.origin", u.Scheme+"://"+u.Host))
				}
			}
			continue
		}
		switch x := v.(type) {
		case string:
			attrs = append(attrs, attribute.String("tap."+k, x))
		case float64:
			attrs = append(attrs, attribute.Int64("tap."+k, int64(x)))
		case bool:
			attrs = append(attrs, attribute.Bool("tap."+k, x))
		default:
			b, _ := json.Marshal(x)
			attrs = append(attrs, attribute.String("tap."+k, string(b)))
		}
	}
	_, s := t.tracer.Start(t.ctx, name, trace.WithTimestamp(start), trace.WithAttributes(attrs...))
	if o, _ := e["outcome"].(string); o != "" && o != "ran" && o != "approved" {
		s.SetStatus(codes.Error, o)
	}
	s.End(trace.WithTimestamp(end))
}

// stop ends the run's span and sends what is waiting. It is bounded: a
// collector that does not answer must not hold a run open.
func (t *telemetry) stop(outcome string, res *Result) {
	if t == nil {
		return
	}
	t.root.SetAttributes(attribute.String("tap.outcome", outcome))
	if res != nil {
		// What the program returned and what it did: a run can complete with
		// the program failing, and a completed run can have been refused.
		t.root.SetAttributes(
			attribute.Int("tap.exit", res.Exit),
			attribute.Int("tap.ran", res.Ran),
			attribute.Int("tap.refused", res.Refused),
			attribute.Int("tap.unknown", res.Unknown),
		)
	}
	if outcome != "completed" || (res != nil && res.Exit != 0) {
		t.root.SetStatus(codes.Error, outcome)
	}
	t.root.End()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := t.provider.Shutdown(ctx); err != nil {
		logf("telemetry  could not be sent: %v", err)
	}
}

var _ io.Writer = (*telemetry)(nil)
