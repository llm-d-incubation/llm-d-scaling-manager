package main

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// labResidencyState is labState with the prefill rows' KV residency
// histograms: the lab run's on 2 pods, rebuilt from its load generator
// (prefill's labReuseAt2; gaps in requests, read here as seconds at one
// request a second).
func labResidencyState(on bool) *learnreplay.State {
	gap := []domain.LatencyBucket{{UpperBound: 1, Rate: 1.61914}, {UpperBound: 4, Rate: 6.30482}, {UpperBound: 16, Rate: 22.7247},
		{UpperBound: 32, Rate: 39.875}, {UpperBound: 64, Rate: 62.965}, {UpperBound: 90.5097, Rate: 65.7542},
		{UpperBound: math.Inf(1), Rate: 65.7542}}
	idle := []domain.LatencyBucket{{UpperBound: 45.2548, Rate: 0}, {UpperBound: 64, Rate: 22.4305}, {UpperBound: 90.5097, Rate: 59.7477},
		{UpperBound: math.Inf(1), Rate: 59.7477}}
	w := &whatif.PromptWindow{}
	for i := range whatif.PromptWindowMinCycles {
		w.Note([]domain.ReplicaMetrics{{Ready: true, PrefillComputedTokenRate: labComputedTwo, PrefillComputedTokenRateKnown: true,
			RequestRate: 1, RequestRateKnown: true, AvgInputTokensRecent: labWholePrompt,
			KVReuseGapBuckets: gap, KVIdleBeforeEvictBuckets: idle}}, i, time.Unix(int64(15*i), 0))
	}
	st := labState(on)
	st.Prefill[0].PromptWindow = w
	return st
}

// With the residency histograms recorded, the switch prices the hit by the
// reuse-gap law at fewer pods and at more: on 1 pod a request computes about
// 11,400 tokens (10,454 measured; the floor read 11,750); on 4 the law reads
// fewer than measured, but with no tier past the GPU its ceiling is assumed
// and the miss is held to halve: 3,766 (3,205 measured; the held hit read
// 7,532). Both are flagged PrefixHitAtCount. The inputs carry the law at every count, the recorded one too, so
// the main view and a backtest price the same one. Off, nothing moves and no
// law is fitted.
func TestTTFT_HitAtCountPricesTheReuseGapLaw(t *testing.T) {
	h := 1 - labComputedTwo/labWholePrompt

	one := fleetOf(t, labResidencyState(true), answerFlags{prefill: 1}, "prefill")
	require.Nil(t, one.refused)
	require.NotNil(t, one.in.HitAtCount)
	assert.Equal(t, string(prefill.HitReuseGap), one.in.HitAtCount.Rule)
	assert.InDelta(t, 11410, one.fleet.Prefill.PromptTokens, 100)
	assert.Contains(t, one.flags, whatif.PrefixHitAtCount)
	assert.Contains(t, strings.Join(one.in.Assumed, "; "), "the reuse-gap rule over the KV residency histograms")
	assert.Less(t, one.fleet.Prefill.PromptHitRate, labPrefixHitRate, "the GPU hit falls with the law")

	four := fleetOf(t, labResidencyState(true), answerFlags{prefill: 4}, "prefill")
	require.Nil(t, four.refused)
	require.NotNil(t, four.in.HitAtCount)
	assert.Equal(t, string(prefill.HitReuseGap), four.in.HitAtCount.Rule)
	assert.Greater(t, four.in.HitAtCount.Hit, h)
	assert.InDelta(t, labComputedTwo/2, four.fleet.Prefill.PromptTokens, 1e-6)
	assert.Contains(t, four.flags, whatif.PrefixHitAtCount)
	assert.NotContains(t, four.flags, whatif.PrefixHitHeld)
	// The law alone reads a GPU hit of 0.816 at 4 pods; the total is held
	// under it, and the GPU's hits are among the total's.
	assert.LessOrEqual(t, four.fleet.Prefill.PromptHitRate, four.in.HitAtCount.Hit)
	assert.Greater(t, four.fleet.Prefill.PromptHitRate, labPrefixHitRate)

	two := fleetOf(t, labResidencyState(true), answerFlags{prefill: 2}, "prefill")
	require.Nil(t, two.refused)
	require.NotNil(t, two.hitModel, "fitted at the recorded count too")
	assert.Nil(t, two.in.HitAtCount)
	assert.Equal(t, labComputedTwo, two.fleet.Prefill.PromptTokens)

	off := fleetOf(t, labResidencyState(false), answerFlags{prefill: 1}, "prefill")
	require.Nil(t, off.refused)
	assert.Nil(t, off.hitModel)
	assert.Equal(t, labComputedTwo, off.fleet.Prefill.PromptTokens)
}

// Under the reuse-gap rule the GPU hit moves by the law's GPU ratio even
// where an unsized tier past the GPU holds the total, and never above the
// total priced; under the floor it moves by the total's own ratio, and a
// held hit leaves it.
func TestGPUHitAt(t *testing.T) {
	held := prefill.HitAtCount{Rule: prefill.HitReuseGap, FitHit: 0.8, Hit: 0.8, GPUScale: 0.6, TierScale: 1.9}
	assert.InDelta(t, 0.3, gpuHitAt(0.5, held), 1e-12, "the tier holds the total; the GPU's share falls")

	capped := prefill.HitAtCount{Rule: prefill.HitReuseGap, FitHit: 0.526, Hit: 0.764, GPUScale: 1.552, TierScale: 1}
	assert.Equal(t, 0.764, gpuHitAt(0.526, capped))

	floor := prefill.HitAtCount{Rule: prefill.HitFloor, FitHit: 0.5, Hit: 0.25, GPUScale: 1, TierScale: 1}
	assert.InDelta(t, 0.2, gpuHitAt(0.4, floor), 1e-12)
	assert.Equal(t, 0.4, gpuHitAt(0.4, prefill.HitAtCount{Rule: prefill.HitHeld, FitHit: 0.5, Hit: 0.5}))
}
