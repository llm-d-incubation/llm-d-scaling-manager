package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A stated cv prices the bound of the histogram it was read from
// (whatifapi.Knobs.PromptMaxRatio, a backtest's step) where the replay turned
// slo.SwitchTTFTPromptSupport on, and none without one: a typed cv says
// nothing of the longest prompt. The window's own bound stays the measured
// cv's.
func TestTTFT_StatedCVCarriesItsHistogramsBound(t *testing.T) {
	st := learnedState()
	st.Prefill[0].PromptWindow = measuredWindow()
	st.Prefill[0].PromptSupport = true
	read := st.Prefill[0].PromptWindow.Spread().MaxRatio
	require.Positive(t, read)

	pf := fleetOf(t, st, answerFlags{})
	assert.Equal(t, read, pf.fleet.Prefill.PromptMaxRatio, "the window's own bound")

	pf = fleetOf(t, st, answerFlags{assumePromptCV: 0.3, statedPromptMaxRatio: 1.25}, "assume-prompt-cv")
	assert.Equal(t, 0.3, pf.fleet.Prefill.PromptCV)
	assert.Equal(t, 1.25, pf.fleet.Prefill.PromptMaxRatio, "the stated cv's histogram's bound")

	pf = fleetOf(t, st, answerFlags{assumePromptCV: 0.3}, "assume-prompt-cv")
	assert.Zero(t, pf.fleet.Prefill.PromptMaxRatio, "a typed cv carries no bound")

	st.Prefill[0].PromptSupport = false
	pf = fleetOf(t, st, answerFlags{assumePromptCV: 0.3, statedPromptMaxRatio: 1.25}, "assume-prompt-cv")
	assert.Zero(t, pf.fleet.Prefill.PromptMaxRatio, "the switch off prices no bound")
}
