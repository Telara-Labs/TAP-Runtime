package manifest

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
)

// RunDigest is the digest a package is listed under and checked by before it
// runs: sha256 over primitive.yaml and the entrypoint program, each
// length-prefixed under a domain label (RunDigestOf), in hex. The runner (host packageDigest) and discover's pointers (pack) both
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
	return RunDigestOf(raw, script), m, nil
}

// RunDigestOf is the digest of one manifest and one program. Each part is
// length-prefixed under a domain label, so no byte can move from one into the
// other without changing the digest. A bare concatenation let a manifest that
// ended in a comment holding program text digest the same as a manifest
// without it and a program that began with that text.
func RunDigestOf(raw, script []byte) string {
	h := sha256.New()
	h.Write([]byte("tap-run-digest-v2\n"))
	var n [8]byte
	for _, part := range [][]byte{raw, script} {
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		h.Write(n[:])
		h.Write(part)
	}
	return hex.EncodeToString(h.Sum(nil))
}
