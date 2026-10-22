package main

import (
	"os"
	"path/filepath"

	"github.com/Telara-Labs/TAP-Runtime/internal/sharedwire"
)

// replaceConfigFile replaces the file at path with data in one step: the bytes
// are written and synced to a file beside it, which is then renamed over path.
// A reader, or a crash, sees the old file or the whole new one, never a
// truncated one. A path that is a symbolic link keeps its link, and the file
// it points to is replaced.
func replaceConfigFile(path string, data []byte, perm os.FileMode) error {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tap-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// lockBeside takes an exclusive cross-process lock on a file named for path and
// returns the function that releases it. Every process that changes the same
// store takes it first, so one read-modify-write cannot lose another's.
func lockBeside(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := sharedwire.LockFile(f); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}
