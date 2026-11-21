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

// onePrefillCycle runs one cycle of a P/D model with one prefill pod
// computing tokens tok/s of 2000-token prompts with queue requests waiting,
// and returns the variant's estimate.
func (h *observeHarness) onePrefillCycle(tokens float64, queue int) prefill.Estimate {
	h.t.Helper()
	states := []domain.VariantReplicaState{
		{VariantName: "decode", Role: domain.RoleDecode, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
		{VariantName: "prefill", Role: domain.RolePrefill, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	pre := makeReplicaMetrics("prefill-0", "prefill", 0, 16000, queue, 2000, 1)
	pre.RequestRate, pre.RequestRateKnown = tokens/2000, true
	pre.PrefillComputedTokenRate, pre.PrefillComputedTokenRateKnown = tokens, true
	rows := []domain.ReplicaMetrics{makeReplicaMetrics("decode-0", "decode", 5000, 16000, 0, 4000, 500), pre}
	_, err := h.a.Analyze(h.ctx, makeAnalyzerInput(rows, states))
	require.NoError(h.t, err)
	h.clock = h.clock.Add(time.Minute)
	est, _, ok := h.a.PrefillEstimate("test-ns", "test-model", "prefill")
	require.True(h.t, ok)
	return est
}

// SwitchPrefillVPFloorServed through Analyze: four light queued minutes
// (the first settles) give a peak under a queue of 4000 tok/s, then the pod computes 16000 with
// no queue. On, V_P is floored at the served 16000; off, it stays 4000.
func TestPrefillVPFloorServed_ThroughAnalyze(t *testing.T) {
	run := func(on bool) prefill.Estimate {
		h := newObserveHarness(t, &capacity.EngineParams{Engine: inferenceengine.EngineVLLM})
		require.NoError(t, h.a.SetSwitchesForReplay(map[string]bool{SwitchPrefillVPFloorServed: on}))
		for range 4 {
			h.onePrefillCycle(4000, 2)
		}
		var est prefill.Estimate
		for range 5 {
			est = h.onePrefillCycle(16000, 0)
		}
		return est
	}
	on, off := run(true), run(false)
	require.Empty(t, on.VPRefused, "%+v", on)
	require.Empty(t, off.VPRefused, "%+v", off)
	assert.Equal(t, prefill.SourcePeakUnderQueue, on.VPSource)
	assert.Equal(t, 16000.0, on.VP)
	assert.True(t, on.VPFlooredAtServed)
	assert.Equal(t, 4000.0, off.VP)
	assert.False(t, off.VPFlooredAtServed)
}
