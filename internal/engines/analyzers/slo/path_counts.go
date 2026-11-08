package slo

import (
	"sync/atomic"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// The per-path counters a replay reads (docs/proposals/analyzer-learner.md,
// 2.5), as the saturation analyzer keeps its own: how often this analyzer's
// own ITL line came from each fit. Its tracker is its own instance, so the
// saturation analyzer's counts say nothing of it.
const (
	// PathITLOLS counts the cycles a variant's SLO line came from a full fit.
	PathITLOLS = "slo-itl-ols"
	// PathITLPinned counts the cycles a variant's SLO line came from the
	// pinned fit, a held slope included.
	PathITLPinned = "slo-itl-pinned"
)

// pathCounters is the analyzer's per-path counts. Atomic, so a path counts
// without taking a.mu.
type pathCounters struct {
	ols, pinned atomic.Int64
}

// countFit counts the tier of one fit: nothing for no line.
func (c *pathCounters) countFit(tier string) {
	switch tier {
	case itl.TierOLS:
		c.ols.Add(1)
	case itl.TierPinnedB:
		c.pinned.Add(1)
	}
}

// PathNames is every per-path counter PathCounts reports.
func PathNames() []string { return []string{PathITLOLS, PathITLPinned} }

// PathCounts is how often each of the analyzer's learner paths ran since it
// was made, keyed by PathNames. Read by the replay; nothing in the
// controller reads it.
func (a *Analyzer) PathCounts() map[string]int64 {
	return map[string]int64{PathITLOLS: a.paths.ols.Load(), PathITLPinned: a.paths.pinned.Load()}
}
