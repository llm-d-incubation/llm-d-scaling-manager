package saturation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// SwitchPrefillVPPromptCoverage through Analyze: four queued minutes of
// 2000-token prompts at 16000 tok/s give a peak under a queue. On, V_P
// carries the prompts of its run, 2000 to 2000; off, none, and V_P is the
// same.
func TestPrefillVPPromptCoverage_ThroughAnalyze(t *testing.T) {
	run := func(on bool) prefill.Estimate {
		h := newObserveHarness(t, &capacity.EngineParams{Engine: inferenceengine.EngineVLLM})
		require.NoError(t, h.a.SetSwitchesForReplay(map[string]bool{SwitchPrefillVPPromptCoverage: on}))
		var est prefill.Estimate
		for range 5 {
			est = h.onePrefillCycle(16000, 2)
		}
		return est
	}
	on, off := run(true), run(false)
	require.Empty(t, on.VPRefused, "%+v", on)
	require.Empty(t, off.VPRefused, "%+v", off)
	assert.Equal(t, off.VP, on.VP)
	assert.InDelta(t, 2000.0, on.VPPromptLow, 1e-6)
	assert.InDelta(t, 2000.0, on.VPPromptHigh, 1e-6)
	assert.Zero(t, off.VPPromptLow)
	assert.Zero(t, off.VPPromptHigh)
}
