package prefill

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// labReuseAt2 is the lab run's KV residency histograms on 2 prefill pods
// (hit 0.526), synthesised from the load generator's law: the run had no
// --kv-cache-metrics. Che's model over the law, with the GPU cache's
// 5,980 blocks and the duplication that reproduces the measured hit (k
// 1.22), gives each block's reuse gap; the hits are the gaps under the
// horizon, 69.3 requests, and every eviction idles that long. Gaps are in
// requests a pod: the law is scale-free in time, so seconds at any rate
// read the same hits. Counts are a request's block hits and evictions.
func labReuseAt2() *ReuseHistograms {
	gap := []ReuseBucket{{0.25, 0.407535}, {0.353553, 0.575803}, {0.5, 0.813231}, {0.707107, 1.14793}, {1, 1.61914},
		{1.41421, 2.28128}, {2, 3.20928}, {2.82843, 4.50503}, {4, 6.30482}, {5.65685, 8.78627}, {8, 12.1722},
		{11.3137, 16.7253}, {16, 22.7247}, {22.6274, 30.409}, {32, 39.875}, {45.2548, 50.9325}, {64, 62.965},
		{90.5097, 65.7542}, {128, 65.7542}, {181.019, 65.7542}, {math.Inf(1), 65.7542}}
	idle := []ReuseBucket{{22.6274, 0}, {32, 0}, {45.2548, 0}, {64, 22.4305}, {90.5097, 59.7477}, {128, 59.7477},
		{math.Inf(1), 59.7477}}
	return &ReuseHistograms{Gap: gap, Idle: idle}
}

// The lab's count transfer: the law fitted on 2 pods prices 0.288 at 1 pod
// and 0.816 at 4, where the pods measured 0.345 and 0.792 and the floor and
// the held hit read 0.263 and 0.526. At 1 pod it reads low: one count does
// not show the duplication a second pod adds (k), so it is read as none,
// which prices more computed tokens, the safe side for capacity. The
// figures are release-tools/hitmodel/gomirror.py's, the same fit in Python.
func TestHitModel_TheLabCountTransfer(t *testing.T) {
	m, ok := FitHitModel(HitModelInputs{TotalHit: 0.526, GPUHit: 0.526, FitCount: 2, Reuse: labReuseAt2(), K: 1, A: 1})
	require.True(t, ok)
	assert.True(t, m.CeilingAssumed, "no tier past the GPU: the ceiling is not read")
	assert.Equal(t, 1.0, m.HInf)
	assert.InDelta(t, 69.29, m.HorizonSeconds, 0.01)
	assert.InDelta(t, 0.526, m.F(m.HorizonSeconds), 1e-9, "F(T_C) is the GPU hit")

	want := map[int]float64{1: 0.2880, 2: 0.526, 3: 0.6953, 4: 0.8165, 8: 1.0}
	prev := 0.0
	for _, n := range []int{1, 2, 3, 4, 8} {
		gpu, total, ok := m.HitAt(n, 1)
		require.True(t, ok)
		assert.InDelta(t, want[n], gpu, 0.002, "%d pods", n)
		assert.Equal(t, gpu, total, "no tier: the total is the GPU's")
		assert.Greater(t, gpu, prev-1e-12, "the hit rises with the pods")
		prev = gpu
	}

	// The duplication, where it is known, closes the 1-pod gap: k = 1.22
	// at 2 pods and 1 on one pod read about the measured 0.345.
	m.K = 1.22
	at1, _, _ := m.HitAt(1, 1)
	assert.InDelta(t, 0.345, at1, 0.01)
}

// lawReuse is the residency histograms an LRU with horizon tc reads under
// the law F(g) = hinf Phi((ln g - mu)/sigma): the hit gaps are F's under tc,
// and every eviction idles tc (the idle histogram's median, interpolated).
func lawReuse(hinf, mu, sigma, tc float64) *ReuseHistograms {
	f := func(g float64) float64 { return hinf * normalCDF((math.Log(g)-mu)/sigma) }
	var gap []ReuseBucket
	for e := 0.05; e < 100*tc; e *= 1.2 {
		gap = append(gap, ReuseBucket{UpperEdge: e, Count: f(min(e, tc))})
	}
	gap = append(gap, ReuseBucket{UpperEdge: math.Inf(1), Count: f(tc)})
	idle := []ReuseBucket{{UpperEdge: tc - 10, Count: 0}, {UpperEdge: tc + 10, Count: 1}, {UpperEdge: math.Inf(1), Count: 1}}
	return &ReuseHistograms{Gap: gap, Idle: idle}
}

// The fit reads the law back from the histograms it gives, with the
// ceiling a tier's total hit gives.
func TestFitHitModel_ReadsTheLawBack(t *testing.T) {
	const hinf, mu, sigma, tc = 0.9, 3.0, 1.2, 60.0
	gpu := hinf * normalCDF((math.Log(tc)-mu)/sigma)
	m, ok := FitHitModel(HitModelInputs{TotalHit: hinf, GPUHit: gpu, FitCount: 3, Reuse: lawReuse(hinf, mu, sigma, tc), K: 1, A: 1})
	require.True(t, ok)
	assert.False(t, m.CeilingAssumed)
	assert.True(t, m.TierHit)
	assert.Equal(t, hinf, m.HInf)
	assert.InDelta(t, tc, m.HorizonSeconds, 1e-9)
	assert.InDelta(t, mu, m.Mu, 0.05)
	assert.InDelta(t, sigma, m.Sigma, 0.05)
}

// A tier past the GPU whose size is not known holds the total at its read:
// fewer pods move only the GPU's share of it, the rest going to the tier.
// A known per-pod tier loses hits at fewer pods; a shared one of the same
// size, the fleet's, loses fewer.
func TestHitModel_Tiers(t *testing.T) {
	const hinf, mu, sigma, tc = 0.95, 3.5, 1.3, 40.0
	gpu := hinf * normalCDF((math.Log(tc)-mu)/sigma)
	in := HitModelInputs{TotalHit: hinf, GPUHit: gpu, FitCount: 4, Reuse: lawReuse(hinf, mu, sigma, tc), K: 1, A: 1}
	m, ok := FitHitModel(in)
	require.True(t, ok)
	g2, t2, _ := m.HitAt(2, 1)
	assert.Less(t, g2, gpu-0.05, "the GPU hit falls at half the pods")
	assert.Equal(t, hinf, t2, "an unsized tier takes up the rest")

	in.Tiers = []HitTier{{Ratio: 2}}
	perPod, _ := FitHitModel(in)
	_, p4, _ := perPod.HitAt(4, 1)
	pg2, p2, _ := perPod.HitAt(2, 1)
	assert.Greater(t, p4, gpu, "the tier adds hits at the fit")
	assert.Less(t, p2, p4, "a per-pod tier splits with the pods")
	assert.Greater(t, p2, pg2)

	in.Tiers = []HitTier{{Ratio: 8, Shared: true}}
	shared, _ := FitHitModel(in)
	_, s2, _ := shared.HitAt(2, 1)
	assert.Greater(t, s2, p2, "a shared tier of the per-pod tiers' fleet size keeps more at fewer pods")

	// A shared tier sees the fleet's accesses, one copy of a prefix,
	// whatever the pods and the duplication across them: its hit does not
	// move with the count, on one pod (no duplication) as on many.
	in.Tiers, in.K = []HitTier{{Ratio: 10, Shared: true}}, 2
	dup, ok := FitHitModel(in)
	require.True(t, ok)
	g4, s4, _ := dup.HitAt(4, 1)
	require.Greater(t, s4, g4, "the shared tier serves past the GPU")
	for _, n := range []int{1, 2, 8} {
		g, s, _ := dup.HitAt(n, 1)
		require.Greater(t, s, g, "%d pods", n)
		assert.InDelta(t, s4, s, 1e-9, "%d pods", n)
	}
}

// With a = 1 the gaps move with the rate and the hit does not; with a = 0
// they are fixed in seconds, and more load shortens the horizon.
func TestHitModel_Elasticity(t *testing.T) {
	in := HitModelInputs{TotalHit: 0.526, GPUHit: 0.526, FitCount: 2, Reuse: labReuseAt2(), K: 1, A: 1}
	m, _ := FitHitModel(in)
	a1, _, _ := m.HitAt(2, 2)
	assert.InDelta(t, 0.526, a1, 1e-9)
	in.A = 0
	m0, _ := FitHitModel(in)
	hi, _, _ := m0.HitAt(2, 2)
	lo, _, _ := m0.HitAt(2, 0.5)
	assert.Less(t, hi, 0.526)
	assert.Greater(t, lo, 0.526)
	// Twice the rate on twice the pods is the fit's load a pod.
	same, _, _ := m0.HitAt(4, 2)
	assert.InDelta(t, 0.526, same, 1e-6)
}

func TestFitHitModel_RefusesWhatDoesNotCloseIt(t *testing.T) {
	ok := HitModelInputs{TotalHit: 0.526, GPUHit: 0.526, FitCount: 2, Reuse: labReuseAt2(), K: 1, A: 1}
	_, fitted := FitHitModel(ok)
	require.True(t, fitted, "the control fits")
	for name, edit := range map[string]func(in *HitModelInputs){
		"no histograms":           func(in *HitModelInputs) { in.Reuse = nil },
		"no gap histogram":        func(in *HitModelInputs) { in.Reuse.Gap = nil },
		"no idle histogram":       func(in *HitModelInputs) { in.Reuse.Idle = nil },
		"no hit counted":          func(in *HitModelInputs) { in.Reuse.Gap = []ReuseBucket{{10, 0}, {math.Inf(1), 0}} },
		"counts falling":          func(in *HitModelInputs) { in.Reuse.Gap[3].Count = 0 },
		"a NaN count":             func(in *HitModelInputs) { in.Reuse.Idle[4].Count = math.NaN() },
		"gaps past the horizon":   func(in *HitModelInputs) { in.Reuse.Idle = []ReuseBucket{{20, 0}, {25, 1}, {math.Inf(1), 1}} },
		"no GPU hit":              func(in *HitModelInputs) { in.GPUHit = 0 },
		"a total under the GPU's": func(in *HitModelInputs) { in.TotalHit = 0.5 },
		"a total of 1":            func(in *HitModelInputs) { in.TotalHit, in.GPUHit = 1, 0.9 },
		"no pods":                 func(in *HitModelInputs) { in.FitCount = 0 },
	} {
		in := ok
		in.Reuse = labReuseAt2()
		edit(&in)
		_, fitted := FitHitModel(in)
		assert.False(t, fitted, name)
	}
}

// HitAtCountOf prices with the law where it is given, at fewer pods and at
// more, within the bracket; with none, the floor and the held hit, as
// before.
func TestHitAtCountOf_WithTheLaw(t *testing.T) {
	m, ok := FitHitModel(HitModelInputs{TotalHit: 0.526, GPUHit: 0.526, FitCount: 2, Reuse: labReuseAt2(), K: 1, A: 1})
	require.True(t, ok)
	h := 1 - labComputedAt2/labWhole

	down, ok := HitAtCountOf(h, labComputedAt2, 2, 1, &m)
	require.True(t, ok)
	assert.Equal(t, HitReuseGap, down.Rule)
	// h, 0.528, is over the law's 0.526: it moves by the computed share's
	// ratio.
	assert.InDelta(t, 1-(1-h)*(1-0.288)/(1-0.526), down.Hit, 0.002)
	assert.InDelta(t, 0.288/0.526, down.GPUScale, 0.01)
	assert.Equal(t, 1.0, down.TierScale, "no tier: nothing to move")
	assert.Equal(t, h/2, down.Lo, "the bracket keeps the floor")
	assert.InDelta(t, labComputedAt2*(1-down.Hit)/(1-h), down.Computed, 1e-6)
	// About 11,310 computed tokens a request against the 10,454 measured;
	// the floor read 11,750 and the held hit 7,532.
	assert.InDelta(t, 11310, down.Computed, 60)

	up, ok := HitAtCountOf(h, labComputedAt2, 2, 4, &m)
	require.True(t, ok)
	assert.Equal(t, HitReuseGap, up.Rule)
	// The law reads 0.8165 at 4 pods; with its ceiling assumed the miss
	// falls at most by half, to 0.236 (0.208 measured).
	assert.InDelta(t, 1-(1-h)/2, up.Hit, 1e-9)
	assert.InDelta(t, labComputedAt2/2, up.Computed, 1e-6, "against the 3,205 measured; the held hit read 7,532")

	// Negative control: no law, the rule as before.
	floor, _ := HitAtCountOf(h, labComputedAt2, 2, 1, nil)
	held, _ := HitAtCountOf(h, labComputedAt2, 2, 4, nil)
	assert.Equal(t, HitFloor, floor.Rule)
	assert.Equal(t, HitHeld, held.Rule)
	assert.Equal(t, 1.0, floor.GPUScale)

	// A hit under the law's moves by its hit ratio.
	low, ok := HitAtCountOf(0.3, 5000, 2, 3, &m)
	require.True(t, ok)
	assert.Equal(t, HitReuseGap, low.Rule)
	assert.InDelta(t, 0.3*0.6953/0.526, low.Hit, 0.003)
	// With the ceiling assumed the law reads 1 at 8 pods; the miss is held
	// to fall no faster than the pods rise.
	high, ok := HitAtCountOf(0.7, 5000, 2, 8, &m)
	require.True(t, ok)
	assert.InDelta(t, 1-0.3/4, high.Hi, 1e-12)
	assert.InDelta(t, high.Hi, high.Hit, 1e-12)
	// A hit above the law's moves by its computed share's ratio.
	above, ok := HitAtCountOf(0.6, 5000, 2, 1, &m)
	require.True(t, ok)
	assert.InDelta(t, 1-0.4*(1-0.288)/(1-0.526), above.Hit, 0.003)

	// At h = 0 nothing moves, law or not.
	zero, ok := HitAtCountOf(0, 5000, 2, 1, &m)
	require.True(t, ok)
	assert.Equal(t, 0.0, zero.Hit)
}

// K from the affinity the router gave up: 1 at none and on one pod, the
// count at an even spread, and the lab's figures (2 pods, 6.65% given up
// on the fit arm: 1.142 (1.1418); 4 pods, 0.46%: 1.009). A loss not in [0, 1) reads 1.
func TestDuplicationOfAffinityLoss(t *testing.T) {
	assert.Equal(t, 1.0, DuplicationOfAffinityLoss(0, 2))
	assert.Equal(t, 1.0, DuplicationOfAffinityLoss(0.3, 1), "one pod holds every copy")
	assert.InDelta(t, 2, DuplicationOfAffinityLoss(0.5, 2), 1e-12)
	assert.InDelta(t, 4, DuplicationOfAffinityLoss(0.75, 4), 1e-12)
	assert.InDelta(t, 1.1418, DuplicationOfAffinityLoss(0.0665, 2), 1e-4)
	assert.InDelta(t, 1.0093, DuplicationOfAffinityLoss(0.0046, 4), 1e-4)
	for _, bad := range []float64{-0.1, 1, math.NaN(), math.Inf(1)} {
		assert.Equal(t, 1.0, DuplicationOfAffinityLoss(bad, 3), "%v", bad)
	}
}
