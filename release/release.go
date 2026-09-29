// Command release builds what is published: the runner for each platform,
// the bash-compatible interpreter, their checksums, and the licence notices
// of everything compiled in.
//
//	go run ./release build  --version 0.1.0 --out dist [--key release.key]
//	go run ./release verify --dir dist [--pub release.pub]
//	go run ./release keygen --out release
//
// A build is reproducible: the same source and the same toolchain give the
// same bytes, so a published checksum can be checked by building again.
//
// Signing uses an ed25519 key read from a file the caller names. Where that
// key lives, and who may use it, is not decided here.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Platforms are the systems a release is built for.
var Platforms = []string{"darwin/arm64", "darwin/amd64", "linux/amd64", "linux/arm64", "windows/amd64"}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: release build|verify|keygen [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "build":
		fs := flag.NewFlagSet("build", flag.ExitOnError)
		version := fs.String("version", "", "version to stamp, such as 0.1.0")
		out := fs.String("out", "dist", "directory to write")
		key := fs.String("key", "", "ed25519 private key file; the checksums are signed when given")
		only := fs.String("only", "", "build one platform, such as linux/amd64")
		fs.Parse(os.Args[2:])
		platforms := Platforms
		if *only != "" {
			platforms = []string{*only}
		}
		err = Build(".", *out, *version, platforms, *key)
	case "verify":
		fs := flag.NewFlagSet("verify", flag.ExitOnError)
		dir := fs.String("dir", "dist", "directory to check")
		pub := fs.String("pub", "", "ed25519 public key file; the signature is checked when given")
		fs.Parse(os.Args[2:])
		if err = Verify(*dir, *pub); err == nil {
			fmt.Println("verified")
		}
	case "keygen":
		fs := flag.NewFlagSet("keygen", flag.ExitOnError)
		out := fs.String("out", "release", "path without extension; .key and .pub are written")
		fs.Parse(os.Args[2:])
		err = Keygen(*out)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

func run(dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, errb.String())
	}
	return out.Bytes(), nil
}

// flags make the output depend on the source and the toolchain only.
func flags(version string) []string {
	return []string{"-trimpath", "-buildvcs=false", "-ldflags", "-s -w -buildid= -X main.version=" + version}
}

// Build writes a release into out.
func Build(repo, out, version string, platforms []string, keyFile string) error {
	if version == "" {
		return fmt.Errorf("a version is needed")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	abs, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	base := []string{"CGO_ENABLED=0", "GOWORK=off", "GOFLAGS=-mod=readonly"}
	var files []string
	for _, p := range platforms {
		goos, goarch, ok := strings.Cut(p, "/")
		if !ok {
			return fmt.Errorf("platform %q is not os/arch", p)
		}
		name := fmt.Sprintf("tap-runtime-%s-%s-%s", version, goos, goarch)
		if goos == "windows" {
			name += ".exe"
		}
		args := append(append([]string{"build"}, flags(version)...), "-o", filepath.Join(abs, name), "./host")
		if _, err := run(repo, append(base, "GOOS="+goos, "GOARCH="+goarch), "go", args...); err != nil {
			return err
		}
		files = append(files, name)
	}
	// The interpreter nobody else publishes.
	sh := fmt.Sprintf("sh-%s.wasm", version)
	args := append(append([]string{"build"}, flags(version)...), "-o", filepath.Join(abs, sh), "./guest-sh")
	if _, err := run(repo, append(base, "GOOS=wasip1", "GOARCH=wasm"), "go", args...); err != nil {
		return err
	}
	files = append(files, sh)

	notices, err := Notices(repo)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(abs, "THIRD_PARTY_NOTICES.txt"), notices, 0o644); err != nil {
		return err
	}
	files = append(files, "THIRD_PARTY_NOTICES.txt")

	sums, err := checksums(abs, files)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(abs, "SHA256SUMS"), sums, 0o644); err != nil {
		return err
	}
	if keyFile != "" {
		key, err := readKey(keyFile, ed25519.PrivateKeySize)
		if err != nil {
			return err
		}
		sig := ed25519.Sign(ed25519.PrivateKey(key), sums)
		if err := os.WriteFile(filepath.Join(abs, "SHA256SUMS.sig"), []byte(hex.EncodeToString(sig)+"\n"), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func checksums(dir string, files []string) ([]byte, error) {
	sort.Strings(files)
	var b bytes.Buffer
	for _, f := range files {
		raw, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(raw)
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(sum[:]), f)
	}
	return b.Bytes(), nil
}

// Verify checks every file in a release against its checksum, and the
// checksums against their signature when a public key is given.
func Verify(dir, pubFile string) error {
	sums, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		return err
	}
	if pubFile != "" {
		pub, err := readKey(pubFile, ed25519.PublicKeySize)
		if err != nil {
			return err
		}
		sigHex, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS.sig"))
		if err != nil {
			return fmt.Errorf("the release is not signed: %w", err)
		}
		sig, err := hex.DecodeString(strings.TrimSpace(string(sigHex)))
		if err != nil || !ed25519.Verify(ed25519.PublicKey(pub), sums, sig) {
			return fmt.Errorf("the signature does not match the checksums")
		}
	}
	listed := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(sums)), "\n") {
		want, name, ok := strings.Cut(line, "  ")
		if !ok || strings.ContainsAny(name, "/\\") {
			return fmt.Errorf("SHA256SUMS has a line that cannot be read: %q", line)
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("%s is listed and missing", name)
		}
		got := sha256.Sum256(raw)
		if hex.EncodeToString(got[:]) != want {
			return fmt.Errorf("%s does not match its checksum", name)
		}
		listed[name] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if n := e.Name(); !listed[n] && n != "SHA256SUMS" && n != "SHA256SUMS.sig" {
			return fmt.Errorf("%s is in the release and not in its checksums", n)
		}
	}
	return nil
}

// Keygen writes a new signing key and its public half.
func Keygen(path string) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path+".key", []byte(hex.EncodeToString(priv)+"\n"), 0o600); err != nil {
		return err
	}
	return os.WriteFile(path+".pub", []byte(hex.EncodeToString(pub)+"\n"), 0o644)
}

func readKey(file string, size int) ([]byte, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(key) != size {
		return nil, fmt.Errorf("%s does not hold a key of the expected size", file)
	}
	return key, nil
}

// Notices gathers the licence of every module compiled into the runner or
// the interpreter, and of the library kept in third_party. It is derived
// from what the build uses, not from a list somebody keeps.
func Notices(repo string) ([]byte, error) {
	type module struct {
		Path    string
		Version string
		Dir     string
	}
	seen := map[string]module{}
	own := ""
	for _, target := range []struct {
		pkg string
		env []string
	}{
		{"./host", nil},
		{"./guest-sh", []string{"GOOS=wasip1", "GOARCH=wasm"}},
	} {
		out, err := run(repo, append([]string{"GOWORK=off"}, target.env...), "go", "list", "-deps", "-f",
			"{{with .Module}}{{.Path}}\t{{.Version}}\t{{.Dir}}\t{{.Main}}{{end}}", target.pkg)
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(out), "\n") {
			f := strings.Split(line, "\t")
			if len(f) != 4 {
				continue
			}
			if f[3] == "true" {
				own = f[0]
				continue
			}
			seen[f[0]] = module{Path: f[0], Version: f[1], Dir: f[2]}
		}
	}
	// A module of this repository is this software, not software it
	// includes: contract/ is compiled in as a module so that others can
	// import it alone.
	for p := range seen {
		if own != "" && strings.HasPrefix(p, own+"/") {
			delete(seen, p)
		}
	}
	var paths []string
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var b bytes.Buffer
	b.WriteString("tap-runtime includes the following software.\n")
	write := func(title, dir string) error {
		text, name := licenceIn(dir)
		if text == nil {
			return fmt.Errorf("%s has no licence file; it cannot be shipped without one", title)
		}
		fmt.Fprintf(&b, "\n%s\n%s\n(%s)\n\n%s\n", strings.Repeat("=", 72), title, name, bytes.TrimSpace(text))
		return nil
	}
	if err := write("mvdan.cc/sh/v3 v3.14.1, modified, in third_party/sh", filepath.Join(repo, "third_party", "sh")); err != nil {
		return nil, err
	}
	for _, p := range paths {
		if err := write(p+" "+seen[p].Version, seen[p].Dir); err != nil {
			return nil, err
		}
	}
	return b.Bytes(), nil
}

func licenceIn(dir string) ([]byte, string) {
	for _, n := range []string{"LICENSE", "LICENSE.md", "LICENSE.txt", "LICENCE", "COPYING", "LICENSE-MIT"} {
		if raw, err := os.ReadFile(filepath.Join(dir, n)); err == nil {
			return raw, n
		}
	}
	return nil, ""
}
