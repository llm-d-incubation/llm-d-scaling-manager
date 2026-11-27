package saturation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// SwitchPrefillVPLoneRate through Analyze: a prefill pod that never queues,
// at a tenth of a request in prefill: 2000-token prompts at 0.91 requests/s,
// each 0.11 s in prefill. On, V_P is the lone rate, 2000 tokens over 0.11 s,
// not a bound (the pod never served more), and the observed line carries the
// reading; off, V_P is refused for want of a queue and the line carries no
// lone-rate field.
func TestPrefillVPLoneRate_ThroughAnalyze(t *testing.T) {
	run := func(on bool) (prefill.Estimate, string) {
		h := newObserveHarness(t, &capacity.EngineParams{Engine: inferenceengine.EngineVLLM})
		require.NoError(t, h.a.SetSwitchesForReplay(map[string]bool{SwitchPrefillVPLoneRate: on}))
		var est prefill.Estimate
		for range 14 {
			// rho/(1 - rho) at rho 1/11 is 0.1 in prefill: a request alone
			// for 0.11 s at 0.91 requests/s.
			est = h.psPrefillCycle(1.0 / 11)
		}
		lines := observedLines(h.lines)
		require.NotEmpty(t, lines)
		return est, lines[len(lines)-1]
	}
	on, onLine := run(true)
	off, offLine := run(false)
	require.Empty(t, on.VPRefused, "%+v", on)
	assert.Equal(t, prefill.SourceLoneRate, on.VPSource)
	assert.InDelta(t, 2000.0/(0.1/(1.0/11*20000/2000)), on.VP, 1e-6)
	assert.False(t, on.VPLowerBound)
	assert.Contains(t, onLine, `"vpLoneIsVP"=true`)
	assert.Contains(t, onLine, `"vpLoneMinutes"=13`, "the first cycle reads no minute")
	assert.Equal(t, prefill.NoPrefillCapacity, off.VPRefused)
	assert.Zero(t, off.VPLone)
	assert.NotContains(t, offLine, "vpLone", "off, the observed line is as before the switch")
}
