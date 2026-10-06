package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

type releaseCommand func(dir string, env []string, name string, args ...string) ([]byte, error)

// packageVSIX uses the pinned official packager on only the runtime files
// from the clean tag export. Version stamping never edits the shared checkout.
func packageVSIX(repo, out, version string, command releaseCommand) (string, error) {
	if !versionRe.MatchString(version) {
		return "", fmt.Errorf("invalid extension release version %q", version)
	}
	stage, err := os.MkdirTemp("", "tap-vsix-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	for _, name := range []string{"package.json", "extension.js", "README.md", "LICENSE"} {
		source := filepath.Join(repo, "vscode", name)
		info, err := os.Lstat(source)
		if err != nil || !info.Mode().IsRegular() {
			return "", fmt.Errorf("extension %s must be a regular file", name)
		}
		raw, err := os.ReadFile(source)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(stage, name), raw, 0o644); err != nil {
			return "", err
		}
	}
	raw, err := os.ReadFile(filepath.Join(stage, "package.json"))
	if err != nil {
		return "", err
	}
	var pkg map[string]json.RawMessage
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return "", err
	}
	if pkg == nil {
		return "", fmt.Errorf("extension package.json must be an object")
	}
	var dev, dependencies map[string]string
	if err := json.Unmarshal(pkg["devDependencies"], &dev); err != nil {
		return "", fmt.Errorf("extension needs a pinned @vscode/vsce dependency: %w", err)
	}
	pin := dev["@vscode/vsce"]
	if !versionRe.MatchString(pin) {
		return "", fmt.Errorf("@vscode/vsce must have an exact version, got %q", pin)
	}
	if len(pkg["dependencies"]) != 0 {
		if err := json.Unmarshal(pkg["dependencies"], &dependencies); err != nil || len(dependencies) != 0 {
			return "", fmt.Errorf("extension runtime dependencies require a reviewed packaging change")
		}
	}
	pkg["version"], _ = json.Marshal(version)
	pkg["files"], _ = json.Marshal([]string{"extension.js", "package.json", "README.md", "LICENSE"})
	raw, err = json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(stage, "package.json"), append(raw, '\n'), 0o644); err != nil {
		return "", err
	}
	abs, err := filepath.Abs(out)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", err
	}
	artifact := filepath.Join(abs, "tap-vscode-"+version+".vsix")
	if _, err := command(stage, nil, "npm", "exec", "--yes", "--package=@vscode/vsce@"+pin, "--", "vsce", "package", "--no-dependencies", "--out", artifact); err != nil {
		os.Remove(artifact)
		return "", err
	}
	// vsce's ZIP timestamps reflect staging time. Normalize that container
	// metadata while preserving the official manifest and payload bytes.
	if err := normalizeVSIX(artifact); err != nil {
		os.Remove(artifact)
		return "", err
	}
	return artifact, nil
}

func normalizeVSIX(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return err
	}
	sort.Slice(zr.File, func(i, j int) bool { return zr.File[i].Name < zr.File[j].Name })
	allowed := map[string]bool{"[Content_Types].xml": true, "extension.vsixmanifest": true,
		"extension/package.json": true, "extension/extension.js": true, "extension/readme.md": true, "extension/LICENSE.txt": true}
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for _, f := range zr.File {
		if !allowed[f.Name] {
			return fmt.Errorf("unexpected VSIX member %q", f.Name)
		}
		delete(allowed, f.Name)
		r, err := f.Open()
		if err != nil {
			return err
		}
		data, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			return err
		}
		h := &zip.FileHeader{Name: f.Name, Method: zip.Deflate}
		h.SetMode(0o644)
		w, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
	}
	if len(allowed) != 0 {
		return fmt.Errorf("VSIX is missing required members: %v", allowed)
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, out.Bytes(), 0o644)
}
