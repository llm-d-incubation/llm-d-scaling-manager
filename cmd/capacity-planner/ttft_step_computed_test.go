package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// A backtest step states the whole prompt and the prompt its prefill pods
// computed (whatifapi.Knobs.PrefillComputedTokens). With
// slo.SwitchTTFTPrefixAttention on, the prefill work is the computed prompt
// and the rest of the whole one is the cached context the attention spans
// (issue llm-d-incubation/llm-d-scaling-manager#311): a step at a 0.75 hit
// computes a quarter of its prompt and attends over the other three.
func TestTTFT_PrefixAttentionPricesTheStepsComputedPrompt(t *testing.T) {
	t.Parallel()
	st := tierState(t, tierRecording(t, true))
	const whole = 24000.0
	knobs := whatifapi.Knobs{Headroom: 0.8, PromptTokens: whole, PrefillComputedTokens: whole / 4}

	sf := tierFleet(t, st, false, true, false, knobs)
	p := sf.pf.fleet.Prefill
	assert.InDelta(t, whole/4, p.PromptTokens, 1e-9, "the prefill pods' computed prompt")
	assert.InDelta(t, whole, p.WholePromptTokens, 1e-9)
	assert.InDelta(t, 0.75, p.PromptHitRate, 1e-12)
	require.True(t, p.CachedContextRead, "the cached context is the whole prompt less the computed one")
	assert.InDelta(t, 0.75*whole, p.CachedContextTokens, 1e-9)
	assert.Contains(t, sf.pf.in.Assumed, "prompt 6000 tokens computed of 24000, as the step's prefill pods computed them")
}

// Off, the computed prompt moves nothing: the whole prompt is netted as
// before. On, a step whose hit is at most whatif.MaterialPrefixHitMove
// (the decode and prefill rows' readings agree only to within it with no
// cache) moves nothing either.
func TestTTFT_TheStepsComputedPromptMovesNothingOffOrImmaterial(t *testing.T) {
	t.Parallel()
	st := tierState(t, tierRecording(t, true))
	const whole = 24000.0
	plain := whatifapi.Knobs{Headroom: 0.8, PromptTokens: whole}

	withStep := plain
	withStep.PrefillComputedTokens = whole / 4
	assert.Equal(t, tierFleet(t, st, false, false, false, plain).pf.fleet.Prefill,
		tierFleet(t, st, false, false, false, withStep).pf.fleet.Prefill, "off")

	withStep.PrefillComputedTokens = 0.95 * whole
	assert.Equal(t, tierFleet(t, st, false, true, false, plain).pf.fleet.Prefill,
		tierFleet(t, st, false, true, false, withStep).pf.fleet.Prefill, "on, a 0.05 hit")
}

// With saturation.SwitchPrefillHitAtCount on too, a step's computed prompt
// stands: its prefill pods computed it at the step's own count, so the fit's
// hit is not moved to that count over it. On the lab run fitted on 2 pods
// and stepped on 1, the step's 10,454 computed tokens are priced as read;
// without a computed prompt the switch moves the whole prompt as before.
func TestTTFT_HitAtCountLeavesTheStepsComputedPrompt(t *testing.T) {
	t.Parallel()
	fleet := func(computed float64) *pricedFleet {
		st := labState(true)
		st.Prefill[0].PrefixAttention = true
		pf := fleetOf(t, st, answerFlags{prefill: 1, inputTokens: labWholePrompt, prefillComputed: computed}, "assume-input-tokens")
		require.Nil(t, pf.refused)
		return pf
	}
	step := fleet(labComputedOne)
	assert.True(t, step.stepComputed, "precondition: the step's computed prompt is priced")
	assert.InDelta(t, labComputedOne, step.fleet.Prefill.PromptTokens, 1e-9, "the step's computed prompt, as measured")
	assert.InDelta(t, 1-labComputedOne/labWholePrompt, step.fleet.Prefill.PromptHitRate, 1e-12)
	assert.Nil(t, step.in.HitAtCount, "the hit is not moved to the step's count")

	plain := fleet(0)
	assert.False(t, plain.stepComputed)
	assert.NotNil(t, plain.in.HitAtCount, "precondition: the switch moves a prompt with no computed one")
}
