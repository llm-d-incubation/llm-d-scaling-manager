package throughput

import (
	"sync/atomic"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// The per-path counters a replay reads (docs/proposals/analyzer-learner.md,
// 2.5), as the saturation and slo analyzers keep their own: how often this
// analyzer's ITL line, on its own tracker (SwitchITLSteadyGate), came from
// each fit, and how many readings it stamped settling. A golden set that
// never reaches a path pins nothing about it. The parent's line
// (SwitchITLSteadyGate off) counts nothing.
const (
	// PathITLOLS counts the cycles a variant's line came from a full fit
	// (T1-ols).
	PathITLOLS = "throughput-itl-ols"
	// PathITLPinned counts the cycles a variant's line came from the pinned
	// fit, its slope fitted that cycle (T2-pinned, T2-default).
	PathITLPinned = "throughput-itl-pinned"
	// PathITLHeld counts the cycles a variant's line held an earlier slope
	// (T2-held).
	PathITLHeld = "throughput-itl-held"
	// PathITLSettling counts the readings the line stamped settling and kept
	// out of its fits: a joiner's, or a replica's whose load came back.
	PathITLSettling = "throughput-itl-settling"
	// PathITLAboveBand counts the cycles a variant's line came from the
	// above-band fallback (T2-above-band). Not required of the goldens: a
	// recording reaches it only by starting into saturated load.
	PathITLAboveBand = "throughput-itl-above-band"
	// PathITLAtCap counts the readings above the band the line admitted
	// because their replica was at its sequence cap (itl.Reading.Capped).
	PathITLAtCap = "throughput-itl-at-cap"
)

// pathCounters is the analyzer's per-path counts. Atomic, so a path counts
// without taking a.mu.
type pathCounters struct {
	ols, pinned, held, settling, aboveBand, atCap atomic.Int64
}

// count counts one cycle's reading of a variant's line.
func (c *pathCounters) count(r itl.Reading) {
	switch {
	case r.Tier == itl.TierOLS:
		c.ols.Add(1)
	case r.Tier == itl.TierPinnedB && r.AboveBandFit:
		c.aboveBand.Add(1)
	case r.Tier == itl.TierPinnedB && r.SlopeHeld:
		c.held.Add(1)
	case r.Tier == itl.TierPinnedB:
		c.pinned.Add(1)
	}
	c.settling.Add(int64(r.Settling))
	c.atCap.Add(int64(r.Capped))
}

// PathNames is every per-path counter PathCounts reports.
func PathNames() []string {
	return []string{PathITLOLS, PathITLPinned, PathITLHeld, PathITLSettling, PathITLAboveBand, PathITLAtCap}
}

// PathCounts is how often each of the analyzer's learner paths ran since it
// was made, keyed by PathNames. Read by the replay; nothing in the
// controller reads it.
func (a *ThroughputAnalyzer) PathCounts() map[string]int64 {
	return map[string]int64{
		PathITLOLS:       a.paths.ols.Load(),
		PathITLPinned:    a.paths.pinned.Load(),
		PathITLHeld:      a.paths.held.Load(),
		PathITLSettling:  a.paths.settling.Load(),
		PathITLAboveBand: a.paths.aboveBand.Load(),
		PathITLAtCap:     a.paths.atCap.Load(),
	}
}
