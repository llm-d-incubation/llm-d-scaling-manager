package whatifapi

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// hourOfCycles is n cycles a minute apart from t0 at rate, each with the
// router's TTFT histogram ttft (mean ttftMean) and a TPOT histogram whose
// every request falls in 10-20 ms.
func hourOfCycles(t0 time.Time, n int, rate float64, ttft map[float64]float64, ttftMean float64) ([]SeriesSample, []LatencySample) {
	s := make([]SeriesSample, 0, n)
	l := make([]LatencySample, 0, n)
	for i := range n {
		at := t0.Add(time.Duration(i) * time.Minute)
		ls := routerTTFT(at, ttft, ttftMean)
		ls.Router.TPOTBuckets = []domain.LatencyBucket{{UpperBound: 0.01, Rate: 0}, {UpperBound: 0.02, Rate: 10}, {UpperBound: math.Inf(1), Rate: 10}}
		ls.Router.TPOTSumRate, ls.Router.TPOTCountRate = 0.15, 10
		s = append(s, SeriesSample{At: at, Rate: rate, RateKnown: true})
		l = append(l, ls)
	}
	return s, l
}

// The busiest hour is the one of the highest mean rate, and what the router
// measured there is given as measured: the mean and the p90 placed where the
// histogram places them, a percentile in a bucket too wide to place it as
// its bucket, never as a point.
func TestBusiestHourMeasured(t *testing.T) {
	inf := math.Inf(1)
	t0 := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	quiet := map[float64]float64{0.5: 10, 1: 10, 2: 10, inf: 10}
	busy := map[float64]float64{0.5: 1, 1: 2, 1.1: 10, inf: 10}
	s1, l1 := hourOfCycles(t0, 30, 2, quiet, 0.3)
	s2, l2 := hourOfCycles(t0.Add(time.Hour), 30, 12, busy, 0.9)
	m := BusiestHourMeasured(append(s1, s2...), append(l1, l2...))
	require.NotNil(t, m)
	assert.Equal(t, t0.Add(time.Hour), m.From, "the busier hour")
	assert.InDelta(t, 12, m.Rate, 1e-9)
	require.Len(t, m.TTFT, 3)
	require.NotNil(t, m.TTFT[0].Value)
	assert.InDelta(t, 0.9, *m.TTFT[0].Value, 1e-9, "the mean")
	require.NotNil(t, m.TTFT[1].Value, "p90 in a bucket narrow enough to place it")
	assert.Greater(t, *m.TTFT[1].Value, 1.0)
	require.Len(t, m.TPOT, 3)
	assert.Nil(t, m.TPOT[1].Value, "TPOT p90 in a 10-20 ms bucket is not placed")
	require.NotNil(t, m.TPOT[1].Bucket)
	assert.Equal(t, Band{Lo: 0.01, Hi: 0.02}, *m.TPOT[1].Bucket)

	// No router histogram, no hour: nothing measured is made up.
	plain := append([]SeriesSample{}, s1...)
	none := make([]LatencySample, len(plain))
	assert.Nil(t, BusiestHourMeasured(plain, none))
	assert.Nil(t, BusiestHourMeasured(plain, l1[:3]), "latency not aligned with the series")
}
