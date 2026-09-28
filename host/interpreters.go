package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
	// No upstream publishes a bash-compatible interpreter as wasm. It is this
	// repository's own guest-sh, and becomes a release file here once the
	// repository cuts releases. Until then it is built locally.
	".sh": {
		Kind:  "sh",
		File:  "sh.wasm",
		Build: "GOOS=wasip1 GOARCH=wasm go build -o <store>/sh.wasm ./guest-sh",
	},
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
			return nil, in, "", fmt.Errorf("interpreter %s: %w (source: %s)", in.File, err, in.URL)
		}
		if got := digest(b); got != in.SHA256 {
			return nil, in, "", fmt.Errorf("interpreter %s: downloaded file has sha256 %s, pinned %s; refused", in.File, got, in.SHA256)
		}
		if err := os.MkdirAll(store, 0o755); err != nil {
			return nil, in, "", err
		}
		if err := os.WriteFile(path, b, 0o644); err != nil {
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
