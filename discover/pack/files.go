package pack

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"time"
)

func PackFiles(files map[string][]byte, executable func(string) bool) ([]byte, string, error) {
	var buf bytes.Buffer
	gz, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	gz.ModTime = time.Unix(0, 0)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		body := files[n]
		mode := int64(0o644)
		if executable(n) {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: mode, Size: int64(len(body)), ModTime: time.Unix(0, 0), Format: tar.FormatPAX}); err != nil {
			return nil, "", err
		}
		if _, err := tw.Write(body); err != nil {
			return nil, "", err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, "", err
	}
	if err := gz.Close(); err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), "sha256:" + hex.EncodeToString(sum[:]), nil
}
