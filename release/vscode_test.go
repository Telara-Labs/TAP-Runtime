package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func extensionFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "vscode"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"package.json", "extension.js", "README.md", "LICENSE"} {
		data, err := os.ReadFile(filepath.Join(repo, "vscode", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "vscode", name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func vsixContents(t *testing.T, path string) map[string][]byte {
	t.Helper()
	z, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	files := map[string][]byte{}
	for _, f := range z.File {
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if _, duplicate := files[f.Name]; duplicate {
			t.Fatalf("duplicate member %s", f.Name)
		}
		files[f.Name] = data
	}
	return files
}

// This runs the pinned official packager, rather than substituting a fake ZIP.
func TestVSIXOfficialPackagingAndSigning(t *testing.T) {
	dir := extensionFixture(t)
	source, _ := os.ReadFile(filepath.Join(dir, "vscode", "package.json"))
	os.WriteFile(filepath.Join(dir, "vscode", "unrelated-private.txt"), []byte("must not ship"), 0o600)
	one, err := packageVSIX(dir, t.TempDir(), "0.2.3", run)
	if err != nil {
		t.Fatal(err)
	}
	files := vsixContents(t, one)
	if len(files) != 6 {
		t.Fatalf("unexpected files: %v", files)
	}
	var pkg struct{ Name, Publisher, Version, Main string }
	if err := json.Unmarshal(files["extension/package.json"], &pkg); err != nil {
		t.Fatal(err)
	}
	if pkg.Name != "tap-vscode" || pkg.Publisher != "telara-labs" || pkg.Version != "0.2.3" || pkg.Main != "./extension.js" {
		t.Fatalf("incorrect extension identity: %+v", pkg)
	}
	manifest := string(files["extension.vsixmanifest"])
	if !strings.Contains(manifest, `Id="tap-vscode"`) || !strings.Contains(manifest, `Version="0.2.3"`) || !strings.Contains(manifest, `Publisher="telara-labs"`) {
		t.Fatalf("incorrect VSIX identity: %s", manifest)
	}
	for member, name := range map[string]string{"extension/extension.js": "extension.js", "extension/readme.md": "README.md", "extension/LICENSE.txt": "LICENSE"} {
		want, _ := os.ReadFile(filepath.Join(dir, "vscode", name))
		if !bytes.Equal(files[member], want) {
			t.Fatalf("payload differs: %s", member)
		}
	}
	after, _ := os.ReadFile(filepath.Join(dir, "vscode", "package.json"))
	if !bytes.Equal(source, after) {
		t.Fatal("packaging changed the source manifest")
	}
	two, err := packageVSIX(dir, t.TempDir(), "0.2.3", run)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(one)
	b, _ := os.ReadFile(two)
	if !bytes.Equal(a, b) {
		t.Fatal("identical extension builds are not reproducible")
	}
	t.Logf("official @vscode/vsce@4.0.0: telara-labs.tap-vscode version=%s bytes=%d sha256=%x; exact six members and payload bytes verified; second build byte-identical", pkg.Version, len(a), sha256.Sum256(a))
	key := filepath.Join(t.TempDir(), "key")
	if err := Keygen(key); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := Build(repo, out, "0.2.3", []string{host}, key+".key", "", one); err != nil {
		t.Fatal(err)
	}
	if err := Verify(out, key+".pub"); err != nil {
		t.Fatal(err)
	}
	sums, _ := os.ReadFile(filepath.Join(out, "SHA256SUMS"))
	if !strings.Contains(string(sums), "tap-vscode-0.2.3.vsix") {
		t.Fatal("VSIX not in signed checksum list")
	}
	if err := os.WriteFile(filepath.Join(out, filepath.Base(one)), append(a, 0), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Verify(out, key+".pub"); err == nil {
		t.Fatal("altered extension was accepted")
	}
	t.Log("generated VSIX listed in real Ed25519-signed SHA256SUMS; release verified with temporary public key; altered VSIX rejected")
}

func TestVSIXRefusesUnreviewedPackagingInputs(t *testing.T) {
	t.Run("invalid manifest", func(t *testing.T) {
		dir := extensionFixture(t)
		os.WriteFile(filepath.Join(dir, "vscode", "package.json"), []byte("null"), 0o644)
		if _, err := packageVSIX(dir, t.TempDir(), "0.2.3", run); err == nil {
			t.Fatal("null manifest accepted")
		}
	})
	t.Run("invalid version", func(t *testing.T) {
		if _, err := packageVSIX(extensionFixture(t), t.TempDir(), "../elsewhere", run); err == nil {
			t.Fatal("path-like version accepted")
		}
	})
	for _, test := range []struct{ name, field, value string }{
		{"range", "devDependencies", `{"@vscode/vsce":"^4.0.0"}`},
		{"runtime dependencies", "dependencies", `{"hidden-module":"1.0.0"}`},
		{"missing pin", "devDependencies", `{}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := extensionFixture(t)
			path := filepath.Join(dir, "vscode", "package.json")
			raw, _ := os.ReadFile(path)
			var pkg map[string]json.RawMessage
			json.Unmarshal(raw, &pkg)
			pkg[test.field] = json.RawMessage(test.value)
			raw, _ = json.Marshal(pkg)
			os.WriteFile(path, raw, 0o644)
			called := false
			_, err := packageVSIX(dir, t.TempDir(), "0.2.3", func(string, []string, string, ...string) ([]byte, error) { called = true; return nil, nil })
			if err == nil || called {
				t.Fatalf("bad packaging input reached packager: err=%v called=%v", err, called)
			}
		})
	}
	t.Run("symlink", func(t *testing.T) {
		dir := extensionFixture(t)
		path := filepath.Join(dir, "vscode", "extension.js")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(dir, "vscode", "README.md"), path); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		_, err := packageVSIX(dir, t.TempDir(), "0.2.3", run)
		if err == nil {
			t.Fatal("symlink accepted")
		}
	})
}

func TestVSIXNormalizationRejectsUnexpectedOrMissingPayload(t *testing.T) {
	base := []string{"[Content_Types].xml", "extension.vsixmanifest", "extension/package.json", "extension/extension.js", "extension/readme.md", "extension/LICENSE.txt"}
	for _, test := range []struct {
		name    string
		members []string
	}{
		{"extra", append(append([]string{}, base...), "extension/private.txt")},
		{"missing", base[:len(base)-1]},
		{"duplicate", append(append([]string{}, base...), base[0])},
	} {
		t.Run(test.name, func(t *testing.T) {
			var buf bytes.Buffer
			z := zip.NewWriter(&buf)
			for _, name := range test.members {
				w, err := z.Create(name)
				if err != nil {
					t.Fatal(err)
				}
				w.Write([]byte("fixture"))
			}
			if err := z.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "test.vsix")
			os.WriteFile(path, buf.Bytes(), 0o644)
			if err := normalizeVSIX(path); err == nil {
				t.Fatal("invalid archive was accepted")
			}
		})
	}
}

func TestPublisherPackagesTheCleanTagAndSignsExtra(t *testing.T) {
	dir := extensionFixture(t)
	git(t, dir, "init", "--quiet", "-b", "main")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "--quiet", "-m", "extension fixture")
	git(t, dir, "tag", "v0.2.3")
	want, _ := os.ReadFile(filepath.Join(dir, "vscode", "extension.js"))
	os.WriteFile(filepath.Join(dir, "vscode", "extension.js"), []byte("uncommitted working tree contents"), 0o644)
	var calls [][]string
	command := func(src string, env []string, name string, args ...string) ([]byte, error) {
		if name != "go" {
			return run(src, env, name, args...)
		}
		calls = append(calls, append([]string(nil), args...))
		if len(args) > 2 && args[2] == "build" {
			extra := args[len(args)-1]
			if args[len(args)-2] != "--extra" {
				return nil, fmt.Errorf("missing signed extra: %v", args)
			}
			files := vsixContents(t, extra)
			if !bytes.Equal(files["extension/extension.js"], want) {
				return nil, fmt.Errorf("packaged dirty working tree")
			}
		}
		return nil, nil
	}
	if err := buildFromExportWithRunner(dir, "owner.key", "o/r", command)("v0.2.3", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0][2] != "build" || calls[1][2] != "verify" {
		t.Fatalf("wrong publisher commands: %v", calls)
	}
	joined := strings.Join(calls[0], " ")
	if !strings.Contains(joined, "--version 0.2.3") || !strings.Contains(joined, "--key owner.key") || !strings.Contains(joined, "--download-base https://github.com/o/r/releases/download/v0.2.3") {
		t.Fatalf("release options changed: %s", joined)
	}
	// Packaging failure must prevent the release build and verification.
	var failedCalls []string
	fail := func(_ string, _ []string, name string, _ ...string) ([]byte, error) {
		failedCalls = append(failedCalls, name)
		return nil, fmt.Errorf("packager unavailable")
	}
	if err := buildFromExportWithRunner(dir, "owner.key", "o/r", fail)("v0.2.3", t.TempDir()); err == nil || strings.Join(failedCalls, ",") != "npm" {
		t.Fatalf("packaging failure lost: %v", err)
	}
}
