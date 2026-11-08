package slo

import (
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// P1's figures, read from the saturation analyzer (docs/proposals/slo-tiers.md,
// "Where P1's figures come from").
//
// P1's observer lives in the saturation analyzer, which feeds it decode's
// back-pressure and the back-pressure bound, two inputs this analyzer does
// not have. So this analyzer runs no observer of its own: it reads each
// prefill variant's latest estimate from the saturation analyzer's stash,
// through PrefillEstimates, and never rebuilds the observer's key.
//
// Lock order: the read takes the saturation analyzer's mu (and that
// analyzer takes the observer's lock only with its own released); this
// analyzer's own mu is taken only after the read, never around it. The read
// holds no lock of this analyzer's, so a caller on an Analyze path that
// already holds a.mu cannot deadlock itself (sync.Mutex is not reentrant),
// and the saturation analyzer never calls back here, so no cycle forms.

// PrefillEstimates reads a prefill variant's latest P1 estimate, when it was
// written on the engine's cycle clock, and whether there is one.
// *saturation.SaturationAnalyzer implements it.
//
// A caller reads it once per prefill variant per cycle: each call takes the
// saturation analyzer's lock and copies the estimate's maps.

type PrefillEstimates interface {
	PrefillEstimate(namespace, modelID, variant string) (prefill.Estimate, time.Time, bool)
}

// WithPrefillEstimates sets where the analyzer reads P1's prefill estimates
// from: the engine and the replay pass their saturation analyzer, which
// reads the same clock. Unset, no prefill variant has an estimate. Call it
// before the analyzer is used.
func (a *Analyzer) WithPrefillEstimates(p PrefillEstimates) *Analyzer {
	a.prefillEstimates = p
	return a
}

// prefillEstimate is the variant's estimate when it was written this cycle:
// stamped at now, the analyzer's own clock, which is the saturation
// analyzer's too. An estimate from another cycle is refused, so one left by
// a cycle in which the observation did not run is never read. The engine
// runs saturation before this analyzer, so the estimate is the one made from
// this cycle's rows.
//
// The stamp check is exact only while the cycle clock is pinned across both
// analyzers for the cycle (cycleclock in the engine, the replay's clock in
// learnreplay): two cycles at the same instant would pass the earlier
// cycle's estimate as this one's.
//
// It takes no lock of this analyzer's: call it before taking a.mu (the lock
// order above). It takes the saturation analyzer's lock, through the handle.
func (a *Analyzer) prefillEstimate(namespace, modelID, variant string, now time.Time) (prefill.Estimate, bool) {
	if a.prefillEstimates == nil {
		return prefill.Estimate{}, false
	}
	est, at, ok := a.prefillEstimates.PrefillEstimate(namespace, modelID, variant)
	if !ok || !at.Equal(now) {
		return prefill.Estimate{}, false
	}
	return est, true
}
