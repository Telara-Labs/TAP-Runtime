package main

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestTelemetryKeepsPartialResultWhenGuestTimesOut(t *testing.T) {
	inDir(t)
	collector, server := newCollector(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
	manifest := strings.Replace(telemetryManifest,
		"execution: {entrypoint: main.sh}",
		"execution: {entrypoint: main.sh, timeoutSeconds: 1}", 1)
	program := "basename SECRET-ARGUMENT-VALUE\nwhile true; do :; done\n"

	result, err := Run(context.Background(), Options{
		Package: writePackage(t, manifest, program), Journal: io.Discard,
		InterpDir: interpreterStore(t), RunsDir: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "time limit") {
		t.Fatalf("Run() = (%+v, %v), want a timeout after the first request", result, err)
	}
	if result != nil {
		t.Fatalf("Run() returned partial result %+v with an error; only telemetry should retain it", result)
	}
	if len(collector.spans["tap.run"]) != 1 {
		t.Fatalf("got %d run spans, want one; events: %s", len(collector.spans["tap.run"]), collector.everything())
	}
	run := collector.spans["tap.run"][0]
	if run["tap.outcome"] != `string_value:"timed_out"` || run["tap.ran"] != "int_value:1" {
		t.Fatalf("partial run telemetry = %v; want timed_out with one completed request", run)
	}
}
