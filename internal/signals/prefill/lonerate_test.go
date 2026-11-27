package prefill

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// V_P from light load, the lone rate (Cycle.VPLoneRate; issue
// llm-d-incubation/llm-d-scaling-manager#406).

// atOccupancy is a minute of p-0, never queued, whose engines each compute
// prompts of prompt tokens one at a time at v tok/s (prefill time prompt/v)
// and hold x requests in prefill each (Little's law), over ranks engines.
func atOccupancy(prompt, v, x float64, ranks int) domain.ReplicaMetrics {
	rm := base("p-0")
	rm.EngineRanks = ranks
	t := prompt / v
	rm.RequestRate = x * float64(ranks) / t
	rm.PrefillComputedTokenRate = rm.RequestRate * prompt
	rm.AvgPrefillTime, rm.AvgPrefillTimeKnown = t, true
	rm.AvgServiceTime, rm.AvgTTFT = t, t+0.05
	return rm
}

// servedMinute is a minute of p-0, never queued, computing rate tok/s of prompts
// of prompt tokens at two requests in prefill: busy, not lone.
func servedMinute(prompt, rate float64) domain.ReplicaMetrics {
	rm := base("p-0")
	rm.RequestRate, rm.PrefillComputedTokenRate = rate/prompt, rate
	rm.AvgPrefillTime, rm.AvgPrefillTimeKnown = 2/rm.RequestRate, true
	rm.AvgServiceTime, rm.AvgTTFT = rm.AvgPrefillTime, rm.AvgPrefillTime+0.05
	return rm
}

// loneCycles is the rows in order, one cycle each, the lone-rate switch set
// to on.
func loneCycles(on bool, rows ...domain.ReplicaMetrics) []Cycle {
	out := make([]Cycle, len(rows))
	for i, rm := range rows {
		out[i] = cycle(t0.Add(minute*time.Duration(i)), rm)
		out[i].VPLoneRate = on
	}
	return out
}

// repeat is n copies of rm.
func repeat(n int, rm domain.ReplicaMetrics) []domain.ReplicaMetrics {
	out := make([]domain.ReplicaMetrics, n)
	for i := range out {
		out[i] = rm
	}
	return out
}

// A pod seen to serve no more than LoneServedTolerance over its lone rate is
// compute-bound alone: the lone rate is V_P, not a bound. Off, the fleet
// never queued and V_P is refused.
func TestLoneRate_IsVPWhereTheServedPeakAgrees(t *testing.T) {
	rows := append(repeat(4, servedMinute(2000, 21000)), repeat(12, atOccupancy(2000, 20000, 0.1, 1))...)
	est := run(NewObserver(), loneCycles(true, rows...)...)
	require.Empty(t, est.VPRefused, "%+v", est)
	assert.Equal(t, SourceLoneRate, est.VPSource)
	assert.InDelta(t, 20000.0, est.VP, 1e-6)
	assert.False(t, est.VPLowerBound)
	assert.True(t, est.VPLoneIsVP)
	assert.Equal(t, 12, est.VPLoneMinutes)
	assert.Equal(t, 12, est.VPSamples)
	assert.InDelta(t, 2000.0, est.VPPromptTokens, 1e-6)

	off := run(NewObserver(), loneCycles(false, rows...)...)
	assert.Equal(t, NoPrefillCapacity, off.VPRefused)
	assert.Zero(t, off.VPLone, "off, the lone rate is not read")
}

// A pod seen to serve more than LoneServedTolerance over its lone rate
// gained from batching: the lone rate is only a lower bound, recorded and
// not taken.
func TestLoneRate_ABatchingGainLeavesItALowerBound(t *testing.T) {
	rows := append(repeat(4, servedMinute(2000, 30000)), repeat(12, atOccupancy(2000, 20000, 0.1, 1))...)
	est := run(NewObserver(), loneCycles(true, rows...)...)
	assert.Equal(t, NoPrefillCapacity, est.VPRefused)
	assert.InDelta(t, 20000.0, est.VPLone, 1e-6)
	assert.False(t, est.VPLoneIsVP)
}

// Fewer than MinLoneMinutes lone minutes at the prompt priced is no reading,
// and a minute at or over LoneMaxOccupancy is not lone.
func TestLoneRate_AbstainsWithTooFewMinutes(t *testing.T) {
	rows := append(repeat(MinLoneMinutes-1, atOccupancy(2000, 20000, 0.1, 1)),
		repeat(5, atOccupancy(2000, 20000, LoneMaxOccupancy, 1))...)
	est := run(NewObserver(), loneCycles(true, append(rows, atOccupancy(2000, 20000, 0.1, 1))...)...)
	assert.Equal(t, SourceLoneRate, est.VPSource)
	assert.Equal(t, MinLoneMinutes, est.VPLoneMinutes, "the last minute makes ten")
	assert.InDelta(t, 20000.0, est.VPLone, 1e-6)

	short := run(NewObserver(), loneCycles(true, rows...)...)
	assert.Equal(t, MinLoneMinutes-1, short.VPLoneMinutes)
	assert.Zero(t, short.VPLone)
	assert.Equal(t, NoPrefillCapacity, short.VPRefused)
}

// A speed is a speed at a prompt length: only minutes within LonePromptBand
// of the prompt the cycle's rows carry count.
func TestLoneRate_OnlyThePromptPricedCounts(t *testing.T) {
	long := repeat(12, atOccupancy(8000, 16000, 0.1, 1))
	short := repeat(12, atOccupancy(2000, 20000, 0.1, 1))
	atShort := run(NewObserver(), loneCycles(true, append(append([]domain.ReplicaMetrics{}, long...), short...)...)...)
	require.Equal(t, SourceLoneRate, atShort.VPSource)
	assert.InDelta(t, 20000.0, atShort.VP, 1e-6)
	assert.InDelta(t, 2000.0, atShort.VPPromptTokens, 1e-6)
	atLong := run(NewObserver(), loneCycles(true, append(append([]domain.ReplicaMetrics{}, short...), long...)...)...)
	require.Equal(t, SourceLoneRate, atLong.VPSource)
	assert.InDelta(t, 16000.0, atLong.VP, 1e-6)
	assert.InDelta(t, 8000.0, atLong.VPPromptTokens, 1e-6)
}

// Busy minutes of a single-rank pod are V_P itself: the lone rate is read
// beside them and does not replace them.
func TestLoneRate_BusyMinutesWin(t *testing.T) {
	cs := loneCycles(true, repeat(12, atOccupancy(1000, 20000, 0.1, 1))...)
	for i := range 6 {
		c := cycle(t0.Add(minute*(12+time.Duration(i))), queued("p-0", 22000, 3))
		c.QueueMin = map[string]float64{"p-0": 1}
		c.VPLoneRate = true
		cs = append(cs, c)
	}
	est := run(NewObserver(), cs...)
	assert.Equal(t, SourceBusyMinutes, est.VPSource)
	assert.Equal(t, 22000.0, est.VP)
}

// A lower bound, a peak under a queue with no busy minutes read, is raised to
// a lone rate that is V_P, and a lower bound above it, within
// LoneServedTolerance, is kept: the larger of the two, not a bound. Off, the
// peak under a queue stays a lower bound.
func TestLoneRate_RaisesALowerBoundToTheLargerOfTheTwo(t *testing.T) {
	at := func(on bool, peak float64) Estimate {
		rows := append(repeat(12, atOccupancy(1000, 20000, 0.1, 1)), repeat(PeakRun, queued("p-0", peak, 3))...)
		return run(NewObserver(), loneCycles(on, rows...)...)
	}
	under := at(true, 19000)
	require.Equal(t, SourceLoneRate, under.VPSource, "%+v", under)
	assert.InDelta(t, 20000.0, under.VP, 1e-6)
	assert.False(t, under.VPLowerBound)
	assert.Empty(t, under.VPLowerBoundReason)

	over := at(true, 21500)
	require.Equal(t, SourceLoneRate, over.VPSource, "%+v", over)
	assert.InDelta(t, 21500.0, over.VP, 1e-6, "the larger of the two")
	assert.False(t, over.VPLowerBound)

	off := at(false, 19000)
	assert.Equal(t, SourcePeakUnderQueue, off.VPSource)
	assert.True(t, off.VPLowerBound)
	assert.InDelta(t, 19000.0, off.VP, 1e-6)
}

// A row of four engines computes four lone requests at once: its lone rate
// is four engines' speed, V_P being a row's.
func TestLoneRate_ADataParallelRowCountsItsRanks(t *testing.T) {
	est := run(NewObserver(), loneCycles(true, repeat(12, atOccupancy(2000, 20000, 0.1, 4))...)...)
	require.Equal(t, SourceLoneRate, est.VPSource, "%+v", est)
	assert.InDelta(t, 80000.0, est.VP, 1e-6)
}

// A served peak older than Retention still bounds the lone rate: a fleet
// that served 30k tok/s at its busy hour keeps a lone rate of 20k, read in
// the quiet hours after it until that hour has left the samples kept, a
// lower bound.
func TestLoneRate_AnAgedServedPeakStillBounds(t *testing.T) {
	cs := loneCycles(true, repeat(4, servedMinute(2000, 30000))...)
	// Three lone minutes in a row each hour, so the samples kept at the end
	// hold a served peak of their own, the lone minutes' 2k.
	for i, c := range loneCycles(true, repeat(21, atOccupancy(2000, 20000, 0.1, 1))...) {
		c.Now = t0.Add(time.Hour*time.Duration(1+i/3) + minute*time.Duration(i%3))
		cs = append(cs, c)
	}
	est := run(NewObserver(), cs...)
	assert.Equal(t, NoPrefillCapacity, est.VPRefused, "%+v", est)
	assert.InDelta(t, 20000.0, est.VPLone, 1e-6)
	assert.False(t, est.VPLoneIsVP)
}
