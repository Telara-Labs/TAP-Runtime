package main

import (
	"strings"
	"testing"
)

const digestManifest = "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.test, name: v, version: 1.0.0}\nexecution: {entrypoint: main.sh}\n"

func TestEveryPackageIdentityUsesThePublishedDigestVector(t *testing.T) {
	pkg := writePackage(t, digestManifest, "echo ok\n")
	// Independently derived using SHA-256, the v2 domain and big-endian lengths.
	const want = "5715266e9da6b90125e2791c63aeecbc4367920fc15d6db020ab360cbd23a132"
	digest, _, err := packageDigest(pkg)
	if err != nil || digest != want {
		t.Fatalf("trust identity = %q, %v", digest, err)
	}
	prepared, err := preparePackage(Options{Package: pkg, ExpectedDigest: want})
	if err != nil || prepared.digest != want {
		t.Fatalf("execution identity = %+v, %v", prepared, err)
	}
	diff, err := readDiffPackage(pkg)
	if err != nil || diff.identity.Digest != want {
		t.Fatalf("diff identity = %+v, %v", diff, err)
	}
}

func TestExecutionAndDiffRejectMovedManifestBytes(t *testing.T) {
	// Concatenating these files produces the same bytes in both packages.
	// Their programs differ, so the discovered identity must refuse the move.
	payload := "echo must-not-be-admitted\n"
	old := writePackage(t, digestManifest+"#"+payload, "echo ok\n")
	next := writePackage(t, digestManifest+"#", payload+"echo ok\n")
	digest, _, err := packageDigest(old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := preparePackage(Options{Package: next, ExpectedDigest: digest}); err == nil || !strings.Contains(err.Error(), "changed since discovery") {
		t.Fatalf("moved bytes retained execution identity: %v", err)
	}
	diff, err := comparePackages(old, next)
	if err != nil || diff.Before.Digest == diff.After.Digest || diff.Status != "review_required" {
		t.Fatalf("moved bytes retained diff identity: %+v, %v", diff, err)
	}
	store := &trustStore{path: t.TempDir() + "/trusted.json"}
	if err := store.add(digest, "v", old); err != nil {
		t.Fatal(err)
	}
	if refusal := admitPackage(store, nil, next); refusal == "" {
		t.Fatal("moved bytes inherited approval")
	}
}
