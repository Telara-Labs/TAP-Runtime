package pack

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Telara-Labs/TAP-Runtime/contract/manifest"
	"golang.org/x/mod/semver"
)

const BuildReceiptFile = "BUILD.json"

type BuildReceipt struct {
	Kind           string `json:"kind"`
	SourceDigest   string `json:"source_digest"`
	ArtifactDigest string `json:"artifact_digest"`
	Entrypoint     string `json:"entrypoint"`
	Toolchain      string `json:"toolchain"`
	Build          string `json:"build"`
}

// ContentDigest binds all regular package files, excluding only generated TAP
// installation metadata. It catches source/dependency and documentation edits
// as well as changes to the manifest and executable.
func ContentDigest(dir string, exclude ...string) (string, error) {
	ignore := map[string]bool{SavedMarker: true, "SKILL.md": true}
	for _, name := range exclude {
		ignore[filepath.ToSlash(filepath.Clean(name))] = true
	}
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if ignore[filepath.ToSlash(rel)] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("package member %s is not a regular file", path)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		s := sha256.Sum256(b)
		info, err := d.Info()
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = fmt.Sprintf("%s;executable=%t", hex.EncodeToString(s[:]), info.Mode()&0o111 != 0)
		return nil
	})
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(files)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func bytesDigest(b []byte) string { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }

// VersionHistoryDir is outside the active package, so old versions never
// become dependencies of a new package's digest or rebuild.
func VersionHistoryDir(root, publisher, name string) string {
	s := sha256.Sum256([]byte(publisher + "/" + name))
	return filepath.Join(root, ".versions", hex.EncodeToString(s[:12]))
}

// CheckLifecycle never executes source or a build command. A compiled receipt
// proves freshness of the author's build inputs, not independent publication
// sealing; a publisher must still use its isolated rebuild verifier.
func CheckLifecycle(dir, root string) (*manifest.Manifest, error) {
	m, err := manifest.Load(dir)
	if err != nil {
		return nil, err
	}
	if p := m.RunProblems(); len(p) > 0 {
		return nil, fmt.Errorf("primitive.yaml cannot be saved: %s", strings.Join(p, "; "))
	}
	if !SkillName.MatchString(m.Metadata.Name) || strings.TrimSpace(m.Metadata.Publisher) == "" || strings.ContainsAny(m.Metadata.Publisher, "/@ \t\n\r") {
		return nil, fmt.Errorf("manifest needs a valid name and publisher")
	}
	v := "v" + m.Metadata.Version
	if !semver.IsValid(v) || semver.Canonical(v) != strings.Split(v, "+")[0] {
		return nil, fmt.Errorf("metadata.version must be a full semantic version, such as 0.1.0")
	}
	b, err := os.ReadFile(filepath.Join(dir, "CHANGELOG.md"))
	if err != nil {
		return nil, fmt.Errorf("authoring requires CHANGELOG.md for version %s: %w", m.Metadata.Version, err)
	}
	heading := regexp.MustCompile(`(?m)^##[ \t]+\[?v?` + regexp.QuoteMeta(m.Metadata.Version) + `\]?(?:[ \t]+-[^\n]*)?[ \t]*\r?$`)
	loc := heading.FindIndex(b)
	if loc == nil {
		return nil, fmt.Errorf("CHANGELOG.md needs a ## %s entry", m.Metadata.Version)
	}
	body := string(b[loc[1]:])
	if next := regexp.MustCompile(`(?m)^##\s`).FindStringIndex(body); next != nil {
		body = body[:next[0]]
	}
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("CHANGELOG.md entry for %s must describe the change", m.Metadata.Version)
	}
	entry := m.Execution.Entrypoint
	artifact, err := os.ReadFile(filepath.Join(dir, entry))
	if err != nil {
		return nil, fmt.Errorf("entrypoint: %w", err)
	}
	if filepath.Ext(entry) == ".wasm" {
		if m.Provenance == nil || m.Provenance.Source == "" || m.Provenance.Source == entry || m.Provenance.Toolchain == "" || m.Provenance.Build == "" {
			return nil, fmt.Errorf("compiled authoring needs provenance.source, toolchain and build")
		}
		if filepath.IsAbs(m.Provenance.Source) || filepath.Clean(m.Provenance.Source) == ".." || strings.HasPrefix(filepath.Clean(m.Provenance.Source), ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("provenance.source must stay in the package")
		}
		if _, err := os.Stat(filepath.Join(dir, m.Provenance.Source)); err != nil {
			return nil, fmt.Errorf("compiled source is missing: %w", err)
		}
		if len(artifact) < 8 || string(artifact[:8]) != "\x00asm\x01\x00\x00\x00" {
			return nil, fmt.Errorf("compiled entrypoint is not a WebAssembly module")
		}
		var r BuildReceipt
		b, err := os.ReadFile(filepath.Join(dir, BuildReceiptFile))
		if err != nil {
			return nil, fmt.Errorf("compiled authoring requires BUILD.json; run tap discover build --approve-build <package>")
		}
		if err = json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("BUILD.json: %w", err)
		}
		source, err := ContentDigest(dir, entry, BuildReceiptFile)
		if err != nil {
			return nil, err
		}
		if r.Kind != "tap.build/v1" || r.SourceDigest != source || r.ArtifactDigest != bytesDigest(artifact) || r.Entrypoint != entry || r.Toolchain != m.Provenance.Toolchain || r.Build != m.Provenance.Build {
			return nil, fmt.Errorf("compiled artifact or source/manifest changed since build; run tap discover build --approve-build <package> again")
		}
	}
	digest, err := ContentDigest(dir)
	if err != nil {
		return nil, err
	}
	if root == "" {
		return m, nil
	}
	previous := []string{filepath.Join(root, m.Metadata.Name)}
	items, err := os.ReadDir(VersionHistoryDir(root, m.Metadata.Publisher, m.Metadata.Name))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, it := range items {
		if it.IsDir() && !strings.HasPrefix(it.Name(), ".") {
			previous = append(previous, filepath.Join(VersionHistoryDir(root, m.Metadata.Publisher, m.Metadata.Name), it.Name()))
		}
	}
	for _, old := range previous {
		if _, err := os.Stat(old); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return nil, err
		}
		prior, err := manifest.Load(old)
		if err != nil {
			return nil, fmt.Errorf("existing package %s has an invalid manifest: %w", old, err)
		}
		if prior.Metadata.Name != m.Metadata.Name || prior.Metadata.Publisher != m.Metadata.Publisher {
			return nil, fmt.Errorf("saved folder belongs to another primitive; publisher/name cannot be replaced")
		}
		if !semver.IsValid("v" + prior.Metadata.Version) {
			return nil, fmt.Errorf("saved package has invalid version %q", prior.Metadata.Version)
		}
		cmp := semver.Compare(v, "v"+prior.Metadata.Version)
		if cmp < 0 {
			return nil, fmt.Errorf("version %s regresses saved version %s", m.Metadata.Version, prior.Metadata.Version)
		}
		if cmp == 0 {
			before, err := ContentDigest(old)
			if err != nil {
				return nil, err
			}
			if digest != before {
				return nil, fmt.Errorf("package changed under version %s; update primitive.yaml version and CHANGELOG.md, then rebuild compiled output", m.Metadata.Version)
			}
		}
	}
	return m, nil
}

// InstallVersioned serializes cooperating authoring saves, validates the
// staged bytes and retains the old version before the atomic active swap.
func InstallVersioned(root, name string, pkg []byte, m Marker, skill string) (string, bool, error) {
	if !SkillName.MatchString(name) {
		return "", false, fmt.Errorf("invalid package name %q", name)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", false, err
	}
	lock := filepath.Join(root, "."+name+".saving-lock")
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	if err := lockSave(f); err != nil {
		return "", false, fmt.Errorf("another save may be in progress: %w", err)
	}
	// Keep the lock inode stable for concurrent savers. The operating system
	// releases this advisory lock if a process exits without cleaning up.
	defer unlockSave(f)
	stage, err := os.MkdirTemp(root, "."+name+".checking-")
	if err != nil {
		return "", false, err
	}
	defer os.RemoveAll(stage)
	if err := Unpack(pkg, stage); err != nil {
		return "", false, err
	}
	checked, err := CheckLifecycle(stage, root)
	if err != nil {
		return "", false, err
	}
	if checked.Metadata.Name != name || m.Name != checked.Metadata.Publisher+"/"+name {
		return "", false, fmt.Errorf("saved identity does not match primitive.yaml")
	}
	old := filepath.Join(root, name)
	if _, err := os.Stat(old); err == nil {
		if _, err := os.Stat(filepath.Join(old, SavedMarker)); err != nil {
			return "", false, &ErrNotSaved{Path: old}
		}
		prior, err := manifest.Load(old)
		if err != nil {
			return "", false, err
		}
		if prior.Metadata.Version != checked.Metadata.Version {
			archive := filepath.Join(VersionHistoryDir(root, prior.Metadata.Publisher, name), prior.Metadata.Version)
			if _, err := os.Stat(archive); os.IsNotExist(err) {
				if err := os.MkdirAll(filepath.Dir(archive), 0o755); err != nil {
					return "", false, err
				}
				tmp, err := os.MkdirTemp(filepath.Dir(archive), ".retaining-")
				if err != nil {
					return "", false, err
				}
				defer os.RemoveAll(tmp)
				if err := copyRegularTree(old, tmp); err != nil {
					return "", false, err
				}
				if err := os.Rename(tmp, archive); err != nil {
					return "", false, err
				}
			} else if err != nil {
				return "", false, err
			} else {
				retained, e := ContentDigest(archive)
				active, e2 := ContentDigest(old)
				if e != nil || e2 != nil || retained != active {
					return "", false, fmt.Errorf("retained version %s differs from active package; refusing overwrite", prior.Metadata.Version)
				}
			}
		}
	}
	return Install(root, name, pkg, m, skill)
}

func copyRegularTree(from, to string) error {
	return filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err
		}
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", p)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, info.Mode().Perm())
	})
}

// CheckFiles validates a generated proposal before it is offered for saving.
func CheckFiles(files map[string][]byte, root string) error {
	archive, _, err := PackFiles(files, func(string) bool { return false })
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "tap-proposal-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := Unpack(archive, dir); err != nil {
		return err
	}
	_, err = CheckLifecycle(dir, root)
	return err
}
