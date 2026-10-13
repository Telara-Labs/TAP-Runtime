package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ulikunitz/xz"
)

// A component that needs WASI Preview 3 runs in a separate process: the
// official wasmtime command-line program, at one pinned version. Preview 1
// modules and Preview 2 components keep running inside this process, and a
// person who never runs a Preview 3 component never downloads wasmtime.
//
// The process is held the way an in-process guest is: its standard input and
// output are the broker's pipes, it gets the primitive's arguments and
// nothing else (no directory, no environment variable, no network, no HTTP),
// its memory is capped at the runner's guest limit, and it is killed when the
// run's time is up or the run stops.

const wasmtimeVersion = "49.0.2"

// wasmtimeBuild is one platform's official release archive of the pinned
// version, and the digest of the program inside it.
type wasmtimeBuild struct {
	URL           string
	ArchiveSHA256 string
	ExeSHA256     string
}

// wasmtimeBuilds are the official archives of wasmtime v49.0.2, from
// https://github.com/bytecodealliance/wasmtime/releases/tag/v49.0.2.
//
// How the numbers were obtained, to repeat at the next version:
//  1. Download each archive below from the release page.
//  2. sha256 of the archive; it must equal the asset's "digest" field in
//     `gh api repos/bytecodealliance/wasmtime/releases/tags/vX.Y.Z`
//     (GitHub computes that digest itself when the asset is uploaded).
//  3. Extract it and take the sha256 of the wasmtime program inside
//     (wasmtime.exe for Windows).
//
// Only the platforms the runner is released for are listed. A platform
// missing here cannot run Preview 3 components; nothing else is affected.
var wasmtimeBuilds = map[string]wasmtimeBuild{
	"darwin/arm64": {
		URL:           "https://github.com/bytecodealliance/wasmtime/releases/download/v49.0.2/wasmtime-v49.0.2-aarch64-macos.tar.xz",
		ArchiveSHA256: "ea5a79261caa39819b863fd850879428cf90548353e1e5cf9bcbcc4e7addf2c1",
		ExeSHA256:     "4408e44af098f7581366b0396088924d7e181720b9309574b4c30bac1c86b6dc",
	},
	"darwin/amd64": {
		URL:           "https://github.com/bytecodealliance/wasmtime/releases/download/v49.0.2/wasmtime-v49.0.2-x86_64-macos.tar.xz",
		ArchiveSHA256: "b24513efff6a5dbeb196e2a01097ba632a8b0ea1563ba297ada26ceb3aa067d9",
		ExeSHA256:     "3e4920a081ee945c7633ac82ad4cf8456551b1913586da77eea6c0520a32eadc",
	},
	"linux/arm64": {
		URL:           "https://github.com/bytecodealliance/wasmtime/releases/download/v49.0.2/wasmtime-v49.0.2-aarch64-linux.tar.xz",
		ArchiveSHA256: "ca14988c6da3d92512bd9c3bad6cbd06b1f5743b6911a9e709f1a6fa30637d7f",
		ExeSHA256:     "776cf37c495c70ee22144ff4c881bd18c1997edcb7a226dcc4583fd19cefc492",
	},
	"linux/amd64": {
		URL:           "https://github.com/bytecodealliance/wasmtime/releases/download/v49.0.2/wasmtime-v49.0.2-x86_64-linux.tar.xz",
		ArchiveSHA256: "a4d6e9e3a5a60f527cf7793d674c48930c80c2e8977995b8a275cad3254b9322",
		ExeSHA256:     "d0a014e0d5b0cf48dd3549c38e2e6ecd78ff09fb3e3f10d751b87b72bdfd8635",
	},
	"windows/amd64": {
		URL:           "https://github.com/bytecodealliance/wasmtime/releases/download/v49.0.2/wasmtime-v49.0.2-x86_64-windows.zip",
		ArchiveSHA256: "c95a3925acde6398808baabe5ad13d0b1df02b004bc256c00cf8e2a36daccea8",
		ExeSHA256:     "51165ae46e2e4ca71b52efff33432d9e58db41bf236cb7c768adb03f653a7152",
	},
}

// wasmtimeFile is the pinned program's name in the interpreter store. The
// version is in the name, so the cache clean-up removes other versions.
func wasmtimeFile() string {
	name := "wasmtime-v" + wasmtimeVersion
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// wasmtimeCacheDirName is the directory, under a component's compiled/<sha256>
// directory, that holds what this wasmtime compiled from it. Code compiled by
// one wasmtime cannot be loaded by another.
func wasmtimeCacheDirName() string {
	return "wasmtime-v" + wasmtimeVersion + "-" + runtime.GOARCH + "-" + runtime.GOOS
}

// wasmtimeConfig is the fixed part of every command line: settings that
// decide how code is compiled (and so must match between compile and run),
// and every grant the program could otherwise make, denied. wasmtime gives
// no directory and no environment variable unless --dir or --env is passed,
// and neither ever is. -C cache=n keeps it from writing a cache of its own
// or reading a cache configuration file; --config is never passed, so no
// configuration file is read at all.
var wasmtimeConfig = []string{"-C", "cache=n", "-S", "p3=y"}

var wasmtimeDenied = []string{
	"-S", "inherit-env=n",
	"-S", "inherit-network=n",
	"-S", "tcp=n",
	"-S", "udp=n",
	"-S", "allow-ip-name-lookup=n",
	"-S", "http=n",
	"-S", "tls=n",
	"-S", "config=n",
	"-S", "keyvalue=n",
	"-S", "nn=n",
}

// wasmtimeRunArgs is the command line that runs a compiled component. The
// memory ceiling is the one an in-process guest has, and one linear memory
// is allowed, so that ceiling holds for the whole program.
func wasmtimeRunArgs(cwasm string, args []string) []string {
	out := []string{"run"}
	out = append(out, wasmtimeConfig...)
	out = append(out, wasmtimeDenied...)
	out = append(out,
		"-W", "max-memory-size="+strconv.FormatInt(int64(guestMemoryPages)*65536, 10),
		"-W", "max-memories=1",
		"--allow-precompiled",
		"--argv0", "primitive",
		cwasm)
	return append(out, args...)
}

// wasmtimeVerified remembers the files already found to be the pinned
// program in this process, by path, size and modification time, so a burst
// of runs hashes the 60 MB program once.
var wasmtimeVerified = struct {
	sync.Mutex
	files map[string]string // path -> "size mtime"
}{files: map[string]string{}}

func fileStamp(path string) (string, bool) {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	return fmt.Sprintf("%d %d", fi.Size(), fi.ModTime().UnixNano()), true
}

// isPinnedWasmtime reports whether the file at path is the pinned program,
// by its SHA-256. got is the digest it has.
func isPinnedWasmtime(path, want string) (ok bool, got string, err error) {
	stamp, exists := fileStamp(path)
	if !exists {
		return false, "", os.ErrNotExist
	}
	wasmtimeVerified.Lock()
	known := wasmtimeVerified.files[path] == stamp
	wasmtimeVerified.Unlock()
	if known {
		return true, want, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return false, "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, "", err
	}
	got = hex.EncodeToString(h.Sum(nil))
	if got != want {
		return false, got, nil
	}
	wasmtimeVerified.Lock()
	wasmtimeVerified.files[path] = stamp
	wasmtimeVerified.Unlock()
	return true, got, nil
}

var obtainingWasmtime sync.Mutex

// obtainWasmtime returns the path of the pinned wasmtime program. It uses,
// in order: the copy in the store; a wasmtime on PATH that is exactly the
// official build of the pinned version (its SHA-256 matches); the official archive, downloaded once into the store and checked
// against the archive's and the program's digests.
func obtainWasmtime(store string) (string, error) {
	build, ok := wasmtimeBuilds[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		return "", fmt.Errorf("this component needs WASI Preview 3, which the runner runs in wasmtime %s; no official wasmtime build is pinned for %s/%s, so Preview 3 components cannot run here (Preview 1 modules and Preview 2 components can)", wasmtimeVersion, runtime.GOOS, runtime.GOARCH)
	}
	obtainingWasmtime.Lock()
	defer obtainingWasmtime.Unlock()

	cached := filepath.Join(store, wasmtimeFile())
	switch ok, got, err := isPinnedWasmtime(cached, build.ExeSHA256); {
	case ok:
		return cached, nil
	case err == nil:
		logf("wasmtime   %s has sha256 %s, not the pinned %s; replacing it", cached, got, build.ExeSHA256)
		os.Remove(cached)
	}

	if p, err := exec.LookPath("wasmtime"); err == nil {
		if why := localWasmtimeProblem(p, build.ExeSHA256); why == "" {
			logf("wasmtime   using %s (the official %s build)", p, wasmtimeVersion)
			return p, nil
		} else {
			logf("wasmtime   not using %s: %s; using the pinned official build instead", p, why)
		}
	}

	logf("fetching   %s (once; Preview 3 components run in it)", build.URL)
	archive, err := download(build.URL)
	if err != nil {
		return "", fmt.Errorf("wasmtime %s: %w (source: %s)%s", wasmtimeVersion, err, build.URL, noNetworkHint())
	}
	if got := digest(archive); got != build.ArchiveSHA256 {
		return "", fmt.Errorf("wasmtime %s: downloaded archive has sha256 %s, pinned %s; refused", wasmtimeVersion, got, build.ArchiveSHA256)
	}
	exe, err := extractWasmtime(build.URL, archive)
	if err != nil {
		return "", fmt.Errorf("wasmtime %s: %w", wasmtimeVersion, err)
	}
	if got := digest(exe); got != build.ExeSHA256 {
		return "", fmt.Errorf("wasmtime %s: the program in the archive has sha256 %s, pinned %s; refused", wasmtimeVersion, got, build.ExeSHA256)
	}
	if err := writeFileAtomic(cached, exe, 0o700); err != nil {
		return "", err
	}
	return cached, nil
}

// localWasmtimeProblem says why a wasmtime found on this machine is not
// used, or "" when it is the official build of the pinned version. Only its
// SHA-256 is checked, and it is never run to ask: a program that is not the
// pinned build is not executed at all. A build of the same version from
// source, or from a package manager, is a different program.
func localWasmtimeProblem(p, wantSHA string) string {
	ok, got, err := isPinnedWasmtime(p, wantSHA)
	if err != nil {
		return err.Error()
	}
	if !ok {
		return fmt.Sprintf("its sha256 is %s, not that of the official %s build (%s)", got, wasmtimeVersion, wantSHA)
	}
	return ""
}

// extractWasmtime takes the wasmtime program out of an official archive:
// a .tar.xz for macOS and Linux, a .zip for Windows.
func extractWasmtime(url string, archive []byte) ([]byte, error) {
	const limit = 256 << 20
	if strings.HasSuffix(url, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if path.Base(f.Name) != "wasmtime.exe" || f.FileInfo().IsDir() {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(io.LimitReader(rc, limit))
		}
		return nil, errors.New("the archive holds no wasmtime.exe")
	}
	xr, err := xz.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(xr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, errors.New("the archive holds no wasmtime program")
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && path.Base(h.Name) == "wasmtime" {
			return io.ReadAll(io.LimitReader(tr, limit))
		}
	}
}

// writeFileAtomic writes b beside path and renames it into place, so a run
// never finds half a file and two runs writing at once both end with a
// whole one.
func writeFileAtomic(path string, b []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	cerr := f.Close()
	if err := errors.Join(werr, cerr, os.Chmod(tmp, mode)); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// wasmtimeProgram is a Preview 3 component compiled by the pinned wasmtime,
// ready to run.
type wasmtimeProgram struct {
	exe, cwasm string
}

// prepareWasmtime obtains wasmtime and compiles the component once, into
// compiled/<sha256 of the component>/<wasmtimeCacheDirName>/. Later runs load
// the compiled file directly, which starts in milliseconds.
func prepareWasmtime(interpOverride, compiledOverride, sum, entrypoint string, wasm []byte) (*wasmtimeProgram, error) {
	store, err := storeDir(interpOverride)
	if err != nil {
		return nil, err
	}
	exe, err := obtainWasmtime(store)
	if err != nil {
		return nil, err
	}
	root, err := compiledDir(compiledOverride)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, sum, wasmtimeCacheDirName())
	name := strings.TrimSuffix(filepath.Base(entrypoint), filepath.Ext(entrypoint)) + ".cwasm"
	cwasm := filepath.Join(dir, name)
	unlock := compileLock(wasm)
	defer unlock()
	if _, err := os.Stat(cwasm); err == nil {
		return &wasmtimeProgram{exe: exe, cwasm: cwasm}, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// wasmtime compiles from a file. The component is written from the bytes
	// whose digest was checked, not read again from the package.
	src, err := os.CreateTemp(dir, ".component-*.wasm")
	if err != nil {
		return nil, err
	}
	defer os.Remove(src.Name())
	_, werr := src.Write(wasm)
	if err := errors.Join(werr, src.Close()); err != nil {
		return nil, err
	}
	out := filepath.Join(dir, "."+name+"."+strconv.Itoa(os.Getpid())+".tmp")
	defer os.Remove(out)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	args := append(append([]string{"compile"}, wasmtimeConfig...), "-o", out, src.Name())
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Env = []string{}
	cmd.Dir = dir
	if b, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("wasmtime could not compile the component: %v\n%s", err, clipLines(strings.TrimSpace(string(b)), 12))
	}
	if err := os.Rename(out, cwasm); err != nil {
		return nil, err
	}
	return &wasmtimeProgram{exe: exe, cwasm: cwasm}, nil
}

// onWasmtimeStart is called with the process ID of each wasmtime process the
// runner starts. Tests set it to check that the process is gone afterwards.
var onWasmtimeStart func(pid int)

// run starts wasmtime on the broker's pipes and waits for it. Cancelling ctx
// kills the process.
func (p *wasmtimeProgram) run(ctx context.Context, args []string, stdin, stdout *os.File, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, p.exe, wasmtimeRunArgs(p.cwasm, args)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	// Nothing of the runner's environment reaches wasmtime, so no variable
	// changes how it behaves either.
	cmd.Env = []string{}
	cmd.Dir = filepath.Dir(p.cwasm)
	// Kill sends SIGKILL on Unix and calls TerminateProcess on Windows.
	// wasmtime starts no processes of its own, so this ends all of it.
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return err
	}
	if onWasmtimeStart != nil {
		onWasmtimeStart(cmd.Process.Pid)
	}
	err := cmd.Wait()
	if err != nil && ctx.Err() != nil {
		// The runner stopped it: after its result, at the deadline, or when
		// the run was interrupted. Each of those is reported by the runner;
		// "signal: killed" would only obscure it.
		return nil
	}
	return err
}
