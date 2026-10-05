package main

import (
	"github.com/tetratelabs/wazero"

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
