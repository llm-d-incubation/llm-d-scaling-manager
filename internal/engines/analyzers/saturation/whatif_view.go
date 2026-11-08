package saturation

import (
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// ITLTier is the what-if tier (whatif.TierOLS and the rest) of the line a
// variant was priced on, from the source its ITL learner report names and
// whether the line was borrowed (domain.BorrowedITLLine): "" when the
// variant held no line.
func ITLTier(source learn.Source, borrowed bool) string {
	switch {
	case source == itlSourceOLS:
		return whatif.TierOLS
	case source == itlSourceHeld:
		return whatif.TierHeld
	case source == itlSourcePinned:
		return whatif.TierPinnedB
	case borrowed:
		return whatif.TierBorrowed
	}
	return ""
}

// Read-only views of the analyzer's learned state for the what-if planner
// (docs/proposals/slo-capacity-planner.md, "Packaging": accessors). A replay
// stopped at a cycle reads them (learnreplay.StateAt); none changes what the
// analyzer decides next.

// ITLReadings is a copy of the ITL readings filed under key, a pricing key
// as the ITL learner's report names it (learn.Report.PricingKey): the main
// and steady windows, and the above-band fallback's.
func (a *SaturationAnalyzer) ITLReadings(key string) itl.KeyReadings {
	return a.itlTracker.Readings(key)
}

// ITLOneSided is how the analyzer last priced the line filed under key
// one-sided at the sequence cap (SwitchITLOneSidedAtCap): the admission's
// one-sided fields, at its pricing k and its prior on B, for itl.Refit, and
// raised, whether that line was priced at the prior
// (itl.Reading.PinnedAtBPrior). The zero Admission and false for a key it
// has not priced so, or with the switch off.
func (a *SaturationAnalyzer) ITLOneSided(key string) (adm itl.Admission, raised bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rec := a.itlOneSidedLast[key]
	return rec.adm, rec.raised
}

// PricingBatchCap is the batch cap per engine rank the analyzer prices a
// variant at and its source ("flags", "declared", "learned", "default", or ""
// with none), in pricingBatchCap's order. learning is the policy's
// enableBatchCapLearning. A learned cap read here is not stamped as used.
func (a *SaturationAnalyzer) PricingBatchCap(namespace, modelID, variant string,
	params *capacity.EngineParams, learning bool) (perRank int64, source string) {
	p := a.pricingBatchCapFor(namespace, modelID, variant, params, learning, false)
	return p.perRank, p.source
}
