package prefill

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// f per pod count (docs/proposals/slo-tiers.md, decision 1; S1 test 4). F
// is a median over every count the variant has had, so a learned f from 3
// pods would be read at 4. FByPods keeps each count's minutes apart.

// fixedShares is minutes of n pods, the first carrying weight times the
// others' requests, at about 400 requests a pod: an imbalance known by
// construction, which the per-count medians must keep apart.
func fixedShares(rng *rand.Rand, minutes int, weights ...float64) []Cycle {
	return sharedMinutes(rng, minutes, 400, weights...)
}

// Minutes at 3 pods and then at 4: each count has its own median, F mixes
// them, a count with too few minutes gives no f, and the counts' samples
// add up to FSamples.
func TestImbalance_ByPodsKeepsCountsApart(t *testing.T) {
	rng := rand.New(rand.NewPCG(4, 1))
	three := fixedShares(rng, 8, 3, 1, 1)   // f about 1.8 at 3 pods
	four := fixedShares(rng, 6, 1, 1, 1, 1) // f about 1 at 4 pods
	two := fixedShares(rng, 2, 1, 1)        // too few minutes at 2 pods
	cs := append(append(append([]Cycle{}, three...), four...), two...)
	for i := range cs {
		cs[i].Now = t0.Add(time.Duration(i) * minute)
	}
	est := run(NewObserver(), cs...)

	require.Equal(t, map[int]int{3: 8, 4: 6, 2: 2}, est.FSamplesByPods)
	assert.Equal(t, est.FSamples, 8+6+2)
	require.Contains(t, est.FByPods, 3)
	require.Contains(t, est.FByPods, 4)
	assert.NotContains(t, est.FByPods, 2, "a count with fewer than MinImbalanceMinutes gives no f")
	assert.InDelta(t, 1.8, est.FByPods[3], 0.15, "3 pods at 3:1:1")
	assert.InDelta(t, 1.0, est.FByPods[4], 0.1, "4 equal pods")
	assert.Greater(t, est.FByPods[3]-est.FByPods[4], 0.5, "the counts are not mixed")
	assert.Equal(t, 2, est.FPodsLast)
}

// A learned f at 3 pods is not read at 4: the variant scaled to 4 pods and
// has fewer than MinImbalanceMinutes there, so FByPods has no 4, though F,
// over every count, is set.
func TestImbalance_ALearnedFAtThreeIsNotReadAtFour(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 1))
	cs := append(fixedShares(rng, 8, 3, 1, 1), fixedShares(rng, MinImbalanceMinutes-1, 1, 1, 1, 1)...)
	for i := range cs {
		cs[i].Now = t0.Add(time.Duration(i) * minute)
	}
	est := run(NewObserver(), cs...)
	assert.Positive(t, est.F, "F is over every count")
	assert.Equal(t, 4, est.FPodsLast)
	assert.NotContains(t, est.FByPods, 4, "too few minutes at 4 pods")
	assert.Equal(t, MinImbalanceMinutes-1, est.FSamplesByPods[4])
	assert.Contains(t, est.FByPods, 3)
}

// A count with exactly MinImbalanceMinutes minutes gives an f; one fewer
// does not.
func TestImbalance_ByPodsAtTheMinuteBoundary(t *testing.T) {
	rng := rand.New(rand.NewPCG(8, 1))
	cs := append(fixedShares(rng, MinImbalanceMinutes, 3, 1, 1), fixedShares(rng, MinImbalanceMinutes-1, 1, 1, 1, 1)...)
	for i := range cs {
		cs[i].Now = t0.Add(time.Duration(i) * minute)
	}
	est := run(NewObserver(), cs...)
	require.Equal(t, map[int]int{3: MinImbalanceMinutes, 4: MinImbalanceMinutes - 1}, est.FSamplesByPods)
	assert.Contains(t, est.FByPods, 3, "exactly MinImbalanceMinutes")
	assert.NotContains(t, est.FByPods, 4, "one fewer")
}

// No minute kept: both maps nil. One pod: no minute is a share, so none.
func TestImbalance_ByPodsIsNilWithNoMinute(t *testing.T) {
	rng := rand.New(rand.NewPCG(6, 1))
	est := run(NewObserver(), fixedShares(rng, 10, 1)...)
	assert.Nil(t, est.FByPods)
	assert.Nil(t, est.FSamplesByPods)
}

// A minute rewritten inside SampleSpacing takes the later pod count: the
// count is the minute's, not the first row's.
func TestImbalance_ARewrittenMinuteTakesItsPodCount(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 1))
	a := fixedShares(rng, 1, 1, 1, 1)[0]
	b := fixedShares(rng, 1, 1, 1)[0]
	a.Now, b.Now = t0, t0.Add(10*time.Second)
	est := run(NewObserver(), a, b)
	assert.Equal(t, map[int]int{2: 1}, est.FSamplesByPods)
}

// Serving is the pod rule every per-count figure is counted by: Ready, and
// not a warm-pool bridge.
func TestServing_ReadyAndNotABridge(t *testing.T) {
	for _, tc := range []struct {
		name          string
		ready, bridge bool
		want          bool
		reason        string
	}{
		{"ready", true, false, true, ""},
		{"not ready", false, false, false, rowNotReady},
		{"bridge, not ready", false, true, false, rowNotReady},
		{"ready bridge", true, true, false, rowBridge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rm := base("p")
			rm.Ready, rm.FromWarmPool = tc.ready, tc.bridge
			assert.Equal(t, tc.want, Serving(rm))
			assert.Equal(t, tc.reason, refusalFor(rm))
		})
	}
}

// Observe counts pods by Serving: three serving rows, one bridge and one
// row not Ready keep their minutes at 3 pods, and refuse the other two by
// the reason refusalFor names.
func TestImbalance_ByPodsCountsServingPodsOnly(t *testing.T) {
	rng := rand.New(rand.NewPCG(9, 1))
	cs := fixedShares(rng, 6, 1, 1, 1)
	for i := range cs {
		br := base("bridge")
		br.FromWarmPool = true
		br.RequestRate, br.PrefillComputedTokenRate = 7, 7000
		down := base("down")
		down.Ready = false
		cs[i].Rows = append(cs[i].Rows, br, down)
		cs[i].Now = t0.Add(time.Duration(i) * minute)
	}
	est := run(NewObserver(), cs...)
	assert.Equal(t, map[int]int{3: 6}, est.FSamplesByPods)
	assert.Equal(t, 3, est.FPodsLast)
	assert.Contains(t, est.FByPods, 3)
	assert.Equal(t, map[string]int{rowBridge: 1, rowNotReady: 1}, est.Refused)
}
