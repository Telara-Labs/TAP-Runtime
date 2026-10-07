package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Telara-Labs/TAP-Runtime/discover/author"
	"github.com/Telara-Labs/TAP-Runtime/discover/pack"
)

func TestSaveLifecycleFailuresAreRefusedBeforeConsent(t *testing.T) {
	interpreterStore(t) // build before disposable HOME changes, not into its module cache
	for _, failure := range []string{"missing changelog", "wrong identity", "mutable version"} {
		t.Run(failure, func(t *testing.T) {
			saveHome(t)
			asked := false
			c := startServer(t, true, func(p map[string]any) map[string]any { asked = true; return accept(p) })
			userConfigDir = os.UserConfigDir
			draft := authoredDraft(t)
			switch failure {
			case "missing changelog":
				os.Remove(filepath.Join(draft, "CHANGELOG.md"))
			case "wrong identity":
				b, _ := os.ReadFile(filepath.Join(draft, "primitive.yaml"))
				os.WriteFile(filepath.Join(draft, "primitive.yaml"), []byte(strings.Replace(string(b), "publisher: local.me", "publisher: other.me", 1)), 0600)
			case "mutable version":
				root, err := pack.CollectionDir()
				if err != nil {
					t.Fatal(err)
				}
				if _, _, _, err := author.SavePackage(draft, root, nil, ""); err != nil {
					t.Fatal(err)
				}
				os.WriteFile(filepath.Join(draft, "main.sh"), []byte("echo changed\n"), 0600)
			}
			res := c.call("tools/call", map[string]any{"name": "tap_save", "arguments": map[string]any{"package": draft}})
			if asked || res["isError"] != true {
				t.Fatalf("asked=%v res=%+v", asked, res)
			}
		})
	}
}

func TestSaveUsesReviewedSnapshotWhenDraftChangesDuringConsent(t *testing.T) {
	saveHome(t)
	draft := authoredDraft(t)
	c := startServer(t, true, func(p map[string]any) map[string]any {
		os.WriteFile(filepath.Join(draft, "main.sh"), []byte("echo unreviewed-edit\n"), 0600)
		return accept(p)
	})
	userConfigDir = os.UserConfigDir
	res := c.call("tools/call", map[string]any{"name": "tap_save", "arguments": map[string]any{"package": draft}})
	if res["isError"] == true {
		t.Fatal(res)
	}
	root, err := pack.CollectionDir()
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := os.ReadFile(filepath.Join(root, "release-check", "main.sh"))
	if string(saved) != "echo saved-and-ran\n" {
		t.Fatalf("saved unreviewed bytes: %s", saved)
	}
}

func TestRetainedVersionsAreDiscoverableAndRunTheirOriginalBytes(t *testing.T) {
	root := t.TempDir()
	draft := authoredDraft(t)
	old, _, _, err := author.SavePackage(draft, root, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	oldEntry, err := readCatalogEntry(old, "catalog-root")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(draft, "primitive.yaml"))
	os.WriteFile(filepath.Join(draft, "primitive.yaml"), []byte(strings.Replace(string(b), "version: 0.1.0", "version: 0.2.0", 1)), 0600)
	os.WriteFile(filepath.Join(draft, "CHANGELOG.md"), []byte("## 0.2.0\n\n- Revised output.\n\n## 0.1.0\n\n- Initial check.\n"), 0600)
	os.WriteFile(filepath.Join(draft, "main.sh"), []byte("echo revised-and-ran\n"), 0600)
	if _, _, _, err := author.SavePackage(draft, root, nil, ""); err != nil {
		t.Fatal(err)
	}
	catalog, err := localCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := resolveCatalog(catalog, oldEntry.Ref, oldEntry.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if retained.Path == oldEntry.Path {
		t.Fatal("old ref resolved to replaced active folder")
	}
	current, err := resolveCatalog(catalog, "local.me/release-check@0.2.0", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		entry catalogEntry
		want  string
	}{{retained, "saved-and-ran\n"}, {current, "revised-and-ran\n"}} {
		res, err := Run(context.Background(), Options{Package: tc.entry.Path, ExpectedDigest: tc.entry.Digest, Journal: io.Discard, InterpDir: interpreterStore(t), RunsDir: t.TempDir()})
		if err != nil || res.Stdout != tc.want {
			t.Fatalf("%s: %+v %v", tc.entry.Ref, res, err)
		}
	}
}
