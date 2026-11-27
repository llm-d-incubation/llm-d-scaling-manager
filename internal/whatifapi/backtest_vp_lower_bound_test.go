package whatifapi

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// lowerBoundVPPoint is a mean TTFT predicted at v seconds from a V_P that
// is only a lower bound, as figureOf puts it on the wire.
func lowerBoundVPPoint(v float64) BacktestPoint {
	f := figureOf(whatif.TTFT{Seconds: v, Flags: []whatif.Flag{whatif.VPLowerBound}}, unitSeconds, true)
	return BacktestPoint{Statistic: statisticMean, Predicted: f}
}

// A TTFT priced from a lower bound of V_P is flagged so, and is no upper
// bound on the wire: only its V_P terms are bounded (#515).
func TestFigure_ATTFTFromALowerBoundOfVPIsNoUpperBound(t *testing.T) {
	f := figureOf(whatif.TTFT{Seconds: 0.5, Flags: []whatif.Flag{whatif.VPLowerBound}}, unitSeconds, true)
	assert.False(t, f.UpperBound)
	assert.Contains(t, f.Flags, string(whatif.VPLowerBound))
	assert.True(t, lowerBoundVPFigure(f))
	b, err := json.Marshal(f)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "upperBound", "the key is never set")

	assumed := figureOf(whatif.TTFT{Seconds: 0.5, Flags: []whatif.Flag{whatif.VPLowerBound, whatif.AssumedVP}}, unitSeconds, true)
	assert.False(t, lowerBoundVPFigure(assumed), "a stated V_P is priced as given")
	assert.False(t, lowerBoundVPFigure(figureOf(whatif.TTFT{Seconds: 0.5}, unitSeconds, true)))
	rate := figureOf(whatif.Answer{Value: 4, Flags: []whatif.Flag{whatif.VPLowerBound}}, unitRate, false)
	assert.False(t, lowerBoundVPFigure(rate), "a rate is not a latency")
}

// A TTFT priced from a lower bound of V_P keeps its ratio, over or under
// what was measured, and is marked to be scored apart; a plain figure is
// scored as it always was (#515).
func TestBacktest_ALowerBoundVPFigureIsScoredApart(t *testing.T) {
	m := 1.0
	for _, v := range []float64{1525, 1.2, 0.9, 0.5} {
		p := scoredPoint(lowerBoundVPPoint(v), &m, 1000, 100, "", nil)
		assert.Equal(t, UnscoredVPLowerBound, p.Unscored, "%g", v)
		require.NotNil(t, p.Ratio, "%g: the ratio is kept", v)
		assert.InDelta(t, v, *p.Ratio, 1e-9)
	}
	plain := scoredPoint(BacktestPoint{Statistic: statisticMean, Predicted: Figure{Value: ptr(1525), Unit: unitSeconds}}, &m, 1000, 100, "", nil)
	require.NotNil(t, plain.Ratio)
	assert.Empty(t, plain.Unscored)

	// Measured above the histogram's top bucket, or in a coarse bucket wholly
	// above the figure: it read under.
	inf := math.Inf(1)
	top := scoredPoint(lowerBoundVPPoint(5), &inf, 1000, 10, "", nil)
	assert.True(t, top.AboveHistogram)
	assert.True(t, readUnder(top))
	under := scoredPoint(lowerBoundVPPoint(0.1), &m, 1000, 100, "", &Band{Lo: 0.8, Hi: 1.6})
	assert.Equal(t, OutsideUnder, under.OutsideBucket)
	assert.True(t, readUnder(under))
	over := scoredPoint(lowerBoundVPPoint(5), &m, 1000, 100, "", &Band{Lo: 0.8, Hi: 1.6})
	assert.Equal(t, OutsideOver, over.OutsideBucket)
	assert.False(t, readUnder(over))
	// No tolerance: 0.99x reads under.
	assert.True(t, readUnder(scoredPoint(lowerBoundVPPoint(0.99), &m, 1000, 100, "", nil)))
}

// The scores keep the pairs from a lower bound of V_P out of the point
// score, over or under what was measured, and count apart how many read
// under: a coverage gain made of them does not read as an accuracy loss,
// and a figure under what was measured is not hidden (#515).
func TestBacktest_ScoresApartCountTheFiguresReadUnder(t *testing.T) {
	m := 1.0
	steps := make([]BacktestStep, 0, 6)
	for _, v := range []float64{0.5, 0.8, 1.3, 2, 3} {
		steps = append(steps, BacktestStep{TTFT: []BacktestPoint{scoredPoint(lowerBoundVPPoint(v), &m, 1000, 100, "", nil)}})
	}
	plain := BacktestPoint{Statistic: statisticMean, Predicted: Figure{Value: ptr(1.1), Unit: unitSeconds}}
	steps = append(steps, BacktestStep{TTFT: []BacktestPoint{scoredPoint(plain, &m, 1000, 100, "", nil)}})
	sc := scoreOf(0, steps, func(st BacktestStep) []BacktestPoint { return st.TTFT }, 0)
	assert.Equal(t, 1, sc.All.Steps, "only the plain figure is a point")
	assert.InDelta(t, 1.1, *sc.All.Bias, 1e-9)
	require.NotNil(t, sc.VPLowerBound)
	assert.Equal(t, BacktestApart{Steps: 5, AtOrOver: 3, Under: 2, Bias: ptr(1.3)}, *sc.VPLowerBound)
	assert.Equal(t, 5, sc.Unscored[UnscoredVPLowerBound])

	line, quoted := verdictPart("TTFT", []BacktestScore{sc}, "steps")
	require.NotNil(t, quoted)
	assert.Contains(t, line, "In 5 more steps its mean TTFT was priced from a lower bound of V_P, which is neither a point figure nor a bound")
	assert.Contains(t, line, "it read under the measured TTFT in 2 of them, median ratio 1.30")
	assert.NotContains(t, line, "upper bound")
}

// With every figure from a lower bound of V_P the verdict says so, and the
// self-check counts them apart, no rule reading them.
func TestBacktest_OnlyFiguresApart(t *testing.T) {
	m := 1.0
	steps := make([]BacktestStep, 0, 3)
	for _, v := range []float64{0.5, 2, 3} {
		steps = append(steps, BacktestStep{TTFT: []BacktestPoint{scoredPoint(lowerBoundVPPoint(v), &m, 1000, 100, "", nil)}})
	}
	sc := scoreOf(0, steps, func(st BacktestStep) []BacktestPoint { return st.TTFT }, 0)
	assert.Zero(t, sc.All.Steps)
	line, quoted := verdictPart("TTFT", []BacktestScore{sc}, "steps")
	assert.Nil(t, quoted)
	assert.Contains(t, line, "was not scored as a point in any step. In 3 steps its mean TTFT was priced from a lower bound of V_P")
	assert.Contains(t, line, "in 1 of them")

	p90 := sc
	p90.Statistic = statP90
	f := selfCheckFigure([]BacktestScore{sc, p90}, 3)
	require.NotNil(t, f)
	assert.Equal(t, 3, f.VPLowerBound)
	assert.Equal(t, 1, f.VPLowerBoundUnder)
	assert.Zero(t, f.UpperBound, "never set")
	assert.Zero(t, f.Steps, "no rule reads a figure scored apart")
	assert.Contains(t, f.apartWords(), "3 more priced from a lower bound of V_P, scored apart: 1 read under the measured figure")
}

// A predicted TTFT over its target from a lower bound of V_P is no predicted
// miss: the miss is not known. Under it, it is met, as any figure.
func TestBacktest_ALowerBoundVPFigureOverTheTargetIsNoPredictedMiss(t *testing.T) {
	i := statIndex(t, 0)
	points := make([]BacktestPoint, len(backtestStats))
	m := measured{ttft: make([]*float64, len(backtestStats)), ttftWhy: make([]string, len(backtestStats)),
		ttftBucket: make([]*Band, len(backtestStats)), requests: 1000}
	m.ttft[i] = ptr(0.1)
	target := &ReportTarget{Percentile: 0, Seconds: 0.2}

	points[i] = lowerBoundVPPoint(0.5)
	j := judgeTarget(target, points, m)
	assert.Equal(t, judgedUnknown, j.Predicted, "0.5 s against 0.2 s from a lower bound of V_P: not known")
	assert.Equal(t, judgedMet, j.Measured)

	points[i] = lowerBoundVPPoint(0.15)
	assert.Equal(t, judgedMet, judgeTarget(target, points, m).Predicted)

	points[i] = BacktestPoint{Statistic: statisticMean, Predicted: Figure{Value: ptr(0.5), Unit: unitSeconds}}
	assert.Equal(t, judgedMissed, judgeTarget(target, points, m).Predicted, "a plain figure over the target is a miss")
}
