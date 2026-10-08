//go:build !windows

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/Telara-Labs/TAP-Runtime/internal/sharedwire"
)

// becomeRelay replaces this process with the small relay, which passes the
// session to the shared runner. It returns only when it could not: the
// build has no relay, or it could not be written or started.
func becomeRelay(args []string) error {
	if len(relayBinary) == 0 {
		return errors.New("this build has no small relay")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	path, err := relayFile()
	if err != nil {
		return err
	}
	argv := append([]string{path, "--key", sharedKey(), "--runner", exe, "--"}, args...)
	return syscall.Exec(path, argv, os.Environ())
}

// relayFile is the relay on disk, in the runner's private directory, named
// by its digest. A file there that does not hold exactly these bytes is
// written again.
func relayFile() (string, error) {
	dir, err := sharedwire.Dir()
	if err != nil {
		return "", err
	}
	dir = filepath.Join(dir, "bin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	os.Chmod(dir, 0o700)
	sum := sha256.Sum256(relayBinary)
	path := filepath.Join(dir, "tap-relay-"+hex.EncodeToString(sum[:8]))
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm() == 0o700 {
		if b, err := os.ReadFile(path); err == nil && bytes.Equal(b, relayBinary) {
			return path, nil
		}
	}
	tmp, err := os.CreateTemp(dir, ".tap-relay-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(relayBinary); err != nil || tmp.Chmod(0o700) != nil || tmp.Close() != nil {
		tmp.Close()
		return "", fmt.Errorf("writing the relay: %v", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}
