package slo

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// SwitchTTFTRouterWaitInPrefill reaches the priced fleet through Analyze: off
// from the constructor, and on only where a replay turns it on.
func TestTTFT_TheRouterWaitInPrefillSwitchReachesTheFleet(t *testing.T) {
	inputs := func(sw map[string]bool) ttftInputs {
		var last *ttftInputs
		onTTFTPriced = func(in ttftInputs, _ float64, _ whatif.TTFTTarget) { last = &in }
		t.Cleanup(func() { onTTFTPriced = nil })
		r := newTTFTRig(ttftEstimate(), prefillRows("pre", 4000))
		require.NoError(t, r.a.SetSwitchesForReplay(sw))
		r.one(t)
		require.NotNil(t, last, "the variant was priced")
		return *last
	}
	a := New(nil, nil)
	require.False(t, a.ttftRouterWaitInPrefill, "off from the constructor")
	require.NotEqual(t, whatif.DefaultRouterWaitInPrefill, a.ttftRouterWaitInPrefill,
		"precondition: the planner's default is on, the controller's is off")

	off := inputs(nil)
	assert.False(t, off.routerWaitInPrefill)
	fl, _ := ttftFleet(off, off.pods)
	assert.False(t, fl.RouterWaitInPrefill)

	on := inputs(map[string]bool{SwitchTTFTRouterWaitInPrefill: true})
	assert.True(t, on.routerWaitInPrefill)
	assert.False(t, on.routerWait, "the gate stage stays off")
	fl, _ = ttftFleet(on, on.pods)
	assert.True(t, fl.RouterWaitInPrefill)
	assert.False(t, fl.PriceRouterWait)
}

// Controller and planner parity (issue #367): with the switch on, the
// analyzer prices the fleet the capacity planner prices at its defaults
// (--router-wait-in-prefill on, --price-router-wait off): the same figure,
// flags and verdict at the current rate, and the same published rate, below
// the prefill flow-control onset, past it inside and beyond
// whatif.MaxRouterWaitKneeShare of the knee, and past the knee. With it off,
// past the onset inside the share the two part: the analyzer flags the wait
// AboveFlowControlOnset and counts a miss where the planner reads the figure.
func TestTTFT_TheRouterWaitInPrefillSwitchIsThePlannersPrice(t *testing.T) {
	in := ttftInputsAt(2, 1.5)
	in.flow = whatif.FlowControl{State: whatif.FlowControlOn, QueueThreshold: 0.2, KVThreshold: 0.8}
	in.decode = testDecode()
	in.processorSharing = true
	in.spread = whatif.PromptSpread{CV: 0.06} // inside MaxPSPromptCV, where the switch takes

	// planner is the fleet the planner prices: the analyzer's, with the
	// planner's router-wait defaults.
	planner := func(in ttftInputs) whatif.TTFTFleet {
		fl, _ := ttftFleet(in, in.pods)
		fl.RouterWaitInPrefill, fl.PriceRouterWait = whatif.DefaultRouterWaitInPrefill, whatif.DefaultPriceRouterWait
		return fl
	}
	on, ok := whatif.FlowControlOnsets(planner(in)).(whatif.Onsets)
	require.True(t, ok)
	knee, ok := whatif.PrefillKnee(planner(in).Prefill).(whatif.Answer)
	require.True(t, ok)
	within := on.Prefill * 1.01
	require.Less(t, within, whatif.MaxRouterWaitKneeShare*knee.Value, "precondition: a rate above the onset inside the share")
	require.Less(t, within, on.Decode, "precondition: under the decode onset")
	cases := []struct {
		name string
		rate float64
	}{
		{"below the onset", 0.9 * on.Prefill},
		{"above the onset", within},
		{"past the share", (whatif.MaxRouterWaitKneeShare + 1) / 2 * knee.Value},
		{"past the knee", knee.Value * 1.01},
	}
	targets := []whatif.TTFTTarget{{Seconds: 100, Percentile: 0.9}, {Seconds: 100, Percentile: 0.95},
		{Seconds: 1e-3, Percentile: 0.9}, {Seconds: 2, Percentile: 0.95}}

	in.routerWaitInPrefill = true
	pl := planner(in)
	for _, c := range cases {
		for _, target := range targets {
			got := priceTTFT(in, c.rate, target)
			want := whatif.TTFTAt(pl, c.rate, target.Percentile)
			met, known := whatif.TTFTMeets(want, target)
			assert.Equal(t, known && !met, got.unmet, "%s, %v: verdict", c.name, target)
			switch w := want.(type) {
			case whatif.TTFT:
				require.NotNil(t, got.at, "%s, %v", c.name, target)
				assert.Equal(t, w.Seconds, got.at.Seconds, "%s, %v: figure", c.name, target)
				assert.Equal(t, w.Flags, got.at.Flags, "%s, %v: flags", c.name, target)
			case whatif.Unanswerable:
				require.NotNil(t, got.atRefused, "%s, %v", c.name, target)
				assert.Equal(t, w.Reason, got.atRefused.Reason, "%s, %v", c.name, target)
			}
			switch w := whatif.RateForTTFT(pl, target).(type) {
			case whatif.Answer:
				require.NotNil(t, got.rate, "%s, %v", c.name, target)
				assert.Equal(t, w.Value, got.rate.Value, "%s, %v: published rate", c.name, target)
				assert.Equal(t, w.Basis.BoundBy, got.rate.Basis.BoundBy, "%s, %v: its bound", c.name, target)
				assert.Equal(t, w.Flags, got.rate.Flags, "%s, %v: its flags", c.name, target)
			case whatif.Unanswerable:
				require.NotNil(t, got.rateRefused, "%s, %v", c.name, target)
				assert.Equal(t, w.Reason, got.rateRefused.Reason, "%s, %v", c.name, target)
			}
		}
	}
	// Where the switch takes, the figure decides: met against a loose
	// target, flagged FlowControlWaitPriced, as the planner reads it.
	at := priceTTFT(in, within, targets[0])
	require.NotNil(t, at.at)
	assert.True(t, at.at.HasFlag(whatif.FlowControlWaitPriced), "%v", at.at.Flags)
	assert.False(t, at.unmet)

	// Off, the analyzer prices as before: the same figure, flagged
	// AboveFlowControlOnset, a miss however low the figure, where the
	// planner's default reads it met.
	in.routerWaitInPrefill = false
	off := priceTTFT(in, within, targets[0])
	require.NotNil(t, off.at)
	assert.Equal(t, at.at.Seconds, off.at.Seconds, "the switch moves no figure")
	assert.True(t, off.at.HasFlag(whatif.AboveFlowControlOnset), "%v", off.at.Flags)
	assert.True(t, off.unmet)
}
