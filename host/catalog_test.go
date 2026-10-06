package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLocalCatalogScansExplicitRootAndRejectsInvalidPackages(t *testing.T) {
	home, cfg, root := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	oldConfig := userConfigDir
	userConfigDir = func() (string, error) { return cfg, nil }
	t.Cleanup(func() { userConfigDir = oldConfig })

	valid := writePackage(t, `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: example.test, name: notes, version: 1.0.0, description: Search notes}
execution: {entrypoint: main.sh}
`, "echo ok\n")
	if err := os.Rename(valid, filepath.Join(root, "notes")); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(root, "bad")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "primitive.yaml"), []byte("not: [valid"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := writePackage(t, `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: example.test, name: outside, version: 1.0.0}
execution: {entrypoint: main.sh}
`, "echo outside\n")
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}

	entries, err := localCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Ref != "example.test/notes@1.0.0" || entries[0].Source != "catalog-root" {
		t.Fatalf("unexpected catalog entries: %#v", entries)
	}
	if got := searchCatalog(entries, "SEARCH", 10); len(got) != 1 || got[0].Digest != entries[0].Digest {
		t.Fatalf("search returned %#v", got)
	}
	if got := searchCatalog(entries, "missing", 10); len(got) != 0 {
		t.Fatalf("unexpected search results %#v", got)
	}
	resolved, err := resolveCatalog(entries, entries[0].Ref, entries[0].Digest)
	if err != nil || resolved.Path != entries[0].Path {
		t.Fatalf("resolve: %#v, %v", resolved, err)
	}
	if _, err := resolveCatalog(entries, "no/such@1", ""); err == nil {
		t.Fatal("unknown ref resolved")
	}
}

func TestResolveCatalogRejectsAmbiguousReference(t *testing.T) {
	entries := []catalogEntry{{Ref: "example.test/tool@1", Digest: "aaa"}, {Ref: "example.test/tool@1", Digest: "bbb"}}
	if _, err := resolveCatalog(entries, entries[0].Ref, ""); err == nil {
		t.Fatal("ambiguous reference resolved without digest")
	}
	if got, err := resolveCatalog(entries, entries[0].Ref, "bbb"); err != nil || got.Digest != "bbb" {
		t.Fatalf("digest-qualified resolve: %#v, %v", got, err)
	}
}

func TestLocalCatalogKeepsDistinctDigestsForSameRef(t *testing.T) {
	home, cfg, root := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	oldConfig := userConfigDir
	userConfigDir = func() (string, error) { return cfg, nil }
	t.Cleanup(func() { userConfigDir = oldConfig })
	for name, script := range map[string]string{"copy-a": "echo a\n", "copy-b": "echo b\n"} {
		pkg := writePackage(t, `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: example.test, name: notes, version: 1.0.0, description: Notes primitive}
execution: {entrypoint: main.sh}
`, script)
		if err := os.Rename(pkg, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := localCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Digest == entries[1].Digest {
		t.Fatalf("wanted two distinct package digests, got %#v", entries)
	}
	if got := searchCatalog(entries, "notes", 10); len(got) != 2 {
		t.Fatalf("search returned %d entries, want both versions of the same ref", len(got))
	}
	if _, err := resolveCatalog(entries, entries[0].Ref, ""); err == nil {
		t.Fatal("ambiguous ref resolved without a digest")
	}
	for _, entry := range entries {
		got, err := resolveCatalog(entries, entry.Ref, entry.Digest)
		if err != nil || got.Digest != entry.Digest {
			t.Fatalf("digest-qualified resolve for %s: %#v, %v", entry.Digest, got, err)
		}
	}
}

func TestLocalCatalogDiscoversInstalledSavedPrimitive(t *testing.T) {
	home, cfg := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	oldConfig := userConfigDir
	userConfigDir = func() (string, error) { return cfg, nil }
	t.Cleanup(func() { userConfigDir = oldConfig })
	skills := filepath.Join(home, ".claude", "skills")
	installed := filepath.Join(skills, "notes")
	if err := os.MkdirAll(installed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installed, "primitive.yaml"), []byte("apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: example.test, name: notes, version: 1.0.0}\nexecution: {entrypoint: main.sh}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installed, "main.sh"), []byte("echo saved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// discover/pack.Install writes this exact Marker shape. Its digest is the
	// packaged archive digest, not host.packageDigest's manifest+entrypoint digest.
	marker := `{"name":"example.test/notes","digest":"archive-sha256","validation":"not_run"}`
	if err := os.WriteFile(filepath.Join(installed, ".tap-primitive.json"), []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := localCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Ref != "example.test/notes@1.0.0" || entries[0].Source != "claude" {
		t.Fatalf("installed saved primitive not found: %#v", entries)
	}
}

func TestSavedSkillCatalogRequiresNamedMarker(t *testing.T) {
	home, cfg := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	oldConfig := userConfigDir
	userConfigDir = func() (string, error) { return cfg, nil }
	t.Cleanup(func() { userConfigDir = oldConfig })
	skill := filepath.Join(home, ".claude", "skills", "notes")
	if err := os.MkdirAll(filepath.Dir(skill), 0o755); err != nil {
		t.Fatal(err)
	}
	pkg := writePackage(t, `apiVersion: primitives.telara.dev/v3
kind: Primitive
metadata: {publisher: example.test, name: notes, version: 1.0.0}
execution: {entrypoint: main.sh}
`, "echo ok\n")
	if err := os.Rename(pkg, skill); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, ".tap-primitive.json"), []byte(`{"name":"example.test/other","digest":"archive-digest"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := localCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("mismatched saved skill entered catalog: %#v", entries)
	}
}

// The search the agent ran in the clean-machine test, against the primitive
// it had just saved: its own words, not the description verbatim.
func TestSearchMatchesTheAgentsOwnWords(t *testing.T) {
	entries := []catalogEntry{
		{Ref: "local.me/runner-release-check@0.1.0", Digest: "a", Description: "Check whether a GitLab Runner stable-branch commit is ready to release after a given tag: release status, pipeline, what changed since the tag, and CHANGELOG rewrites of already-released versions."},
		{Ref: "dev.example/recent-mail@1.0.0", Digest: "b", Description: "List recent mail threads"},
	}
	got := searchCatalog(entries, "release readiness check commit after tag", 10)
	if len(got) != 1 || got[0].Digest != "a" {
		t.Fatalf("search returned %#v", got)
	}
	if got := searchCatalog(entries, "deploy kubernetes rollout", 10); len(got) != 0 {
		t.Fatalf("unrelated search returned %#v", got)
	}
	if got := searchCatalog(entries, "runner-release-check", 10); len(got) != 1 {
		t.Fatalf("name search returned %#v", got)
	}
}
