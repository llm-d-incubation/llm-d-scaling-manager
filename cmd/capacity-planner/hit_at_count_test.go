package main

import (
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

// The lab run of issue #384 as the planner reads it on 2 prefill pods: a
// request computed 7,532 of its 15,962 prompt tokens (a total hit of 0.528)
// and the prefix-cache counter read 0.526. On 1 pod the same traffic
// computed 10,454 a request; on 4, 3,205.
const (
	labWholePrompt   = 15962.0
	labComputedTwo   = 7532.0
	labComputedOne   = 10454.0
	labComputedFour  = 3205.0
	labPrefixHitRate = 0.526
)

// labWindow is the prefill prompt window of the lab run on 2 pods: one
// request a second computing labComputedTwo of labWholePrompt tokens.
func labWindow() *whatif.PromptWindow {
	w := &whatif.PromptWindow{}
	for i := range whatif.PromptWindowMinCycles {
		w.Note([]domain.ReplicaMetrics{{Ready: true, PrefillComputedTokenRate: labComputedTwo, PrefillComputedTokenRateKnown: true,
			RequestRate: 1, RequestRateKnown: true, AvgInputTokensRecent: labWholePrompt}}, i, time.Unix(int64(15*i), 0))
	}
	return w
}

// labState is the lab run's replay on 2 prefill pods, with
// saturation.SwitchPrefillHitAtCount as on.
func labState(on bool) *learnreplay.State {
	st := learnedState()
	ps := &st.Prefill[0]
	ps.Pods, ps.PromptTokens, ps.PromptWindow, ps.HitAtCount = 2, labComputedTwo, labWindow(), on
	ps.Estimate.VPPromptTokens = labComputedTwo
	st.Variants = []learnreplay.VariantState{{Variant: "pre", Role: domain.RolePrefill,
		Shape: learnreplay.MeasuredShape{InputTokens: labWholePrompt, PrefixHitRate: labPrefixHitRate, FromCycle: 21, ToCycle: 40, Cycles: 20}}}
	return st
}

// labFleet prices the lab run's fleet at pods prefill pods.
func labFleet(t *testing.T, on bool, pods int) *pricedFleet {
	t.Helper()
	pf := fleetOf(t, labState(on), answerFlags{prefill: pods}, "prefill")
	require.Nil(t, pf.refused)
	return pf
}

// Fitted on 2 prefill pods and priced on 1, the switch prices the floor hit
// 0.528 x 1/2, so a request computes about 11,750 tokens: at or over the
// 10,454 measured on 1 pod. Off, the 2-pod hit is held and a request
// computes 7,532, 28% under what 1 pod computed, so the prefill knee reads
// high. The hit beside the prompt halves too, the figure is flagged
// PrefixHitAtCount, the inputs carry the rule and its range, and the
// figure's class carries the priced hit.
func TestTTFT_HitAtCountPricesTheFloorAtFewerPods(t *testing.T) {
	off, on := labFleet(t, false, 1), labFleet(t, true, 1)
	t.Logf("1 prefill pod: off %.0f computed tokens a request, on %.0f, measured %.0f",
		off.fleet.Prefill.PromptTokens, on.fleet.Prefill.PromptTokens, labComputedOne)
	assert.Equal(t, labComputedTwo, off.fleet.Prefill.PromptTokens, "off: the 2-pod prompt held")
	assert.Less(t, off.fleet.Prefill.PromptTokens, labComputedOne, "off reads under what 1 pod computed")
	assert.Nil(t, off.in.HitAtCount)
	assert.NotContains(t, off.flags, whatif.PrefixHitAtCount)

	h := 1 - labComputedTwo/labWholePrompt
	p := on.fleet.Prefill
	assert.InDelta(t, labWholePrompt*(1-h/2), p.PromptTokens, 1e-6)
	assert.GreaterOrEqual(t, p.PromptTokens, labComputedOne, "on: never under what 1 pod computed")
	assert.InDelta(t, labPrefixHitRate/2, p.PromptHitRate, 1e-12)
	assert.Equal(t, p.PromptTokens, on.in.PromptTokens)
	assert.Contains(t, on.flags, whatif.PrefixHitAtCount)
	require.NotNil(t, on.in.HitAtCount)
	assert.Equal(t, wireHitAtCount{FitHit: h, Hit: h / 2, FitCount: 2, Count: 1, Rule: string(prefill.HitFloor), Lo: h / 2, Hi: h,
		FitPromptTokens: labComputedTwo, PromptTokens: p.PromptTokens}, *on.in.HitAtCount)
	assert.Contains(t, strings.Join(on.in.Assumed, "; "), "prefix hit 0.264 on 1 prefill pods from 0.528 read on 2 (the floor hit x N'/N")
	assert.InDelta(t, h/2, on.class().PrefixHitRate, 1e-12, "the class carries the hit priced")

	kOff := whatif.PrefillKnee(off.fleet.Prefill).(whatif.Answer).Value
	kOn := whatif.PrefillKnee(on.fleet.Prefill).(whatif.Answer).Value
	assert.Less(t, kOn, kOff, "more computed tokens a request: a lower prefill knee")
}

// Priced on more pods than the fit's, the hit is held: the prompt is the
// 2-pod one, over the 3,205 that 4 pods computed (safe for capacity), and
// the figure is flagged PrefixHitHeld with the bracket's upper edge named.
func TestTTFT_HitAtCountHoldsTheHitAtMorePods(t *testing.T) {
	off, on := labFleet(t, false, 4), labFleet(t, true, 4)
	assert.Equal(t, off.fleet.Prefill, on.fleet.Prefill, "held: the prefill inputs do not move")
	assert.GreaterOrEqual(t, on.fleet.Prefill.PromptTokens, labComputedFour)
	assert.Contains(t, on.flags, whatif.PrefixHitHeld)
	assert.NotContains(t, off.flags, whatif.PrefixHitHeld)
	require.NotNil(t, on.in.HitAtCount)
	assert.Equal(t, string(prefill.HitHeld), on.in.HitAtCount.Rule)
	assert.Equal(t, 1.0, on.in.HitAtCount.Hi)
	assert.Contains(t, strings.Join(on.in.Assumed, "; "), "prefix hit 0.528 read on 2 prefill pods held on 4")
}

// At the recorded count nothing moves with the switch on: the fleet, its
// flags and its inputs are the switch's off ones.
func TestTTFT_HitAtCountMovesNothingAtTheRecordedCount(t *testing.T) {
	for _, pods := range []int{0, 2} {
		off, on := labFleet(t, false, pods), labFleet(t, true, pods)
		assert.Equal(t, off.fleet, on.fleet, "pods %d", pods)
		assert.Equal(t, off.flags, on.flags, "pods %d", pods)
		assert.Equal(t, off.in, on.in, "pods %d", pods)
	}
}

// A hit the question states governs and is not moved; a whole prompt
// stated with the hit read is moved as the measured one is; and with no
// prefix hit read, nothing moves.
func TestTTFT_HitAtCountLeavesAStatedHit(t *testing.T) {
	stated := hitDecode(0.5, 0)
	stated.assumedHit = true
	pf := hitFleetOf(t, labState(true), stated, answerFlags{prefill: 1, inputTokens: 8000}, "prefill", "assume-input-tokens")
	require.Nil(t, pf.refused)
	assert.Nil(t, pf.in.HitAtCount, "a stated hit governs")
	assert.NotContains(t, pf.flags, whatif.PrefixHitAtCount)

	read := hitDecode(0.5, 0)
	read.assumedHit, read.hitRead = true, true
	pf = hitFleetOf(t, labState(true), read, answerFlags{prefill: 1, inputTokens: 8000}, "prefill", "assume-input-tokens")
	require.Nil(t, pf.refused)
	require.NotNil(t, pf.in.HitAtCount, "a hit read from the load is moved")
	// The load's whole prompt is netted by the prefill pods' computed-token
	// hit, not the decode variant's 0.5 (netByComputedHit), and that hit is
	// the one moved.
	h, ok := labState(true).Prefill[0].PromptWindow.ComputedHit()
	require.True(t, ok)
	assert.InDelta(t, 8000*(1-h/2), pf.fleet.Prefill.PromptTokens, 1e-9, "8000 tokens at half the prefill pods' hit read")

	none := labState(true)
	none.Prefill[0].PromptWindow = nil
	none.Variants = nil
	pf = fleetOf(t, none, answerFlags{prefill: 1}, "prefill")
	require.Nil(t, pf.refused)
	assert.Nil(t, pf.in.HitAtCount, "no hit read: nothing to move")
	assert.Equal(t, labComputedTwo, pf.fleet.Prefill.PromptTokens)
}

// A prompt window that spans a scale-down from 4 prefill pods to 2 reads its
// hit partly on 4, so the hit was read on 4, not on the 2 of the last cycle:
// priced on 1 pod it is the floor h x 1/4, not h x 1/2, which on a window
// read mostly at 4 pods would price over what 1 pod hits. On 2 pods, the
// cycle's own count, it is the floor h x 2/4 too; on 4 nothing moves, and
// on 6 it is held.
func TestTTFT_HitAtCountReadsTheWindowsMostPods(t *testing.T) {
	w := &whatif.PromptWindow{}
	row := domain.ReplicaMetrics{Ready: true, PrefillComputedTokenRate: labComputedTwo, PrefillComputedTokenRateKnown: true,
		RequestRate: 1, RequestRateKnown: true, AvgInputTokensRecent: labWholePrompt}
	for i := range whatif.PromptWindowMinCycles {
		rows := []domain.ReplicaMetrics{row, row}
		if i < 3 {
			rows = append(rows, row, row)
		}
		w.Note(rows, i, time.Unix(int64(15*i), 0))
	}
	require.Equal(t, 4, w.MaxPods())
	h := 1 - labComputedTwo/labWholePrompt
	for _, c := range []struct {
		pods int
		hit  float64
		rule prefill.HitRule
	}{{1, h / 4, prefill.HitFloor}, {2, h / 2, prefill.HitFloor}, {6, h, prefill.HitHeld}} {
		st := labState(true)
		st.Prefill[0].PromptWindow = w
		pf := fleetOf(t, st, answerFlags{prefill: c.pods}, "prefill")
		require.Nil(t, pf.refused)
		require.NotNil(t, pf.in.HitAtCount, "%d pods", c.pods)
		assert.Equal(t, 4, pf.in.HitAtCount.FitCount, "%d pods", c.pods)
		assert.Equal(t, string(c.rule), pf.in.HitAtCount.Rule, "%d pods", c.pods)
		assert.InDelta(t, c.hit, pf.in.HitAtCount.Hit, 1e-12, "%d pods", c.pods)
		assert.InDelta(t, labWholePrompt*(1-c.hit), pf.fleet.Prefill.PromptTokens, 1e-6, "%d pods", c.pods)
	}
	st := labState(true)
	st.Prefill[0].PromptWindow = w
	pf := fleetOf(t, st, answerFlags{prefill: 4}, "prefill")
	require.Nil(t, pf.refused)
	assert.Nil(t, pf.in.HitAtCount, "the count the hit was read on: nothing moves")
	assert.Equal(t, labComputedTwo, pf.fleet.Prefill.PromptTokens)
}
