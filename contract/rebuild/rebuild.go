// Package rebuild checks that a primitive's program is what its source
// builds to. A wasm module is bytes nobody
// can read, so an approval that binds only its digest approves nothing. A
// release binds source plus a reproducible build, and the publish pipeline
// builds again and refuses if the bytes differ.
//
// This package is that check. Wiring it into the publish pipeline, and the
// attestation a sealed package hands to adopters, belong to agent-service
// .
//
// Verify RUNS THE BUILD COMMAND THE MANIFEST NAMES. That is arbitrary code
// chosen by the package's author. Whoever calls Verify on a package they do
// not trust must do so somewhere that code can do no harm. This package
// withholds the caller's environment and works in a copy; it is not a
// sandbox.
package rebuild

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Telara-Labs/TAP-Runtime/contract/manifest"
)

// Result is what a rebuild found.
type Result struct {
	Entrypoint string
	Shipped    string // sha256 of the program in the package
	Rebuilt    string // sha256 of the program the source built to
	Built      bool   // false when the entrypoint is its own source
	Seconds    float64
}

func digest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func copyTree(from, to string) error {
	return filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		if d.Type()&fs.ModeSymlink != 0 {
			// A link could point the build at files outside the package.
			return fmt.Errorf("%s is a symbolic link; a package that is rebuilt may not contain one", rel)
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

// Builder runs a build command in a directory and returns what it printed.
// The command is the author's. A Builder is where it runs, and so what it
// can reach.
type Builder interface {
	Run(ctx context.Context, dir, command string) ([]byte, error)
}

// Here runs the build as a process of the caller's, with the caller's files
// and network and without its environment. It is for an author checking
// their own package, never for a package somebody else wrote.
type Here struct{}

func (Here) Run(ctx context.Context, dir, command string) ([]byte, error) {
	home := filepath.Join(dir, ".home")
	os.MkdirAll(home, 0o755)
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	configureBuildProcess(cmd)
	// A compiler subprocess may inherit the shell's output pipes. Bound
	// draining those pipes after cancellation instead of waiting indefinitely.
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + os.TempDir(),
		"GOWORK=off", "GOFLAGS=-mod=mod", "CGO_ENABLED=0"}
	for _, keep := range []string{"GOMODCACHE", "GOPROXY", "GOCACHE"} {
		if v, ok := os.LookupEnv(keep); ok {
			cmd.Env = append(cmd.Env, keep+"="+v)
		}
	}
	return cmd.CombinedOutput()
}

// Verify rebuilds the package's program from its source, as a process of the
// caller's, and compares.
func Verify(dir string) (*Result, error) { return VerifyWith(dir, Here{}) }

// VerifyWith rebuilds the package's program from its source with b and
// compares.
func VerifyWith(dir string, b Builder) (*Result, error) {
	m, err := manifest.Load(dir)
	if err != nil {
		return nil, err
	}
	if p := m.RunProblems(); len(p) > 0 {
		return nil, fmt.Errorf("primitive.yaml cannot be run: %s", strings.Join(p, "; "))
	}
	entry := m.Execution.Entrypoint
	shipped, err := digest(filepath.Join(dir, entry))
	if err != nil {
		return nil, err
	}
	res := &Result{Entrypoint: entry, Shipped: shipped, Rebuilt: shipped}
	if filepath.Ext(entry) != ".wasm" {
		// The entrypoint is the source. There is nothing between what a
		// person reads and what runs.
		return res, nil
	}
	if m.Provenance == nil || strings.TrimSpace(m.Provenance.Build) == "" || strings.HasPrefix(m.Provenance.Build, manifest.TODO) {
		return nil, fmt.Errorf("%s is compiled and the manifest names no build; it cannot be checked against its source", entry)
	}
	if m.Provenance.Source == "" || !exists(filepath.Join(dir, m.Provenance.Source)) {
		return nil, fmt.Errorf("the manifest names source %q, which the package does not contain", m.Provenance.Source)
	}
	work, err := os.MkdirTemp("", "tap-rebuild-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	if err := copyTree(dir, work); err != nil {
		return nil, err
	}
	// The shipped program is removed, so a build that produces nothing
	// cannot pass by leaving it where it was.
	if err := os.Remove(filepath.Join(work, entry)); err != nil {
		return nil, err
	}
	t0 := time.Now()
	out, err := b.Run(context.Background(), work, m.Provenance.Build)
	if err != nil {
		return nil, fmt.Errorf("the build the manifest names failed: %w\n%s", err, tail(string(out)))
	}
	res.Built, res.Seconds = true, time.Since(t0).Seconds()
	rebuilt, err := digest(filepath.Join(work, entry))
	if err != nil {
		return nil, fmt.Errorf("the build ran and did not produce %s", entry)
	}
	res.Rebuilt = rebuilt
	if rebuilt != shipped {
		return res, fmt.Errorf("%s is not what its source builds to: the package ships %s and the source builds %s", entry, shipped, rebuilt)
	}
	return res, nil
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func tail(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > 12 {
		lines = lines[len(lines)-12:]
	}
	return strings.Join(lines, "\n")
}

// The rebuild verdicts of a release record.
const (
	Reproduced    = "reproduced"
	NotReproduced = "not_reproduced"
	NotAttempted  = "not_attempted"
)

// Verdict is the release record's verdict. With no Builder NOTHING THE
// AUTHOR WROTE IS RUN: a package whose entrypoint is its own source is
// reproduced, because there is nothing between what a person reads and what
// runs, and a compiled one is not_attempted. With a Builder a compiled one is
// built with it, which is as safe as the Builder is: package buildbox is one
// made for a caller that holds credentials.
//
// The error says why a verdict is not Reproduced, or why none could be
// reached. A release that is not Reproduced is not distributable.
func Verdict(dir string, b Builder) (string, *Result, error) {
	m, err := manifest.Load(dir)
	if err != nil {
		return NotAttempted, nil, err
	}
	entry := m.Execution.Entrypoint
	if filepath.Ext(entry) == ".wasm" && b == nil {
		shipped, err := digest(filepath.Join(dir, entry))
		if err != nil {
			return NotAttempted, nil, err
		}
		return NotAttempted, &Result{Entrypoint: entry, Shipped: shipped},
			fmt.Errorf("%s is compiled, and this pipeline does not run an author's build; it was not checked against its source", entry)
	}
	if b == nil {
		b = Here{} // a source entrypoint: nothing is built, and b is not used
	}
	res, err := VerifyWith(dir, b)
	switch {
	case err == nil:
		return Reproduced, res, nil
	case res != nil && res.Built && res.Rebuilt != res.Shipped:
		return NotReproduced, res, err
	default:
		return NotAttempted, res, err
	}
}
