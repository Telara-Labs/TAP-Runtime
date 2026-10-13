package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
)

// The runner keeps exactly one version of what it uses. Interpreters are
// downloaded and compiled once, and a newer runner pins different ones; without
// a clean-up every release left its predecessor's files behind (measured: 17
// old interpreters and 35 compiled copies of one interpreter, 2.3 GB in all).
//
// What stays in the cache directory:
//
//	interpreters/  one file per pinned interpreter (and its .sha256 note), and
//	               the pinned wasmtime program once a Preview 3 component ran
//	compiled/      one directory per module the runner still uses, named by the
//	               module's SHA-256, holding only the current wazero version's
//	               entries for it, or the current wasmtime version's compiled
//	               file for a Preview 3 component
//	runs/          untouched here: it has its own retention (--retention-days)
//
// wazero names each compiled entry by a hash that includes its internal module
// identity and the CPU's feature bits, so the runner cannot compute which entry
// belongs to which module. Instead each module gets its own cache directory
// (compiled/<sha256 of the module>), so the question "is this entry still
// used" is the question "is this module still used", which is answerable.

// testCacheRoot is empty in every runner that is shipped. Tests that build the
// runner and run it as a separate process set it at link time
// (-ldflags=-X main.testCacheRoot=DIR), which is the only way to give another
// process a different default without an environment variable or a flag.
var testCacheRoot string

// cacheRoot is where the runner keeps everything it can rebuild or discard.
// A variable so that a test can move it: tests must never write to the cache
// of the person running them.
var cacheRoot = func() (string, error) {
	if testCacheRoot != "" {
		return testCacheRoot, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "tap-runtime"), nil
}

// cacheSub is the named directory of the cache root.
func cacheSub(name string) (string, error) {
	root, err := cacheRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, name), nil
}

const pruneStampName = "pruned-by"

var digestDirName = regexp.MustCompile(`^[0-9a-f]{64}$`)

// wazeroCacheDirName is the name wazero gives its own directory inside the
// one it is handed. It is the module's version, so a newer wazero starts
// empty. ok is false when the version cannot be read from the build.
func wazeroCacheDirName() (string, bool) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	for _, d := range bi.Deps {
		if d.Path == "github.com/tetratelabs/wazero" {
			v := d.Version
			if d.Replace != nil {
				v = d.Replace.Version
			}
			if v == "" || v == "(devel)" {
				return "", false
			}
			return "wazero-" + v + "-" + runtime.GOARCH + "-" + runtime.GOOS, true
		}
	}
	return "", false
}

type pruneReport struct {
	Removed []string // paths, relative to the cache they were in
	Freed   int64
	Errors  []error
}

func (r *pruneReport) remove(base, path string) {
	size := treeSize(path)
	if err := os.RemoveAll(path); err != nil {
		r.Errors = append(r.Errors, err)
		return
	}
	rel, err := filepath.Rel(base, path)
	if err != nil {
		rel = path
	}
	rel = filepath.Join(filepath.Base(base), rel)
	r.Removed = append(r.Removed, rel)
	r.Freed += size
}

// treeSize is the number of bytes of the regular files under path.
func treeSize(path string) int64 {
	var n int64
	filepath.WalkDir(path, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			n += info.Size()
		}
		return nil
	})
	return n
}

// currentInterpreters is the interpreter files this runner pins, and the
// digest of each module the runner will compile from them. A runner built from
// source pins no digest, so the digest is of the file it finds.
func currentInterpreters(interpDir string) (files map[string]bool, digests map[string]bool) {
	files, digests = map[string]bool{}, map[string]bool{}
	for _, in := range interpreters {
		files[in.File] = true
		files[in.File+".sha256"] = true
		files[wasmtimeFile()] = true
		switch {
		case in.SHA256 != "":
			digests[in.SHA256] = true
		case interpDir != "":
			if b, err := os.ReadFile(filepath.Join(interpDir, in.File)); err == nil {
				digests[digest(b)] = true
			}
		}
	}
	return files, digests
}

// installedModules is the digest of the compiled program of every installed
// primitive that has one. Saved earlier versions of a primitive are history,
// not installed, and do not count.
func installedModules() (map[string]bool, error) {
	entries, err := localCatalog()
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, e := range entries {
		if e.Manifest == nil || filepath.Ext(e.Manifest.Execution.Entrypoint) != ".wasm" {
			continue
		}
		if strings.Contains(filepath.ToSlash(e.Path), "/.versions/") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(e.Path, filepath.FromSlash(e.Manifest.Execution.Entrypoint)))
		if err != nil {
			continue
		}
		out[digest(b)] = true
	}
	return out, nil
}

// pruneCaches removes from interpDir and compiledDir everything the runner no
// longer uses. An empty directory name is skipped. keep is the digest of modules
// to keep besides the pinned interpreters and the installed compiled
// primitives; the run that is starting names its own module there.
func pruneCaches(interpDir, compiledDir string, keep map[string]bool) pruneReport {
	var rep pruneReport
	files, ipDigests := currentInterpreters(interpDir)
	if interpDir != "" {
		items, err := os.ReadDir(interpDir)
		if err != nil && !os.IsNotExist(err) {
			rep.Errors = append(rep.Errors, err)
		}
		for _, it := range items {
			if !it.Type().IsRegular() || files[it.Name()] {
				continue
			}
			rep.remove(interpDir, filepath.Join(interpDir, it.Name()))
		}
	}
	if compiledDir == "" {
		return rep
	}
	installed, err := installedModules()
	if err != nil {
		// Without knowing what is installed, nothing compiled is surely unused.
		rep.Errors = append(rep.Errors, fmt.Errorf("compiled entries kept: %w", err))
		return rep
	}
	want := map[string]bool{}
	for _, m := range []map[string]bool{ipDigests, installed, keep} {
		for d := range m {
			want[d] = true
		}
	}
	items, err := os.ReadDir(compiledDir)
	if err != nil {
		if !os.IsNotExist(err) {
			rep.Errors = append(rep.Errors, err)
		}
		return rep
	}
	current, haveCurrent := wazeroCacheDirName()
	for _, it := range items {
		p := filepath.Join(compiledDir, it.Name())
		switch {
		case it.IsDir() && digestDirName.MatchString(it.Name()):
			if !want[it.Name()] {
				rep.remove(compiledDir, p)
				continue
			}
			inner, err := os.ReadDir(p)
			if err != nil {
				rep.Errors = append(rep.Errors, err)
				continue
			}
			for _, w := range inner {
				// A wazero version that cannot be read from the build keeps
				// every wazero directory: none is known to be stale.
				if w.IsDir() && haveCurrent && strings.HasPrefix(w.Name(), "wazero-") && w.Name() != current {
					rep.remove(compiledDir, filepath.Join(p, w.Name()))
				}
				if w.IsDir() && strings.HasPrefix(w.Name(), "wasmtime-") && w.Name() != wasmtimeCacheDirName() {
					rep.remove(compiledDir, filepath.Join(p, w.Name()))
				}
			}
		case it.IsDir() && strings.HasPrefix(it.Name(), "wazero-"):
			// The layout of earlier runners: every module's entries together,
			// named by wazero's hash, which cannot be matched to a module.
			rep.remove(compiledDir, p)
		}
	}
	return rep
}

// pruneOnFirstStart prunes the default interpreter and compiled directories
// the first time this version of the runner starts. A stamp in the cache root
// records the version that last pruned. It reports problems and returns; a
// cache that cannot be tidied never stops a run.
func pruneOnFirstStart(interpOverride, compiledOverride string, keep map[string]bool) {
	defer func() {
		if r := recover(); r != nil {
			logf("cache      clean-up failed: %v", r)
		}
	}()
	root, err := cacheRoot()
	if err != nil {
		return
	}
	stamp := filepath.Join(root, pruneStampName)
	if b, err := os.ReadFile(stamp); err == nil && strings.TrimSpace(string(b)) == version {
		return
	}
	var interpDir, compiledDir string
	if interpOverride == "" {
		interpDir = filepath.Join(root, "interpreters")
	}
	if compiledOverride == "" {
		compiledDir = filepath.Join(root, "compiled")
	}
	if interpDir == "" && compiledDir == "" {
		return
	}
	rep := pruneCaches(interpDir, compiledDir, keep)
	for _, e := range rep.Errors {
		logf("cache      clean-up: %v", e)
	}
	if len(rep.Removed) > 0 {
		logf("cache      removed %d old entr%s (%s) left by earlier versions", len(rep.Removed), plural1(len(rep.Removed)), humanBytes(rep.Freed))
	}
	if len(rep.Errors) > 0 {
		return // try again at the next start
	}
	if err := os.MkdirAll(root, 0o700); err == nil {
		if err := os.WriteFile(stamp, []byte(version+"\n"), 0o600); err != nil {
			logf("cache      could not record the clean-up: %v", err)
		}
	}
}

func plural1(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// cacheSizes is what the cache holds, by directory.
func cacheSizes() (root string, sizes map[string]int64, total int64, err error) {
	root, err = cacheRoot()
	if err != nil {
		return "", nil, 0, err
	}
	sizes = map[string]int64{}
	items, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return root, sizes, 0, nil
		}
		return root, nil, 0, err
	}
	for _, it := range items {
		n := treeSize(filepath.Join(root, it.Name()))
		sizes[it.Name()] = n
		total += n
	}
	return root, sizes, total, nil
}

// cacheSummary is the line `tap version` prints.
func cacheSummary() string {
	root, sizes, total, err := cacheSizes()
	if err != nil {
		return ""
	}
	return fmt.Sprintf("cache %s  %s (compiled %s, interpreters %s, runs %s)", root, humanBytes(total),
		humanBytes(sizes["compiled"]), humanBytes(sizes["interpreters"]), humanBytes(sizes["runs"]))
}

// cacheCommand is `tap cache [prune]`.
func cacheCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cache", flag.ContinueOnError)
	fs.SetOutput(stderr)
	interpDir := fs.String("interpreters", "", "interpreter store; default is the user cache directory")
	cacheDir := fs.String("cache", "", "compiled-interpreter cache; default is the user cache directory")
	sub := "size"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	switch sub {
	case "size":
		fmt.Fprintln(stdout, cacheSummary())
		return 0
	case "prune":
		interp, err := storeDir(*interpDir)
		if err != nil {
			fmt.Fprintln(stderr, "tap:", err)
			return 1
		}
		compiled, err := compiledDir(*cacheDir)
		if err != nil {
			fmt.Fprintln(stderr, "tap:", err)
			return 1
		}
		rep := pruneCaches(interp, compiled, nil)
		sort.Strings(rep.Removed)
		for _, r := range rep.Removed {
			fmt.Fprintln(stdout, "removed", r)
		}
		fmt.Fprintf(stdout, "removed %d item(s), freed %s\n", len(rep.Removed), humanBytes(rep.Freed))
		for _, e := range rep.Errors {
			fmt.Fprintln(stderr, "tap: cache:", e)
		}
		if len(rep.Errors) == 0 && *interpDir == "" && *cacheDir == "" {
			if root, err := cacheRoot(); err == nil && os.MkdirAll(root, 0o700) == nil {
				os.WriteFile(filepath.Join(root, pruneStampName), []byte(version+"\n"), 0o600)
			}
		}
		if len(rep.Errors) > 0 {
			return 1
		}
		return 0
	}
	fmt.Fprintln(stderr, "usage: tap cache [size | prune [--interpreters DIR] [--cache DIR]]")
	return 2
}
