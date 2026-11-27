package whatifapi

import (
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// bkRouter is a router window of rate r: TTFT buckets ttft (cumulative
// shares at each edge) around mean ttftMean, TPOT buckets tpot around
// tpotMean.
func bkRouter(r float64, ttft [][2]float64, ttftMean float64, tpot [][2]float64, tpotMean float64) *domain.RouterLatency {
	hist := func(xs [][2]float64) []domain.LatencyBucket {
		b := make([]domain.LatencyBucket, 0, len(xs)+1)
		for _, x := range xs {
			b = append(b, domain.LatencyBucket{UpperBound: x[0], Rate: x[1] * r})
		}
		return append(b, domain.LatencyBucket{UpperBound: math.Inf(1), Rate: r})
	}
	return &domain.RouterLatency{WindowSeconds: 300, TTFTBuckets: hist(ttft), TTFTSumRate: ttftMean * r, TTFTCountRate: r,
		TPOTBuckets: hist(tpot), TPOTSumRate: tpotMean * r, TPOTCountRate: r}
}

// statIndex is q's index in backtestStats.
func statIndex(t *testing.T, q float64) int {
	t.Helper()
	for i, x := range backtestStats {
		if x == q {
			return i
		}
	}
	t.Fatalf("no statistic %g", q)
	return -1
}

// A measured percentile is placed only where the bucket it fell in is no
// wider than 20% of its lower edge (#291). The 0.6B staircase's B phase:
// TTFT buckets 0.2 to 0.4 s, TPOT 5 to 10 ms. Interpolated, the p90 reads
// the grid; with the prediction inside the bucket it is shown, with its
// bucket, and not scored. Where the buckets are fine it is scored.
func TestBacktest_APercentileInACoarseBucketIsUnresolvable(t *testing.T) {
	p90 := statIndex(t, 0.9)
	coarse := measuredOf(LatencySample{Router: bkRouter(10, [][2]float64{{0.2, 0.7}, {0.4, 1}}, 0.16,
		[][2]float64{{0.005, 0.3}, {0.01, 1}}, 0.0075)}, MeasuredRouter, 300, true)
	require.NotNil(t, coarse.ttftBucket[p90], "TTFT p90 in 0.2 to 0.4 s")
	assert.Equal(t, Band{Lo: 0.2, Hi: 0.4}, *coarse.ttftBucket[p90])
	require.NotNil(t, coarse.tpotBucket[p90], "TPOT p90 in 5 to 10 ms")
	assert.Equal(t, Band{Lo: 0.005, Hi: 0.01}, *coarse.tpotBucket[p90])
	assert.Nil(t, coarse.ttftBucket[statIndex(t, 0)], "the mean is no bucket's")

	pred := BacktestPoint{Statistic: statP90, Predicted: Figure{Value: ptr(0.3)}}
	p := scoredPoint(pred, coarse.ttft[p90], coarse.requests, coarse.ttftTop, coarse.ttftWhy[p90], coarse.ttftBucket[p90])
	assert.Equal(t, UnscoredUnresolvable, p.Unscored)
	assert.Nil(t, p.Ratio, "not scored")
	assert.Empty(t, p.OutsideBucket)
	require.NotNil(t, p.Measured, "the interpolated figure is still shown")
	assert.InDelta(t, 0.2+0.2*(0.9-0.7)/0.3, *p.Measured, 1e-12)
	assert.Equal(t, &Band{Lo: 0.2, Hi: 0.4}, p.MeasuredBucket)

	// Buckets 1.09x and 1.2x their lower edge place the p90: scored.
	fine := measuredOf(LatencySample{Router: bkRouter(10, [][2]float64{{0.1, 0.1}, {0.11, 0.5}, {0.12, 0.95}, {0.2, 1}}, 0.115,
		[][2]float64{{0.010, 0.5}, {0.012, 0.95}, {0.05, 1}}, 0.011)}, MeasuredRouter, 300, true)
	assert.Nil(t, fine.ttftBucket[p90])
	assert.Nil(t, fine.tpotBucket[p90], "10 to 12 ms is 1.2x, on the edge: placed")
	p = scoredPoint(pred, fine.ttft[p90], fine.requests, fine.ttftTop, fine.ttftWhy[p90], fine.ttftBucket[p90])
	assert.Empty(t, p.Unscored)
	require.NotNil(t, p.Ratio)
	assert.Nil(t, p.MeasuredBucket)

	// Above the top bucket is a miss, scored as before, not unresolvable.
	top := measuredOf(LatencySample{Router: bkRouter(10, [][2]float64{{0.2, 0.5}, {0.4, 0.8}}, 0.3,
		[][2]float64{{0.01, 1}}, 0.005)}, MeasuredRouter, 300, true)
	assert.Nil(t, top.ttftBucket[p90])
	p = scoredPoint(pred, top.ttft[p90], top.requests, top.ttftTop, top.ttftWhy[p90], top.ttftBucket[p90])
	assert.True(t, p.AboveHistogram)
}

// A bucket is judged against its own lower edge, not the mean: a
// percentile below the mean sits in a bucket the mean says nothing about.
// TPOT 5 to 10 ms is 2x its edge, and 17% of a 30 ms mean: the p50 in it
// is not placed. The first bucket, from 0, never places one.
func TestBacktest_ABucketIsJudgedOnItsOwnEdge(t *testing.T) {
	m := measuredOf(LatencySample{Router: bkRouter(10, [][2]float64{{0.2, 0.5}, {0.22, 1}}, 0.2,
		[][2]float64{{0.005, 0.3}, {0.01, 0.6}, {0.012, 0.7}, {0.1, 1}}, 0.03)}, MeasuredRouter, 300, true)
	p50 := statIndex(t, 0.5)
	require.NotNil(t, m.tpotBucket[p50], "a percentile below the mean in a 2x bucket")
	assert.Equal(t, Band{Lo: 0.005, Hi: 0.01}, *m.tpotBucket[p50])

	h, ok := histOf([]domain.LatencyBucket{{UpperBound: 0.010, Rate: 0.5}, {UpperBound: 0.012, Rate: 0.95},
		{UpperBound: 0.025, Rate: 1}, {UpperBound: math.Inf(1), Rate: 1}})
	require.True(t, ok)
	assert.Nil(t, h.coarseBucket(0.9), "10 to 12 ms: 1.2x")
	assert.Equal(t, &Band{Lo: 0.012, Hi: 0.025}, h.coarseBucket(0.97), "12 to 25 ms: 2.1x")
	assert.Equal(t, &Band{Lo: 0, Hi: 0.010}, h.coarseBucket(0.3), "a first bucket from 0 never places one")
}

// A prediction more than 20% beyond the nearer edge of the coarse bucket
// the measured percentile fell in is a certain miss: scored against that
// edge, never within, and read low under the bucket. The split recording's
// p90 TTFT predicted at 5.09 s against a bucket of 15 to 20 s is such a
// miss, not an unknown. Nearer the bucket it could be within: unresolvable.
func TestBacktest_APredictionOutsideACoarseBucketIsScoredAtItsEdge(t *testing.T) {
	p90 := statIndex(t, 0.9)
	m := measuredOf(LatencySample{Router: bkRouter(10, [][2]float64{{15, 0.7}, {20, 1}}, 11.4,
		[][2]float64{{0.01, 1}}, 0.005)}, MeasuredRouter, 300, true)
	require.NotNil(t, m.ttftBucket[p90])
	at := func(v float64) BacktestPoint {
		return scoredPoint(BacktestPoint{Statistic: statP90, Predicted: Figure{Value: ptr(v)}}, m.ttft[p90], m.requests,
			m.ttftTop, m.ttftWhy[p90], m.ttftBucket[p90])
	}
	under := at(5.09)
	require.NotNil(t, under.Ratio)
	assert.Equal(t, OutsideUnder, under.OutsideBucket)
	assert.InDelta(t, 5.09/15, *under.Ratio, 1e-12)
	assert.Equal(t, &Band{Lo: 15, Hi: 20}, under.MeasuredBucket)
	over := at(30)
	require.NotNil(t, over.Ratio)
	assert.Equal(t, OutsideOver, over.OutsideBucket)
	assert.InDelta(t, 1.5, *over.Ratio, 1e-12)
	for _, v := range []float64{12.5, 14.5, 15, 17, 20, 24} {
		p := at(v)
		assert.Equal(t, UnscoredUnresolvable, p.Unscored, "%g s could be within 20%% of a truth in 15 to 20 s", v)
		assert.Empty(t, p.OutsideBucket, "%g s", v)
		assert.Nil(t, p.Ratio, "%g s", v)
	}
	assert.Equal(t, OutsideUnder, at(11.9).OutsideBucket, "11.9 s is under 0.8 x 15 s")

	steps := []BacktestStep{{TTFT: []BacktestPoint{{}, {}, under, {}}}, {TTFT: []BacktestPoint{{}, {}, over, {}}},
		{TTFT: []BacktestPoint{{}, {}, at(17), {}}}}
	ttft, _ := scoresOf(steps)
	sc := ttft[p90]
	assert.Equal(t, 2, sc.All.Steps)
	assert.Equal(t, 1, sc.Unscored[UnscoredUnresolvable])
	assert.InDelta(t, 0.0, *sc.All.Within, 1e-12, "a certain miss is never within")
	assert.Equal(t, 1, sc.All.Low, "5.09 s is read low by its edge; 30 s reads high")
}

// A measured percentile beside a refusal keeps its bucket, and the range a
// saturated fleet served at is the bucket, not the figure the grid set.
func TestBacktest_ARefusedStepKeepsTheMeasuredBucket(t *testing.T) {
	p90 := statIndex(t, 0.9)
	m := measuredOf(LatencySample{Router: bkRouter(10, [][2]float64{{0.2, 0.7}, {0.4, 1}}, 0.16,
		[][2]float64{{0.01, 1}}, 0.005)}, MeasuredRouter, 300, true)
	sat := BacktestPoint{Statistic: statP90, Predicted: Figure{Refused: &Refusal{Reason: string(whatif.Saturated)}}}
	p := scoredPoint(sat, m.ttft[p90], m.requests, m.ttftTop, m.ttftWhy[p90], m.ttftBucket[p90])
	assert.Equal(t, UnscoredRefused, p.Unscored)
	assert.Equal(t, &Band{Lo: 0.2, Hi: 0.4}, p.MeasuredBucket)
	ttft, _ := scoresOf([]BacktestStep{{TTFT: []BacktestPoint{{}, {}, p, {}}}})
	require.NotNil(t, ttft[p90].PastKnee)
	assert.Equal(t, Band{Lo: 0.2, Hi: 0.4}, *ttft[p90].PastKnee)
}

// The summary counts an unresolvable percentile apart: out of the steps
// scored, the median ratio and within 20%.
func TestBacktest_UnresolvableIsOutOfTheScore(t *testing.T) {
	pt := func(ratio float64, unscored string) BacktestPoint {
		p := BacktestPoint{Statistic: statP90, Predicted: Figure{Value: ptr(ratio)}, Measured: ptr(1.0), Unscored: unscored}
		if unscored == "" {
			p.Ratio = ptr(ratio)
		} else {
			p.MeasuredBucket = &Band{Lo: 0.5, Hi: 2}
		}
		return p
	}
	steps := []BacktestStep{
		{TPOT: []BacktestPoint{{}, {}, pt(1.0, ""), {}}},
		{TPOT: []BacktestPoint{{}, {}, pt(1.1, ""), {}}},
		{TPOT: []BacktestPoint{{}, {}, pt(0.5, UnscoredUnresolvable), {}}},
		{TPOT: []BacktestPoint{{}, {}, pt(0.4, UnscoredUnresolvable), {}}},
	}
	_, tpot := scoresOf(steps)
	sc := tpot[statIndex(t, 0.9)]
	assert.Equal(t, 2, sc.All.Steps)
	assert.Equal(t, 2, sc.Unscored[UnscoredUnresolvable])
	assert.InDelta(t, 1.05, *sc.All.Bias, 1e-12)
	assert.InDelta(t, 1.0, *sc.All.Within, 1e-12)
	assert.Zero(t, sc.All.Low, "the steps a coarse bucket read low are not counted low")
}

// An upper bound is judged against a coarse bucket's edges: covered at or
// over its upper edge, not at or under its lower, unknown between.
func TestBacktest_CoverageInACoarseBucketIsByItsEdges(t *testing.T) {
	b := &Band{Lo: 0.2, Hi: 0.4}
	for bound, want := range map[float64][2]bool{0.5: {true, true}, 0.4: {true, true}, 0.2: {false, true}, 0.3: {false, false}} {
		c, known := coveredBy(BacktestPoint{Bound: ptr(bound), Measured: ptr(0.35), MeasuredBucket: b})
		assert.Equal(t, want, [2]bool{c, known}, "bound %g", bound)
	}
}

// scoresAt is a figure's scores: the mean scored in meanSteps steps with
// median ratio meanBias, the p90 in p90Steps with p90Bias and unresolvable
// in the rest of n.
func scoresAt(meanSteps int, meanBias float64, p90Steps int, p90Bias float64, n int) []BacktestScore {
	score := func(stat string, steps int, bias float64) BacktestScore {
		s := BacktestScore{Statistic: stat, Unscored: map[string]int{}, Refused: map[string]int{}, Outside: map[string]int{}}
		s.All.Steps = steps
		if steps > 0 {
			s.All.Bias, s.All.Within = ptr(bias), ptr(1.0)
		}
		return s
	}
	out := []BacktestScore{score(statisticMean, meanSteps, meanBias), score("p50", 0, 0), score(statP90, p90Steps, p90Bias), score("p95", 0, 0)}
	out[2].Unscored[UnscoredUnresolvable] = n - p90Steps
	return out
}

// The verdict and the self-check quote the mean, which no bucket sets, and
// the verdict says beside it how many p90s a bucket could not place.
func TestBacktest_TheMeanIsTheQuotedFigure(t *testing.T) {
	scores := scoresAt(10, 0.92, 0, 0, 10)
	line, quoted := verdictPart("TPOT", scores, "five-minute steps")
	require.NotNil(t, quoted)
	assert.Equal(t, statisticMean, quoted.Statistic)
	assert.True(t, strings.HasPrefix(line, "mean TPOT: within 20% of measured in 10 of the 10 five-minute steps scored"), line)
	assert.Contains(t, line, "p90 TPOT not scored in 10: the measured p90 fell in a histogram bucket wider than 20% of its lower edge")

	assert.True(t, strings.HasSuffix(line, "too wide to place it, with the prediction inside it or within 20% of its edge. "+TPOTBucketHint), line)
	assert.Contains(t, TPOTBucketHint, "metrics.requestTPOTBuckets")
	assert.Contains(t, TPOTBucketHint, "--custom-histogram-buckets")
	line, _ = verdictPart("TPOT", scoresAt(10, 0.92, 10, 0.9, 10), "five-minute steps")
	assert.NotContains(t, line, TPOTBucketHint, "every p90 placed: no hint")

	line, _ = verdictPart("TTFT", scoresAt(10, 1.02, 4, 0.85, 10), "five-minute steps")
	assert.Contains(t, line, "p90 TTFT: within 20% in 4 of 4, median ratio 0.85.")
	assert.Contains(t, line, "p90 TTFT not scored in 6")
	assert.NotContains(t, line, TPOTBucketHint, "the hint names the TPOT buckets")

	// With no mean scored the p90 leads, as before.
	_, quoted = verdictPart("TPOT", scoresAt(0, 0, 8, 1.1, 8), "five-minute steps")
	require.NotNil(t, quoted)
	assert.Equal(t, statP90, quoted.Statistic)

	f := selfCheckFigure(scoresAt(10, 0.92, 0, 0, 10), 10)
	require.NotNil(t, f)
	assert.Equal(t, statisticMean, f.Statistic)
	assert.Equal(t, 10, f.Steps)
	assert.InDelta(t, 0.92, *f.Bias, 1e-12)
	f = selfCheckFigure(scoresAt(0, 0, 0, 0, 10), 10)
	require.NotNil(t, f, "nothing scored: the p90's refusals still count")
	assert.Equal(t, statP90, f.Statistic)
	assert.Zero(t, f.Steps)
}

// A TTFT target is judged against a coarse bucket's edges: a histogram
// bucket counts requests over its lower edge up to its upper, so a target
// at the lower edge is missed, at the upper met, and inside unknown.
func TestBacktest_ATargetIsJudgedOnACoarseBucketsEdges(t *testing.T) {
	m := measuredOf(LatencySample{Router: bkRouter(1000, [][2]float64{{0.2, 0.7}, {0.4, 1}}, 0.16,
		[][2]float64{{0.01, 1}}, 0.005)}, MeasuredRouter, 300, true)
	points := []BacktestPoint{{}, {}, {Predicted: Figure{Value: ptr(0.25)}}, {}}
	for target, want := range map[float64]string{0.3: judgedUnknown, 0.15: judgedMissed, 0.2: judgedMissed, 0.4: judgedMet, 0.5: judgedMet} {
		j := judgeTarget(&ReportTarget{Percentile: 0.9, Seconds: target}, points, m)
		assert.Equal(t, want, j.Measured, "target %g", target)
	}
	// The first bucket, from 0, is coarse: a target inside it is unknown.
	low := measuredOf(LatencySample{Router: bkRouter(1000, [][2]float64{{0.1, 0.95}, {0.4, 1}}, 0.05,
		[][2]float64{{0.01, 1}}, 0.005)}, MeasuredRouter, 300, true)
	assert.Equal(t, judgedUnknown, judgeTarget(&ReportTarget{Percentile: 0.9, Seconds: 0.05}, points, low).Measured)
	assert.Equal(t, judgedMet, judgeTarget(&ReportTarget{Percentile: 0.9, Seconds: 0.1}, points, low).Measured)
}
