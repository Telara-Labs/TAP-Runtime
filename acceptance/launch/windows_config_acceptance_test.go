//go:build windows

package launch

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

func TestPublishedWindowsVolumeRootsAreRefused(t *testing.T) {
	v := version(t)
	m := newMachine(t)
	tap := npmTap(t, m, v)
	for i, path := range []string{`//`, `\\server\share`} {
		primitive := filepath.Join(m.home, fmt.Sprintf("root-fixture-%d", i))
		if err := os.Mkdir(primitive, 0o700); err != nil {
			t.Fatal(err)
		}
		quoted, err := json.Marshal(path)
		if err != nil {
			t.Fatal(err)
		}
		manifest := "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: dev.example, name: root-fixture, version: 0.1.0}\nexecution: {entrypoint: main.py}\nfiles:\n  - {path: " + string(quoted) + ", access: read}\n"
		for name, body := range map[string]string{"primitive.yaml": manifest, "main.py": "print(\"ROOT_PRIMITIVE_EXECUTED\")\n"} {
			if err := os.WriteFile(filepath.Join(primitive, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		r := m.run("", tap, primitive)
		want(t, r, 1, "root of the file system")
		for _, forbidden := range []string{"ROOT_PRIMITIVE_EXECUTED", "RESULT (exit", "compiled interpreter", "fetching"} {
			if strings.Contains(r.out, forbidden) {
				t.Errorf("root %q reached execution before refusal: %s", path, r.out)
			}
		}
	}
}

func TestPublishedWindowsConfigRoundtrip(t *testing.T) {
	v := version(t)
	m := newMachine(t)
	tap := npmTap(t, m, v)
	primitive := filepath.Join(m.home, "primitive with spaces café")
	copyTree(t, filepath.Join(root, "examples", "hello-sh"), primitive)
	digest, _, err := mf.RunDigest(primitive)
	if err != nil {
		t.Fatal(err)
	}
	defaultConfig := filepath.Join(m.home, "AppData", "Roaming")
	explicitConfig := filepath.Join(m.home, "explicit config café")
	for _, c := range []struct {
		name, dir string
		args      []string
	}{
		{"default APPDATA", defaultConfig, nil},
		{"typed override", explicitConfig, []string{"--config-dir", explicitConfig}},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := func(command string, tail ...string) []string {
				return append(append([]string{command}, c.args...), tail...)
			}
			// A published noninteractive command must refuse trust instead of
			// trying to read a human answer from the Windows console. It must not
			// create a grant as a side effect of that refusal.
			want(t, m.run("", tap, args("trust", primitive)...), 1, "no terminal to ask")
			trustPath := filepath.Join(c.dir, "tap", "trusted.json")
			if _, err := os.Stat(trustPath); !os.IsNotExist(err) {
				t.Fatalf("refused trust created a grant at %s: %v", trustPath, err)
			}

			// Seed an explicit fixture for the independent list/forget/config
			// roundtrip. This represents a prior human approval; redirected stdin
			// is never treated as approval by this test.
			if err := os.MkdirAll(filepath.Dir(trustPath), 0o700); err != nil {
				t.Fatal(err)
			}
			approved := map[string]struct {
				Name string `json:"name"`
				Path string `json:"path"`
				At   string `json:"at"`
			}{digest: {Name: "approved fixture", Path: primitive, At: "2026-10-13T00:00:00Z"}}
			b, err := json.MarshalIndent(approved, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(trustPath, append(b, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			want(t, m.run("", tap, args("trust", "--list")...), 0, digest[:12], primitive)
			b, err = os.ReadFile(trustPath)
			if err != nil {
				t.Fatal(err)
			}
			var trusted map[string]struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(b, &trusted); err != nil || trusted[digest].Path != primitive {
				t.Fatalf("trusted digest/path in %s: %v, %v", trustPath, trusted, err)
			}
			windowsPrivateACL(t, trustPath)
			want(t, m.run("", tap, args("bind", "--client", "acceptance", "fs.read", "server with spaces café")...), 0, "now uses")
			want(t, m.run("", tap, args("bind", "--list")...), 0, "acceptance", "fs.read", "server with spaces café")
			bindingPath := filepath.Join(c.dir, "tap", "bindings.json")
			b, err = os.ReadFile(bindingPath)
			if err != nil {
				t.Fatal(err)
			}
			var bindings map[string]map[string]string
			if err := json.Unmarshal(b, &bindings); err != nil || bindings["acceptance"]["fs.read"] != "server with spaces café" {
				t.Fatalf("binding roundtrip: %v, %v", bindings, err)
			}
			windowsPrivateACL(t, bindingPath)
			want(t, m.run("", tap, args("trust", "--forget", digest)...), 0, "forgot 1 package(s)")
			windowsPrivateACL(t, trustPath)
			want(t, m.run("", tap, args("trust", "--list")...), 0, "no packages are trusted")
			want(t, m.run("", tap, args("bind", "--client", "acceptance", "--forget", "fs.read")...), 0, "forgot the choice")
			want(t, m.run("", tap, args("bind", "--list")...), 0, "no choices are kept")
		})
	}
	// An override must not create a third store relative to the current directory.
	if _, err := os.Stat(filepath.Join(m.home, "tap", "trusted.json")); !os.IsNotExist(err) {
		t.Fatalf("unexpected working-directory config store: %v", err)
	}
	if strings.EqualFold(defaultConfig, explicitConfig) {
		t.Fatal("default and explicit test stores must differ")
	}
}
