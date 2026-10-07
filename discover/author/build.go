package author

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/contract/manifest"
	"github.com/Telara-Labs/TAP-Runtime/contract/rebuild"
	"github.com/Telara-Labs/TAP-Runtime/discover/pack"
)

// BuildPackage runs only as an explicit author operation, never during save or
// execution. Failed builds leave the author's current executable untouched.
func BuildPackage(dir string) (*pack.BuildReceipt, error) {
	m, err := manifest.Load(dir)
	if err != nil {
		return nil, err
	}
	if p := m.RunProblems(); len(p) > 0 {
		return nil, fmt.Errorf("invalid manifest: %v", p)
	}
	entry := m.Execution.Entrypoint
	if filepath.Ext(entry) != ".wasm" {
		return nil, fmt.Errorf("%s is source: no compiled build is needed", entry)
	}
	if m.Provenance == nil || m.Provenance.Source == "" || m.Provenance.Toolchain == "" || m.Provenance.Build == "" {
		return nil, fmt.Errorf("compiled package needs provenance.source, toolchain and build")
	}
	if filepath.IsAbs(m.Provenance.Source) || filepath.Clean(m.Provenance.Source) == ".." || strings.HasPrefix(filepath.Clean(m.Provenance.Source), ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("provenance.source must stay in the package")
	}
	before, err := pack.ContentDigest(dir, entry, pack.BuildReceiptFile)
	if err != nil {
		return nil, err
	}
	archive, _, err := PackageDir(dir)
	if err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp("", "tap-author-build-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	if err := pack.Unpack(archive, work); err != nil {
		return nil, err
	}
	if err := os.Remove(filepath.Join(work, entry)); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	os.Remove(filepath.Join(work, pack.BuildReceiptFile))
	os.Remove(filepath.Join(work, "SKILL.md"))
	os.Remove(filepath.Join(work, pack.SavedMarker))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	output, err := (rebuild.Here{}).Run(ctx, work, m.Provenance.Build)
	if err != nil {
		return nil, fmt.Errorf("author build failed: %w\n%s", err, output)
	}
	// Verify against the original snapshot, excluding compiler-created caches
	// or source files a build command may have rewritten.
	verification, err := os.MkdirTemp("", "tap-author-verify-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(verification)
	if err := pack.Unpack(archive, verification); err != nil {
		return nil, err
	}
	os.Remove(filepath.Join(verification, "SKILL.md"))
	os.Remove(filepath.Join(verification, pack.SavedMarker))
	artifact, err := os.ReadFile(filepath.Join(work, entry))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(verification, entry)), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(verification, entry), artifact, 0o644); err != nil {
		return nil, err
	}
	checked, err := rebuild.VerifyWith(verification, timedBuild{})
	if err != nil {
		return nil, err
	}
	after, err := pack.ContentDigest(dir, entry, pack.BuildReceiptFile)
	if err != nil {
		return nil, err
	}
	if before != after {
		return nil, fmt.Errorf("authoring files changed during build; retry with stable inputs")
	}
	if len(artifact) < 8 || string(artifact[:8]) != "\x00asm\x01\x00\x00\x00" {
		return nil, fmt.Errorf("build did not produce a WebAssembly module")
	}
	r := &pack.BuildReceipt{Kind: "tap.build/v1", SourceDigest: before, ArtifactDigest: checked.Shipped, Entrypoint: entry, Toolchain: m.Provenance.Toolchain, Build: m.Provenance.Build}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, entry)), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, entry), artifact, 0o644); err != nil {
		return nil, err
	}
	b, _ := json.MarshalIndent(r, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, pack.BuildReceiptFile), append(b, '\n'), 0o644); err != nil {
		return nil, err
	}
	return r, nil
}

func BuildCommand(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("discover build", flag.ContinueOnError)
	fs.SetOutput(errOut)
	approve := fs.Bool("approve-build", false, "run this authored package's build recipe locally, with the author's OS access")
	pos, flags := SplitPositional(args)
	if err := fs.Parse(flags); err != nil {
		return 2
	}
	pos = append(pos, fs.Args()...)
	if len(pos) != 1 {
		fmt.Fprintln(errOut, "usage: tap discover build --approve-build <package>")
		return 2
	}
	if !*approve {
		fmt.Fprintln(errOut, "review provenance.build, then use --approve-build to run the author's build command; nothing built")
		return 1
	}
	r, err := BuildPackage(pos[0])
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	json.NewEncoder(out).Encode(r)
	return 0
}

// Bound the verifier rebuild as well as the first explicit local build.
type timedBuild struct{}

func (timedBuild) Run(ctx context.Context, dir, command string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return (rebuild.Here{}).Run(ctx, dir, command)
}
