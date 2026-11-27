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

// lineCycle runs one cycle of a prefill pod that never queues, serving
// prompts of prompt computed tokens on the line S = 0.2 s + prompt/20000 at
// 0.3 requests in prefill (its prefill time S*(1.3)), and returns the
// prefill variant's estimate.
func (h *observeHarness) lineCycle(prompt float64) prefill.Estimate {
	h.t.Helper()
	states := []domain.VariantReplicaState{
		{VariantName: "decode", Role: domain.RoleDecode, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
		{VariantName: "prefill", Role: domain.RolePrefill, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	t := (0.2 + prompt/20000) * 1.3
	pre := makeReplicaMetrics("prefill-0", "prefill", 0, 16000, 0, 2000, 1)
	pre.RequestRate, pre.RequestRateKnown = 0.3/t, true
	pre.PrefillComputedTokenRate, pre.PrefillComputedTokenRateKnown = pre.RequestRate*prompt, true
	pre.AvgPrefillTime, pre.AvgPrefillTimeKnown = t, true
	rows := []domain.ReplicaMetrics{makeReplicaMetrics("decode-0", "decode", 5000, 16000, 0, 4000, 500), pre}
	_, err := h.a.Analyze(h.ctx, makeAnalyzerInput(rows, states))
	require.NoError(h.t, err)
	h.clock = h.clock.Add(time.Minute)
	est, _, ok := h.a.PrefillEstimate("test-ns", "test-model", "prefill")
	require.True(h.t, ok)
	return est
}

// SwitchPrefillVPServiceLine through Analyze: a prefill pod that never
// queues, its prompts cycling from 1000 to 4000 computed tokens. On, V_P is
// the line's, E[x] / (0.2 + E[x]/20000) at the minutes' mean prompt E[x],
// not a bound, and the observed line
// carries the reading; off, V_P is refused for want of a queue and the line
// carries no service-line field.
func TestPrefillVPServiceLine_ThroughAnalyze(t *testing.T) {
	run := func(on bool) (prefill.Estimate, string) {
		h := newObserveHarness(t, &capacity.EngineParams{Engine: inferenceengine.EngineVLLM})
		require.NoError(t, h.a.SetSwitchesForReplay(map[string]bool{SwitchPrefillVPServiceLine: on}))
		var est prefill.Estimate
		for i := range 48 {
			est = h.lineCycle(float64(1000 * (1 + i%4)))
		}
		lines := observedLines(h.lines)
		require.NotEmpty(t, lines)
		return est, lines[len(lines)-1]
	}
	on, onLine := run(true)
	off, offLine := run(false)
	require.Empty(t, on.VPRefused, "%+v", on)
	assert.Equal(t, prefill.SourceServiceLine, on.VPSource)
	p := on.VPPromptTokens
	assert.InDelta(t, 2500.0, p, 100, "the minutes' mean computed prompt, the settling ones left out")
	assert.InDelta(t, p/(0.2+p/20000), on.VP, 1e-6)
	assert.False(t, on.VPLowerBound)
	assert.InDelta(t, 0.2, on.VPLineS0, 1e-6)
	assert.InDelta(t, 0.2, on.VPRequestSeconds, 1e-6, "s0 over the pod's one rank")
	assert.Contains(t, onLine, `"vpLineRefused"=""`)
	assert.Equal(t, prefill.NoPrefillCapacity, off.VPRefused)
	assert.Zero(t, off.VPLine)
	assert.Zero(t, off.VPRequestSeconds)
	assert.NotContains(t, offLine, "vpLine", "off, the observed line is as before the switch")
}
