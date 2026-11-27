package prefill

import (
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pooled sets FPooledMinutes on every cycle of cs.
func pooled(cs []Cycle, on bool) []Cycle {
	for i := range cs {
		cs[i].FPooledMinutes = on
	}
	return cs
}

// Two pods at 1.3:1 serving 60 requests a pod a minute, the long prompts'
// load: no minute carries MinRequestsPerPodMinute, so off (the negative
// control) learns no f at all. On, pairs of minutes carry 120 a pod, and f
// is the pools' median: the busiest share 2.6/2.3 = 1.130, less the chance
// excess at 120 a pod, about 0.05, and the de-bias errs low on a real
// imbalance (imbalance.go).
func TestImbalance_PooledMinutesLearnFUnderTheThreshold(t *testing.T) {
	for seed := range uint64(10) {
		mk := func(on bool) []Cycle {
			return pooled(sharedMinutes(rand.New(rand.NewPCG(seed, 3)), 30, 60, 1.3, 1), on)
		}
		off := run(NewObserver(), mk(false)...)
		assert.Equal(t, TooFewImbalanceMinutes, off.FRefused, "seed %d: off, no minute is read", seed)
		assert.Empty(t, off.FByPods, "seed %d", seed)

		on := run(NewObserver(), mk(true)...)
		require.Empty(t, on.FRefused, "seed %d", seed)
		assert.InDelta(t, 1.09, on.F, 0.05, "seed %d: the pools' f", seed)
		assert.InDelta(t, on.F, on.FByPods[2], 1e-12, "seed %d: learned at two pods", seed)
		assert.InDelta(t, 14, on.FSamples, 1, "seed %d: thirty minutes, pooled in pairs, a pair short of 100 taking a third", seed)
		assert.Positive(t, on.FLoad, "seed %d", seed)
	}
}

// A minute that carries MinRequestsPerPodMinute alone is read alone: on and
// off learn the same f from the same minutes, with four cycles a minute, each
// later one rewriting the minute's reading, as the rows are one-minute rates.
func TestImbalance_AMinuteAtTheThresholdIsReadAlone(t *testing.T) {
	for seed := range uint64(5) {
		mk := func(on bool) []Cycle {
			cs := sharedMinutes(rand.New(rand.NewPCG(seed, 5)), 40, 300, 1.2, 1, 1)
			for i := range cs {
				cs[i].Now = t0.Add(time.Duration(i) * 15 * time.Second)
			}
			return pooled(cs, on)
		}
		off := run(NewObserver(), mk(false)...)
		on := run(NewObserver(), mk(true)...)
		require.Empty(t, off.FRefused, "seed %d", seed)
		assert.InDelta(t, off.F, on.F, 1e-9, "seed %d", seed)
		assert.InDelta(t, off.FLoad, on.FLoad, 1e-6, "seed %d", seed)
		assert.Equal(t, off.FSamples, on.FSamples, "seed %d", seed)
		assert.Equal(t, 10, on.FSamples, "seed %d: forty cycles, four a minute", seed)
		assert.InDelta(t, off.FByPods[3], on.FByPods[3], 1e-9, "seed %d", seed)
	}
}

// A pool ends unread at a break: a gap of two minutes with no cycle, a change
// of the pods, or MaxPooledMinutes minutes that still carry too few requests.
// Each gives no f, on as off.
func TestImbalance_APoolEndsAtABreak(t *testing.T) {
	rng := rand.New(rand.NewPCG(9, 9))

	gaps := pooled(sharedMinutes(rng, 20, 60, 1, 1), true)
	for i := range gaps {
		gaps[i].Now = t0.Add(time.Duration(2*i) * minute)
	}
	assert.Equal(t, TooFewImbalanceMinutes, run(NewObserver(), gaps...).FRefused, "a gap ends the pool")

	swaps := pooled(sharedMinutes(rng, 20, 60, 1, 1), true)
	for i := range swaps {
		if i%2 == 1 {
			swaps[i].Rows[1].PodName = "p-c"
		}
	}
	assert.Equal(t, TooFewImbalanceMinutes, run(NewObserver(), swaps...).FRefused, "a change of pods ends the pool")

	thin := pooled(sharedMinutes(rng, 30, 15, 1, 1), true)
	assert.Equal(t, TooFewImbalanceMinutes, run(NewObserver(), thin...).FRefused,
		"five minutes of 15 a pod carry 75: the oldest drops, and no pool reaches 100")

	// The control: a thin load at 25 a pod reaches 100 in four or five
	// minutes.
	enough := run(NewObserver(), pooled(sharedMinutes(rng, 30, 25, 1, 1), true)...)
	assert.Empty(t, enough.FRefused)
	assert.LessOrEqual(t, enough.FSamples, 7, "thirty minutes in pools of four or five")
}

// A minute that carries MinRequestsPerPodMinute alone is read alone even
// right after lighter minutes, whose pool it would otherwise join: light
// minutes at 3:1 and 60 a pod alternate with busy ones at 1.2:1 and 300 a
// pod. Off reads the busy minutes; on reads the same busy minutes, each
// alone, and the light ones never reach a pool of their own (one light
// minute between two busy ones carries 60).
func TestImbalance_AMinuteAtTheThresholdAfterLightOnesIsReadAlone(t *testing.T) {
	for seed := range uint64(5) {
		rng := rand.New(rand.NewPCG(seed, 11))
		light, busy := sharedMinutes(rng, 20, 60, 3, 1), sharedMinutes(rng, 20, 300, 1.2, 1)
		cs := make([]Cycle, 0, 2*len(light))
		for i := range light {
			cs = append(cs, light[i], busy[i])
		}
		off := run(NewObserver(), pooled(slices.Clone(cs), false)...)
		on := run(NewObserver(), pooled(slices.Clone(cs), true)...)
		require.Empty(t, off.FRefused, "seed %d", seed)
		assert.Equal(t, 20, off.FSamples, "seed %d: the busy minutes", seed)
		assert.Equal(t, off.FSamples, on.FSamples, "seed %d", seed)
		assert.InDelta(t, off.F, on.F, 1e-9, "seed %d", seed)
		assert.InDelta(t, off.FLoad, on.FLoad, 1e-6, "seed %d", seed)
	}
}
