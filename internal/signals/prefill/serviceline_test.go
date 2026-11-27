package prefill

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// V_P from the prefill service line (Cycle.VPServiceLine).

// onLine is a minute of p-0, never queued, whose engines each serve prompts
// of prompt computed tokens with the service line S = s0 + prompt/v, at occ
// requests in prefill each (Little's law), over ranks engines: the prefill
// time is S stretched by processor sharing, S*(1 + occ).
func onLine(prompt, s0, v, occ float64, ranks int) domain.ReplicaMetrics {
	rm := base("p-0")
	rm.EngineRanks = ranks
	t := (s0 + prompt/v) * (1 + occ)
	rm.RequestRate = occ * float64(ranks) / t
	rm.PrefillComputedTokenRate = rm.RequestRate * prompt
	rm.AvgPrefillTime, rm.AvgPrefillTimeKnown = t, true
	rm.AvgServiceTime, rm.AvgTTFT = t, t+0.05
	return rm
}

// spread is n minutes on the line s0 + x/v at 0.3 requests in prefill an
// engine, over ranks, cycling through
// prompts of 1000, 2000, 3000 and 4000 computed tokens: a mean of 2500.
func spread(n int, s0, v float64, ranks int) []domain.ReplicaMetrics {
	out := make([]domain.ReplicaMetrics, n)
	for i := range out {
		out[i] = onLine(float64(1000*(1+i%4)), s0, v, 0.3, ranks)
	}
	return out
}

// lineCycles is the rows in order, one cycle each, the service-line switch
// set to on.
func lineCycles(on bool, rows ...domain.ReplicaMetrics) []Cycle {
	out := make([]Cycle, len(rows))
	for i, rm := range rows {
		out[i] = cycle(t0.Add(minute*time.Duration(i)), rm)
		out[i].VPServiceLine = on
	}
	return out
}

// A fleet that never queued gets V_P from its line, s0 kept in the capacity
// at the minutes' mean computed prompt: ranks * E[x] / (s0 + E[x]/V), not a
// bound. Off, V_P is refused for want of a queue.
func TestServiceLine_FillsAFleetThatNeverQueued(t *testing.T) {
	rows := spread(40, 0.25, 20000, 4)
	est := run(NewObserver(), lineCycles(true, rows...)...)
	require.Empty(t, est.VPRefused, "%+v", est)
	assert.Equal(t, SourceServiceLine, est.VPSource)
	assert.InDelta(t, 4*2500/(0.25+2500.0/20000), est.VP, 1e-6)
	assert.False(t, est.VPLowerBound)
	assert.InDelta(t, 0.25, est.VPLineS0, 1e-9)
	assert.InDelta(t, 1.0/20000, est.VPLineSlope, 1e-12)
	assert.InDelta(t, est.VP, est.VPLine, 1e-9)
	assert.Equal(t, 40, est.VPLineMinutes)
	assert.Equal(t, 40, est.VPSamples)
	assert.InDelta(t, 2500.0, est.VPPromptTokens, 1e-6)
	// s0 a request on one engine is s0/4 pod-seconds on the row, under the
	// whole work at E[x], E[x]/V_P.
	assert.InDelta(t, 0.25/4, est.VPRequestSeconds, 1e-9)
	assert.Less(t, est.VPRequestSeconds, est.VPPromptTokens/est.VP)

	off := run(NewObserver(), lineCycles(false, rows...)...)
	assert.Equal(t, NoPrefillCapacity, off.VPRefused)
	assert.Zero(t, off.VPLine, "off, the line is not read")
	assert.Zero(t, off.VPRequestSeconds, "off, no split")
	assert.Empty(t, off.VPLineRefused)
}

// Busier minutes stretch each request's prefill time by the sharing; the
// processor-sharing inversion takes it out, so the line read across loads
// is the line, not a steeper one.
func TestServiceLine_TheSharingIsTakenOut(t *testing.T) {
	rows := make([]domain.ReplicaMetrics, 40)
	for i := range rows {
		occ := 0.1
		if i%4 >= 2 {
			occ = 1.5 // the longer prompts at the busier minutes
		}
		rows[i] = onLine(float64(1000*(1+i%4)), 0.25, 20000, occ, 1)
	}
	est := run(NewObserver(), lineCycles(true, rows...)...)
	require.Equal(t, SourceServiceLine, est.VPSource, "%+v", est)
	assert.InDelta(t, 0.25, est.VPLineS0, 1e-9)
	assert.InDelta(t, 1.0/20000, est.VPLineSlope, 1e-12)
}

// Fewer than MinTimingMinutes timed minutes is no reading.
func TestServiceLine_AbstainsWithTooFewMinutes(t *testing.T) {
	est := run(NewObserver(), lineCycles(true, spread(MinTimingMinutes-1, 0.25, 20000, 1)...)...)
	assert.Equal(t, TooFewTimedMinutes, est.VPLineRefused)
	assert.Equal(t, MinTimingMinutes-1, est.VPLineMinutes)
	assert.Zero(t, est.VPLine)
	assert.Equal(t, NoPrefillCapacity, est.VPRefused)

	enough := run(NewObserver(), lineCycles(true, spread(MinTimingMinutes, 0.25, 20000, 1)...)...)
	assert.Equal(t, SourceServiceLine, enough.VPSource)
}

// One prompt length cannot tell s0 from 1/V.
func TestServiceLine_RefusesPromptsThatDoNotSpread(t *testing.T) {
	rows := make([]domain.ReplicaMetrics, 40)
	for i := range rows {
		rows[i] = onLine(2000+float64(i%4)*100, 0.25, 20000, 0.3, 1)
	}
	est := run(NewObserver(), lineCycles(true, rows...)...)
	assert.Equal(t, NoPromptSpread, est.VPLineRefused)
	assert.Equal(t, NoPrefillCapacity, est.VPRefused)
}

// A line whose intercept reads under zero, or whose slope is not above it,
// is refused: s0 is a time and V a speed.
func TestServiceLine_RefusesANegativeInterceptOrSlope(t *testing.T) {
	neg := run(NewObserver(), lineCycles(true, spread(40, -0.05, 10000, 1)...)...)
	assert.Equal(t, NonPositiveIntercept, neg.VPLineRefused)
	assert.Equal(t, NoPrefillCapacity, neg.VPRefused)

	rows := make([]domain.ReplicaMetrics, 40)
	for i := range rows {
		// The prefill time falls as the prompt grows.
		rows[i] = onLine(float64(1000*(1+i%4)), 0.5-float64(i%4)*0.1, 1e12, 0.3, 1)
	}
	flat := run(NewObserver(), lineCycles(true, rows...)...)
	assert.Equal(t, NonPositiveSlope, flat.VPLineRefused)
}

// A slope whose standard error is above MaxVPLineSlopeRelSE of it does not
// identify V.
func TestServiceLine_RefusesAnUncertainSlope(t *testing.T) {
	rows := spread(40, 0.25, 20000, 1)
	for i := range rows {
		// Half the minutes stall: their prefill time doubles, whatever the
		// prompt.
		if i%8 >= 4 {
			rows[i].AvgPrefillTime *= 2
		}
	}
	est := run(NewObserver(), lineCycles(true, rows...)...)
	assert.Equal(t, SlopeUncertain, est.VPLineRefused, "%+v", est)
	assert.Equal(t, NoPrefillCapacity, est.VPRefused)
}

// A V_P that is not a lower bound stands: busy minutes of a single-rank pod
// win over the line, which is still recorded.
func TestServiceLine_BusyMinutesWin(t *testing.T) {
	cycles := lineCycles(true, spread(40, 0.25, 20000, 1)...)
	for i := range 6 {
		c := cycle(t0.Add(minute*time.Duration(40+i)), queued("p-0", 9000, 2))
		c.QueueMin, c.VPServiceLine = map[string]float64{"p-0": 1}, true
		cycles = append(cycles, c)
	}
	est := run(NewObserver(), cycles...)
	require.Equal(t, SourceBusyMinutes, est.VPSource, "%+v", est)
	assert.False(t, est.VPLowerBound)
	assert.InDelta(t, 9000.0, est.VP, 1e-6)
	assert.InDelta(t, 2500/(0.25+2500.0/20000), est.VPLine, 1e-6, "recorded beside it")
}

// lineWithTail is the never-queued minutes of spread on ranks engines, the
// switch on, then the tail cycles after them, each a minute apart.
func lineWithTail(ranks int, tail ...Cycle) []Cycle {
	cycles := lineCycles(true, spread(40, 0.25, 20000, ranks)...)
	for i, c := range tail {
		c.Now, c.VPServiceLine = t0.Add(minute*time.Duration(40+i)), true
		cycles = append(cycles, c)
	}
	return cycles
}

// A peak under a queue stands, under the line or above it: the queue read
// the pod at capacity, the line only extrapolates its unqueued minutes to it
// (issue llm-d-incubation/llm-d-scaling-manager#449). The line is recorded,
// not taken, and splits nothing.
func TestServiceLine_APeakUnderAQueueStands(t *testing.T) {
	line := 2500 / (0.25 + 2500.0/20000)
	at := func(peak float64) Estimate {
		tail := make([]Cycle, PeakRun)
		for i := range tail {
			tail[i] = cycle(time.Time{}, queued("p-0", peak, 2))
		}
		return run(NewObserver(), lineWithTail(1, tail...)...)
	}
	for _, peak := range []float64{line / 2, line * 2} {
		est := at(peak)
		assert.Equal(t, SourcePeakUnderQueue, est.VPSource, "peak %v: %+v", peak, est)
		assert.True(t, est.VPLowerBound, "peak %v", peak)
		assert.InDelta(t, peak, est.VP, 1e-6, "peak %v", peak)
		assert.InDelta(t, line, est.VPLine, 1e-6, "peak %v: recorded beside it", peak)
		assert.Zero(t, est.VPRequestSeconds, "peak %v: a line recorded but not taken splits nothing", peak)
	}
}

// Busy minutes over data-parallel ranks are a lower bound (DataParallelRanks)
// read under a queue: they stand under the line.
func TestServiceLine_BusyMinutesOverRanksStand(t *testing.T) {
	line := 4 * 2500 / (0.25 + 2500.0/20000)
	tail := make([]Cycle, MinBusyMinutes)
	for i := range tail {
		rm := queued("p-0", line/3, 3)
		rm.EngineRanks = 4
		tail[i] = cycle(time.Time{}, rm)
		tail[i].QueueMin = map[string]float64{"p-0": 2}
	}
	est := run(NewObserver(), lineWithTail(4, tail...)...)
	require.Equal(t, SourceBusyMinutes, est.VPSource, "%+v", est)
	assert.True(t, est.VPLowerBound)
	assert.Equal(t, DataParallelRanks, est.VPLowerBoundReason)
	assert.InDelta(t, line/3, est.VP, 1e-6)
	assert.InDelta(t, line, est.VPLine, 1e-6, "recorded beside it")
}

// The back-pressure bound is read from the router's queue: it stands under
// the line.
func TestServiceLine_TheBackPressureBoundStands(t *testing.T) {
	line := 2500 / (0.25 + 2500.0/20000)
	cycles := lineCycles(true, spread(40, 0.25, 20000, 1)...)
	cycles[len(cycles)-1].Bound = Bound{Tokens: line / 2, Samples: MinBoundSamples}
	est := run(NewObserver(), cycles...)
	require.Equal(t, SourceBackPressureBound, est.VPSource, "%+v", est)
	assert.True(t, est.VPLowerBound)
	assert.InDelta(t, line/2, est.VP, 1e-6)
	assert.InDelta(t, line, est.VPLine, 1e-6)
}

// The served peak of a fleet that never queued (NeverQueued) is replaced by
// the line under it; one above it stands, as the true V_P is at or above
// the peak.
func TestServiceLine_AServedPeakStandsOnlyAboveTheLine(t *testing.T) {
	line := 2500 / (0.25 + 2500.0/20000)
	at := func(peak float64) Estimate {
		tail := make([]Cycle, PeakRun)
		for i := range tail {
			tail[i] = cycle(time.Time{}, queued("p-0", peak, 0))
		}
		cycles := lineWithTail(1, tail...)
		for i := range cycles {
			cycles[i].VPServedUnqueued = true
		}
		return run(NewObserver(), cycles...)
	}
	under := at(line / 2)
	assert.Equal(t, SourceServiceLine, under.VPSource, "%+v", under)
	assert.InDelta(t, line, under.VP, 1e-6)
	assert.False(t, under.VPLowerBound)
	assert.Empty(t, under.VPLowerBoundReason)

	above := at(line * 2)
	assert.Equal(t, SourceServedPeak, above.VPSource, "%+v", above)
	assert.Equal(t, NeverQueued, above.VPLowerBoundReason)
	assert.InDelta(t, line*2, above.VP, 1e-6)
	assert.InDelta(t, line, above.VPLine, 1e-6)
	assert.Zero(t, above.VPRequestSeconds)
}

// A peak the window kept from a run Retention aged out (VPWindowBest) was
// read under a queue: it stands, with the minute it was read (VPLearnedAt).
func TestServiceLine_AWindowPeakStandsWithItsAge(t *testing.T) {
	line := 2500 / (0.25 + 2500.0/20000)
	cycles := make([]Cycle, 0, PeakRun+int(Retention/minute)+40)
	for i := range PeakRun {
		cycles = append(cycles, cycle(t0.Add(minute*time.Duration(i)), queued("p-0", line/2, 2)))
	}
	cycles = append(cycles, lineCycles(true, spread(int(Retention/minute)+40, 0.25, 20000, 1)...)...)
	for i := range cycles {
		cycles[i].Now = t0.Add(minute * time.Duration(i))
		cycles[i].VPWindowBest, cycles[i].VPServiceLine = true, true
	}
	est := run(NewObserver(), cycles...)
	require.Equal(t, SourcePeakUnderQueue, est.VPSource, "%+v", est)
	assert.InDelta(t, line/2, est.VP, 1e-6)
	assert.False(t, est.VPLearnedAt.IsZero(), "the window's peak keeps its age")
	assert.InDelta(t, line, est.VPLine, 1e-6)
}
