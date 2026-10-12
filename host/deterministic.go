package main

import (
	"crypto/rand"
	"io"
	"time"

	"github.com/tetratelabs/wazero"

	"github.com/Telara-Labs/TAP-Runtime/internal/component"
	runlog "github.com/Telara-Labs/TAP-Runtime/journal"
)

// observed gives the program its clock and its random bytes through the
// run's record. A program that is started
// again is given the readings it took before, in the order it took them, so
// it takes the path it took before. Once it has caught up with its record it
// is given the real clock and real random bytes.
func observed(cfg wazero.ModuleConfig, o *runlog.Observed) wazero.ModuleConfig {
	return cfg.
		WithWalltime(o.Walltime, 1_000_000).
		WithNanotime(o.Nanotime, 1_000_000).
		WithRandSource(o)
}

// componentConfig gives a WASI Preview 2 component what guestConfig and
// observed give a Preview 1 module: its arguments after "primitive", the
// broker's protocol on stdin and stdout, stderr, and clocks and random bytes
// read through the run's record. Nothing else.
func componentConfig(args []string, stdin io.Reader, stdout, stderr io.Writer, run *runlog.Journal) component.Config {
	start := time.Now()
	cfg := component.Config{
		Args:  append([]string{"primitive"}, args...),
		Stdin: stdin, Stdout: stdout, Stderr: stderr,
		Walltime: func() (int64, int32) { n := time.Now(); return n.Unix(), int32(n.Nanosecond()) },
		Nanotime: func() int64 { return int64(time.Since(start)) },
		Rand:     rand.Reader,
	}
	if run != nil {
		cfg.Walltime, cfg.Nanotime, cfg.Rand = run.Observed.Walltime, run.Observed.Nanotime, run.Observed
	}
	return cfg
}
