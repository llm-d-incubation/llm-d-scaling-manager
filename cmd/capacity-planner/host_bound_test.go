package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// hostVariant is a decode variant on an ols line of intercept b, with a
// floor read below the band.
func hostVariant(name string, b, floor float64) learnreplay.VariantState {
	return learnreplay.VariantState{Variant: name, Role: string(domain.RoleDecode), Line: itl.Model{A: 0.03, B: b},
		Tier: whatif.TierOLS, BelowBandITL: floor}
}

// hostFigure is a priced TPOT percentile.
var hostFigure whatif.Result = whatif.Answer{Value: 0.004}

// A TPOT percentile read off coarse buckets is refused, variant by variant,
// where that variant's decode step is host-bound (#291): its measured
// intercept and floor under 5 ms, as a sub-1B model's 1.5 to 2.5 ms. A fine
// bucket, a step of 5 ms or more, a floor of 5 ms or more, an intercept no
// fit measured (a default, the least ITL above the band, or none), or an
// assumed ratio prices as before.
func TestTPOTRatio_AHostBoundCoarsePercentileIsRefused(t *testing.T) {
	coarse := (&answerFlags{set: map[string]bool{}}).ratioFor(windowsState([]whatif.TPOTWindow{coarseWindow}, nil), 0.9)
	require.Nil(t, coarse.refused)
	require.Contains(t, coarse.flags, whatif.CoarseTPOTBucket)
	fine := (&answerFlags{set: map[string]bool{}}).ratioFor(windowsState([]whatif.TPOTWindow{fineWindow}, nil), 0.9)
	require.NotContains(t, fine.flags, whatif.CoarseTPOTBucket)
	assumed := (&answerFlags{set: map[string]bool{"assume-tpot-ratio": true}, tpotRatio: 1.3}).ratioFor(
		windowsState([]whatif.TPOTWindow{coarseWindow}, nil), 0.9)

	aboveBand := hostVariant("d", 0.0017, 0)
	aboveBand.Tier = whatif.TierAboveBand
	borrowed := hostVariant("d", 0.0017, 0)
	borrowed.Tier = whatif.TierBorrowed
	for name, c := range map[string]struct {
		vs      learnreplay.VariantState
		r       resolvedRatio
		refused bool
	}{
		"0.6B: B 1.7 ms, floor 1.75 ms, coarse":  {vs: hostVariant("d", 0.0017, 0.00175), r: coarse, refused: true},
		"no floor read, coarse":                  {vs: hostVariant("d", 0.0017, 0), r: coarse, refused: true},
		"fine buckets":                           {vs: hostVariant("d", 0.0017, 0.00175), r: fine},
		"32B: B 12 ms, coarse":                   {vs: hostVariant("d", 0.012, 0), r: coarse},
		"B 1.7 ms, but the floor 6 ms":           {vs: hostVariant("d", 0.0017, 0.006), r: coarse},
		"assumed ratio":                          {vs: hostVariant("d", 0.0017, 0.00175), r: assumed},
		"a default (zero) intercept is not read": {vs: hostVariant("d", 0, 0), r: coarse},
		"no line":                                {vs: learnreplay.VariantState{Variant: "d", Role: string(domain.RoleDecode)}, r: coarse},
		"an above-band intercept":                {vs: aboveBand, r: coarse},
		"a borrowed line":                        {vs: borrowed, r: coarse},
	} {
		t.Run(name, func(t *testing.T) {
			got := onHostBound(hostFigure, pricedVariant{state: c.vs}, c.r)
			if !c.refused {
				assert.Equal(t, hostFigure, got)
				return
			}
			u, ok := got.(whatif.Unanswerable)
			require.True(t, ok, "%+v", got)
			assert.Equal(t, whatif.TPOTBucketTooWide, u.Reason)
			assert.Contains(t, u.Detail, "host-bound")
			assert.Contains(t, u.Detail, "router's TPOT histogram")
			assert.Contains(t, u.Detail, "--assume-tpot-ratio")
		})
	}
}

// Each variant is judged on its own line: a host-bound variant beside a
// 12 ms one, or beside one with no line, is refused while the other prices.
// This is what --variant picks from too: the variant priced is the one
// judged, whatever the others read.
func TestTPOTRatio_HostBoundIsPerVariant(t *testing.T) {
	r := (&answerFlags{set: map[string]bool{}}).ratioFor(windowsState([]whatif.TPOTWindow{coarseWindow}, nil), 0.9)
	host := pricedVariant{state: hostVariant("small", 0.0017, 0.00175)}
	big := pricedVariant{state: hostVariant("big", 0.012, 0)}
	none := pricedVariant{state: learnreplay.VariantState{Variant: "none", Role: string(domain.RoleDecode)}}
	assert.IsType(t, whatif.Unanswerable{}, onHostBound(hostFigure, host, r))
	assert.Equal(t, hostFigure, onHostBound(hostFigure, big, r))
	assert.Equal(t, hostFigure, onHostBound(hostFigure, none, r))
}

// A refusal the library gave stands: an overloaded fleet reads Saturated,
// not a bucket too wide, and the refusal's flags are the read's, engine's
// histogram included, added by onRatio as for any refusal priced on R_q.
func TestTPOTRatio_HostBoundNeverDisplacesARefusal(t *testing.T) {
	r := (&answerFlags{set: map[string]bool{}}).ratioFor(windowsState(nil, []whatif.TPOTWindow{coarseWindow}), 0.9)
	require.Nil(t, r.refused)
	require.Contains(t, r.flags, whatif.EngineTPOTHistogram)
	host := pricedVariant{state: hostVariant("d", 0.0017, 0.00175)}
	sat := whatif.Unanswerable{Reason: whatif.Saturated}
	assert.Equal(t, sat, onHostBound(sat, host, r))

	got := onRatio(onHostBound(hostFigure, host, r), r.ratio, r.flags)
	u, ok := got.(whatif.Unanswerable)
	require.True(t, ok)
	assert.Equal(t, whatif.TPOTBucketTooWide, u.Reason)
	assert.Contains(t, u.Detail, "engine's TPOT histogram")
	assert.Contains(t, u.Flags, whatif.CoarseTPOTBucket)
	assert.Contains(t, u.Flags, whatif.EngineTPOTHistogram)
}

// freeze refuses the same percentile on a host-bound decode, recording why
// and no figure, so the staircase analyzer scores no stand-in for it.
func TestFreeze_AHostBoundPercentileIsRefused(t *testing.T) {
	st := windowsState([]whatif.TPOTWindow{coarseWindow}, nil)
	vs := hostVariant("d", 0.0017, 0.00175)
	out := percentileTPOTs(st, &vs, whatif.Decode{}, 1, []float64{90})
	require.Len(t, out, 1)
	assert.Zero(t, out[0].Seconds)
	assert.Contains(t, out[0].Refused, "TPOTBucketTooWide")
	assert.Contains(t, out[0].Refused, "host-bound")
}
