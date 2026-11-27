package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// stepSpreadState is the public 0.6B staircase warm-up as the planner reads
// it at its end: the line held through its floor, the KV a running request
// held on the 8000/200 plateau, and a shape window over its last minutes of
// 8 to 26-token outputs, whose histograms read an output spread D of 1.26.
func stepSpreadState(on bool) *learnreplay.State {
	st := residenceState(true, false, 0)
	st.StepOutputDispersion = on
	d := &st.Variants[0]
	d.Shape = learnreplay.MeasuredShape{InputTokens: 8005, OutputTokens: 21.3, OutputDispersion: 1.263}
	d.Resident = learnreplay.ResidentKV{PerRequest: 8417, Running: 193, Cycles: 16}
	return st
}

// A backtest step is priced at its own lengths, and with
// saturation.SwitchBacktestStepOutputDispersion at its own output spread too
// (issue llm-d-incubation/llm-d-scaling-manager#274): the staircase's
// 1000/1000 A phase at D 1.00, not the 1.26 the warm-up's last minutes read.
// The KV per request falls to the context the A phase's pods held (about
// 1570 tokens a running request), and the fill at the A phase's rate with
// it. Off, or with no spread read at the step, the fit's spread is priced,
// as before.
func TestStepOutputDispersion_TheStepsSpreadOnTheSwitch(t *testing.T) {
	price := func(on bool, step float64) pricedVariant {
		f := answerFlags{set: map[string]bool{"assume-input-tokens": true, "assume-output-tokens": true},
			inputTokens: 1000, outputTokens: 1000, stepOutputDispersion: step}
		vs, err := f.decodeVariants(stepSpreadState(on))
		require.NoError(t, err)
		require.Len(t, vs, 1)
		return vs[0]
	}
	fill := func(pv pricedVariant) float64 {
		a, ok := whatif.FillAtRate(pv.decode, 32).(whatif.Answer)
		require.True(t, ok, "the A phase's top rate is under the knee")
		return a.Value
	}
	off, on, unread := price(false, 1), price(true, 1), price(true, 0)

	// The fit's spread, 8417 x (1000 + 1.263 x 500) / (8005 + 1.263 x 10.65).
	assert.InDelta(t, 1712, off.decode.KVPerRequest, 1)
	assert.InDelta(t, 1.263, off.shape.OutputDispersion, 1e-9)
	assert.Equal(t, off.decode, unread.decode, "a step that read no spread keeps the fit's")

	// The step's, 8417 x 1500 / (8005 + 1.263 x 10.65): the reading's own
	// context is still at the fit's spread.
	assert.InDelta(t, 1575, on.decode.KVPerRequest, 1)
	assert.InDelta(t, 1, on.shape.OutputDispersion, 1e-9)
	assert.NotContains(t, on.flags, whatif.OutputSpreadUnread)
	// What the answer says it priced is the spread it priced at.
	assert.Contains(t, off.shapeText(), "output spread D 1.263")
	assert.Contains(t, on.shapeText(), "output spread D 1.000")
	assert.InDelta(t, 1, inputsOf(&answerFlags{}, on).Shape.OutputDispersion, 1e-9)
	assert.InDelta(t, 1.263, inputsOf(&answerFlags{}, unread).Shape.OutputDispersion, 1e-9, "unread at the step: the fit's")

	// Near the knee the fill moves by more than the KV per request does.
	assert.Less(t, fill(on), fill(off)*0.85, "on %.3f, off %.3f", fill(on), fill(off))
}
