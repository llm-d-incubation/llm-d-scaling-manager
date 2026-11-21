package slo

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// f across pod counts (SwitchTTFTImbalanceAcrossCounts): opt-in, off by
// default at the CLI's default, a replay may turn it on. Off, an unlearned
// count prices at max(1.5, f); on, at the curve fitted to the counts P1
// learned, or whatif.FreshImbalance with none, flagged either way, and the
// prefill knee at that count rises with the lower f.
func TestTTFT_TheImbalanceAcrossCountsSwitch(t *testing.T) {
	a := newTTFTRig(ttftEstimate(), prefillRows("pre", 4000)).a
	require.Equal(t, whatif.DefaultImbalanceAcrossCounts, a.ttftImbalanceAcrossCounts, "the analyzer reads the shared default, off")
	require.False(t, a.ttftImbalanceAcrossCounts, "opt-in: off by default")
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchTTFTImbalanceAcrossCounts: true}))
	require.True(t, a.ttftImbalanceAcrossCounts)

	// Two pods with f learned there, priced at three.
	in := ttftInputsAt(2, 1.0691)
	knee := func(in ttftInputs) float64 {
		fl, _ := ttftFleet(in, 3)
		k, ok := whatif.PrefillKnee(fl.Prefill).(whatif.Answer)
		require.True(t, ok)
		return k.Value
	}

	in.acrossCounts = false
	off, _ := ttftFleet(in, 3)
	assert.Equal(t, whatif.ImbalanceFloor, off.Prefill.Imbalance)
	assert.True(t, off.Prefill.ImbalanceAssumed)
	offKnee := knee(in)

	in.acrossCounts = true
	on, _ := ttftFleet(in, 3)
	assert.InDelta(t, 1.104, on.Prefill.Imbalance, 0.001, "the curve through f(2)")
	assert.True(t, on.Prefill.ImbalanceAssumed, "fitted, not learned: flagged")
	_, _, source := in.imbalance(3)
	assert.Equal(t, "fitted from counts {2}", source)
	assert.InEpsilon(t, offKnee*whatif.ImbalanceFloor/on.Prefill.Imbalance, knee(in), 0.01,
		"the knee at three pods scales as 1/f")

	// The learned count itself is read as learned, on or off.
	fl, _ := ttftFleet(in, 2)
	assert.Equal(t, 1.0691, fl.Prefill.Imbalance)
	assert.False(t, fl.Prefill.ImbalanceAssumed)

	// One pod has no f.
	f, assumed, source := in.imbalance(1)
	assert.Zero(t, f)
	assert.False(t, assumed)
	assert.Equal(t, whatif.ImbalanceOnePod, source)
}

// With no count learned the switch prices FreshImbalance, at the current
// count raised to the raw share where that reads higher, and the other counts
// read the curve through it; routing() still
// reads the raw share as before.
func TestTTFT_TheImbalanceAcrossCountsDefault(t *testing.T) {
	in := ttftInputsAt(3, 0)
	in.acrossCounts = true
	f, assumed, source := in.imbalance(4)
	assert.Equal(t, whatif.FreshImbalance, f)
	assert.True(t, assumed)
	assert.Equal(t, whatif.ImbalanceDefault, source)

	f, _, _ = in.imbalance(3)
	assert.Equal(t, whatif.FreshImbalance, f, "no raw share read")

	in.shares = shareWindow{largest: 1.3}
	f, assumed, source = in.imbalance(3)
	assert.Equal(t, 1.3, f, "the current count's raw share, above the default")
	assert.True(t, assumed)
	assert.Equal(t, whatif.ImbalanceRawShare, source)
	f, _, _ = in.imbalance(4)
	assert.Greater(t, f, 1.3, "the raw share reaches the next count through the curve")

	in.shares = shareWindow{largest: 2.9, least: 2.9, full: true}
	bound, _ := in.routing()
	assert.True(t, bound, "routing-bound reads the raw share, switch or not")
	in.acrossCounts = false
	boundOff, _ := in.routing()
	assert.Equal(t, boundOff, bound)
}

// A hot pod at the current count reaches N+1 (review of #177): three pods,
// nothing learned, a raw share of 2.5. Before, f at four pods was
// whatif.FreshImbalance, and N+1 was priced cooler than N.
func TestTTFT_AHotPodReachesNPlusOne(t *testing.T) {
	in := ttftInputsAt(3, 0)
	in.acrossCounts = true
	in.shares = shareWindow{largest: 2.5}
	now, _ := ttftFleet(in, 3)
	next, _ := ttftFleet(in, 4)
	assert.Equal(t, 2.5, now.Prefill.Imbalance)
	assert.GreaterOrEqual(t, next.Prefill.Imbalance, now.Prefill.Imbalance, "N+1 is not priced cooler than N")
	assert.True(t, next.Prefill.ImbalanceAssumed)
	_, _, source := in.imbalance(4)
	assert.Equal(t, "fitted from the raw share at 3", source)
}
