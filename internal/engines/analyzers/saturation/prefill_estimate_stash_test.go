package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// The stash of P1's estimates for the slo analyzer
// (docs/proposals/slo-tiers.md, "Where P1's figures come from"; S1 test 1):
// observePrefill writes each prefill variant's estimate, stamped with the
// cycle; forgetVariant and the eviction sweep delete it.

// Each cycle writes the prefill variant's estimate, stamped with the
// analyzer's clock at the cycle, and the estimate is the one logged. The
// decode variant gets none.
func TestPrefillEstimate_EachCycleStashesTheVariantsEstimate(t *testing.T) {
	h := newObserveHarness(t, &capacity.EngineParams{Engine: inferenceengine.EngineVLLM})
	_, _, ok := h.a.PrefillEstimate("test-ns", "test-model", "prefill")
	require.False(t, ok, "precondition: nothing before a cycle")
	var at time.Time
	for range 5 {
		at = h.clock
		h.cycle(20000)
	}
	est, stamped, ok := h.a.PrefillEstimate("test-ns", "test-model", "prefill")
	require.True(t, ok)
	assert.Equal(t, at, stamped, "stamped with the cycle that wrote it")
	assert.InDelta(t, 20000, est.VP, 1e-9)
	assert.Equal(t, prefill.SourcePeakUnderQueue, est.VPSource)
	_, _, ok = h.a.PrefillEstimate("test-ns", "test-model", "decode")
	assert.False(t, ok, "a decode variant is not observed")
}

// The switch off stashes nothing: with prefill-capacity-observe off, every
// prefill variant reads no estimate.
func TestPrefillEstimate_TheSwitchOffStashesNothing(t *testing.T) {
	h := newObserveHarness(t, &capacity.EngineParams{Engine: inferenceengine.EngineVLLM})
	require.NoError(t, h.a.SetSwitchesForReplay(map[string]bool{SwitchPrefillCapacityObserve: false}))
	prefillObserveRunOn(t, h, 3)
	_, _, ok := h.a.PrefillEstimate("test-ns", "test-model", "prefill")
	assert.False(t, ok)
}

// prefillObserveRunOn runs cycles on h without requiring an observed line.
func prefillObserveRunOn(t *testing.T, h *observeHarness, cycles int) {
	t.Helper()
	for range cycles {
		h.lines = nil
		_, err := h.a.Analyze(h.ctx, observeHarnessInput(20000))
		require.NoError(t, err)
		h.clock = h.clock.Add(time.Minute)
	}
}

// observeHarnessInput is the input observeHarness.cycle analyzes: one decode
// and one prefill replica, the prefill pod queued at rate tokens/s.
func observeHarnessInput(rate float64) domain.AnalyzerInput {
	states := []domain.VariantReplicaState{
		{VariantName: "decode", Role: domain.RoleDecode, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
		{VariantName: "prefill", Role: domain.RolePrefill, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	dec := makeReplicaMetrics("decode-0", "decode", 5000, 16000, 0, 4000, 500)
	pre := makeReplicaMetrics("prefill-0", "prefill", 0, 16000, 3, 4000, 1)
	pre.PrefillComputedTokenRate, pre.PrefillComputedTokenRateKnown = rate, true
	pre.RequestRate, pre.RequestRateKnown = rate/4000, true
	return makeAnalyzerInput([]domain.ReplicaMetrics{dec, pre}, states)
}

// The handle hands out copies: changing the returned estimate's maps does
// not change the stash.
func TestPrefillEstimate_TheHandleReturnsACopy(t *testing.T) {
	h := newObserveHarness(t, &capacity.EngineParams{Engine: inferenceengine.EngineVLLM})
	h.cycle(20000)
	est, _, ok := h.a.PrefillEstimate("test-ns", "test-model", "prefill")
	require.True(t, ok)
	est.Refused["mutated"] = 1
	again, _, _ := h.a.PrefillEstimate("test-ns", "test-model", "prefill")
	assert.NotContains(t, again.Refused, "mutated")

	// The per-count maps too: a stash holding both, as a multi-pod variant's
	// does, read and mutated.
	k := prefillBoundStashKey("test-ns", "test-model", "prefill-3")
	h.a.mu.Lock()
	h.a.prefillEstimates[k] = stashedEstimate{at: h.clock, est: prefill.Estimate{
		FByPods: map[int]float64{3: 1.8}, FSamplesByPods: map[int]int{3: 8}}}
	h.a.mu.Unlock()
	est, _, ok = h.a.PrefillEstimate("test-ns", "test-model", "prefill-3")
	require.True(t, ok)
	est.FByPods[3], est.FByPods[4] = 9, 9
	est.FSamplesByPods[3], est.FSamplesByPods[4] = 99, 99
	again, _, _ = h.a.PrefillEstimate("test-ns", "test-model", "prefill-3")
	assert.Equal(t, map[int]float64{3: 1.8}, again.FByPods, "FByPods is a copy")
	assert.Equal(t, map[int]int{3: 8}, again.FSamplesByPods, "FSamplesByPods is a copy")
}

// At a 120s interval, the sweep that runs before each cycle finds the last
// cycle's estimate two minutes old: it keeps it, and evicts nothing, every
// cycle. Swept on prefillBoundsStaleAfter, it was evicted (and the engine
// logged an eviction) on every cycle.
func TestPrefillEstimate_ASlowIntervalEvictsNothing(t *testing.T) {
	h := newObserveHarness(t, &capacity.EngineParams{Engine: inferenceengine.EngineVLLM})
	for i := range 6 {
		ev := h.a.EvictStaleHistory()
		assert.Zero(t, ev.PrefillEstimates, "cycle %d", i)
		assert.False(t, ev.Any(), "cycle %d: an eviction would be logged: %+v", i, ev)
		h.cycle(20000)
		h.clock = h.clock.Add(time.Minute) // cycle adds one: 120s apart
	}
	_, _, ok := h.a.PrefillEstimate("test-ns", "test-model", "prefill")
	assert.True(t, ok)
}

// Forgetting the variant deletes its stashed estimate and leaves a
// sibling's; a flush clears it and the next cycle writes it again; the
// eviction sweep drops one older than the variant timeout and counts it,
// and keeps a younger one.
func TestPrefillEstimate_ForgetAndEvictionDeleteIt(t *testing.T) {
	t.Run("forget", func(t *testing.T) {
		h := newObserveHarness(t, &capacity.EngineParams{Engine: inferenceengine.EngineVLLM})
		h.cycle(20000)
		h.a.mu.Lock()
		h.a.prefillEstimates[prefillBoundStashKey("test-ns", "test-model", "prefill-2")] = stashedEstimate{at: h.clock}
		h.a.mu.Unlock()
		h.a.ForgetVariant("test-ns", "test-model", "prefill", "t1")
		_, _, ok := h.a.PrefillEstimate("test-ns", "test-model", "prefill")
		assert.False(t, ok, "the forgotten variant's estimate")
		_, _, ok = h.a.PrefillEstimate("test-ns", "test-model", "prefill-2")
		assert.True(t, ok, "the sibling's")
	})
	t.Run("flush", func(t *testing.T) {
		h := newObserveHarness(t, &capacity.EngineParams{Engine: inferenceengine.EngineVLLM})
		h.cycle(20000)
		h.a.ForgetLearnedState()
		_, _, ok := h.a.PrefillEstimate("test-ns", "test-model", "prefill")
		assert.False(t, ok, "a flush takes it with the observation")
		// The flush clears the map, not nils it: the next cycle writes again.
		at := h.clock
		h.cycle(20000)
		_, stamped, ok := h.a.PrefillEstimate("test-ns", "test-model", "prefill")
		assert.True(t, ok, "the next cycle after the flush stashes again")
		assert.Equal(t, at, stamped)
	})
	t.Run("eviction", func(t *testing.T) {
		h := newObserveHarness(t, &capacity.EngineParams{Engine: inferenceengine.EngineVLLM})
		h.cycle(20000)
		h.clock = h.clock.Add(-time.Minute) // the cycle's own clock
		ev := h.a.EvictStaleHistory()
		assert.Zero(t, ev.PrefillEstimates, "one stashed this cycle stays")
		h.clock = h.clock.Add(h.a.Horizons().VariantTimeout)
		ev = h.a.EvictStaleHistory()
		assert.Zero(t, ev.PrefillEstimates, "at the variant timeout it stays")
		h.clock = h.clock.Add(time.Second)
		ev = h.a.EvictStaleHistory()
		assert.Equal(t, 1, ev.PrefillEstimates)
		assert.True(t, ev.Any())
		_, _, ok := h.a.PrefillEstimate("test-ns", "test-model", "prefill")
		assert.False(t, ok)
	})
}
