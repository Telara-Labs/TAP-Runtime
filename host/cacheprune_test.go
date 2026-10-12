package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// moveCacheRoot gives one test a cache root of its own.
func moveCacheRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "tap-runtime")
	was := cacheRoot
	cacheRoot = func() (string, error) { return root, nil }
	t.Cleanup(func() { cacheRoot = was })
	return root
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// compiledEntry puts one compiled entry of module sum in wazeroDir.
func compiledEntry(t *testing.T, compiled, sum, wazeroDir string) string {
	t.Helper()
	p := filepath.Join(compiled, sum, wazeroDir, strings.Repeat("e", 64))
	writeFile(t, p, "compiled code")
	return p
}

func TestPruneKeepsTheCurrentInterpretersAndRemovesOtherFiles(t *testing.T) {
	store := t.TempDir()
	for _, in := range interpreters {
		writeFile(t, filepath.Join(store, in.File), "current")
		writeFile(t, filepath.Join(store, in.File+".sha256"), "note")
	}
	writeFile(t, filepath.Join(store, "sh-0.0.1.wasm"), "an old interpreter")
	writeFile(t, filepath.Join(store, "sh-0.0.1.wasm.sha256"), "its note")
	writeFile(t, filepath.Join(store, "python-2.7.wasm"), "another old one")
	writeFile(t, filepath.Join(store, "kept-dir", "file"), "a directory is not an interpreter file")

	rep := pruneCaches(store, "", nil)
	if len(rep.Errors) > 0 {
		t.Fatal(rep.Errors)
	}
	for _, gone := range []string{"sh-0.0.1.wasm", "sh-0.0.1.wasm.sha256", "python-2.7.wasm"} {
		if exists(filepath.Join(store, gone)) {
			t.Errorf("%s was kept", gone)
		}
	}
	for _, in := range interpreters {
		for _, name := range []string{in.File, in.File + ".sha256"} {
			if !exists(filepath.Join(store, name)) {
				t.Errorf("current %s was removed", name)
			}
		}
	}
	if !exists(filepath.Join(store, "kept-dir", "file")) {
		t.Error("a directory in the interpreter store was removed")
	}
	if rep.Freed != int64(len("an old interpreter")+len("its note")+len("another old one")) || len(rep.Removed) != 3 {
		t.Errorf("report: %+v", rep)
	}
}

func TestPruneRemovesOtherWazeroVersionsAndTheOldLayout(t *testing.T) {
	store, compiled := t.TempDir(), t.TempDir()
	current, ok := wazeroCacheDirName()
	if !ok {
		t.Fatal("this build cannot say which wazero it carries")
	}
	sh := interpreters[".sh"]
	writeFile(t, filepath.Join(store, sh.File), "interpreter bytes")
	sum := digest([]byte("interpreter bytes"))
	if sh.SHA256 != "" {
		sum = sh.SHA256
	}
	keep := compiledEntry(t, compiled, sum, current)
	otherVersion := compiledEntry(t, compiled, sum, "wazero-v0.0.1-"+runtime.GOARCH+"-"+runtime.GOOS)
	otherPlatform := compiledEntry(t, compiled, sum, "wazero-"+strings.TrimPrefix(strings.SplitN(current, "-", 3)[1], "")+"-riscv64-plan9")
	legacy := filepath.Join(compiled, "wazero-v1.0.0-arm64-darwin", strings.Repeat("a", 64))
	writeFile(t, legacy, "entry of the old layout")
	writeFile(t, filepath.Join(compiled, "NOTES"), "not ours")
	writeFile(t, filepath.Join(compiled, "other", "file"), "not ours")

	rep := pruneCaches(store, compiled, nil)
	if len(rep.Errors) > 0 {
		t.Fatal(rep.Errors)
	}
	if !exists(keep) {
		t.Error("the current wazero entry of a current module was removed")
	}
	for name, p := range map[string]string{"another wazero version": otherVersion, "another platform": otherPlatform, "the old layout": legacy} {
		if exists(p) {
			t.Errorf("%s was kept", name)
		}
	}
	if !exists(filepath.Join(compiled, "NOTES")) || !exists(filepath.Join(compiled, "other", "file")) {
		t.Error("something that is not a compiled entry was removed")
	}
}

func TestPruneKeepsEntriesOfInterpretersAndInstalledPrimitivesOnly(t *testing.T) {
	cfg := t.TempDir()
	was := userConfigDir
	userConfigDir = func() (string, error) { return cfg, nil }
	t.Cleanup(func() { userConfigDir = was })
	t.Setenv("HOME", t.TempDir())

	store, compiled := t.TempDir(), t.TempDir()
	current, _ := wazeroCacheDirName()
	// Every interpreter the runner pins: the sum is the pinned digest, or the
	// digest of the file when a build from source pins none.
	var interpreterEntries []string
	for i, in := range interpreters {
		content := "interpreter " + i
		writeFile(t, filepath.Join(store, in.File), content)
		sum := in.SHA256
		if sum == "" {
			sum = digest([]byte(content))
		}
		interpreterEntries = append(interpreterEntries, compiledEntry(t, compiled, sum, current))
	}

	wasm := "\x00asm a compiled primitive"
	pkg := filepath.Join(cfg, "tap", "primitives", "compiled-one")
	writeFile(t, filepath.Join(pkg, "primitive.yaml"), "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: example.test, name: compiled-one, version: 1.0.0, description: A compiled primitive}\nexecution: {entrypoint: main.wasm}\n")
	writeFile(t, filepath.Join(pkg, "main.wasm"), wasm)
	installed := compiledEntry(t, compiled, digest([]byte(wasm)), current)
	replaced := compiledEntry(t, compiled, digest([]byte("the previous build of it")), current)
	stale := compiledEntry(t, compiled, digest([]byte("a module nobody uses")), current)

	rep := pruneCaches(store, compiled, nil)
	if len(rep.Errors) > 0 {
		t.Fatal(rep.Errors)
	}
	for _, p := range interpreterEntries {
		if !exists(p) {
			t.Errorf("an interpreter's entry was removed: %s", p)
		}
	}
	if !exists(installed) {
		t.Error("the entry of an installed compiled primitive was removed")
	}
	if exists(replaced) {
		t.Error("the entry of a primitive's previous build was kept")
	}
	if exists(stale) {
		t.Error("an entry no module uses was kept")
	}

	// The module a run is starting counts, wherever its package lives.
	runStarting := compiledEntry(t, compiled, digest([]byte("a package outside the catalog")), current)
	pruneCaches(store, compiled, map[string]bool{digest([]byte("a package outside the catalog")): true})
	if !exists(runStarting) {
		t.Error("the module of the run that is starting was removed")
	}
}

func TestPruneRunsOncePerRunnerVersion(t *testing.T) {
	root := moveCacheRoot(t)
	was := version
	t.Cleanup(func() { version = was })
	version = "1.0.0"
	stale := filepath.Join(root, "interpreters", "sh-0.0.1.wasm")

	writeFile(t, stale, "old")
	pruneOnFirstStart("", "", nil)
	if exists(stale) {
		t.Fatal("the first start of a version kept a stale interpreter")
	}
	if b, _ := os.ReadFile(filepath.Join(root, pruneStampName)); strings.TrimSpace(string(b)) != "1.0.0" {
		t.Fatalf("stamp %q", b)
	}

	writeFile(t, stale, "old again")
	pruneOnFirstStart("", "", nil)
	if !exists(stale) {
		t.Fatal("a second start of the same version pruned again")
	}

	version = "1.0.1"
	pruneOnFirstStart("", "", nil)
	if exists(stale) {
		t.Fatal("the first start of a newer version did not prune")
	}
}

func TestPruneLeavesWhatIsNotACache(t *testing.T) {
	root := moveCacheRoot(t)
	cfg := t.TempDir()
	was := userConfigDir
	userConfigDir = func() (string, error) { return cfg, nil }
	t.Cleanup(func() { userConfigDir = was })

	keep := []string{
		filepath.Join(root, "runs", "20261012-a", "record.json"),
		filepath.Join(root, "requests-claude-code.json"),
		filepath.Join(root, "relay", "socket"),
		filepath.Join(root, "shared", "r-1.lock"),
		filepath.Join(cfg, "tap", "trusted_publishers.json"),
		filepath.Join(cfg, "tap", "trust", "store.json"),
		filepath.Join(cfg, "tap", "bindings.json"),
	}
	for _, p := range keep {
		writeFile(t, p, "mine")
	}
	writeFile(t, filepath.Join(root, "interpreters", "sh-0.0.1.wasm"), "old")
	writeFile(t, filepath.Join(root, "compiled", strings.Repeat("b", 64), "wazero-v0.0.1-x-y", "e"), "old")
	pruneOnFirstStart("", "", nil)
	for _, p := range keep {
		if b, err := os.ReadFile(p); err != nil || string(b) != "mine" {
			t.Errorf("%s was touched: %v", p, err)
		}
	}
	if exists(filepath.Join(root, "interpreters", "sh-0.0.1.wasm")) || exists(filepath.Join(root, "compiled", strings.Repeat("b", 64))) {
		t.Error("the stale cache entries were kept")
	}

	// Directories a person chose with --interpreters and --cache are not cleaned
	// at start.
	chosen := filepath.Join(t.TempDir(), "mine")
	writeFile(t, filepath.Join(chosen, "sh-0.0.1.wasm"), "old")
	version = "again"
	t.Cleanup(func() { version = "dev" })
	pruneOnFirstStart(chosen, chosen, nil)
	if !exists(filepath.Join(chosen, "sh-0.0.1.wasm")) {
		t.Error("a directory the person chose was cleaned at start")
	}
}

func TestAPruneThatFailsDoesNotFailARun(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("a read-only directory cannot be made to refuse a removal here")
	}
	root := moveCacheRoot(t)
	store := filepath.Join(root, "interpreters")
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}
	built, err := os.ReadFile(filepath.Join(interpreterStore(t), "sh.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(store, interpreters[".sh"].File), string(built))
	writeFile(t, filepath.Join(store, "sh-0.0.1.wasm"), "old")
	os.Chmod(store, 0o500) // files can be read, not removed
	t.Cleanup(func() { os.Chmod(store, 0o700) })

	pkg := writePackage(t, "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: example.test, name: hi, version: 1.0.0}\nexecution: {entrypoint: main.sh}\n", "echo still runs\n")
	res, err := Run(context.Background(), Options{Package: pkg, Journal: io.Discard})
	if err != nil || !strings.Contains(res.Stdout, "still runs") {
		t.Fatalf("a failed clean-up stopped the run: %v %+v", err, res)
	}
	if exists(filepath.Join(root, pruneStampName)) {
		t.Error("a clean-up that failed was recorded as done")
	}
}

// snapshot lists every file under dir with its size and modification time.
func snapshot(dir string) []string {
	var out []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			out = append(out, p+" "+info.ModTime().String()+" "+humanBytes(info.Size()))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func TestARunLeavesOneInterpreterAndOneCompiledEntry(t *testing.T) {
	root := moveCacheRoot(t)
	store := filepath.Join(root, "interpreters")
	built, err := os.ReadFile(filepath.Join(interpreterStore(t), "sh.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(store, interpreters[".sh"].File), string(built))
	// What earlier runners left behind.
	current, _ := wazeroCacheDirName()
	writeFile(t, filepath.Join(store, "sh-0.1.2.wasm"), "old")
	writeFile(t, filepath.Join(store, "sh-0.1.2.wasm.sha256"), "old")
	writeFile(t, filepath.Join(root, "compiled", "wazero-"+strings.TrimPrefix(current, "wazero-"), strings.Repeat("c", 64)), strings.Repeat("x", 4096))
	compiledEntry(t, filepath.Join(root, "compiled"), digest([]byte("a build of the interpreter nobody has now")), current)

	// The real cache, which this test must not change.
	realBase, _ := os.UserCacheDir()
	realRoot := filepath.Join(realBase, "tap-runtime")
	if realRoot == root {
		t.Fatal("the test cache is the real one")
	}
	before := append(snapshot(filepath.Join(realRoot, "compiled")), snapshot(filepath.Join(realRoot, "interpreters"))...)

	pkg := writePackage(t, "apiVersion: primitives.telara.dev/v3\nkind: Primitive\nmetadata: {publisher: example.test, name: hi, version: 1.0.0}\nexecution: {entrypoint: main.sh}\n", "echo hello\n")
	for i := 0; i < 2; i++ {
		res, err := Run(context.Background(), Options{Package: pkg, Journal: io.Discard})
		if err != nil || !strings.Contains(res.Stdout, "hello") {
			t.Fatalf("run %d: %v %+v", i, err, res)
		}
	}

	var files []string
	for _, p := range snapshot(store) {
		files = append(files, strings.Fields(p)[0])
	}
	if len(files) != 2 || filepath.Base(files[0]) != interpreters[".sh"].File || filepath.Base(files[1]) != interpreters[".sh"].File+".sha256" {
		t.Fatalf("interpreters left: %v", files)
	}
	var entries []string
	for _, p := range snapshot(filepath.Join(root, "compiled")) {
		entries = append(entries, strings.Fields(p)[0])
	}
	if len(entries) != 1 {
		t.Fatalf("compiled entries left: %v", entries)
	}
	rel, _ := filepath.Rel(filepath.Join(root, "compiled"), entries[0])
	if parts := strings.Split(rel, string(filepath.Separator)); len(parts) != 3 || parts[0] != digest(built) || parts[1] != current {
		t.Fatalf("compiled entry %s is not <interpreter digest>/<wazero version>/<key>", rel)
	}
	t.Logf("steady state for an sh-only primitive: interpreter %s + compiled %s = %s",
		humanBytes(treeSize(store)), humanBytes(treeSize(filepath.Join(root, "compiled"))),
		humanBytes(treeSize(store)+treeSize(filepath.Join(root, "compiled"))))

	after := append(snapshot(filepath.Join(realRoot, "compiled")), snapshot(filepath.Join(realRoot, "interpreters"))...)
	if strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Errorf("a run with default options changed the real cache:\nbefore %v\nafter  %v", before, after)
	}
}

func TestTheDefaultCacheRootIsNotTheRealOneUnderTest(t *testing.T) {
	realBase, err := os.UserCacheDir()
	if err != nil {
		t.Skip(err)
	}
	for _, name := range []string{"interpreters", "compiled", "runs", "relay"} {
		p, err := cacheSub(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(p, realBase+string(filepath.Separator)) {
			t.Errorf("%s resolves to %s, under the real cache", name, p)
		}
	}
	if d, err := storeDir(""); err != nil || strings.HasPrefix(d, realBase) {
		t.Errorf("storeDir %q %v", d, err)
	}
}

func TestCacheCommandPrunesAndReports(t *testing.T) {
	root := moveCacheRoot(t)
	writeFile(t, filepath.Join(root, "interpreters", "sh-0.0.1.wasm"), strings.Repeat("o", 2048))
	var out, errb strings.Builder
	if code := cacheCommand([]string{"prune"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "removed interpreters"+string(filepath.Separator)+"sh-0.0.1.wasm") || !strings.Contains(out.String(), "freed 2.0 KB") {
		t.Fatalf("output %q", out.String())
	}
	if exists(filepath.Join(root, "interpreters", "sh-0.0.1.wasm")) {
		t.Fatal("not removed")
	}
	out.Reset()
	if code := cacheCommand(nil, &out, &errb); code != 0 || !strings.Contains(out.String(), "cache "+root) {
		t.Fatalf("size line %q", out.String())
	}
}
