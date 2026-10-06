//go:build windows

package launch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mf "github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

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
			want(t, m.run("", tap, args("trust", primitive)...), 0, "trusted:")
			want(t, m.run("", tap, args("trust", "--list")...), 0, digest[:12], primitive)
			trustPath := filepath.Join(c.dir, "tap", "trusted.json")
			b, err := os.ReadFile(trustPath)
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
