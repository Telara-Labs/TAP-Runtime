package launch

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// signedReleaseAssets authenticates the manifest before using any download
// name. It requires the full published platform set and includes signed extras.
// Unauthenticated GitHub release metadata is not part of this content check.
func signedReleaseAssets(version string, sums, signature, publicKey []byte) ([]string, error) {
	pub, err := hex.DecodeString(strings.TrimSpace(string(publicKey)))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid trusted release public key")
	}
	sig, err := hex.DecodeString(strings.TrimSpace(string(signature)))
	if err != nil || !ed25519.Verify(ed25519.PublicKey(pub), sums, sig) {
		return nil, fmt.Errorf("release manifest signature does not match trusted key")
	}
	seen := map[string]bool{}
	var names []string
	basename := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	for _, line := range strings.Split(strings.TrimSpace(string(sums)), "\n") {
		digest, name, ok := strings.Cut(line, "  ")
		hash, err := hex.DecodeString(digest)
		if !ok || err != nil || len(hash) != sha256.Size || !basename.MatchString(name) ||
			name == "SHA256SUMS" || name == "SHA256SUMS.sig" || name == "SHA256SUMS.pub" {
			return nil, fmt.Errorf("invalid release manifest line: %q", line)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate release asset: %s", name)
		}
		seen[name] = true
		names = append(names, name)
	}
	for _, name := range requiredReleaseAssets(version) {
		if !seen[name] {
			return nil, fmt.Errorf("required release asset missing: %s", name)
		}
	}
	return names, nil
}

func requiredReleaseAssets(version string) []string {
	names := []string{"install.sh", "install.ps1", "THIRD_PARTY_NOTICES.txt", "sh-" + version + ".wasm"}
	for _, platform := range []string{"darwin-amd64", "darwin-arm64", "linux-amd64", "linux-arm64", "windows-amd64.exe"} {
		names = append(names, "tap-"+version+"-"+platform)
	}
	return names
}

func TestSignedReleaseAssets(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicKey := []byte(hex.EncodeToString(pub))
	const version = "0.1.17"
	var lines []string
	for _, name := range requiredReleaseAssets(version) {
		hash := sha256.Sum256([]byte(name))
		lines = append(lines, fmt.Sprintf("%x  %s", hash, name))
	}
	check := func(t *testing.T, lines []string, tamper bool, wantError string) []string {
		t.Helper()
		sums := []byte(strings.Join(lines, "\n") + "\n")
		sig := ed25519.Sign(key, sums)
		if tamper {
			sums[0] ^= 1
		}
		names, err := signedReleaseAssets(version, sums, []byte(hex.EncodeToString(sig)), publicKey)
		if wantError == "" {
			if err != nil {
				t.Fatal(err)
			}
		} else if err == nil || !strings.Contains(err.Error(), wantError) {
			t.Fatalf("got %v, want error containing %q", err, wantError)
		}
		return names
	}
	t.Run("all platforms and signed extra", func(t *testing.T) {
		extra := fmt.Sprintf("%x  extension.vsix", sha256.Sum256([]byte("extra")))
		names := check(t, append(append([]string{}, lines...), extra), false, "")
		if len(names) != 10 || names[len(names)-1] != "extension.vsix" {
			t.Fatalf("lost signed release contents: %v", names)
		}
	})
	t.Run("tampered before parsing", func(t *testing.T) {
		check(t, []string{"unsafe ../../escape"}, true, "signature")
	})
	t.Run("missing platform", func(t *testing.T) {
		check(t, lines[:len(lines)-1], false, "missing: tap-0.1.17-windows-amd64.exe")
	})
	t.Run("duplicate", func(t *testing.T) {
		check(t, append(append([]string{}, lines...), lines[0]), false, "duplicate")
	})
	for _, name := range []string{"../escape", `dir\escape`, "/absolute", "C:escape", ".", "..", "SHA256SUMS", "SHA256SUMS.sig", "SHA256SUMS.pub", "asset?query"} {
		t.Run("unsafe "+name, func(t *testing.T) {
			bad := fmt.Sprintf("%x  %s", sha256.Sum256([]byte("bad")), name)
			check(t, append(append([]string{}, lines...), bad), false, "invalid")
		})
	}
	t.Run("invalid digest", func(t *testing.T) {
		check(t, append(append([]string{}, lines...), "abc  extra"), false, "invalid")
	})
	t.Run("wrong trusted key", func(t *testing.T) {
		sums := []byte(strings.Join(lines, "\n") + "\n")
		sig := []byte(hex.EncodeToString(ed25519.Sign(key, sums)))
		if _, err := signedReleaseAssets(version, sums, sig, []byte("00")); err == nil {
			t.Fatal("accepted invalid trusted key")
		}
	})
}
