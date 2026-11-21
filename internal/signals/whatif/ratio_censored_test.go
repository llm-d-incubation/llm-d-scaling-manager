package whatif

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lognormalWindow is a window of n requests whose TPOT is lognormal with
// mean m and log-spread sigma, binned at edges (the last +Inf), its sum
// the distribution's mean times n.
func lognormalWindow(n, m, sigma float64, edges []float64) TPOTWindow {
	mu := math.Log(m) - sigma*sigma/2
	w := TPOTWindow{Sum: m * n, Count: n, SumMeasured: true}
	for _, e := range edges {
		c := n
		if !math.IsInf(e, 1) {
			c = n * 0.5 * math.Erfc(-(math.Log(e)-mu)/(sigma*math.Sqrt2))
		}
		w.Buckets = append(w.Buckets, HistogramBucket{UpperEdge: e, Count: c})
	}
	return w
}

// trueP90 is a lognormal's 0.9-quantile over its mean.
func trueP90(sigma float64) float64 {
	const q = 0.9
	return math.Exp(sigma*math.Sqrt2*math.Erfinv(2*q-1) - sigma*sigma/2)
}

// The router's TPOT edges (10, 25, 50, 75 ms) around a 15 ms mean with a
// per-request spread of 0.2: the p90's bucket is 10 to 25 ms, and the edge
// read prices R_q at 25/15 = 1.67 where the truth is 1.26 (#279 reads the
// same overshoot as a ratio over the p50, not the mean: predicted p90 over
// p50 1.55 against a measured 1.27 to 1.31). The censored fit
// reads the bucket's mass and lands within 5% of the truth, and never above
// the edge reading.
func TestTPOTRatioCensored_ReadsTheBucketMassNotItsEdge(t *testing.T) {
	edges := []float64{0.010, 0.025, 0.050, 0.075, math.Inf(1)}
	for _, c := range []struct{ mean, sigma float64 }{{0.015, 0.2}, {0.012, 0.15}, {0.022, 0.3}, {0.030, 0.5}} {
		w := lognormalWindow(1000, c.mean, c.sigma, edges)
		ws := []TPOTWindow{w, w, w}
		edge, _, u := TPOTRatioFromWindows(ws, 0.9)
		require.Nil(t, u)
		got, _, u := TPOTRatioCensored(ws, 0.9)
		require.Nil(t, u)
		want := trueP90(c.sigma)
		assert.InEpsilon(t, max(1, want), got.Value, 0.05, "mean %g sigma %g: edge %.3f", c.mean, c.sigma, edge.Value)
		assert.LessOrEqual(t, got.Value, edge.Value, "never above the edge reading")
	}
}

// A refusal of the edge read is the censored read's too: no histogram, or
// a quantile in the +Inf bucket.
func TestTPOTRatioCensored_RefusesAsTheEdgeReadDoes(t *testing.T) {
	_, _, u := TPOTRatioCensored(nil, 0.9)
	require.NotNil(t, u)
	assert.Equal(t, NoTPOTHistogram, u.Reason)
	w := TPOTWindow{Sum: 1, Count: 100, SumMeasured: true, Buckets: []HistogramBucket{{UpperEdge: 0.005, Count: 50}, {UpperEdge: math.Inf(1), Count: 100}}}
	_, _, u = TPOTRatioCensored([]TPOTWindow{w}, 0.9)
	require.NotNil(t, u)
	assert.Equal(t, TPOTQuantileUnbounded, u.Reason)
}

// A window whose every request sits in one bucket does not identify the
// spread: the likelihood rises as sigma falls, and the fit read a ratio of
// about 1 (on the Qwen3-32B shape-A phase, p90 TPOT 22 ms against a measured
// 38). Such a window keeps the edge reading.
func TestTPOTRatioCensored_OneBucketKeepsTheEdgeReading(t *testing.T) {
	w := TPOTWindow{Sum: 1.5, Count: 100, SumMeasured: true,
		Buckets: []HistogramBucket{{UpperEdge: 0.010, Count: 0}, {UpperEdge: 0.020, Count: 100}, {UpperEdge: math.Inf(1), Count: 100}}}
	ws := []TPOTWindow{w, w, w}
	edge, _, u := TPOTRatioFromWindows(ws, 0.9)
	require.Nil(t, u)
	got, _, u := TPOTRatioCensored(ws, 0.9)
	require.Nil(t, u)
	assert.InDelta(t, edge.Value, got.Value, 1e-12)
	assert.Greater(t, got.Value, 1.05)
}

// Pooled, the windows that spill into the next bucket set the spread for
// those that do not: one-bucket windows beside lognormal ones (sigma 0.25 at
// a 15 ms mean on the router's 10, 20, 40 ms edges) read a ratio well above
// 1 and never above the edge reading.
func TestTPOTRatioCensored_PooledWindowsSetTheSpread(t *testing.T) {
	edges := []float64{0.010, 0.020, 0.040, math.Inf(1)}
	one := TPOTWindow{Sum: 1.5, Count: 100, SumMeasured: true,
		Buckets: []HistogramBucket{{UpperEdge: 0.010, Count: 0}, {UpperEdge: 0.020, Count: 100}, {UpperEdge: 0.040, Count: 100}, {UpperEdge: math.Inf(1), Count: 100}}}
	spill := lognormalWindow(100, 0.015, 0.25, edges)
	ws := []TPOTWindow{one, one, one, one, spill, spill, spill}
	edge, _, u := TPOTRatioFromWindows(ws, 0.9)
	require.Nil(t, u)
	got, _, u := TPOTRatioCensored(ws, 0.9)
	require.Nil(t, u)
	assert.Greater(t, got.Value, 1.1)
	assert.Less(t, got.Value, trueP90(0.25)+1e-9)
	assert.LessOrEqual(t, got.Value, edge.Value)
}

// A sliver spilled past an edge, one request in a hundred, does not set the
// spread (review of #279's fit): it is no real mass in a second bucket, and
// pooled with one-bucket windows it read a ratio well under the edge
// reading the counts can support. The edge reading stands.
func TestTPOTRatioCensored_ASliverDoesNotSetTheSpread(t *testing.T) {
	one := TPOTWindow{Sum: 1.5, Count: 100, SumMeasured: true,
		Buckets: []HistogramBucket{{UpperEdge: 0.010, Count: 0}, {UpperEdge: 0.020, Count: 100}, {UpperEdge: 0.040, Count: 100}, {UpperEdge: math.Inf(1), Count: 100}}}
	sliver := one
	sliver.Buckets = []HistogramBucket{{UpperEdge: 0.010, Count: 0}, {UpperEdge: 0.020, Count: 99}, {UpperEdge: 0.040, Count: 100}, {UpperEdge: math.Inf(1), Count: 100}}
	ws := []TPOTWindow{one, one, one, one, sliver}
	edge, _, u := TPOTRatioFromWindows(ws, 0.9)
	require.Nil(t, u)
	got, _, u := TPOTRatioCensored(ws, 0.9)
	require.Nil(t, u)
	assert.InDelta(t, edge.Value, got.Value, 1e-12)
}

// By fill, each bucket's windows are fitted alone and never read above
// that bucket's own edge reading. The 0.4 to 0.6 bucket's windows spread
// (sigma 0.5 at 15 ms) and read near the truth; the 0 to 0.2 bucket's sit
// in one bucket, 25 to 50 ms at a 45 ms mean, an edge reading of 1.11: they
// do not identify sigma, and take the global R_q capped there, not the
// global R_q of 1.5 the spreading windows set.
func TestTPOTRatioCensored_ByFillNeverAboveTheBucketsOwnEdge(t *testing.T) {
	edges := []float64{0.010, 0.025, 0.050, 0.075, math.Inf(1)}
	ws := make([]TPOTWindow, 0, 10)
	for range 5 {
		one := TPOTWindow{Sum: 4.5, Count: 100, SumMeasured: true, Fill: 0.1, FillKnown: true,
			Buckets: []HistogramBucket{{UpperEdge: 0.010, Count: 0}, {UpperEdge: 0.025, Count: 0}, {UpperEdge: 0.050, Count: 100},
				{UpperEdge: 0.075, Count: 100}, {UpperEdge: math.Inf(1), Count: 100}}}
		spread := lognormalWindow(2000, 0.015, 0.5, edges)
		spread.Fill, spread.FillKnown = 0.5, true
		ws = append(ws, one, spread)
	}
	edge, _, u := TPOTRatioFromWindows(ws, 0.9)
	require.Nil(t, u)
	require.Len(t, edge.ByFill, 5)
	got, _, u := TPOTRatioCensored(ws, 0.9)
	require.Nil(t, u)
	require.Len(t, got.ByFill, 5)
	assert.Greater(t, got.Value, 1.3, "the spreading windows set the global R_q")

	low := got.ByFill[0]
	assert.InDelta(t, 0.050/0.045, edge.ByFill[0].Learned, 1e-9)
	assert.LessOrEqual(t, low.Value, edge.ByFill[0].Learned, "the global R_q is capped at the bucket's own edge")
	assert.InDelta(t, low.Value, low.Learned, 1e-12, "Learned is the bucket's own reading")

	mid := got.ByFill[2]
	assert.InEpsilon(t, trueP90(0.5), mid.Learned, 0.05)
	assert.LessOrEqual(t, mid.Learned, edge.ByFill[2].Learned)
	for i, b := range got.ByFill {
		if i > 0 {
			assert.GreaterOrEqual(t, b.Value, got.ByFill[i-1].Value, "never falling as the fill rises")
		}
		if b.Thin {
			assert.GreaterOrEqual(t, b.Value, got.Value, "a thin bucket reads the global R_q, raised")
		}
	}
}

// A window that carries no +Inf bucket still counts every request in
// _count and _sum: the requests past its last edge are the tail, and the
// fit reads them as a cell from that edge up. Dropped, the mean keeps them
// while the cells do not, and the spread reads short (#279 round 2).
func TestTPOTRatioCensored_KeepsTheMassPastTheLastEdge(t *testing.T) {
	w := lognormalWindow(1000, 0.015, 0.4, []float64{0.010, 0.025})
	require.Less(t, w.Buckets[1].Count, w.Count-50, "the fixture holds a tail past 25 ms")
	ws := []TPOTWindow{w, w, w}
	got, _, u := TPOTRatioCensored(ws, 0.9)
	require.Nil(t, u)
	// Dropped, the tail reads sigma 0.35 and R_q 1.47 against the true 1.54.
	assert.InEpsilon(t, trueP90(0.4), got.Value, 0.01)
}
