package whatifapi

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A day plan asked for no statistic sizes a bucket of a steady Poisson
// stream at its p95 less counting noise, not at its p95: the one-minute
// rate's counting noise is in the queueing model already, and a p95 of
// such rates counts it twice (#295). At 2 r/s the p95 of 20 one-minute
// rates is about 1.2 sigma, some 18%, over the rate.
func TestDayPlanDefaultSizesForTheP95LessCountingNoise(t *testing.T) {
	res, err := (&stubResolver{}).Resolve(t.Context(), InputsRequest{})
	require.NoError(t, err)
	rng := rand.New(rand.NewPCG(3, 5))
	s := shaped(time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC), 2, 1000)
	for i := range s {
		s[i].Rate = poissonMinutes(rng, 1, 2)[0]
	}
	res.Series = s
	p, err := DayPlan(t.Context(), res, DayPlanRequest{}, "schema-v3")
	require.NoError(t, err)
	require.NotEmpty(t, p.Buckets)
	b := p.Buckets[0]
	assert.Equal(t, SizeForP95Modulation, p.SizeFor)
	assert.Equal(t, SizeForP95Modulation, b.SizedFor)
	require.NotNil(t, b.SizedRate)
	assert.Less(t, *b.SizedRate, b.Rate.P95, "the counting noise is taken out")
	assert.GreaterOrEqual(t, *b.SizedRate, b.Rate.Mean)
	assert.Equal(t, 1, p.Summary.Counts.SizedForP95Modulation)
	assert.Zero(t, p.Summary.Counts.SizedForP95)
}

// The report judges at the p95 less counting noise when no statistic is
// asked (#295).
func TestDefaultLoadStatisticIsTheP95LessCountingNoise(t *testing.T) {
	assert.Equal(t, LoadP95Modulation, DefaultLoadStatistic)
}
