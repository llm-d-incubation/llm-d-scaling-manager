package whatifapi

import (
	"math"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// jointSample is a cycle whose router TPOT histogram puts 85% of 20000
// requests over 300 s under 20 ms and 97% under 40 ms, and, with engine,
// whose decode engines' histogram (half its observations under 10 ms, a
// population the router does not time) puts the router's law at 92% under
// 25 ms above its first edge.
func jointSample(engine bool) LatencySample {
	const window, n = 300.0, 20000.0
	rate := func(edges, f []float64, total float64) []domain.LatencyBucket {
		bs := make([]domain.LatencyBucket, 0, len(edges)+1)
		for i, e := range edges {
			bs = append(bs, domain.LatencyBucket{UpperBound: e, Rate: f[i] * total / window})
		}
		return append(bs, domain.LatencyBucket{UpperBound: math.Inf(1), Rate: total / window})
	}
	l := LatencySample{Router: &domain.RouterLatency{
		TPOTBuckets:   rate([]float64{0.005, 0.01, 0.02, 0.04, 0.06, 0.08}, []float64{0.02, 0.30, 0.85, 0.97, 0.99, 0.995}, n),
		TPOTSumRate:   0.014 * n / window,
		TPOTCountRate: n / window,
	}}
	if engine {
		anchored := func(f float64) float64 { return 0.5 + 0.5*(f-0.3)/0.7 }
		l.EngineTPOT = rate([]float64{0.01, 0.025, 0.05, 0.075, 0.1},
			[]float64{0.5, anchored(0.92), anchored(0.98), anchored(0.993), anchored(0.998)}, 2*n)
	}
	return l
}

// The backtest reads the router's measured TPOT p90 and p95 from every TPOT
// histogram the cycle holds: with the decode engines' 25 ms edge the p90's
// bucket is [20 ms, 25 ms], not the router's 20 ms to 40 ms, and the
// measured figure is the Pareto reading inside it. The p50 and the mean are
// the router's own.
func TestMeasuredOf_TPOTPercentilesReadTheJointBracket(t *testing.T) {
	i50, i90, i95 := slices.Index(backtestStats, 0.5), slices.Index(backtestStats, 0.9), slices.Index(backtestStats, 0.95)
	m := measuredOf(jointSample(true), MeasuredRouter, 300, false)
	require.NotNil(t, m.tpotBucket[i90])
	assert.InDelta(t, 0.020, m.tpotBucket[i90].Lo, 1e-12)
	assert.InDelta(t, 0.025, m.tpotBucket[i90].Hi, 1e-12)
	require.NotNil(t, m.tpot[i90])
	assert.InDelta(t, 0.020*math.Pow(1.25, math.Log(0.15/0.10)/math.Log(0.15/0.08)), *m.tpot[i90], 1e-9)
	require.NotNil(t, m.tpotBucket[i95])
	assert.InDelta(t, 0.025, m.tpotBucket[i95].Lo, 1e-12)
	assert.InDelta(t, 0.040, m.tpotBucket[i95].Hi, 1e-12)

	alone := measuredOf(jointSample(false), MeasuredRouter, 300, false)
	require.NotNil(t, alone.tpotBucket[i90])
	assert.InDelta(t, 0.040, alone.tpotBucket[i90].Hi, 1e-12, "the router alone: its own bucket")

	assert.Equal(t, alone.tpot[i50], m.tpot[i50], "the p50 is the router's own")
	assert.Equal(t, *alone.tpot[0], *m.tpot[0])

	// Scored: a prediction of 30 ms sits in the router's bucket, so it was
	// unresolvable; it is now more than 20% over the joint bracket.
	p := scoredPoint(BacktestPoint{Statistic: "p90", Predicted: Figure{Value: ptr(0.031)}}, m.tpot[i90], m.tpotRequests, m.tpotTop,
		m.tpotWhy[i90], m.tpotBucket[i90])
	assert.Equal(t, OutsideOver, p.OutsideBucket)
	p = scoredPoint(BacktestPoint{Statistic: "p90", Predicted: Figure{Value: ptr(0.031)}}, alone.tpot[i90], alone.tpotRequests,
		alone.tpotTop, alone.tpotWhy[i90], alone.tpotBucket[i90])
	assert.Equal(t, UnscoredUnresolvable, p.Unscored)
}

// The experimental distribution's scorer counts a predicted percentile in,
// under or over the measured one's joint bracket, the planner's own beside
// it, and the steps the engines' histogram joined.
func TestTPOTDistScorer_CountsAgainstTheJointBracket(t *testing.T) {
	l := jointSample(true)
	r := l.Router
	edges, cum := make([]float64, len(r.TPOTBuckets)), make([]float64, len(r.TPOTBuckets))
	for i, b := range r.TPOTBuckets {
		edges[i], cum[i] = b.UpperBound, b.Rate
	}
	st, ok := whatif.TPOTDistStepOf(edges, cum, 300, 0.014, []whatif.DecodeCycle{{Output: map[float64]float64{2: 0, 100: 1}}})
	require.True(t, ok)
	st = withEngineTPOT(st, l.EngineTPOT)

	var sc tpotDistScorer
	// No spread: every percentile is the mean, 30 ms, above the p90's
	// [20, 25] and inside the p95's [25, 40].
	_, p90, p95 := sc.add(whatif.TPOTDist{}, st, 0.030, []*float64{ptr(0.022), ptr(0.050)})
	assert.False(t, p90)
	assert.True(t, p95)
	got := sc.score()
	assert.Equal(t, 1, got.Steps)
	assert.Equal(t, 0, got.P90Inside)
	assert.Equal(t, 1, got.P90Above)
	assert.Equal(t, 1, got.P95Inside)
	assert.Equal(t, 0, got.P95Below+got.P95Above)
	assert.Equal(t, 1, got.EngineJoined)
	assert.Equal(t, 0, got.Conflicts)
	require.NotNil(t, got.DefaultP90Inside)
	assert.Equal(t, 1, *got.DefaultP90Inside, "22 ms is in [20, 25]")
	assert.Equal(t, 0, *got.DefaultP95Inside, "50 ms is over [25, 40]")
}

// Where the joint bracket reads the router's own bucket linearly (the
// router alone, the bucket closing at every request), the measured figure is
// the router's reading itself, not the same figure recomputed from shares
// and rounded differently.
func TestMeasuredOf_TheRoutersOwnReadingIsKeptExactly(t *testing.T) {
	const window = 300.0
	i90 := slices.Index(backtestStats, 0.9)
	for _, n := range []float64{300, 1000, 1234, 4567, 20000} {
		for _, f := range []float64{0.10, 0.25, 0.30, 0.45, 0.60} {
			edges := []float64{0.005, 0.01, 0.02, 0.04, 0.06, 0.08, math.Inf(1)}
			shares := []float64{0, f, 1, 1, 1, 1, 1}
			bs := make([]domain.LatencyBucket, len(edges))
			for i, e := range edges {
				bs[i] = domain.LatencyBucket{UpperBound: e, Rate: shares[i] * n / window}
			}
			l := LatencySample{Router: &domain.RouterLatency{TPOTBuckets: bs, TPOTSumRate: 0.015 * n / window, TPOTCountRate: n / window}}
			h, ok := histOf(bs)
			require.True(t, ok)
			m := measuredOf(l, MeasuredRouter, window, false)
			require.NotNil(t, m.tpot[i90])
			assert.Equal(t, h.Quantile(0.9), *m.tpot[i90], "n %g, F(10 ms) %g", n, f)
		}
	}
}

// The engines' TPOT rates are over one minute, so their requests are the
// rates times 60 s, not times the router's 300 s window: an engine histogram
// of 100 requests a second puts 3000 above its first edge in its minute,
// joins and narrows the p90 to [20 ms, 25 ms]; at 1 a second it puts 30
// there, under MinEngineAboveFirstEdge, and the bracket is the router's
// alone, though counted over 300 s it would have held 150 and joined.
func TestJointTPOT_TheEnginesCountOverTheirOwnMinute(t *testing.T) {
	i90 := slices.Index(backtestStats, 0.9)
	for _, c := range []struct {
		rate   float64
		joined bool
	}{{100, true}, {1, false}} {
		l := jointSample(true)
		f := []float64{0.5, 0.5 + 0.5*(0.92-0.3)/0.7, 0.5 + 0.5*(0.98-0.3)/0.7, 0.5 + 0.5*(0.993-0.3)/0.7, 0.5 + 0.5*(0.998-0.3)/0.7, 1}
		for i := range l.EngineTPOT {
			l.EngineTPOT[i].Rate = f[i] * c.rate
		}
		st := withEngineTPOT(whatif.TPOTDistStep{Edges: []float64{0.005, 0.01, 0.02, 0.04, 0.06, 0.08, math.Inf(1)},
			Counts: []float64{400, 5600, 11000, 2400, 400, 100, 100}}, l.EngineTPOT)
		b, ok := whatif.JointBracket(st, 0.9)
		require.True(t, ok)
		assert.Equal(t, c.joined, b.Engine, "%g engine requests a second", c.rate)
		m := measuredOf(l, MeasuredRouter, 300, false)
		require.NotNil(t, m.tpotBucket[i90])
		if c.joined {
			assert.InDelta(t, 0.025, m.tpotBucket[i90].Hi, 1e-12)
		} else {
			assert.InDelta(t, 0.040, m.tpotBucket[i90].Hi, 1e-12, "the router's own bucket")
		}
	}
}
