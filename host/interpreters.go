package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// interpreter is one entry of the pinned list. Interpreters are downloaded on
// first use and never compiled into this binary (doc 34 section 13.9, ruling 8).
type interpreter struct {
	Kind   string // how the host drives it: sh, py, js
	File   string // name in the local store
	URL    string // where it is obtained; empty when nobody publishes it yet
	SHA256 string // refused on mismatch; empty only together with URL
	Build  string // how to produce it locally when URL is empty
}

// Where the bash-compatible interpreter of this release is, and its digest.
// The release build sets both (release/release.go): the interpreter is built
// first, and the runner is built knowing what it must be. A build from source
// has neither, and says how to build the interpreter.
var (
	shURL    string
	shSHA256 string
)

// shInterpreter is this repository's guest-sh. No upstream publishes a
// bash-compatible interpreter as wasm, so it is a file of the runner's own
// release. The file is named for the release, so that a newer runner does not
// find an older interpreter in the store and refuse it.
func shInterpreter(url, sum, version string) interpreter {
	if url == "" || sum == "" {
		return interpreter{
			Kind:  "sh",
			File:  "sh.wasm",
			Build: "GOOS=wasip1 GOARCH=wasm go build -o <store>/sh.wasm ./guest-sh",
		}
	}
	return interpreter{Kind: "sh", File: "sh-" + version + ".wasm", URL: url, SHA256: sum}
}

var interpreters = map[string]interpreter{
	".py": {
		Kind:   "py",
		File:   "python-3.12.0.wasm",
		URL:    "https://github.com/vmware-labs/webassembly-language-runtimes/releases/download/python/3.12.0%2B20231211-040d5a6/python-3.12.0.wasm",
		SHA256: "e5dc5a398b07b54ea8fdb503bf68fb583d533f10ec3f930963e02b9505f7a763",
	},
	".js": {
		Kind:   "js",
		File:   "qjs-wasi-0.17.0.wasm",
		URL:    "https://github.com/quickjs-ng/quickjs/releases/download/v0.17.0/qjs-wasi.wasm",
		SHA256: "42a732a676ec2d93488c19411e0fad283bf72658fdad746f089914b523c783b1",
	},
	// TypeScript runs in the JavaScript interpreter, after the runner has
	// removed its types (typescript.go).
	".ts": {
		Kind:   "ts",
		File:   "qjs-wasi-0.17.0.wasm",
		URL:    "https://github.com/quickjs-ng/quickjs/releases/download/v0.17.0/qjs-wasi.wasm",
		SHA256: "42a732a676ec2d93488c19411e0fad283bf72658fdad746f089914b523c783b1",
	},
	".sh": shInterpreter(shURL, shSHA256, version),
}

func storeDir(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "tap-runtime", "interpreters"), nil
}

// obtain returns the interpreter's bytes, downloading it into the store on
// first use. Every read is checked against the pinned digest when one exists,
// so a file altered in the store is refused the same way as a bad download.
func obtain(store, entrypoint string) ([]byte, interpreter, string, error) {
	in, ok := interpreters[filepath.Ext(entrypoint)]
	if !ok {
		if filepath.Ext(entrypoint) == ".wasm" {
			return nil, in, "", fmt.Errorf("a compiled .wasm entrypoint (%q) is not supported yet; write the primitive as main.sh, main.py, main.js or main.ts", entrypoint)
		}
		return nil, in, "", fmt.Errorf("no interpreter is listed for %q", entrypoint)
	}
	path := filepath.Join(store, in.File)
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if in.URL == "" {
			return nil, in, "", fmt.Errorf("interpreter %s is not in %s and is not published anywhere yet; build it with: %s", in.File, store, in.Build)
		}
		logf("fetching   %s", in.URL)
		if b, err = download(in.URL); err != nil {
			return nil, in, "", fmt.Errorf("interpreter %s: %w (source: %s)%s", in.File, err, in.URL, noNetworkHint())
		}
		if got := digest(b); got != in.SHA256 {
			return nil, in, "", fmt.Errorf("interpreter %s: downloaded file has sha256 %s, pinned %s; refused", in.File, got, in.SHA256)
		}
		if err := os.MkdirAll(store, 0o700); err != nil {
			return nil, in, "", err
		}
		if err := os.WriteFile(path, b, 0o600); err != nil {
			return nil, in, "", err
		}
	} else if err != nil {
		return nil, in, "", err
	}
	got := digest(b)
	if in.SHA256 != "" && got != in.SHA256 {
		return nil, in, "", fmt.Errorf("interpreter %s in %s has sha256 %s, pinned %s; refused", in.File, store, got, in.SHA256)
	}
	return b, in, got, nil
}

func digest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func download(url string) ([]byte, error) {
	c := &http.Client{Timeout: 5 * time.Minute}
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download returned %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 256<<20))
}

// noNetworkHint says why a download failed when the reason is known. Codex
// runs the commands of its shell with no network and marks them with this
// variable, which Codex sets and this program only reads. A runner Codex
// starts as an MCP server is not held that way (measured 2026-09-28, Codex
// 0.147.0, read-only sandbox).
func noNetworkHint() string {
	if os.Getenv("CODEX_SANDBOX_NETWORK_DISABLED") == "" {
		return ""
	}
	return "\nThis program was started from Codex's shell, which Codex gives no network. Run the primitive with the tap_run tool instead, or run `tap fetch` once in a terminal of your own so that nothing has to be downloaded here."
}

// fetchCommand is `host fetch`: it puts every interpreter this runner can
// obtain into the store, so that a later run downloads nothing. It is for a
// machine, or a sandbox, that will have no network when a primitive runs.
func fetchCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	interpDir := fs.String("interpreters", "", "interpreter store; default is the user cache directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	store, err := storeDir(*interpDir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	// Two languages can share one interpreter, so the list is of files.
	byFile := map[string]string{}
	for ext, in := range interpreters {
		if in.URL != "" {
			byFile[in.File] = ext
		}
	}
	var files []string
	for f := range byFile {
		files = append(files, f)
	}
	sort.Strings(files)
	code := 0
	for _, f := range files {
		_, _, sum, err := obtain(store, "main"+byFile[f])
		if err != nil {
			fmt.Fprintln(stderr, err)
			code = 1
			continue
		}
		fmt.Fprintf(stdout, "%s  %s\n", sum, filepath.Join(store, f))
	}
	if in := interpreters[".sh"]; in.URL == "" {
		fmt.Fprintf(stdout, "%s is not published for this build; build it with: %s\n", in.File, in.Build)
	}
	return code
}
