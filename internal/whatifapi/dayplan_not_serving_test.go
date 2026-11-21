package whatifapi

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Beside the pods it had, a bucket says the replicas asked for and not
// serving: the median over its cycles, as had's. Here 10:00 had 2 prefill
// and 2 decode serving with 1 decode asked and not serving in 12 of its 20
// cycles (starting, or held unschedulable), and 1 prefill in 5 (under the
// median); 10:05 had none. The hour reads its bucket of the fewest serving,
// the most at a tie; a note says what it is, and a plan with none says
// nothing of it.
func TestDayPlanSaysWhatWasAskedAndNotServing(t *testing.T) {
	res, err := (&stubResolver{}).Resolve(t.Context(), InputsRequest{})
	require.NoError(t, err)
	t0 := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	first := shaped(t0, 2, 1000)
	for i := range first {
		if i < 12 {
			first[i].DecodeNotServing = 1
		}
		if i < 5 {
			first[i].PrefillNotServing = 1
		}
	}
	res.Series = slices.Concat(first, shaped(t0.Add(5*time.Minute), 2, 1000))
	p, err := DayPlan(t.Context(), res, DayPlanRequest{}, "schema-v3")
	require.NoError(t, err)
	require.Len(t, p.Buckets, 2)
	assert.Equal(t, &FleetCounts{Prefill: 2, Decode: 2}, p.Buckets[0].Actual, "had counts those serving only")
	assert.Equal(t, &FleetCounts{Prefill: 0, Decode: 1}, p.Buckets[0].NotServing)
	assert.Nil(t, p.Buckets[1].NotServing, "none asked and not serving")
	require.Len(t, p.Hours, 1)
	assert.Equal(t, &FleetCounts{Prefill: 0, Decode: 1}, p.Hours[0].NotServing)
	assert.True(t, hasNote(p.Notes, "In 1 bucket(s) more were asked for and not serving"), "%q", p.Notes)

	res.Series = append(shaped(t0, 2, 1000), shaped(t0.Add(5*time.Minute), 2, 1000)...)
	p, err = DayPlan(t.Context(), res, DayPlanRequest{}, "schema-v3")
	require.NoError(t, err)
	assert.Nil(t, p.Hours[0].NotServing)
	assert.False(t, hasNote(p.Notes, "asked for and not serving"), "%q", p.Notes)
}

// An hour's had and (+n) read off one bucket: the one with the fewest of
// the role serving. A replica serving in one bucket and restarting in the
// next is not counted twice.
func TestHourNotServingReadsTheBucketOfTheFewest(t *testing.T) {
	b := func(had, not int) *DayBucket {
		x := &DayBucket{Actual: &FleetCounts{Prefill: 1, Decode: had}}
		if not > 0 {
			x.NotServing = &FleetCounts{Decode: not}
		}
		return x
	}
	had := &FleetCounts{Prefill: 1, Decode: 1}
	assert.Equal(t, &FleetCounts{Decode: 1}, hourNotServing(had, []*DayBucket{b(2, 0), b(1, 1)}), "1 serving and 1 restarting: 1(+1)")
	assert.Nil(t, hourNotServing(had, []*DayBucket{b(1, 0), b(2, 1)}), "the restart is in the bucket of 2: 1, not 1(+1)")
	assert.Equal(t, &FleetCounts{Decode: 2}, hourNotServing(had, []*DayBucket{b(1, 1), b(1, 2)}), "the most at a tie")
	assert.Nil(t, hourNotServing(nil, nil))

	// Per role: the fewest prefill and the fewest decode can be different
	// buckets, each column read off its own.
	two := []*DayBucket{
		{Actual: &FleetCounts{Prefill: 1, Decode: 3}, NotServing: &FleetCounts{Prefill: 1, Decode: 2}},
		{Actual: &FleetCounts{Prefill: 2, Decode: 2}, NotServing: &FleetCounts{Prefill: 3, Decode: 1}},
		{NotServing: &FleetCounts{Prefill: 9, Decode: 9}},
	}
	assert.Equal(t, &FleetCounts{Prefill: 1, Decode: 1}, hourNotServing(&FleetCounts{Prefill: 1, Decode: 2}, two),
		"prefill off the first bucket, decode off the second; a bucket with no had is none")
}

// A bucket's had and (+n) describe a fleet its cycles had: half its cycles
// at 2 serving and 1 restarting, half at 3 serving, read 3 serving and
// none more, not 3(+1), four replicas no cycle had.
func TestNotServingOfPairsTheCycles(t *testing.T) {
	samples := make([]SeriesSample, 20)
	for i := range samples {
		samples[i] = SeriesSample{Prefill: 1, Decode: 3}
		if i < 10 {
			samples[i].Decode, samples[i].DecodeNotServing = 2, 1
		}
	}
	assert.Equal(t, FleetCounts{Prefill: 1, Decode: 3}, actualOf(samples))
	assert.Equal(t, FleetCounts{}, notServingOf(samples))
	for i := range samples[:15] {
		samples[i].Decode, samples[i].DecodeNotServing = 2, 1
	}
	assert.Equal(t, FleetCounts{Prefill: 1, Decode: 2}, actualOf(samples))
	assert.Equal(t, FleetCounts{Decode: 1}, notServingOf(samples), "most cycles 2 serving and 1 restarting: 2(+1)")
}

// hasNote says a note holds s.
func hasNote(notes []string, s string) bool {
	for _, n := range notes {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}
