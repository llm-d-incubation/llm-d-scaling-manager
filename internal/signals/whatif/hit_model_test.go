package whatif

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// At a lower hit the histogram is a miss mixture: its mean is the target,
// its total computed's, and it lies between the computed and the prompt
// draws at every share. A target at or under the mean leaves it as it is;
// one at or over the prompt's mean is the prompt's shape.
func TestComputedMissMixture(t *testing.T) {
	c, p := labComputed(), labPrompt()
	ec, _ := CumulativeMean(c)
	ep, _ := CumulativeMean(p)
	target := ec + 0.3*(ep-ec)
	mix := ComputedMissMixture(c, p, target)
	m, ok := CumulativeMean(mix)
	require.True(t, ok)
	assert.InDelta(t, target, m, 1e-6*target)
	assert.Equal(t, 100.0, mix[math.Inf(1)])
	hm, _ := newCumHist(mix)
	hc, _ := newCumHist(c)
	hp, _ := newCumHist(p)
	for _, u := range []float64{0.1, 0.3, 0.5, 0.7, 0.9} {
		assert.GreaterOrEqual(t, hm.ppf(u), hc.ppf(u)*(1-1e-9), "share %g", u)
		assert.LessOrEqual(t, hm.ppf(u), hp.ppf(u)*(1+1e-9), "share %g", u)
	}
	// Not bound scaling: the requests that still hit keep what they
	// computed, so the low quantiles stay where they were.
	scaled := ComputedAtScale(c, p, target/ec)
	hs, _ := newCumHist(scaled)
	assert.Less(t, hm.ppf(0.1), hs.ppf(0.1))

	assert.Equal(t, c, ComputedMissMixture(c, p, ec), "no rise")
	assert.Equal(t, c, ComputedMissMixture(c, p, ec/2), "a fall is not a miss mixture")
	assert.Equal(t, c, ComputedMissMixture(c, nil, target), "no prompt histogram")
	assert.Equal(t, c, ComputedMissMixture(c, p, math.NaN()))
	all := ComputedMissMixture(c, p, 2*ep)
	ma, _ := CumulativeMean(all)
	assert.InDelta(t, ep, ma, 1e-6*ep, "every request misses")
}

// The one move every count is priced through: the floor's hit at fewer
// pods by a miss mixture, the reuse-gap law's at more by scaling the
// bounds, and nothing at the fit's count or with the hit held.
func TestComputedAtCount(t *testing.T) {
	c, p := labComputed(), labPrompt()
	ec, _ := CumulativeMean(c)
	down, moved, ok := ComputedAtCount(c, p, 2, 1, nil)
	require.True(t, ok)
	assert.Equal(t, prefill.HitFloor, down.Rule)
	got, _ := CumulativeMean(moved)
	assert.InDelta(t, ec*down.ComputedScale, got, 1e-6*got)
	assert.Equal(t, ComputedMissMixture(c, p, down.Computed), moved)

	held, same, ok := ComputedAtCount(c, p, 2, 4, nil)
	require.True(t, ok)
	assert.Equal(t, prefill.HitHeld, held.Rule)
	assert.Equal(t, c, same)

	m := &prefill.HitModel{HInf: 1, Mu: 4.143, Sigma: 1.454, HorizonSeconds: 69.29, GapMedianSeconds: 25.07, FitCount: 2,
		TotalHit: 0.526, GPUHit: 0.526, CeilingAssumed: true, K: 1, A: 1}
	up, scaled, ok := ComputedAtCount(c, p, 2, 4, m)
	require.True(t, ok)
	assert.Equal(t, prefill.HitReuseGap, up.Rule)
	assert.Greater(t, up.Hit, up.FitHit)
	assert.Equal(t, ComputedAtScale(c, p, up.ComputedScale), scaled)

	_, none, ok := ComputedAtCount(c, map[float64]float64{100: 0, math.Inf(1): 0}, 2, 1, nil)
	assert.False(t, ok, "no prompt read")
	assert.Equal(t, c, none)
}

// residencyRow is a serving prefill row that read its KV residency
// histograms: hit gaps spread up to 60 s, evictions idle about 70 s.
func residencyRow() domain.ReplicaMetrics {
	rm := computedRow(5000, 1, 10000)
	rm.KVReuseGapBuckets = []domain.LatencyBucket{{UpperBound: 10, Rate: 30}, {UpperBound: 30, Rate: 70},
		{UpperBound: 60, Rate: 100}, {UpperBound: 120, Rate: 100}, {UpperBound: math.Inf(1), Rate: 100}}
	rm.KVIdleBeforeEvictBuckets = []domain.LatencyBucket{{UpperBound: 60, Rate: 0}, {UpperBound: 80, Rate: 90},
		{UpperBound: math.Inf(1), Rate: 90}}
	return rm
}

// The window fits the law from the rows' residency histograms summed over
// its cycles, at its computed-token hit, the GPU's where no tier split was
// read. Without the histograms, or with no whole prompt read, it fits
// none: the rule falls back to the floor and the held hit.
func TestPromptWindow_HitModel(t *testing.T) {
	w := &PromptWindow{}
	for i := range 3 {
		w.Note([]domain.ReplicaMetrics{residencyRow(), residencyRow()}, i, time.Unix(int64(15*i), 0))
	}
	m := w.HitModel(2)
	require.NotNil(t, m)
	assert.InDelta(t, 0.5, m.TotalHit, 1e-9)
	assert.Equal(t, m.TotalHit, m.GPUHit, "no tier split read")
	assert.Equal(t, 2, m.FitCount)
	assert.InDelta(t, 70, m.HorizonSeconds, 1e-9, "the idle median")
	assert.InDelta(t, 20, m.GapMedianSeconds, 1e-9)

	bare := &PromptWindow{}
	row := residencyRow()
	row.KVReuseGapBuckets, row.KVIdleBeforeEvictBuckets = nil, nil
	bare.Note([]domain.ReplicaMetrics{row}, 0, time.Unix(0, 0))
	assert.Nil(t, bare.HitModel(2), "no histograms")
	assert.Nil(t, (*PromptWindow)(nil).HitModel(2))

	// A tier past the GPU read: its total is the ceiling, the device share
	// the GPU's hit.
	tiered := &PromptWindow{}
	tr := tierRow(1, 10000, 3000, map[string]float64{SourceDevice: 5000, SourceHost: 2000})
	tr.KVReuseGapBuckets, tr.KVIdleBeforeEvictBuckets = residencyRow().KVReuseGapBuckets, residencyRow().KVIdleBeforeEvictBuckets
	tiered.Note([]domain.ReplicaMetrics{tr}, 0, time.Unix(0, 0))
	mt := tiered.HitModel(1)
	require.NotNil(t, mt)
	assert.InDelta(t, 0.7, mt.TotalHit, 1e-9)
	assert.InDelta(t, 0.5, mt.GPUHit, 1e-9)
	assert.True(t, mt.TierHit)
	assert.Equal(t, 0.7, mt.HInf)
}

// The duplication K is read from the affinity the router gave up over the
// window's cycles: the lab's 2-pod fit gave up 6.65% of the cached tokens
// it could have hit, which reads K 1.142, and the law then prices more hits
// at one pod than with no duplication. A pair that is not one reading, a
// cycle the window did not keep and no router figures leave K at 1.
func TestPromptWindow_HitModelReadsTheDuplication(t *testing.T) {
	window := func(rl func(i int) *domain.RouterLatency) *PromptWindow {
		w := &PromptWindow{}
		for i := range 3 {
			w.Note([]domain.ReplicaMetrics{residencyRow(), residencyRow()}, i, time.Unix(int64(15*i), 0))
			w.NoteRouterPrefix(i, rl(i))
		}
		return w
	}
	lab := window(func(int) *domain.RouterLatency {
		return &domain.RouterLatency{PrefixPredictedTokenRate: 9335, PrefixBestAvailableTokenRate: 10000}
	})
	loss, ok := lab.AffinityLoss()
	require.True(t, ok)
	assert.InDelta(t, 0.0665, loss, 1e-9)
	m := lab.HitModel(2)
	require.NotNil(t, m)
	assert.InDelta(t, 1.1418, m.K, 1e-4)

	none := window(func(int) *domain.RouterLatency { return nil })
	_, ok = none.AffinityLoss()
	assert.False(t, ok)
	m1 := none.HitModel(2)
	require.NotNil(t, m1)
	assert.Equal(t, 1.0, m1.K)
	g, _, ok := m.HitAt(1, 1)
	require.True(t, ok)
	g1, _, ok := m1.HitAt(1, 1)
	require.True(t, ok)
	assert.Greater(t, g, g1, "a duplicated prefix is read on fewer pods' caches: one pod keeps more of it")

	bad := window(func(i int) *domain.RouterLatency {
		switch i {
		case 0:
			return &domain.RouterLatency{PrefixPredictedTokenRate: 10, PrefixBestAvailableTokenRate: 5}
		case 1:
			return &domain.RouterLatency{PrefixPredictedTokenRate: math.NaN(), PrefixBestAvailableTokenRate: 5}
		}
		return &domain.RouterLatency{PrefixBestAvailableTokenRate: 5}
	})
	_, ok = bad.AffinityLoss()
	assert.False(t, ok, "best under predicted, NaN, none predicted")
	stale := window(func(int) *domain.RouterLatency { return nil })
	stale.NoteRouterPrefix(7, &domain.RouterLatency{PrefixPredictedTokenRate: 1, PrefixBestAvailableTokenRate: 2})
	_, ok = stale.AffinityLoss()
	assert.False(t, ok, "a cycle the window did not keep")
	(*PromptWindow)(nil).NoteRouterPrefix(0, &domain.RouterLatency{})
}

// A bound at or under 0, a NaN rate or an empty histogram is not read.
func TestReuseHistogramsOf(t *testing.T) {
	r := ReuseHistogramsOf(map[float64]float64{0: 1, 10: 5, math.Inf(1): 5, 20: math.NaN()}, map[float64]float64{50: 3})
	require.NotNil(t, r)
	assert.Equal(t, []prefill.ReuseBucket{{UpperEdge: 10, Count: 5}, {UpperEdge: math.Inf(1), Count: 5}}, r.Gap)
	assert.Nil(t, ReuseHistogramsOf(nil, map[float64]float64{50: 3}))
}
