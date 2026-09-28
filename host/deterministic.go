package main

import (
	"crypto/sha256"
	"encoding/binary"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
)

// deterministic fixes what a program can observe besides the answers to its
// requests, so that started again it takes the path it took before.
//
// The clock starts at the moment the run started and advances one
// millisecond each time it is read. It is the run's clock, not the wall's:
// a program resumed a day later sees the day it started.
//
// The random bytes are a stream derived from the run id.
func deterministic(cfg wazero.ModuleConfig, runID string, started time.Time) wazero.ModuleConfig {
	var mu sync.Mutex
	var wall, mono int64
	base := started.UnixNano()
	return cfg.
		WithWalltime(func() (int64, int32) {
			mu.Lock()
			defer mu.Unlock()
			wall++
			t := base + wall*int64(time.Millisecond)
			return t / 1e9, int32(t % 1e9)
		}, 1_000_000).
		WithNanotime(func() int64 {
			mu.Lock()
			defer mu.Unlock()
			mono++
			return mono * int64(time.Millisecond)
		}, 1_000_000).
		WithRandSource(&stream{seed: sha256.Sum256([]byte("tap-runtime run " + runID))})
}

// stream is sha256 in counter mode. It is reproducible, which is the point,
// and so it is not a source of secrets: a primitive has none to make.
type stream struct {
	mu   sync.Mutex
	seed [32]byte
	n    uint64
	buf  []byte
}

func (s *stream) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range p {
		if len(s.buf) == 0 {
			var block [40]byte
			copy(block[:], s.seed[:])
			binary.BigEndian.PutUint64(block[32:], s.n)
			s.n++
			sum := sha256.Sum256(block[:])
			s.buf = sum[:]
		}
		p[i] = s.buf[0]
		s.buf = s.buf[1:]
	}
	return len(p), nil
}
