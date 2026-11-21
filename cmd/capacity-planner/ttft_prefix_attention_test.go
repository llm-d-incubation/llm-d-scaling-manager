package main

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The fixtures here are synthetic: no committed recording has a prefix hit
// (issue llm-d-incubation/llm-d-scaling-manager#311).

// hitState is learnedState with the prefill variant's measured shape: whole
// prompts of 8000 tokens at a hit rate of prefillHit. on is
// slo.SwitchTTFTPrefixAttention as the replay read it.
func hitState(prefillHit float64, on bool) *learnreplay.State {
	st := learnedState()
	st.Prefill[0].PrefixAttention = on
	st.Variants = []learnreplay.VariantState{{Variant: "pre", Role: domain.RolePrefill,
		Shape: learnreplay.MeasuredShape{InputTokens: 8000, PrefixHitRate: prefillHit, FromCycle: 21, ToCycle: 40, Cycles: 20}}}
	return st
}

// hitDecode is a decode variant that served 8000-token prompts at hit rate
// hit, read as asked, with the KV transfer measured at transfer seconds.
func hitDecode(hit, transfer float64) *pricedVariant {
	const il = 8000
	return &pricedVariant{
		state: learnreplay.VariantState{Variant: "dec", Role: domain.RoleDecode,
			Shape:      learnreplay.MeasuredShape{InputTokens: il, OutputTokens: 200, PrefixHitRate: hit, Cycles: 20},
			DecodeTTFT: learnreplay.DecodeTTFTState{TransferSeconds: transfer}},
		shape: shape.New(il, 200, hit),
	}
}

// hitFleetOf is fleetOf with a decode variant.
func hitFleetOf(t *testing.T, st *learnreplay.State, pv *pricedVariant, f answerFlags, set ...string) *pricedFleet {
	t.Helper()
	f.set = map[string]bool{}
	for _, name := range set {
		f.set[name] = true
	}
	ps, err := f.prefillState(st)
	require.NoError(t, err)
	return f.fleetFor(st, ps, ttftQuestion{q: 0.9}, pv)
}

// An assumed prompt is netted by the decode variant's hit rate, as before;
// with the switch on, by the prefill variant's, which is what the prefill
// pods skip. Unread, the prefill variant's leaves the decode one's.
func TestTTFT_PrefixAttentionPricesThePrefillVariantsHit(t *testing.T) {
	pv := hitDecode(0.75, 0)
	off := hitFleetOf(t, hitState(0.5, false), pv, answerFlags{inputTokens: 8000}, "assume-input-tokens")
	require.Nil(t, off.refused)
	p := off.fleet.Prefill
	assert.Equal(t, 2000.0, p.PromptTokens, "off: the decode variant's hit")
	assert.Equal(t, 0.75, p.PromptHitRate)
	assert.Equal(t, 8000.0, p.WholePromptTokens)
	assert.False(t, p.PrefixAttention)

	on := hitFleetOf(t, hitState(0.5, true), pv, answerFlags{inputTokens: 8000}, "assume-input-tokens")
	require.Nil(t, on.refused)
	p = on.fleet.Prefill
	assert.Equal(t, 4000.0, p.PromptTokens, "on: the prefill variant's hit")
	assert.Equal(t, 0.5, p.PromptHitRate)
	assert.Equal(t, 8000.0, p.WholePromptTokens)
	assert.True(t, p.PrefixAttention)

	unread := hitState(0.5, true)
	unread.Variants[0].Shape = learnreplay.MeasuredShape{}
	pf := hitFleetOf(t, unread, pv, answerFlags{inputTokens: 8000}, "assume-input-tokens")
	assert.Equal(t, 2000.0, pf.fleet.Prefill.PromptTokens, "no prefill shape read: as before")
}

// A stated hit rate (--prefix-hit, or a stated IL/OL/hit shape) governs the
// prefill pods too with the switch on, over the prefill variant's measured
// one, and is the hit rate recorded beside the prompt; off, the decode
// shape's, stated or not, as before.
func TestTTFT_PrefixAttentionAStatedHitGovernsThePrefill(t *testing.T) {
	stated := func() *pricedVariant {
		pv := hitDecode(0.75, 0)
		pv.shape, pv.assumedHit = shape.New(8000, 200, 0.25), true
		return pv
	}
	on := hitFleetOf(t, hitState(0.5, true), stated(), answerFlags{inputTokens: 8000}, "assume-input-tokens")
	require.Nil(t, on.refused)
	assert.Equal(t, 6000.0, on.fleet.Prefill.PromptTokens, "on: the stated 25%, not the prefill variant's 50%")
	assert.Equal(t, 0.25, on.fleet.Prefill.PromptHitRate)

	off := hitFleetOf(t, hitState(0.5, false), stated(), answerFlags{inputTokens: 8000}, "assume-input-tokens")
	require.Nil(t, off.refused)
	assert.Equal(t, 6000.0, off.fleet.Prefill.PromptTokens, "off: the decode shape's, here the stated one")
	assert.Equal(t, 0.25, off.fleet.Prefill.PromptHitRate)
}

// The whole prompts and hit rates sit in the inputs row and the JSON beside
// the computed prompts, so a VPShapeMoved the hits raised shows why; where
// no hit is above 0 the row is as before, and where the whole prompt is
// unknown so is the JSON.
func TestTTFT_TheWholePromptAndHitAreShown(t *testing.T) {
	st := hitState(0.75, false)
	st.Prefill[0].Estimate.VPWholePromptTokens = 2000
	pf := fleetOf(t, st, answerFlags{})
	require.Nil(t, pf.refused)
	assert.Contains(t, pf.inputsText, "at 2000 tokens, 2000 whole at a 0.00 prefix hit rate)")
	assert.Contains(t, pf.inputsText, "cycles 21-40, 8000 whole at a 0.75 prefix hit rate)")
	assert.Equal(t, 2000.0, pf.in.VPWholePromptTokens)
	require.NotNil(t, pf.in.VPPromptHitRate)
	assert.Zero(t, *pf.in.VPPromptHitRate)
	assert.Equal(t, 8000.0, pf.in.WholePromptTokens)
	require.NotNil(t, pf.in.PromptHitRate)
	assert.Equal(t, 0.75, *pf.in.PromptHitRate)

	fresh := hitState(0, false)
	fresh.Prefill[0].Estimate.VPWholePromptTokens = 2000
	pf = fleetOf(t, fresh, answerFlags{})
	assert.NotContains(t, pf.inputsText, "whole", "no hit on either side: the row as before")

	pf = fleetOf(t, learnedState(), answerFlags{})
	assert.NotContains(t, pf.inputsText, "whole")
	assert.Zero(t, pf.in.VPWholePromptTokens)
	assert.Nil(t, pf.in.VPPromptHitRate)
	assert.Zero(t, pf.in.WholePromptTokens)
	assert.Nil(t, pf.in.PromptHitRate)
}

// freeze drops the recording's hit rates, V_P's with the shapes', so a V_P
// read at a warm-up's hits is not flagged VPShapeMoved against the
// driver's none.
func TestFreeze_DropsVPsHitsWithTheShapes(t *testing.T) {
	st := hitState(0.75, false)
	st.Prefill[0].Estimate.VPWholePromptTokens, st.Prefill[0].Estimate.VPPromptHitRate = 8000, 0.75
	rates := dropWarmUpHits(st)
	assert.Equal(t, map[string]float64{"pre": 0.75}, rates)
	assert.Zero(t, st.Variants[0].Shape.PrefixHitRate)
	assert.Zero(t, st.Prefill[0].Estimate.VPWholePromptTokens)
	assert.Zero(t, st.Prefill[0].Estimate.VPPromptHitRate)
	pf := fleetOf(t, st, answerFlags{})
	require.Nil(t, pf.refused)
	k, ok := whatif.PrefillKnee(pf.fleet.Prefill).(whatif.Answer)
	require.True(t, ok)
	assert.False(t, k.HasFlag(whatif.VPShapeMoved), "flags %v", k.Flags)
}

// The whole prompt and the hit rate sit beside the measured prompt, on
// whatever the switch says, so V_P is flagged VPShapeMoved where it was
// learned at other hits; a state that recorded none (an older one) prices
// and flags as before.
func TestTTFT_TheHitBesideThePromptRaisesVPShapeMoved(t *testing.T) {
	moved := func(st *learnreplay.State) bool {
		pf := fleetOf(t, st, answerFlags{})
		require.Nil(t, pf.refused)
		k, ok := whatif.PrefillKnee(pf.fleet.Prefill).(whatif.Answer)
		require.True(t, ok)
		return k.HasFlag(whatif.VPShapeMoved)
	}
	st := hitState(0.75, false)
	p := fleetOf(t, st, answerFlags{}).fleet.Prefill
	assert.Equal(t, 2000.0, p.PromptTokens, "the measured computed prompt")
	assert.Equal(t, 8000.0, p.WholePromptTokens)
	assert.Equal(t, 0.75, p.PromptHitRate)
	assert.False(t, moved(st), "V_P recorded no whole prompt: as before")

	// V_P learned on fresh 2k prompts: the same 2k computed, 8k at 75% asked.
	st.Prefill[0].Estimate.VPWholePromptTokens = 2000
	assert.True(t, moved(st), "the hit moved from 0 to 0.75")
	st.Prefill[0].Estimate.VPWholePromptTokens, st.Prefill[0].Estimate.VPPromptHitRate = 8000, 0.75
	assert.False(t, moved(st), "learned at the same hits")
}

// The KV transfer moves the whole prompt less what decode holds: with the
// switch on, T scales by I*(1 - decode hit) asked over read; off, by the
// computed prompt, as before.
func TestTTFT_PrefixAttentionMovesTheTransferByTheDecodeHit(t *testing.T) {
	// Read: the prefill pods computed 2000 a request, decode served 8000 at
	// a 50% hit, the pull took 20 ms. Asked: 16000 at the same hits.
	const transfer = 0.02
	total := func(on bool) (float64, *pricedFleet) {
		pv := hitDecode(0.5, transfer)
		pv.shape = shape.New(16000, 200, 0.5)
		pf := hitFleetOf(t, hitState(0.75, on), pv, answerFlags{inputTokens: 16000}, "assume-input-tokens")
		require.Nil(t, pf.refused)
		p := pf.fleet.Prefill
		return p.TransferSecondsPerToken * p.PromptTokens, pf
	}
	off, pf := total(false)
	assert.Equal(t, 8000.0, pf.fleet.Prefill.PromptTokens)
	assert.InDelta(t, transfer*8000/2000, off, 1e-15, "off: T per computed token read, at the computed prompt asked")
	on, pf := total(true)
	assert.Equal(t, 4000.0, pf.fleet.Prefill.PromptTokens, "the prefill variant's 75%")
	assert.InDelta(t, transfer*(16000*0.5)/(8000*0.5), on, 1e-15, "on: T by the tokens moved, asked over read")
}

// --assume-transfer-base is a fixed part of the measured transfer per
// request; the rest goes with the prompt. 0, the default, prices as before;
// above the measured it is held at the measured; with nothing measured it
// is not applied; a negative one is refused.
func TestTTFT_TheTransferBaseIsAFixedPartOfTheMeasured(t *testing.T) {
	const transfer = 0.02
	at := func(base time.Duration, measured float64) (float64, *pricedFleet) {
		pv := hitDecode(0.5, measured)
		pv.shape = shape.New(16000, 200, 0.5)
		set := []string{"assume-input-tokens"}
		if base != 0 {
			set = append(set, "assume-transfer-base")
		}
		pf := hitFleetOf(t, hitState(0.75, false), pv, answerFlags{inputTokens: 16000, assumeTransferBase: base}, set...)
		require.Nil(t, pf.refused)
		p := pf.fleet.Prefill
		return p.TransferSecondsPerToken * p.PromptTokens, pf
	}
	none, _ := at(0, transfer)
	assert.InDelta(t, transfer*8000/2000, none, 1e-15)
	got, pf := at(5*time.Millisecond, transfer)
	assert.InDelta(t, 0.005+0.015*8000/2000, got, 1e-15)
	assert.Contains(t, strings.Join(pf.in.Assumed, "; "), "KV transfer base 5ms a request (--assume-transfer-base)")
	got, pf = at(30*time.Millisecond, transfer)
	assert.InDelta(t, transfer, got, 1e-15, "a base above the measured is held at it")
	assert.Contains(t, strings.Join(pf.in.Assumed, "; "), "KV transfer base 30ms above the 20ms measured")
	_, pf = at(5*time.Millisecond, 0)
	assert.Zero(t, pf.fleet.Prefill.TransferSecondsPerToken)
	assert.Contains(t, pf.in.Assumed, "--assume-transfer-base not applied: no KV transfer measured on the decode rows")

	// With the switch on and a decode hit rate of 1, no token reads as
	// moved: the base is dropped, and not named as applied.
	pv := hitDecode(1, transfer)
	pv.shape = shape.New(16000, 200, 0.5)
	pf = hitFleetOf(t, hitState(0.75, true), pv, answerFlags{inputTokens: 16000, assumeTransferBase: 5 * time.Millisecond},
		"assume-input-tokens", "assume-transfer-base")
	require.Nil(t, pf.refused)
	p := pf.fleet.Prefill
	assert.InDelta(t, transfer*p.PromptTokens/2000, p.TransferSecondsPerToken*p.PromptTokens, 1e-15, "T over the computed prompt read")
	assumed := strings.Join(pf.in.Assumed, "; ")
	assert.NotContains(t, assumed, "KV transfer base 5ms a request")
	assert.Contains(t, assumed, "--assume-transfer-base not applied")

	f := answerFlags{assumeTransferBase: -time.Millisecond, set: map[string]bool{"assume-transfer-base": true}}
	require.ErrorContains(t, f.checkTTFT(), "--assume-transfer-base -1ms, want a duration of 0 or more")
}
