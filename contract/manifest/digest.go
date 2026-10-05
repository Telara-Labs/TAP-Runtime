package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
)

// RunDigest is the digest a package is listed under and checked by before it
// runs: sha256 over primitive.yaml followed by the entrypoint program, in
// hex. The runner (host packageDigest) and discover's pointers (pack) both
// use it, so a pointer always names what tap_run accepts.
func RunDigest(dir string) (string, *Manifest, error) {
	m, err := Load(dir)
	if err != nil {
		return "", nil, err
	}
	script, err := os.ReadFile(filepath.Join(dir, filepath.Clean(filepath.FromSlash(m.Execution.Entrypoint))))
	if err != nil {
		return "", nil, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "primitive.yaml"))
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(append(append([]byte{}, raw...), script...))
	return hex.EncodeToString(sum[:]), m, nil
}
