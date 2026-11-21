package saturation

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// The capacity planner's switches, through Analyze: off from the
// constructor, so the controller never runs them (TestPlannerSwitches_OffFromTheConstructor).

// TestPlannerSwitches_OffFromTheConstructor holds the eight planner-only
// switches off in a new analyzer: the controller's behaviour is P1's.
func TestPlannerSwitches_OffFromTheConstructor(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	fields := a.switchFields()
	for _, name := range []string{SwitchPrefillALowLoadPerRank, SwitchPrefillAEngineTiming, SwitchPrefillQueueByCapacity,
		SwitchPrefillFBusyMinutes, SwitchITLAdaptiveBandFloor, SwitchITLHoldSteadySlope, SwitchITLLineAtFloor,
		SwitchITLBatchTerm} {
		require.Contains(t, fields, name)
		assert.False(t, *fields[name], "%s is planner-only: off from the constructor", name)
	}
}

// busyGroupCycle runs one cycle of a P/D model whose prefill row is an LWS
// group of four busy engines: rr requests/s of ieff computed tokens, the
// prefill time a0 + b*ieff stretched by the per-engine occupancy, never low
// load. It returns the variant's estimate.
func (h *observeHarness) busyGroupCycle(rr, ieff float64) prefill.Estimate {
	h.t.Helper()
	states := []domain.VariantReplicaState{
		{VariantName: "decode", Role: domain.RoleDecode, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
		{VariantName: "prefill", Role: domain.RolePrefill, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 8},
	}
	const ranks, a0, perToken = 4, 0.25, 80e-6
	b, l := perToken*ieff, rr/ranks
	pt := (a0 + b) / (1 - b*l)
	dec := makeReplicaMetrics("decode-0", "decode", 5000, 16000, 0, 4000, 500)
	pre := makeReplicaMetrics("prefill-0", "prefill", 0, 16000, 2, ieff, 1)
	pre.EngineRanks = ranks
	pre.RequestRate, pre.RequestRateKnown = rr, true
	pre.PrefillComputedTokenRate, pre.PrefillComputedTokenRateKnown = rr*ieff, true
	pre.AvgServiceTime = pt
	pre.AvgPrefillTime, pre.AvgPrefillTimeKnown = pt, true
	pre.AvgQueueTime, pre.AvgQueueTimeKnown = 0.05, true
	pre.AvgTTFT, pre.AvgTTFTKnown = pt+0.05+0.4, true
	_, err := h.a.Analyze(h.ctx, makeAnalyzerInput([]domain.ReplicaMetrics{dec, pre}, states))
	require.NoError(h.t, err)
	h.clock = h.clock.Add(time.Minute)
	est, _, ok := h.a.PrefillEstimate("test-ns", "test-model", "prefill")
	require.True(h.t, ok)
	return est
}

// On, a busy group's a comes from its engines' prefill timing, flagged by
// its source; off, it is refused for too few low-load minutes, as P1
// shipped.
func TestPrefillAEngineTiming_ABusyGroupLearnsA(t *testing.T) {
	run := func(on bool) prefill.Estimate {
		h := newObserveHarness(t, &capacity.EngineParams{Engine: inferenceengine.EngineVLLM})
		require.NoError(t, h.a.SetSwitchesForReplay(map[string]bool{SwitchPrefillAEngineTiming: on}))
		rng := rand.New(rand.NewPCG(3, 5))
		var est prefill.Estimate
		for range 90 {
			est = h.busyGroupCycle(1+3*rng.Float64(), 1000+5000*rng.Float64())
		}
		return est
	}
	on := run(true)
	require.Empty(t, on.ARefused, "%+v", on)
	assert.Equal(t, prefill.ASourceEngineTiming, on.ASource)
	assert.InEpsilon(t, 0.25, on.AIntercept, 0.10)
	assert.InDelta(t, 0.4, on.AOverhead, 1e-9)

	off := run(false)
	assert.Equal(t, prefill.TooFewLowLoadMinutes, off.ARefused)
	assert.Zero(t, off.A)
}

// One cycle of a low-fill decode fleet: n readings on tracedModel from fill
// lo to hi.
func lowFillReadings(variant string, n int, lo, hi float64) []domain.ReplicaMetrics {
	out := make([]domain.ReplicaMetrics, 0, n)
	for i := range n {
		k := lo + (hi-lo)*float64(i)/float64(n-1)
		rm := makeReplicaMetrics(fmt.Sprintf("%s-%d", variant, i), variant, 5000, 16000, 0, 6000, 1000)
		rm.Ready = true
		rm.KvUsageInstant = k
		rm.AvgITL, rm.AvgITLKnown = tracedModel.ITLAt(k), true
		out = append(out, rm)
	}
	return out
}

// A synthetic fleet whose fill runs from 0.06 to 0.39 spreads too little
// inside the default band
// (from 0.15): on, the floor follows it down to 0.06 and the line is the
// fleet's own; off, the line is pinned on the default intercept.
func TestITLAdaptiveBandFloor_ALowFillFleetLearnsItsLine(t *testing.T) {
	const v = "decode-low"
	states := []domain.VariantReplicaState{
		{VariantName: v, Role: domain.RoleDecode, AcceleratorName: srAccel, CurrentReplicas: 10, GPUsPerReplica: 1},
	}
	run := func(on bool) (domain.ServiceRateSignal, *SaturationAnalyzer) {
		a := srAnalyzer(v)
		require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchITLAdaptiveBandFloor: on}))
		ctx, _ := observedCtx(t)
		res, err := a.Analyze(ctx, makeAnalyzerInput(lowFillReadings(v, 10, 0.06, 0.39), states))
		require.NoError(t, err)
		return srSignal(t, res, v), a
	}
	on, a := run(true)
	require.True(t, on.HasLine)
	assert.InDelta(t, tracedModel.A, on.LineSlope, 1e-6)
	assert.InDelta(t, tracedModel.B, on.LineIntercept, 1e-6)
	floor, policy, ok := a.ITLBandFloor(srNS, srModel)
	require.True(t, ok)
	assert.InDelta(t, 0.06, floor, 1e-12)
	assert.InDelta(t, 0.15, policy, 1e-12)

	off, a := run(false)
	assert.NotEqual(t, tracedModel.B, off.LineIntercept)
	if off.HasLine {
		assert.Equal(t, itl.DefaultBaselineSec, off.LineIntercept, "off, the line is pinned on the default B")
	}
	_, _, ok = a.ITLBandFloor(srNS, srModel)
	assert.False(t, ok)
}

// The rule: a fleet that reaches the spread inside the band, or has none to
// give below it, or fewer than adaptiveBandMinEngines engines below the
// policy's floor, keeps the policy's floor; one that has it only below is
// lowered to the 10th percentile of its fills, never under
// AdaptiveBandFloorMin, and held there.
func TestITLAdaptiveBandFloor_TheRule(t *testing.T) {
	at := time.Unix(0, 0)
	// fleet is one reading per engine, engine i at ks[i].
	fleet := func(ks ...float64) []engineFill {
		out := make([]engineFill, len(ks))
		for i, k := range ks {
			out[i] = engineFill{at: at, pod: fmt.Sprintf("e-%d", i), k: k}
		}
		return out
	}
	spread := fleet(0.07, 0.08, 0.09, 0.12, 0.15, 0.20, 0.25, 0.30, 0.35, 0.40)
	// One straggler at 0.01 among 19 engines from 0.08 up: the 10th
	// percentile is the second fill, 0.08, not the straggler's.
	straggler := make([]float64, 0, 20)
	straggler = append(straggler, 0.01)
	for i := range 19 {
		straggler = append(straggler, 0.08+0.32*float64(i)/18)
	}
	// Two engines below the floor, read on many cycles: readings enough,
	// engines too few.
	two := make([]engineFill, 0, 25)
	for range 10 {
		two = append(two, engineFill{at: at, pod: "e-0", k: 0.06}, engineFill{at: at, pod: "e-1", k: 0.07})
	}
	two = append(two, fleet(0.20, 0.25, 0.30, 0.35, 0.40)...)
	for i := range two[20:] {
		two[20+i].pod = fmt.Sprintf("f-%d", i)
	}
	for _, c := range []struct {
		name    string
		fills   []engineFill
		held    float64
		floor   float64
		adapted bool
	}{
		{"reaches the spread inside the band", fleet(0.05, 0.06, 0.07, 0.50), 0, 0, false},
		{"no spread below either", fleet(0.10, 0.11, 0.12, 0.30), 0, 0, false},
		{"low fill with the spread", spread, 0, 0.07, true},
		{"at the minimum", fleet(0.01, 0.02, 0.03, 0.20, 0.40), 0, AdaptiveBandFloorMin, true},
		{"one straggler sets nothing", fleet(straggler...), 0, 0.08, true},
		{"two engines below are not enough", two, 0, 0, false},
		{"the top is the band's", fleet(0.07, 0.08, 0.09, 0.95), 0, 0, false},
		{"no readings", nil, 0, 0, false},
		// An adapted floor holds through a range with no spread, and through
		// none, and goes lower, never higher.
		{"held through no spread", fleet(0.10, 0.11, 0.12, 0.30), 0.07, 0.07, true},
		{"held through no readings", nil, 0.07, 0.07, true},
		{"held, and lower", fleet(0.06, 0.07, 0.08, 0.40), 0.07, 0.06, true},
		{"held, not higher", fleet(0.09, 0.10, 0.11, 0.40), 0.07, 0.07, true},
	} {
		got, adapted := adaptedFloor(c.fills, 0.15, 0.80, bandFloor{floor: c.held})
		assert.Equal(t, c.adapted, adapted, c.name)
		assert.InDelta(t, c.floor, got.floor, 1e-12, c.name)
	}
}

// An adapted floor is released only after the fill spreads
// adaptiveBandReleaseSpread above the policy's floor for
// adaptiveBandReleaseCycles cycles in a row: a spread that only just reaches
// the adapting threshold holds it, and a cycle under the release spread
// starts the count over.
func TestITLAdaptiveBandFloor_ReleaseHasHysteresis(t *testing.T) {
	at := time.Unix(0, 0)
	reach := func(top float64) []engineFill {
		return []engineFill{{at: at, pod: "e-0", k: 0.10}, {at: at, pod: "e-1", k: top}}
	}
	held := bandFloor{floor: 0.07}
	// At the adapting spread (0.15 + 0.32), never released.
	for i := range 3 * adaptiveBandReleaseCycles {
		var ok bool
		held, ok = adaptedFloor(reach(0.47), 0.15, 0.80, held)
		require.True(t, ok, "cycle %d: a spread under the release spread releases nothing", i)
	}
	// Past the release spread: held for the cycles before the last.
	for i := range adaptiveBandReleaseCycles - 2 {
		var ok bool
		held, ok = adaptedFloor(reach(0.55), 0.15, 0.80, held)
		require.True(t, ok, "cycle %d", i)
	}
	// One cycle back under it starts the count over.
	held, ok := adaptedFloor(reach(0.47), 0.15, 0.80, held)
	require.True(t, ok)
	require.Zero(t, held.released)
	for i := range adaptiveBandReleaseCycles - 1 {
		held, ok = adaptedFloor(reach(0.55), 0.15, 0.80, held)
		require.True(t, ok, "cycle %d", i)
	}
	_, ok = adaptedFloor(reach(0.55), 0.15, 0.80, held)
	assert.False(t, ok, "released on the release spread's cycle %d in a row", adaptiveBandReleaseCycles)
}

// lowLoadGroupCycle runs one cycle of the harness's P/D model whose prefill
// row is a group of four engines, each at occupancy 0.08 on prompts of ieff
// tokens: 0.32 read whole. It returns the variant's estimate.
func (h *observeHarness) lowLoadGroupCycle(ieff float64) prefill.Estimate {
	h.t.Helper()
	states := []domain.VariantReplicaState{
		{VariantName: "decode", Role: domain.RoleDecode, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
		{VariantName: "prefill", Role: domain.RolePrefill, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 8},
	}
	svc := 0.010 + 1e-6*ieff
	rr := 4 * 0.08 / svc
	dec := makeReplicaMetrics("decode-0", "decode", 5000, 16000, 0, 4000, 500)
	pre := makeReplicaMetrics("prefill-0", "prefill", 0, 16000, 0, ieff, 1)
	pre.EngineRanks = 4
	pre.RequestRate, pre.RequestRateKnown = rr, true
	pre.PrefillComputedTokenRate, pre.PrefillComputedTokenRateKnown = rr*ieff, true
	pre.AvgServiceTime, pre.AvgTTFT = svc, svc+0.002
	_, err := h.a.Analyze(h.ctx, makeAnalyzerInput([]domain.ReplicaMetrics{dec, pre}, states))
	require.NoError(h.t, err)
	h.clock = h.clock.Add(time.Minute)
	est, _, ok := h.a.PrefillEstimate("test-ns", "test-model", "prefill")
	require.True(h.t, ok)
	return est
}

// SwitchPrefillALowLoadPerRank reaches the observation through Analyze: on,
// a group of four quiet engines is low load and a is learned; off, the row
// read whole is refused, as P1 shipped.
func TestPrefillALowLoadPerRank_AQuietGroupThroughAnalyze(t *testing.T) {
	run := func(on bool) prefill.Estimate {
		h := newObserveHarness(t, &capacity.EngineParams{Engine: inferenceengine.EngineVLLM})
		require.NoError(t, h.a.SetSwitchesForReplay(map[string]bool{SwitchPrefillALowLoadPerRank: on}))
		var est prefill.Estimate
		for _, l := range []float64{1000, 2000, 4000} {
			for range 4 {
				est = h.lowLoadGroupCycle(l)
			}
		}
		return est
	}
	on := run(true)
	require.Empty(t, on.ARefused, "%+v", on)
	assert.InDelta(t, 0.012, on.A, 1e-9)
	assert.Positive(t, on.ASamples)
	off := run(false)
	assert.Equal(t, prefill.TooFewLowLoadMinutes, off.ARefused)
	assert.Zero(t, off.ASamples)
}

// Through Analyze: a cycle that adapts the floor, a cycle with no spread that
// still reads it (the held floor, from the analyzer's own map), cycles that
// spread only to the adapting threshold and hold it, then
// adaptiveBandReleaseCycles cycles past the release spread that release it.
func TestITLAdaptiveBandFloor_HeldThenReleasedThroughAnalyze(t *testing.T) {
	const v = "decode-low"
	states := []domain.VariantReplicaState{
		{VariantName: v, Role: domain.RoleDecode, AcceleratorName: srAccel, CurrentReplicas: 10, GPUsPerReplica: 1},
	}
	a := srAnalyzer(v)
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchITLAdaptiveBandFloor: true}))
	ctx, _ := observedCtx(t)
	cycle := func(lo, hi float64) (float64, bool) {
		t.Helper()
		_, err := a.Analyze(ctx, makeAnalyzerInput(lowFillReadings(v, 10, lo, hi), states))
		require.NoError(t, err)
		floor, _, ok := a.ITLBandFloor(srNS, srModel)
		return floor, ok
	}
	floor, ok := cycle(0.06, 0.39)
	require.True(t, ok, "the first cycle adapts")
	require.InDelta(t, 0.06, floor, 1e-12)
	floor, ok = cycle(0.20, 0.25)
	require.True(t, ok, "a cycle with no spread reads the held floor")
	assert.InDelta(t, 0.06, floor, 1e-12)
	for i := range adaptiveBandReleaseCycles - 1 {
		floor, ok = cycle(0.20, 0.55)
		require.True(t, ok, "cycle %d past the release spread still holds", i)
		assert.InDelta(t, 0.06, floor, 1e-12)
	}
	_, ok = cycle(0.20, 0.55)
	assert.False(t, ok, "released on the release spread's cycle %d in a row", adaptiveBandReleaseCycles)
}

// deferredPrefillCycle runs one cycle of a P/D model whose prefill pod
// computes 10k tok/s with three requests waiting all minute, none of them for
// capacity: deferred, as while a cached prefix loads. It returns the
// variant's estimate.
func (h *observeHarness) deferredPrefillCycle() prefill.Estimate {
	h.t.Helper()
	states := []domain.VariantReplicaState{
		{VariantName: "decode", Role: domain.RoleDecode, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
		{VariantName: "prefill", Role: domain.RolePrefill, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	dec := makeReplicaMetrics("decode-0", "decode", 5000, 16000, 0, 4000, 500)
	pre := makeReplicaMetrics("prefill-0", "prefill", 0, 16000, 3, 2000, 1)
	pre.RequestRate, pre.RequestRateKnown = 5, true
	pre.PrefillComputedTokenRate, pre.PrefillComputedTokenRateKnown = 10000, true
	pre.QueueMin, pre.QueueMinKnown = 2, true
	pre.CapacityQueueLength, pre.CapacityQueueKnown = 0, true
	_, err := h.a.Analyze(h.ctx, makeAnalyzerInput([]domain.ReplicaMetrics{dec, pre}, states))
	require.NoError(h.t, err)
	h.clock = h.clock.Add(time.Minute)
	est, _, ok := h.a.PrefillEstimate("test-ns", "test-model", "prefill")
	require.True(h.t, ok)
	return est
}

// Through Analyze: on, a prefill pod whose waiting requests are all deferred
// never queued for compute, and V_P is refused; off, its minutes are busy and
// V_P is the load it served.
func TestPrefillQueueByCapacity_DeferredWaitsThroughAnalyze(t *testing.T) {
	run := func(on bool) prefill.Estimate {
		h := newObserveHarness(t, &capacity.EngineParams{Engine: inferenceengine.EngineVLLM})
		require.NoError(t, h.a.SetSwitchesForReplay(map[string]bool{SwitchPrefillQueueByCapacity: on}))
		var est prefill.Estimate
		for range 10 {
			est = h.deferredPrefillCycle()
		}
		return est
	}
	on := run(true)
	assert.Equal(t, prefill.NoPrefillCapacity, on.VPRefused, "%+v", on)
	assert.Zero(t, on.VP)

	off := run(false)
	assert.Empty(t, off.VPRefused, "%+v", off)
	assert.Equal(t, 10000.0, off.VP)
}

// twoPrefillCycle runs one cycle of a P/D model with two prefill pods that
// serve perPod requests a minute between them on average, the first w times
// the second's share. It returns the variant's estimate.
func (h *observeHarness) twoPrefillCycle(perPod, w float64) prefill.Estimate {
	h.t.Helper()
	states := []domain.VariantReplicaState{
		{VariantName: "decode", Role: domain.RoleDecode, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
		{VariantName: "prefill", Role: domain.RolePrefill, AcceleratorName: "H100", CurrentReplicas: 2, GPUsPerReplica: 1},
	}
	rows := make([]domain.ReplicaMetrics, 0, 3)
	rows = append(rows, makeReplicaMetrics("decode-0", "decode", 5000, 16000, 0, 4000, 500))
	for i, share := range []float64{w, 1} {
		pre := makeReplicaMetrics(fmt.Sprintf("prefill-%d", i), "prefill", 0, 16000, 0, 2000, 1)
		rr := 2 * perPod / 60 * share / (w + 1)
		pre.RequestRate, pre.RequestRateKnown = rr, true
		pre.PrefillComputedTokenRate, pre.PrefillComputedTokenRateKnown = rr*2000, true
		rows = append(rows, pre)
	}
	_, err := h.a.Analyze(h.ctx, makeAnalyzerInput(rows, states))
	require.NoError(h.t, err)
	h.clock = h.clock.Add(time.Minute)
	est, _, ok := h.a.PrefillEstimate("test-ns", "test-model", "prefill")
	require.True(h.t, ok)
	return est
}

// Through Analyze: twenty light minutes at 3:1 and ten busy ones at 1.1:1.
// On, f is the busy minutes' (raw 1.048, less the chance excess); off, the
// light minutes set the median (raw 1.5).
func TestPrefillFBusyMinutes_LightMinutesThroughAnalyze(t *testing.T) {
	run := func(on bool) prefill.Estimate {
		h := newObserveHarness(t, &capacity.EngineParams{Engine: inferenceengine.EngineVLLM})
		require.NoError(t, h.a.SetSwitchesForReplay(map[string]bool{SwitchPrefillFBusyMinutes: on}))
		for range 20 {
			h.twoPrefillCycle(150, 3)
		}
		var est prefill.Estimate
		for range 10 {
			est = h.twoPrefillCycle(1500, 1.1)
		}
		return est
	}
	on, off := run(true), run(false)
	require.Empty(t, on.FRefused, "%+v", on)
	require.Empty(t, off.FRefused, "%+v", off)
	assert.InDelta(t, 1.033, on.F, 0.005)
	assert.InDelta(t, 1.454, off.F, 0.01)
	assert.Equal(t, off.FSamples, on.FSamples, "the same minutes are kept")
}

// holdRampCycle runs one decode cycle of three replicas at fill k reading
// ITL itl, at the analyzer's clock, and returns the variant's line.
func holdRampCycle(t *testing.T, a *SaturationAnalyzer, v string, k, itlSec float64) domain.ServiceRateSignal {
	t.Helper()
	states := []domain.VariantReplicaState{
		{VariantName: v, Role: domain.RoleDecode, AcceleratorName: srAccel, CurrentReplicas: 3, GPUsPerReplica: 1},
	}
	rows := make([]domain.ReplicaMetrics, 0, 3)
	for i := range 3 {
		rm := makeReplicaMetrics(fmt.Sprintf("%s-%d", v, i), v, int64(k*16000), 16000, 0, 6000, 1000)
		rm.Ready = true
		rm.KvUsageInstant = k
		rm.AvgITL, rm.AvgITLKnown = itlSec, itlSec > 0
		rows = append(rows, rm)
	}
	ctx, _ := observedCtx(t)
	res, err := a.Analyze(ctx, makeAnalyzerInput(rows, states))
	require.NoError(t, err)
	return srSignal(t, res, v)
}

// Through Analyze, issue llm-d-incubation/llm-d-scaling-manager#266: a
// plateau at fill 0.75, a ramp-down whose ITL still reads the plateau's, and
// a long idle. On, the line keeps the plateau's slope to the end; off, the
// ramp-down's readings are fitted once the plateau's steady ones are gone,
// and the line ends steeper.
func TestITLHoldSteadySlope_ARampDownThroughAnalyze(t *testing.T) {
	const v = "decode-ramp"
	run := func(on bool) (last, plateau domain.ServiceRateSignal) {
		a := srAnalyzer(v)
		require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchITLHoldSteadySlope: on}))
		clock := propStart
		a.now = func() time.Time { return clock }
		itlSec := tracedModel.ITLAt(0.75)
		for range 40 {
			plateau = holdRampCycle(t, a, v, 0.75, itlSec)
			clock = clock.Add(15 * time.Second)
		}
		for _, k := range []float64{0.6, 0.5, 0.4, 0.3} {
			last = holdRampCycle(t, a, v, k, itlSec)
			clock = clock.Add(15 * time.Second)
		}
		for range 160 {
			last = holdRampCycle(t, a, v, 0, 0)
			clock = clock.Add(15 * time.Second)
		}
		return last, plateau
	}
	on, onPlateau := run(true)
	off, offPlateau := run(false)
	require.True(t, onPlateau.HasLine, "premise: the plateau fits a line")
	require.InDelta(t, onPlateau.LineSlope, offPlateau.LineSlope, 1e-12, "premise: the switch moves nothing on the plateau")
	require.True(t, on.HasLine)
	require.True(t, off.HasLine)
	assert.InDelta(t, onPlateau.LineSlope, on.LineSlope, 1e-9, "on, the plateau's slope holds")
	assert.Greater(t, off.LineSlope, 1.3*offPlateau.LineSlope, "off, the ramp-down's slope is fitted")
}
