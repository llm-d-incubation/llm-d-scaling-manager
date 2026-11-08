package prefill

import (
	"math"
	"math/rand/v2"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle/settlerow"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// minute is the spacing the tests feed rows at: one sample each.
const minute = time.Minute

// base is a Ready prefill row of pod with every series read and nothing
// served.
func base(pod string) domain.ReplicaMetrics {
	return domain.ReplicaMetrics{
		PodName: pod, VariantName: "p", Namespace: "ns", ModelID: "m", Ready: true,
		KvCacheUsageKnown: true, PrefillComputedTokenRateKnown: true, RequestRateKnown: true,
	}
}

// lowLoad is a row of p-0 at low load: rr requests/s of ieff tokens, each served in
// svc seconds, with a TTFT overhead of overhead seconds.
func lowLoad(ieff, svc, rr, overhead float64) domain.ReplicaMetrics {
	rm := base("p-0")
	rm.RequestRate, rm.PrefillComputedTokenRate = rr, ieff*rr
	rm.AvgServiceTime, rm.AvgTTFT = svc, svc+overhead
	return rm
}

// queued is a row computing rate tokens/s with queue requests waiting at the
// minute's peak.
func queued(pod string, rate float64, queue int) domain.ReplicaMetrics {
	rm := base(pod)
	rm.PrefillComputedTokenRate, rm.RequestRate = rate, rate/1000
	rm.QueueLength = queue
	rm.AvgServiceTime, rm.AvgTTFT = 0.05, 0.5
	return rm
}

// cycle is one cycle of rows at t.
func cycle(t time.Time, rows ...domain.ReplicaMetrics) Cycle {
	return Cycle{Key: "k", Engine: inferenceengine.EngineVLLM, Disaggregated: true, Rows: rows, Now: t}
}

// run observes each cycle a minute apart from t0 and returns the last
// estimate.
func run(o *Observer, cycles ...Cycle) Estimate {
	var est Estimate
	for i, c := range cycles {
		if c.Now.IsZero() {
			c.Now = t0.Add(time.Duration(i) * minute)
		}
		est = o.Observe(c)
	}
	return est
}

// queuedMinutes is one cycle per rate, each queued.
func queuedMinutes(rates ...float64) []Cycle {
	out := make([]Cycle, len(rates))
	for i, r := range rates {
		out[i] = cycle(time.Time{}, queued("p-0", r, 3))
	}
	return out
}

// --- V_P ---

// A peak with no queue is only the offered load, and is not used: twenty
// minutes at 30k tok/s with the queue empty leave V_P refused.
func TestCapacity_APeakWithNoQueueIsNotUsed(t *testing.T) {
	cs := queuedMinutes(30000, 30000, 30000, 30000, 30000, 30000)
	for i := range cs {
		cs[i].Rows[0].QueueLength = 0
	}
	est := run(NewObserver(), cs...)
	assert.Equal(t, NoPrefillCapacity, est.VPRefused)
	assert.Zero(t, est.VP)
}

// The peak under a queue is sustained over PeakRun minutes: a one-minute
// spike is the least of every run it is in, so it never sets V_P.
func TestCapacity_ThePeakUnderAQueueIsSustained(t *testing.T) {
	est := run(NewObserver(), queuedMinutes(10000, 50000, 12000, 11000, 13000)...)
	assert.Equal(t, SourcePeakUnderQueue, est.VPSource)
	assert.True(t, est.VPLowerBound)
	assert.Equal(t, 11000.0, est.VP)
	assert.Empty(t, est.VPRefused)
}

// Two queued minutes are not a run: V_P is refused until the third.
func TestCapacity_TwoQueuedMinutesAreNotARun(t *testing.T) {
	o := NewObserver()
	est := run(o, queuedMinutes(20000, 20000)...)
	assert.Equal(t, NoPrefillCapacity, est.VPRefused)
	est = o.Observe(cycle(t0.Add(2*minute), queued("p-0", 20000, 3)))
	assert.Equal(t, 20000.0, est.VP)
}

// A gap wider than MaxRunGap ends a run.
func TestCapacity_AGapEndsTheRun(t *testing.T) {
	o := NewObserver()
	o.Observe(cycle(t0, queued("p-0", 20000, 3)))
	o.Observe(cycle(t0.Add(minute), queued("p-0", 20000, 3)))
	est := o.Observe(cycle(t0.Add(minute+MaxRunGap+time.Second), queued("p-0", 20000, 3)))
	assert.Equal(t, NoPrefillCapacity, est.VPRefused)
}

// Busy minutes come first, at their median, and only while decode is not
// back-pressuring prefill: with decode saturated the same minutes are only a
// peak under a queue.
func TestCapacity_BusyMinutesAreTheMedianWhenDecodeIsNotFull(t *testing.T) {
	for _, downstream := range []bool{false, true} {
		cs := queuedMinutes(30000, 31000, 29000, 30500, 29500)
		for i := range cs {
			cs[i].QueueMin = map[string]float64{"p-0": 1}
			cs[i].DownstreamSaturated = downstream
		}
		est := run(NewObserver(), cs...)
		if downstream {
			assert.Equal(t, SourcePeakUnderQueue, est.VPSource, "back-pressured minutes are not busy minutes")
			assert.Equal(t, 29000.0, est.VP)
			continue
		}
		assert.Equal(t, SourceBusyMinutes, est.VPSource)
		assert.False(t, est.VPLowerBound)
		assert.Equal(t, 30000.0, est.VP)
		assert.Equal(t, 5, est.VPSamples)
	}
}

// A minute whose queue emptied on some sample is not busy.
func TestCapacity_AMinuteThatEmptiedIsNotBusy(t *testing.T) {
	cs := queuedMinutes(30000, 31000, 29000, 30500, 29500)
	for i := range cs {
		cs[i].QueueMin = map[string]float64{"p-0": 0}
	}
	est := run(NewObserver(), cs...)
	assert.Equal(t, SourcePeakUnderQueue, est.VPSource)
}

// A queue minimum that is not a reading is not a busy minute: NaN, +Inf and a
// negative each leave the minutes a peak under a queue.
func TestCapacity_AQueueMinThatIsNotAReadingIsNotBusy(t *testing.T) {
	for _, qmin := range []float64{math.NaN(), math.Inf(1), -1} {
		cs := queuedMinutes(30000, 31000, 29000, 30500, 29500)
		for i := range cs {
			cs[i].QueueMin = map[string]float64{"p-0": qmin}
		}
		est := run(NewObserver(), cs...)
		assert.Zero(t, est.VPBusyMinutes, "queue min %v", qmin)
		assert.Equal(t, SourcePeakUnderQueue, est.VPSource, "queue min %v", qmin)
	}
}

// A minute on a pod whose KV is full is not busy, whatever its queue
// minimum: the pod can hold a queue it cannot schedule. With the KV full on
// one of the five minutes, four are busy and V_P falls back to the peak.
func TestCapacity_AKVFullMinuteIsNotBusy(t *testing.T) {
	cs := queuedMinutes(30000, 31000, 29000, 30500, 29500)
	for i := range cs {
		cs[i].QueueMin = map[string]float64{"p-0": 1}
	}
	cs[2].KVFull = map[string]bool{"p-0": true}
	est := run(NewObserver(), cs...)
	assert.Equal(t, 4, est.VPBusyMinutes)
	assert.Equal(t, SourcePeakUnderQueue, est.VPSource)

	// The full pod's name alone is what excludes it: another pod's KV full
	// leaves p-0's minutes busy.
	cs[2].KVFull = map[string]bool{"p-1": true}
	est = run(NewObserver(), cs...)
	assert.Equal(t, 5, est.VPBusyMinutes)
	assert.Equal(t, SourceBusyMinutes, est.VPSource)
}

// Busy minutes of a pod with more than one data-parallel rank are a lower
// bound, with the reason: its queue minimum is the ranks' summed, and one
// rank may have idled. One such pod among single-rank pods flags the whole
// median; single-rank pods alone do not.
func TestCapacity_ADataParallelPodsBusyMinutesAreALowerBound(t *testing.T) {
	for _, ranks := range [][2]int{{1, 1}, {2, 1}, {1, 4}} {
		o := NewObserver()
		var est Estimate
		for i := range 5 {
			a, b := queued("p-0", 24000, 3), queued("p-1", 24000, 3)
			a.EngineRanks, b.EngineRanks = ranks[0], ranks[1]
			c := cycle(t0.Add(time.Duration(i)*minute), a, b)
			c.QueueMin = map[string]float64{"p-0": 2, "p-1": 2}
			est = o.Observe(c)
		}
		multi := ranks[0] > 1 || ranks[1] > 1
		assert.Equal(t, SourceBusyMinutes, est.VPSource, "ranks %v", ranks)
		assert.Equal(t, 24000.0, est.VP, "ranks %v", ranks)
		assert.Equal(t, multi, est.VPLowerBound, "ranks %v", ranks)
		if multi {
			assert.Equal(t, DataParallelRanks, est.VPLowerBoundReason, "ranks %v", ranks)
		} else {
			assert.Empty(t, est.VPLowerBoundReason, "ranks %v", ranks)
		}
	}
}

// Busy minutes are read over the last BusyWindow only, thirty minutes (by
// literal: the value the proposal states): five busy minutes from t0 to
// t0+4m are V_P's source at t0+30m, the oldest exactly thirty minutes old,
// and a second later the oldest has left and four are not a source.
func TestCapacity_BusyMinutesOlderThanTheWindowAreExcluded(t *testing.T) {
	for _, tc := range []struct {
		at   time.Duration
		busy int
		want Source
	}{
		{30 * time.Minute, 5, SourceBusyMinutes},
		{30*time.Minute + time.Second, 4, SourcePeakUnderQueue},
		{34*time.Minute + time.Second, 0, SourcePeakUnderQueue},
	} {
		cs := queuedMinutes(30000, 31000, 29000, 30500, 29500)
		for i := range cs {
			cs[i].QueueMin = map[string]float64{"p-0": 1}
		}
		// An unqueued row moves the clock on and adds no busy minute.
		cs = append(cs, cycle(t0.Add(tc.at), base("p-0")))
		est := run(NewObserver(), cs...)
		assert.Equal(t, tc.busy, est.VPBusyMinutes, "at %v", tc.at)
		assert.Equal(t, tc.want, est.VPSource, "at %v", tc.at)
	}
}

// QueueMins takes a row's queue minimum only where QueueMinKnown is set, a
// zero included, and is nil when no row read one.
func TestQueueMins_TakesOnlyAReading(t *testing.T) {
	read := base("p-0")
	read.QueueMin, read.QueueMinKnown = 2, true
	emptied := base("p-1")
	emptied.QueueMinKnown = true
	unread := base("p-2")
	unread.QueueMin = 5
	assert.Equal(t, map[string]float64{"p-0": 2, "p-1": 0}, QueueMins([]domain.ReplicaMetrics{read, emptied, unread}))
	assert.Nil(t, QueueMins([]domain.ReplicaMetrics{unread, base("p-3")}))
	assert.Nil(t, QueueMins(nil))
}

// The back-pressure bound is the last source, and needs MinBoundSamples.
func TestCapacity_TheBackPressureBound(t *testing.T) {
	for _, samples := range []int{MinBoundSamples - 1, MinBoundSamples} {
		c := cycle(t0, base("p-0"))
		c.Bound = Bound{Tokens: 20000, Samples: samples}
		est := NewObserver().Observe(c)
		if samples < MinBoundSamples {
			assert.Equal(t, NoPrefillCapacity, est.VPRefused)
			continue
		}
		assert.Equal(t, SourceBackPressureBound, est.VPSource)
		assert.True(t, est.VPLowerBound)
		assert.Equal(t, 20000.0, est.VP)
	}
}

// SGLang publishes no computed-token series: V_P is refused whatever the
// rows, the queue or the bound say, and so is f.
func TestCapacity_SGLangIsRefused(t *testing.T) {
	o := NewObserver()
	var est Estimate
	for i := range 8 {
		c := cycle(t0.Add(time.Duration(i)*minute), queued("p-0", 30000, 3), queued("p-1", 30000, 3))
		c.Engine = inferenceengine.EngineSGLang
		c.QueueMin = map[string]float64{"p-0": 2, "p-1": 2}
		c.Bound = Bound{Tokens: 25000, Samples: 5}
		for j := range c.Rows {
			c.Rows[j].RequestRate = 10
		}
		est = o.Observe(c)
	}
	assert.Equal(t, SGLangRefused, est.VPRefused)
	assert.Zero(t, est.VP)
	assert.Equal(t, NoComputedTokens, est.FRefused)
	assert.Zero(t, est.F)
}

// A minute with no queue never raises V_P, however fast it read: on random
// histories of queued minutes, inserting unqueued minutes at up to ten times
// the true capacity leaves V_P where it was or lower (an unqueued minute
// breaks a run). And V_P is never above the capacity the queued minutes ran
// at.
func TestCapacity_PropertyAnUnqueuedMinuteNeverRaisesIt(t *testing.T) {
	for seed := range uint64(300) {
		rng := rand.New(rand.NewPCG(seed, 7))
		capacity := 5000 + rng.Float64()*50000
		var hist []domain.ReplicaMetrics
		var mixed []domain.ReplicaMetrics
		for range 4 + rng.IntN(20) {
			// A work-conserving pod under a queue computes at most its
			// capacity; without one, at the offered load.
			if rng.Float64() < 0.6 {
				rm := queued("p-0", capacity*(0.5+0.5*rng.Float64()), 1+rng.IntN(10))
				hist = append(hist, rm)
				mixed = append(mixed, rm)
			}
			if rng.Float64() < 0.4 {
				rm := queued("p-0", capacity*10*rng.Float64(), 0)
				mixed = append(mixed, rm)
			}
		}
		feed := func(rows []domain.ReplicaMetrics) Estimate {
			cs := make([]Cycle, len(rows))
			for i, rm := range rows {
				cs[i] = cycle(time.Time{}, rm)
			}
			return run(NewObserver(), cs...)
		}
		clean, noisy := feed(hist), feed(mixed)
		require.LessOrEqual(t, noisy.VP, clean.VP, "seed %d: an unqueued minute raised V_P", seed)
		require.LessOrEqual(t, clean.VP, capacity, "seed %d: V_P above the capacity", seed)
	}
}

// --- a ---

// lowLoadMinutes is one low-load cycle per prompt length, each minute, on a
// pod whose service time is s0 + c*I and whose overhead is 2.5 ms.
func lowLoadMinutes(s0, c float64, prompts ...float64) []Cycle {
	out := make([]Cycle, len(prompts))
	for i, p := range prompts {
		out[i] = cycle(time.Time{}, lowLoad(p, s0+c*p, 1, 0.0025))
	}
	return out
}

// steadyPrompts is each prompt length held for four minutes. The first
// level's first minute has no history and is not steady; every later level
// steady-gates its first two minutes out and keeps two: seven minutes over
// three levels.
func steadyPrompts(levels ...float64) []float64 {
	out := make([]float64, 0, 4*len(levels))
	for _, l := range levels {
		out = append(out, l, l, l, l)
	}
	return out
}

// The low-load fit recovers the intercept and the slope of a pod's service
// time, and a adds the least overhead.
func TestFixed_RecoversTheLine(t *testing.T) {
	est := run(NewObserver(), lowLoadMinutes(0.008, 2e-6, steadyPrompts(1000, 3000, 6000)...)...)
	require.Empty(t, est.ARefused)
	assert.InDelta(t, 0.008, est.AIntercept, 1e-12)
	assert.InDelta(t, 0.0025, est.AOverhead, 1e-12)
	assert.True(t, est.AOverheadKnown)
	assert.InDelta(t, 0.0105, est.A, 1e-12)
	assert.InDelta(t, 500000, est.SlopeBound, 1e-3)
	assert.Equal(t, 1000.0, est.APromptMin)
	assert.Equal(t, 6000.0, est.APromptMax)
}

// Loaded minutes are not low load: minutes whose occupancy is above
// LowLoadOccupancy, with their service time stretched by shared steps, leave
// a where the low-load minutes put it. So do minutes with a queue.
func TestFixed_LoadedAndQueuedMinutesAreExcluded(t *testing.T) {
	cs := lowLoadMinutes(0.008, 2e-6, steadyPrompts(1000, 3000, 6000)...)
	for i := range 6 {
		// 30 requests/s at 3x the service time: occupancy above 0.1.
		loaded := lowLoad(6000, 3*(0.008+2e-6*6000), 30, 0.0025)
		cs = append(cs, cycle(time.Time{}, loaded))
		if i%2 == 0 {
			q := lowLoad(6000, 3*(0.008+2e-6*6000), 0.5, 0.0025)
			q.QueueLength = 2
			cs = append(cs, cycle(time.Time{}, q))
		}
	}
	est := run(NewObserver(), cs...)
	require.Empty(t, est.ARefused)
	assert.InDelta(t, 0.0105, est.A, 1e-12)
}

// A prompt length that just moved is not steady: the minute after a step,
// whose service time still carries the old level, is kept out of the fit.
func TestFixed_AStepInPromptLengthIsNotSteady(t *testing.T) {
	prompts := steadyPrompts(1000, 3000, 6000)
	cs := make([]Cycle, 0, len(prompts))
	for _, l := range prompts {
		cs = append(cs, cycle(time.Time{}, lowLoad(l, 0.008+2e-6*l, 1, 0.0025)))
	}
	// The first minute of each new level reads the previous level's service
	// time: a fit that took it would move.
	cs[4].Rows[0].AvgServiceTime = 0.008 + 2e-6*1000
	cs[8].Rows[0].AvgServiceTime = 0.008 + 2e-6*3000
	cs[4].Rows[0].AvgTTFT = cs[4].Rows[0].AvgServiceTime + 0.0025
	cs[8].Rows[0].AvgTTFT = cs[8].Rows[0].AvgServiceTime + 0.0025
	est := run(NewObserver(), cs...)
	require.Empty(t, est.ARefused)
	assert.InDelta(t, 0.0105, est.A, 1e-12)
	assert.Equal(t, 7, est.ASamples)
}

// A row with no prompt history to compare against is not steady: the
// replica's first row, and its first after SteadyFor with none.
func TestFixed_NoPromptHistoryIsNotSteady(t *testing.T) {
	ps := &podState{}
	assert.False(t, ps.steady(1000, t0), "no history at all")
	ps.notePrompt(1000, t0)
	assert.False(t, ps.steady(1000, t0), "a prompt read at now is not history")
	assert.True(t, ps.steady(1050, t0.Add(minute)), "within MaxPromptDrift of the last minute")
	assert.False(t, ps.steady(1200, t0.Add(minute)), "beyond it")
	assert.False(t, ps.steady(1000, t0.Add(SteadyFor+time.Second)), "the history is older than SteadyFor")
}

// The refusals of a, each by its state.
func TestFixed_Refusals(t *testing.T) {
	t.Run("too few", func(t *testing.T) {
		est := run(NewObserver(), lowLoadMinutes(0.008, 2e-6, 1000, 6000, 6000)...)
		assert.Equal(t, TooFewLowLoadMinutes, est.ARefused)
		assert.Zero(t, est.A)
	})
	t.Run("no spread", func(t *testing.T) {
		est := run(NewObserver(), lowLoadMinutes(0.008, 2e-6, 1000, 1000, 1000, 1100, 1100, 1100)...)
		assert.Equal(t, NoPromptSpread, est.ARefused)
	})
	t.Run("non-positive slope", func(t *testing.T) {
		est := run(NewObserver(), lowLoadMinutes(0.02, -1e-6, steadyPrompts(1000, 6000)...)...)
		assert.Equal(t, NonPositiveSlope, est.ARefused)
		assert.Zero(t, est.SlopeBound)
	})
	t.Run("non-positive intercept", func(t *testing.T) {
		est := run(NewObserver(), lowLoadMinutes(-0.001, 2e-6, steadyPrompts(1000, 6000)...)...)
		assert.Equal(t, NonPositiveIntercept, est.ARefused)
		assert.Zero(t, est.A)
	})
	t.Run("aggregated", func(t *testing.T) {
		cs := lowLoadMinutes(0.008, 2e-6, steadyPrompts(1000, 3000, 6000)...)
		for i := range cs {
			cs[i].Disaggregated = false
		}
		est := run(NewObserver(), cs...)
		assert.Equal(t, AggregatedRefused, est.ARefused)
		assert.Zero(t, est.A)
	})
}

// With no TTFT read, a is the intercept alone, and says so.
func TestFixed_NoTTFTIsTheInterceptAlone(t *testing.T) {
	cs := lowLoadMinutes(0.008, 2e-6, steadyPrompts(1000, 3000, 6000)...)
	for i := range cs {
		cs[i].Rows[0].AvgTTFT = 0
	}
	est := run(NewObserver(), cs...)
	require.Empty(t, est.ARefused)
	assert.False(t, est.AOverheadKnown)
	assert.InDelta(t, 0.008, est.A, 1e-12)
}

// On random pods the fit gives back s0 plus the least overhead.
func TestFixed_PropertyRecoversRandomPods(t *testing.T) {
	for seed := range uint64(200) {
		rng := rand.New(rand.NewPCG(seed, 11))
		s0, c := 0.001+0.02*rng.Float64(), 1e-7+5e-6*rng.Float64()
		lo := 200 + 2000*rng.Float64()
		// Each level more than MaxPromptDrift from the one before it, so the
		// steady gate keeps the minutes steadyPrompts says.
		levels := []float64{lo, lo * (1.5 + 3*rng.Float64()), lo * (1.2 + 0.1*rng.Float64())}
		prompts := steadyPrompts(levels...)
		cs := make([]Cycle, 0, len(prompts))
		least := math.Inf(1)
		for i, l := range prompts {
			ov := 0.002 + 0.01*rng.Float64()
			// The minutes the steady gate admits (steadyPrompts): not the
			// first, which has no history.
			if (i > 0 && i < 4) || i%4 >= 2 {
				least = math.Min(least, ov)
			}
			cs = append(cs, cycle(time.Time{}, lowLoad(l, s0+c*l, 0.5, ov)))
		}
		est := run(NewObserver(), cs...)
		require.Empty(t, est.ARefused, "seed %d", seed)
		require.InDelta(t, s0, est.AIntercept, 1e-9, "seed %d", seed)
		require.InDelta(t, 1/c, est.SlopeBound, 1e-6/c, "seed %d", seed)
		require.InDelta(t, least, est.AOverhead, 1e-12, "seed %d", seed)
	}
}

// --- settle ---

// A replica inside its settle is not admitted: a pod that joins is refused
// for SteadyFor, then admitted.
func TestSettle_AJoiningReplicaWaits(t *testing.T) {
	tr := settle.NewTracker()
	tr.SetInterval(15 * time.Second)
	o := NewObserver()
	roles := map[string]string{"p": domain.RolePrefill}
	var settling, admitted int
	for i := range 12 {
		now := t0.Add(time.Duration(i) * 15 * time.Second)
		rm := lowLoad(1000, 0.01, 1, 0.003)
		rm.ReadySince = t0
		tr.Observe("ns/m", settlerow.Of([]domain.ReplicaMetrics{rm}, roles), now)
		c := cycle(now, rm)
		c.Settle = &Settle{View: tr, Model: "ns/m"}
		est := o.Observe(c)
		if now.Sub(t0) < SteadyFor {
			assert.Equal(t, 1, est.Refused[rowSettling], "at %v", now.Sub(t0))
			settling++
			continue
		}
		assert.Equal(t, 1, est.Admitted, "at %v", now.Sub(t0))
		admitted++
	}
	assert.Equal(t, 8, settling)
	assert.Equal(t, 4, admitted)
}

// A replica the tracker does not know is not settled.
func TestSettle_AnUnknownReplicaIsNotAdmitted(t *testing.T) {
	c := cycle(t0, lowLoad(1000, 0.01, 1, 0.003))
	c.Settle = &Settle{View: settle.NewTracker(), Model: "ns/m"}
	est := NewObserver().Observe(c)
	assert.Zero(t, est.Admitted)
	assert.Equal(t, 1, est.Refused[rowSettling])
}

// A replica that is not Ready, and a bridge, are refused.
func TestSettle_NotReadyAndBridgesAreRefused(t *testing.T) {
	nr := base("p-0")
	nr.Ready = false
	br := base("p-1")
	br.FromWarmPool = true
	est := NewObserver().Observe(cycle(t0, nr, br))
	assert.Equal(t, 1, est.Refused[rowNotReady])
	assert.Equal(t, 1, est.Refused[rowBridge])
	assert.Zero(t, est.Admitted)
}

// --- f ---
//
// The imbalance tests are seeded, and what each seed reads depends on the
// order sharedMinutes and poisson draw from the generator: per minute, per
// pod, Knuth's uniforms in turn (and, with variable prompts, one exponential
// per request after the count). A change to either order changes every
// figure the bounds below were set against; re-derive them over the seeds
// rather than widening a bound for one.

// sharedMinutes is minutes cycles over pods whose weights split m requests
// per pod per minute on average, each request of 1000 tokens, the counts
// Poisson.
func sharedMinutes(rng *rand.Rand, minutes int, m float64, weights ...float64) []Cycle {
	total := 0.0
	for _, w := range weights {
		total += w
	}
	n := float64(len(weights))
	out := make([]Cycle, minutes)
	for i := range out {
		rows := make([]domain.ReplicaMetrics, len(weights))
		for j, w := range weights {
			count := poisson(rng, m*n*w/total)
			rm := base("p-" + string(rune('a'+j)))
			rm.RequestRate = float64(count) / 60
			rm.PrefillComputedTokenRate = float64(count) * 1000 / 60
			rows[j] = rm
		}
		out[i] = cycle(time.Time{}, rows...)
	}
	return out
}

// poisson draws a Poisson(lambda) count (Knuth's method, in pieces of at
// most 500 so exp(-lambda) does not underflow).
func poisson(rng *rand.Rand, lambda float64) int {
	k := 0
	for lambda > 0 {
		step := math.Min(lambda, 500)
		lambda -= step
		l, p := math.Exp(-step), 1.0
		for {
			p *= rng.Float64()
			if p <= l {
				break
			}
			k++
		}
	}
	return k
}

// Equally loaded pods read an imbalance of about 1: the busiest of four
// Poisson counts of 150 reads about 9% above the mean by chance, and the
// de-bias takes that off.
func TestImbalance_EqualPodsReadAboutOne(t *testing.T) {
	for seed := range uint64(20) {
		rng := rand.New(rand.NewPCG(seed, 3))
		est := run(NewObserver(), sharedMinutes(rng, 30, 150, 1, 1, 1, 1)...)
		require.Empty(t, est.FRefused)
		require.GreaterOrEqual(t, est.F, 1.0)
		require.Less(t, est.F, 1.04, "seed %d: equal pods read f %.3f", seed, est.F)
	}
}

// A pod with twice the others' share is measured: four pods weighted
// 2:1:1:1 have f = 4*2/5 = 1.6. The de-bias is the equal pods' chance
// excess, about 0.03 at 1500 requests a pod, so it reads a little under.
func TestImbalance_ABusierPodIsMeasured(t *testing.T) {
	for seed := range uint64(20) {
		rng := rand.New(rand.NewPCG(seed, 5))
		est := run(NewObserver(), sharedMinutes(rng, 30, 1500, 2, 1, 1, 1)...)
		require.Empty(t, est.FRefused)
		require.InDelta(t, 1.6, est.F, 0.05, "seed %d", seed)
	}
}

// At the fewest requests f is read at, the de-bias reads a real imbalance
// low: the busier pod is the busiest whatever the draw, and the equal pods'
// chance excess, about 0.10 for four pods at 100 requests, comes off it
// anyway. 2:1:1:1 (f 1.6) reads about 1.50 over seeds, against about 1.57 at
// 1500 (TestImbalance_ABusierPodIsMeasured). The bias is documented, not
// corrected (imbalance.go); this pins its direction and size.
func TestImbalance_ABusierPodReadsLowAtFewRequests(t *testing.T) {
	fs := make([]float64, 0, 20)
	for seed := range uint64(20) {
		rng := rand.New(rand.NewPCG(seed, 5))
		est := run(NewObserver(), sharedMinutes(rng, 30, MinRequestsPerPodMinute, 2, 1, 1, 1)...)
		require.Empty(t, est.FRefused)
		fs = append(fs, est.F)
	}
	assert.InDelta(t, 1.50, median(fs), 0.03, "the median over seeds")
	assert.Less(t, median(fs), 1.6-0.05, "it reads low, not true")
}

// Equal pods with variable prompt lengths read above 1: the share is of
// computed tokens, whose count per pod varies sqrt(1+Cs^2) times as much as
// the request count, and the de-bias takes off only the request count's
// excess. With exponential prompts (Cs^2 = 1) at 100 requests a pod, the
// excess left is about (sqrt(2)-1) of the Poisson one; every seed reads
// within 0.10 of 1, the median within that excess and 0.01.
func TestImbalance_EqualPodsWithVariablePromptsReadNearOne(t *testing.T) {
	const m = MinRequestsPerPodMinute
	left := (math.Sqrt2 - 1) * poissonMaxExcess(4, m)
	fs := make([]float64, 0, 20)
	for seed := range uint64(20) {
		rng := rand.New(rand.NewPCG(seed, 7))
		cs := make([]Cycle, 30)
		for i := range cs {
			rows := make([]domain.ReplicaMetrics, 4)
			for j := range rows {
				count := poisson(rng, m)
				tokens := 0.0
				for range count {
					tokens += 1000 * rng.ExpFloat64()
				}
				rm := base("p-" + string(rune('a'+j)))
				rm.RequestRate = float64(count) / 60
				rm.PrefillComputedTokenRate = tokens / 60
				rows[j] = rm
			}
			cs[i] = cycle(time.Time{}, rows...)
		}
		est := run(NewObserver(), cs...)
		require.Empty(t, est.FRefused)
		require.GreaterOrEqual(t, est.F, 1.0)
		require.Less(t, est.F, 1.10, "seed %d: equal pods read f %.3f", seed, est.F)
		fs = append(fs, est.F)
	}
	assert.Greater(t, median(fs), 1.01, "variable prompts read high: the median over seeds")
	assert.Less(t, median(fs), 1+left+0.01, "by about what the de-bias leaves")
}

// The refusals of f, each by its state.
func TestImbalance_Refusals(t *testing.T) {
	t.Run("one pod", func(t *testing.T) {
		rng := rand.New(rand.NewPCG(1, 1))
		est := run(NewObserver(), sharedMinutes(rng, 10, 400, 1)...)
		assert.Equal(t, OnePod, est.FRefused)
		assert.Zero(t, est.F)
	})
	t.Run("too few requests", func(t *testing.T) {
		rng := rand.New(rand.NewPCG(1, 1))
		est := run(NewObserver(), sharedMinutes(rng, 10, 50, 1, 1)...)
		assert.Equal(t, TooFewImbalanceMinutes, est.FRefused)
	})
	t.Run("too few minutes", func(t *testing.T) {
		rng := rand.New(rand.NewPCG(1, 1))
		est := run(NewObserver(), sharedMinutes(rng, MinImbalanceMinutes-1, 400, 1, 1)...)
		assert.Equal(t, TooFewImbalanceMinutes, est.FRefused)
		assert.Equal(t, MinImbalanceMinutes-1, est.FSamples)
	})
	t.Run("a settling pod voids the minute", func(t *testing.T) {
		rng := rand.New(rand.NewPCG(1, 1))
		cs := sharedMinutes(rng, 10, 400, 1, 1)
		tr := settle.NewTracker()
		for i := range cs {
			// The tracker knows only the first pod: the second is never
			// settled, and no minute is a share of the variant's work.
			cs[i].Settle = &Settle{View: tr, Model: "ns/m"}
			cs[i].Now = t0.Add(time.Duration(i) * minute)
			tr.Observe("ns/m", settlerow.Of(cs[i].Rows[:1], map[string]string{"p": domain.RolePrefill}), cs[i].Now.Add(-10*minute))
			tr.Observe("ns/m", settlerow.Of(cs[i].Rows[:1], map[string]string{"p": domain.RolePrefill}), cs[i].Now)
		}
		est := run(NewObserver(), cs...)
		assert.Zero(t, est.FSamples)
		assert.Equal(t, TooFewImbalanceMinutes, est.FRefused)
	})
}

// The expected max of n Poisson counts, against the sum done by brute force
// for small cases, and its shape: none for one pod, falling with the count,
// rising with the pods.
func TestPoissonMaxExcess(t *testing.T) {
	brute := func(n int, m float64) float64 {
		// E[max] = sum over k of 1 - F(k)^n, F summed term by term.
		cdf, pmf, sum := 0.0, math.Exp(-m), 0.0
		for k := 0; k < 200; k++ {
			if k > 0 {
				pmf *= m / float64(k)
			}
			cdf += pmf
			sum += 1 - math.Pow(cdf, float64(n))
		}
		return sum/m - 1
	}
	for _, tc := range []struct {
		n int
		m float64
	}{{2, 1}, {3, 4.5}, {4, 20}, {8, 30}} {
		assert.InDelta(t, brute(tc.n, tc.m), poissonMaxExcess(tc.n, tc.m), 1e-9, "n=%d m=%v", tc.n, tc.m)
	}
	assert.Zero(t, poissonMaxExcess(1, 100))
	assert.Greater(t, poissonMaxExcess(4, 100), poissonMaxExcess(4, 1000))
	assert.Greater(t, poissonMaxExcess(8, 100), poissonMaxExcess(4, 100))
	assert.Zero(t, poissonMaxExcess(4, math.NaN()))
	assert.Zero(t, poissonMaxExcess(4, math.Inf(1)))
}

// --- NaN, Inf and missing inputs ---

// assertFinite fails when any float of est is NaN or infinite.
func assertFinite(t *testing.T, est Estimate, name string) {
	t.Helper()
	v := reflect.ValueOf(est)
	for i := range v.NumField() {
		if f := v.Field(i); f.Kind() == reflect.Float64 {
			x := f.Float()
			assert.False(t, math.IsNaN(x) || math.IsInf(x, 0), "%s: %s is %v", name, v.Type().Field(i).Name, x)
		}
	}
}

// Every input a row carries, NaN, infinite, negative or unread, gives a
// refusal or nothing, never a NaN or an infinite figure; and a figure learned
// before such rows is not moved by them.
func TestInputs_NaNInfAndMissing(t *testing.T) {
	bad := []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1, 0}
	mutate := map[string]func(*domain.ReplicaMetrics, float64){
		"computed rate":   func(rm *domain.ReplicaMetrics, x float64) { rm.PrefillComputedTokenRate = x },
		"request rate":    func(rm *domain.ReplicaMetrics, x float64) { rm.RequestRate = x },
		"service time":    func(rm *domain.ReplicaMetrics, x float64) { rm.AvgServiceTime = x },
		"ttft":            func(rm *domain.ReplicaMetrics, x float64) { rm.AvgTTFT = x },
		"recent prompt":   func(rm *domain.ReplicaMetrics, x float64) { rm.AvgInputTokensRecent = x },
		"prefix hit rate": func(rm *domain.ReplicaMetrics, x float64) { rm.PrefixCacheHitRate = x },
	}
	unread := map[string]func(*domain.ReplicaMetrics){
		"computed rate unread": func(rm *domain.ReplicaMetrics) { rm.PrefillComputedTokenRateKnown = false },
		"request rate unread":  func(rm *domain.ReplicaMetrics) { rm.RequestRateKnown = false },
	}
	learned := func() (*Observer, Estimate) {
		o := NewObserver()
		cs := lowLoadMinutes(0.008, 2e-6, steadyPrompts(1000, 3000, 6000)...)
		cs = append(cs, queuedMinutes(20000, 20000, 20000)...)
		est := run(o, cs...)
		require.Empty(t, est.ARefused)
		require.Equal(t, 20000.0, est.VP)
		return o, est
	}
	check := func(name string, rm domain.ReplicaMetrics, bound Bound) {
		// From nothing: refusals, no NaN.
		cs := make([]Cycle, 0, 8)
		for range 8 {
			c := cycle(time.Time{}, rm, rm)
			c.Rows[1].PodName = "p-1"
			c.Bound = bound
			cs = append(cs, c)
		}
		est := run(NewObserver(), cs...)
		assertFinite(t, est, name)
		// Over a learned figure: it stays.
		o, before := learned()
		for i := range 8 {
			c := cycle(t0.Add(time.Duration(20+i)*minute), rm)
			c.Bound = bound
			est = o.Observe(c)
		}
		assertFinite(t, est, name+" after learning")
		assert.InDelta(t, before.A, est.A, 1e-12, "%s moved a", name)
		assert.LessOrEqual(t, est.VP, before.VP, "%s raised V_P", name)
	}
	for name, set := range mutate {
		for _, x := range bad {
			for _, q := range []int{0, 3} {
				rm := lowLoad(2000, 0.012, 1, 0.003)
				rm.QueueLength = q
				set(&rm, x)
				check(name, rm, Bound{})
			}
		}
	}
	for name, set := range unread {
		rm := lowLoad(2000, 0.012, 1, 0.003)
		set(&rm)
		check(name, rm, Bound{})
	}
	for _, x := range bad {
		check("bound", base("p-0"), Bound{Tokens: x, Samples: 5})
	}
	// A bound that is not a reading is no source.
	c := cycle(t0, base("p-0"))
	c.Bound = Bound{Tokens: math.NaN(), Samples: 5}
	assert.Equal(t, NoPrefillCapacity, NewObserver().Observe(c).VPRefused)
}

// Each reading's own guard refuses what is not a reading, so no guard
// downstream is the only one: a computed-token rate or a request rate that
// is NaN, infinite or negative, or read with no presence bit, is none.
func TestInputs_EachReadingRefusesNaN(t *testing.T) {
	for _, x := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1} {
		rm := base("p-0")
		rm.PrefillComputedTokenRate, rm.RequestRate = x, x
		_, ok := computedRate(rm, false)
		assert.False(t, ok, "computed rate %v", x)
		_, ok = requestRate(rm)
		assert.False(t, ok, "request rate %v", x)
	}
	rm := base("p-0")
	rm.PrefillComputedTokenRate, rm.RequestRate = 1000, 1
	rm.PrefillComputedTokenRateKnown, rm.RequestRateKnown = false, false
	_, ok := computedRate(rm, false)
	assert.False(t, ok, "an unread computed rate")
	_, ok = requestRate(rm)
	assert.False(t, ok, "an unread request rate")
	rm.PrefillComputedTokenRateKnown = true
	_, ok = computedRate(rm, true)
	assert.False(t, ok, "SGLang's computed rate")
}

// --- retention ---

// Forget drops the keys under a prefix and only those; "" drops them all.
// A forgotten key starts again: its peak is gone.
func TestObserver_ForgetDropsTheKeysUnderAPrefix(t *testing.T) {
	o := NewObserver()
	for _, k := range []string{"ns|m|v|H100|1", "ns|m|v|H100|1#args", "ns|m|v2|H100|1"} {
		for i, c := range queuedMinutes(20000, 20000, 20000) {
			c.Key, c.Now = k, t0.Add(time.Duration(i)*minute)
			o.Observe(c)
		}
	}
	require.Equal(t, 3, o.Len())
	assert.Equal(t, 2, o.Forget("ns|m|v|"))
	assert.Equal(t, 1, o.Len(), "v2 is kept")

	c := queuedMinutes(10000)[0]
	c.Key, c.Now = "ns|m|v|H100|1", t0.Add(3*minute)
	est := o.Observe(c)
	assert.Equal(t, NoPrefillCapacity, est.VPRefused, "the forgotten key has no peak")

	assert.Equal(t, 2, o.Forget(""))
	assert.Zero(t, o.Len())
}

// A key with no cycle for Retention is forgotten; one observed is kept.
func TestObserver_ForgetsAKeyPastRetention(t *testing.T) {
	o := NewObserver()
	o.Observe(cycle(t0, base("p-0")))
	c := cycle(t0.Add(Retention/2), base("p-0"))
	c.Key = "other"
	o.Observe(c)
	assert.Equal(t, 2, o.Len())
	c.Now = t0.Add(Retention + time.Minute)
	o.Observe(c)
	assert.Equal(t, 1, o.Len())
}

// --- round 2 ---

// The expected max of n standard normals, against its closed forms for two
// pods (1/sqrt(pi)) and three (3/(2*sqrt(pi))), and the tabulated 1.029375
// for four.
func TestNormalMaxMean(t *testing.T) {
	assert.InDelta(t, 1/math.Sqrt(math.Pi), normalMaxMean(2), 1e-9)
	assert.InDelta(t, 1.5/math.Sqrt(math.Pi), normalMaxMean(3), 1e-9)
	assert.InDelta(t, 1.029375, normalMaxMean(4), 1e-6)
}

// At GaussianMaxMean the normal approximation agrees with the exact sum
// within 0.5% of the excess, for 2 to 64 pods (the worst measured, 0.33%,
// at 64; 0.09% at 4); the error left is the
// Poisson's skew, about (E[Z_max^2]-1)/(6m) against an excess of
// z_n/sqrt(m).
func TestPoissonMaxExcess_TheNormalApproximationAgreesAtTheThreshold(t *testing.T) {
	for _, n := range []int{2, 3, 4, 8, 16, 64} {
		exact := poissonMaxExcess(n, GaussianMaxMean)
		approx := normalMaxMean(n) / math.Sqrt(GaussianMaxMean)
		assert.InEpsilon(t, exact, approx, 0.005, "n=%d: exact %.6g, normal %.6g", n, exact, approx)
		t.Logf("n=%d: exact %.6g, normal %.6g, relative %.2g", n, exact, approx, math.Abs(approx-exact)/exact)
		// Just above it the approximation is what is returned.
		assert.Equal(t, normalMaxMean(n)/math.Sqrt(math.Nextafter(GaussianMaxMean, math.Inf(1))),
			poissonMaxExcess(n, math.Nextafter(GaussianMaxMean, math.Inf(1))))
	}
}

// A huge rate returns fast and finite: the exact sum would run about
// 24*sqrt(1e8) = 240,000 terms per call under the observer's lock.
func TestPoissonMaxExcess_AHugeRateIsFastAndFinite(t *testing.T) {
	start := time.Now()
	for range 100 {
		x := poissonMaxExcess(8, 1e8)
		require.True(t, isFinite(x))
		require.Greater(t, x, 0.0)
	}
	assert.Less(t, time.Since(start), time.Second, "100 calls at m = 1e8")
	assert.InDelta(t, normalMaxMean(8)/1e4, poissonMaxExcess(8, 1e8), 1e-12)
}

// The boundaries, by literal: the back-pressure bound is a source from two
// readings and not one, and busy minutes from five and not four.
func TestCapacity_TheThresholdsAreTwoBoundReadingsAndFiveBusyMinutes(t *testing.T) {
	for samples, want := range map[int]Source{1: "", 2: SourceBackPressureBound} {
		c := cycle(t0, base("p-0"))
		c.Bound = Bound{Tokens: 20000, Samples: samples}
		assert.Equal(t, want, NewObserver().Observe(c).VPSource, "%d bound readings", samples)
	}
	for minutes, want := range map[int]Source{4: SourcePeakUnderQueue, 5: SourceBusyMinutes} {
		rates := []float64{30000, 31000, 29000, 30500, 29500}[:minutes]
		cs := queuedMinutes(rates...)
		for i := range cs {
			cs[i].QueueMin = map[string]float64{"p-0": 1}
		}
		est := run(NewObserver(), cs...)
		assert.Equal(t, want, est.VPSource, "%d busy minutes", minutes)
		assert.Equal(t, minutes, est.VPBusyMinutes)
	}
}

// A refused V_P says which source fell short: the queued minutes, the longest
// run of them, and the busy minutes.
func TestCapacity_ARefusalSaysWhatWasSeen(t *testing.T) {
	cs := queuedMinutes(20000, 20000)
	cs = append(cs, cycle(time.Time{}, base("p-0")), queuedMinutes(20000)[0])
	est := run(NewObserver(), cs...)
	assert.Equal(t, NoPrefillCapacity, est.VPRefused)
	assert.Equal(t, 3, est.VPQueuedMinutes)
	assert.Equal(t, 2, est.VPLongestQueuedRun)
	assert.Zero(t, est.VPBusyMinutes, "no QueueMin was read")
}

// A replica keeps maxSamples samples. 400 queued minutes a minute apart end
// at minute 399, so Retention keeps minutes 39 to 399, 361 samples, and the
// cap keeps 360: the peak at minutes 39 to 41 is gone with minute 39, the
// one sample only the cap drops.
func TestObserver_KeepsMaxSamplesPerReplica(t *testing.T) {
	rates := make([]float64, 400)
	for i := range rates {
		rates[i] = 20000
		if i >= 39 && i < 39+PeakRun {
			rates[i] = 50000
		}
	}
	o := NewObserver()
	est := run(o, queuedMinutes(rates...)...)
	assert.Len(t, o.keys["k"].pods["p-0"].samples, maxSamples)
	assert.Equal(t, 360, maxSamples)
	assert.Equal(t, 20000.0, est.VP, "the peak older than the cap is gone")
}

// A sample older than Retention is pruned while its replica is still seen:
// a peak six hours old is not V_P.
func TestObserver_PrunesSamplesPastRetention(t *testing.T) {
	cs := queuedMinutes(50000, 50000, 50000)
	// Unqueued rows every 30 minutes keep the key and the replica.
	for at := 30 * minute; at <= Retention; at += 30 * minute {
		c := cycle(t0.Add(at), base("p-0"))
		cs = append(cs, c)
	}
	for i := range PeakRun {
		c := queuedMinutes(20000)[0]
		c.Now = t0.Add(Retention + time.Duration(5+i)*minute)
		cs = append(cs, c)
	}
	o := NewObserver()
	est := run(o, cs...)
	assert.Equal(t, 20000.0, est.VP)
	for _, s := range o.keys["k"].pods["p-0"].samples {
		assert.False(t, s.at.Before(t0.Add(5*minute)), "a sample past Retention is kept: %v", s.at)
	}
}

// A replica with no row for Retention is forgotten from its key, which
// another replica keeps alive.
func TestObserver_ForgetsAReplicaPastRetention(t *testing.T) {
	cs := queuedMinutes(50000, 50000, 50000)
	for at := 30 * minute; at <= Retention+30*minute; at += 30 * minute {
		cs = append(cs, cycle(t0.Add(at), base("p-1")))
	}
	o := NewObserver()
	est := run(o, cs...)
	assert.NotContains(t, o.keys["k"].pods, "p-0")
	assert.Contains(t, o.keys["k"].pods, "p-1")
	assert.Equal(t, NoPrefillCapacity, est.VPRefused, "the forgotten replica's peak went with it")
}

// Sweep forgets a key with no cycle for Retention without a cycle of its own,
// and says how many it forgot.
func TestObserver_SweepForgetsIdleKeys(t *testing.T) {
	o := NewObserver()
	o.Observe(cycle(t0, base("p-0")))
	assert.Zero(t, o.Sweep(t0.Add(Retention)))
	assert.Equal(t, 1, o.Len())
	assert.Equal(t, 1, o.Sweep(t0.Add(Retention+time.Second)))
	assert.Zero(t, o.Len())
}

// One outlying prompt length cannot make the spread: the second shortest and
// the second longest must span MinPromptSpread.
func TestFixed_OneOutlyingPromptIsNoSpread(t *testing.T) {
	cs := lowLoadMinutes(0.008, 2e-6, 1000, 1000, 1000, 1000, 1000, 1000)
	// One 6000-token minute, made steady by two loaded minutes before it at
	// the same length (occupancy 0.6, not low load, but their prompts are its
	// history, all of the last SteadyFor).
	loaded := lowLoad(6000, 0.02, 30, 0.0025)
	cs = append(cs, cycle(time.Time{}, loaded), cycle(time.Time{}, loaded),
		cycle(time.Time{}, lowLoad(6000, 0.008+2e-6*6000, 1, 0.0025)))
	est := run(NewObserver(), cs...)
	assert.Equal(t, NoPromptSpread, est.ARefused)
	assert.Equal(t, 6, est.ASamples, "the outlying minute is admitted")
	assert.Equal(t, 1000.0, est.APromptMin)
	assert.Equal(t, 6000.0, est.APromptMax)
}
