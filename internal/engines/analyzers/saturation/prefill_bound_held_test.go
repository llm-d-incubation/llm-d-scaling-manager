package saturation

import (
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// Issue #397: the prefill bound under the gauges' noise
// (prefill_bound_held.go).

// boundAt is the prefill bound end to end with SwitchPrefillBoundHeldConfidence
// set to on.
func boundAt(on bool) func() muLearner {
	return func() muLearner {
		b := newBoundMu(nil)
		b.a.prefillBoundConfident = on
		return b
	}
}

// boundNoises is the noise each n-dependence case of the bound runs, by case
// name (otherCases).
var boundNoises = []struct {
	name  string
	check func(func() muLearner) func(uint64) error
}{
	{"iid-max", func(l func() muLearner) func(uint64) error { return checkMuNDependence(l, false) }},
	{"iid-max-sigma15", func(l func() muLearner) func(uint64) error { return checkMuNDependenceAt(l, arNoiseSpec{0.15, 0}) }},
	{"ar-max", func(l func() muLearner) func(uint64) error { return checkMuNDependenceAt(l, rateNoise10) }},
	{"ar-max-sigma15", func(l func() muLearner) func(uint64) error { return checkMuNDependenceAt(l, rateNoise15) }},
}

// The four n-dependence cases of the bound pass on every seed with the switch
// on, and fail on every seed with it off: off is the running max #397 ledgered
// (10/10 seeds, E 1.160 at N=1 on seed 1).
func TestPrefillBoundHeld_TheClearedLedgerCasesFailWithTheSwitchOff(t *testing.T) {
	for _, c := range boundNoises {
		t.Run(c.name, func(t *testing.T) {
			failed := 0
			var first error
			for seed := uint64(1); seed <= 10; seed++ {
				require.NoError(t, c.check(boundAt(true))(seed), "on, seed %d", seed)
				if err := c.check(boundAt(false))(seed); err != nil {
					failed++
					if first == nil {
						first = err
					}
				}
			}
			t.Logf("off: %d/10 seeds fail; seed 1: %v", failed, first)
			require.Equal(t, 10, failed, "off, the running max must fail every seed")
		})
	}
}

// boundExpectation is E[bound] over trials of minutes at n pods, each pod's
// rate 1 + sigma noise drawn fresh each minute, on seed's stream.
func boundExpectation(on bool, seed uint64, n, minutes, trials int, sigma float64) float64 {
	r := propRand(seed)
	sum := 0.0
	for range trials {
		reads := make([][]float64, minutes)
		for m := range reads {
			reads[m] = make([]float64, n)
			for p := range n {
				reads[m][p] = 1 + sigma*r.NormFloat64()
			}
		}
		sum += muFigure(boundAt(on)(), reads)
	}
	return sum / float64(trials)
}

// The margin the cases clear by, at one pod (the worst N) and the noise
// measured on E1 (7.5-18.9 %): off reads 15-30 % high, on within 3 %
// (propNDepBias) of the truth after ten minutes, and after a day.
func TestPrefillBoundHeld_TheBiasAtTheMeasuredNoise(t *testing.T) {
	for _, sigma := range []float64{0.10, 0.15, 0.19} {
		for _, minutes := range []int{10, 60, 1440} {
			trials := 400
			if minutes > 100 {
				trials = 200
			}
			on := boundExpectation(true, 1, 1, minutes, trials, sigma)
			off := boundExpectation(false, 1, 1, minutes, trials, sigma)
			t.Logf("sigma %.0f%%, %4d min, N=1: E on %.4f (%+.1f%%), off %.4f (%+.1f%%)",
				100*sigma, minutes, on, 100*(on-1), off, 100*(off-1))
			require.LessOrEqual(t, math.Abs(on-1), propNDepBias, "on, sigma %.2f, %d min", sigma, minutes)
			require.Greater(t, off-1, 0.10, "off, the running max reads high")
		}
	}
}

// A fleet the bound grew, behind a full decode at one load: each decode
// episode's readings are that load shared over the prefill fleet, so they
// fall as it grows, with 10 % noise on each pod's rate. On, the held figure
// lets the fleet grow once, by the lower confidence bound's allowance, and
// holds it; off, the max holds it at the first episode's order. Priced on
// the window's mean alone -- the held figure dropped every minute -- the
// fleet grows on every episode.
func TestPrefillBoundHeld_AFleetTheBoundGrewGrowsOnceUnderNoise(t *testing.T) {
	const (
		load     = 12.0 // req/s decode admits, shared over the prefill fleet
		scaleUp  = 0.85
		episodes = 6
	)
	fleets := func(seed uint64, on, dropHeld bool) []int {
		r := propRand(seed)
		b := boundAt(on)().(*boundMu)
		n := 4
		out := make([]int, 0, episodes)
		for range episodes {
			for range 10 {
				for p := range n {
					b.read(fmt.Sprintf("p%d", p), load/float64(n)*(1+0.10*r.NormFloat64()))
				}
				b.next()
				if dropHeld {
					clear(b.a.prefillBoundHeld)
				}
			}
			want := int(math.Ceil(load / (b.figure() * scaleUp)))
			n = max(n, want)
			out = append(out, n)
		}
		return out
	}
	for seed := uint64(1); seed <= 10; seed++ {
		on, off, mean := fleets(seed, true, false), fleets(seed, false, false), fleets(seed, true, true)
		t.Logf("seed %d: on %v, off %v, mean alone %v", seed, on, off, mean)
		require.LessOrEqual(t, on[episodes-1], on[0]+1, "seed %d: on, the fleet grows at most once past the first episode's order", seed)
		require.Equal(t, off[0], off[episodes-1], "seed %d: off, the max holds the first episode's order", seed)
		require.Greater(t, mean[episodes-1], on[episodes-1]+2, "seed %d: without the held figure the fleet keeps growing", seed)
	}
}

// Over a day of one pod at 15 % noise, the held figure stays under the true
// rate: the k-th lower confidence bound is taken at 1 - alpha/(k(k+1)), so all of
// them stand under it with confidence 1 - alpha. At a fixed confidence the
// max of a day's bounds drifts above it.
func TestPrefillBoundHeld_TheHeldFigureStaysUnderTheTruthOverADay(t *testing.T) {
	above := 0
	for seed := uint64(1); seed <= 10; seed++ {
		r := propRand(seed)
		b := boundAt(true)().(*boundMu)
		for range 1440 {
			b.read("p0", 1+0.15*r.NormFloat64())
			b.next()
		}
		b.a.mu.Lock()
		h := b.a.prefillBoundHeld[prefillBoundKey(propBoundKey)]
		b.a.mu.Unlock()
		t.Logf("seed %d: held %.4f after %d lower confidence bounds", seed, h.tokens/propBoundIleff, h.tests)
		if h.tokens/propBoundIleff > 1 {
			above++
		}
	}
	require.LessOrEqual(t, above, 1, "the held figure stood above the true rate on %d of 10 days", above)
}

// studentTQuantile against the published table.
func TestPrefillBoundHeld_StudentTQuantile(t *testing.T) {
	for _, c := range []struct {
		q    float64
		nu   int
		want float64
	}{
		{0.025, 1, 12.7062}, {0.025, 2, 4.3027}, {0.025, 3, 3.1824}, {0.025, 9, 2.2622},
		{0.005, 9, 3.2498}, {0.001, 4, 7.1732}, {0.05, 1, 6.3138}, {0.05, 5, 2.0150},
	} {
		got := studentTQuantile(c.q, c.nu)
		require.InDelta(t, c.want, got, 1e-3, "t(%g, %d)", c.q, c.nu)
		require.InDelta(t, c.q, studentTTail(got, c.nu), 1e-9, "tail at t(%g, %d)", c.q, c.nu)
	}
	require.Equal(t, 5.0, lowerConfidenceBound([]float64{5, 5, 5}, 0.01), "no spread, no allowance")
	// Student's t on n - 1 degrees of freedom over sqrt(n): sd 1, n 3,
	// t(0.025, 2) = 4.302652729749464. A normal quantile (1.96), or the sd
	// not divided by sqrt(n), fails here.
	require.InDelta(t, 2-4.302652729749464/math.Sqrt(3), lowerConfidenceBound([]float64{1, 2, 3}, 0.025), 1e-9)
}

// The k-th lower confidence bound is taken at 1 - alpha/(k(k+1)), at three and
// six complete samples while the window fills and then once per full window
// of fresh samples. A rising level makes each new one bind, so the held
// figure is each test's value, computed here independently.
func TestPrefillBoundHeld_TheKthBoundIsTakenAtAlphaOverKKPlusOne(t *testing.T) {
	b := boundAt(true)().(*boundMu)
	key := prefillBoundKey(propBoundKey)
	held := func() prefillHeldBound {
		b.a.mu.Lock()
		defer b.a.mu.Unlock()
		return b.a.prefillBoundHeld[key]
	}
	var at []int
	for i := range 40 {
		b.a.mu.Lock()
		var complete []float64
		if ra, ok := b.a.saturatedThroughput[key]; ok {
			complete = ra.Snapshot().Values
		}
		b.a.mu.Unlock()
		prev := held()
		b.read("p0", 1+0.01*float64(i)+0.05*float64(2*(i%2)-1))
		b.next()
		h := held()
		if h.tests == prev.tests {
			require.Equal(t, prev.tokens, h.tokens, "sample %d: no test, no move", i)
			continue
		}
		k := h.tests
		require.Equal(t, prev.tests+1, k, "sample %d", i)
		want := lowerConfidenceBound(complete, prefillBoundAlpha/float64(k*(k+1)))
		require.Greater(t, want, prev.tokens, "sample %d: the ramp must make the test bind", i)
		require.InDelta(t, want, h.tokens, 1e-9, "sample %d: test %d", i, k)
		at = append(at, len(complete))
	}
	require.Equal(t, []int{3, 6, 10, 10, 10}, at, "the windows tested, by complete samples")
}

// The held figure carries past a new window after EpisodeGap, as RaiseLast
// carried the max: a lower first minute of the new window does not price.
func TestPrefillBoundHeld_TheHeldFigureCarriesPastANewWindow(t *testing.T) {
	b := boundAt(true)().(*boundMu)
	for _, v := range []float64{4, 4.01, 3.99, 4} {
		b.read("p0", v)
		b.next()
	}
	b.a.mu.Lock()
	h := b.a.prefillBoundHeld[prefillBoundKey(propBoundKey)].tokens / propBoundIleff
	b.a.mu.Unlock()
	require.Greater(t, h, 3.9, "three minutes at 4 hold about 4")
	b.clock = b.clock.Add(capacity.EpisodeGap)
	b.read("p0", 3)
	b.a.mu.Lock()
	n := b.a.saturatedThroughput[prefillBoundKey(propBoundKey)].Len()
	b.a.mu.Unlock()
	require.Equal(t, 1, n, "the minute after the gap starts a new window")
	require.InDelta(t, h, b.figure(), 1e-9, "the held figure, not the new window's 3")
}

// A window restored from a build that raised each sample to its running max
// holds that max: here 1.3 against a true 1.0. The held figure is not in the
// snapshot, so the restart starts it afresh; tested on the restored samples,
// equal values with no spread, it took 1.3 and kept it for good. Only samples
// taken since the held figure started are tested, so a day at the true rate
// holds the true rate, whatever size of window came back.
func TestPrefillBoundHeld_ARestoredRaisedWindowDoesNotPinTheHeldFigure(t *testing.T) {
	for _, size := range []int{2, 3, 5, 6, 10} {
		b := boundAt(true)().(*boundMu)
		for i := range size {
			b.read("p0", 1.3+1e-7*float64(i%2))
			b.next()
		}
		b.a.mu.Lock()
		clear(b.a.prefillBoundHeld) // the restart: held is not in the snapshot
		b.a.mu.Unlock()
		for i := range 1440 {
			b.read("p0", 1.0+1e-7*float64(i%2)) // not an exact repeat
			b.next()
		}
		b.a.mu.Lock()
		h := b.a.prefillBoundHeld[prefillBoundKey(propBoundKey)].tokens / propBoundIleff
		b.a.mu.Unlock()
		t.Logf("restored %2d samples at 1.3, a day at 1.0: held %.4f, bound %.4f", size, h, b.figure())
		require.InDelta(t, 1.0, h, 1e-3, "restored %d: held", size)
		require.InDelta(t, 1.0, b.figure(), 1e-3, "restored %d: bound", size)
	}
}
