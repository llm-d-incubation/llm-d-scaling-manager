package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The worked example's rows print the band the page's session shows, in text
// and in JSON, and it holds the figure.
func TestSensitivity_TheWorkedExample(t *testing.T) {
	q := workedTTFT("replicas-to-slo", "--ttft-percentile", "90")
	text, _ := runCmd(t, q...)
	assert.Regexp(t, `prefill knee +30\.00 r/s +\[25\.50 \.\. 34\.50\]`, text)
	assert.Regexp(t, `TTFT p90 +373\.4 ms +\[\d+\.\d \.\. \d+\.\d\] +end to end`, text)
	assert.Contains(t, text, "(bands: V_P +-15%, a sweep, not a confidence interval)", "one pod has one f")

	out, _ := runCmd(t, append(q, "-o", "json")...)
	w := decodeOutput(t, out)
	for _, r := range w.Roles {
		if r.Role != rolePrefill {
			continue
		}
		k := r.Results.Knee.Answer.Sensitivity
		require.NotNil(t, k)
		assert.InDelta(t, 25.5, k.Lo, 1e-9)
		assert.InDelta(t, 34.5, k.Hi, 1e-9)
		v := r.Results.TTFT.Answer
		require.NotNil(t, v.Sensitivity)
		assert.Less(t, v.Sensitivity.Lo, v.Value)
		assert.Greater(t, v.Sensitivity.Hi, v.Value)
		assert.Empty(t, v.Sensitivity.Refused)
	}
}

// On question 1 the rate and the fleet each carry one: a lower V_P, a lower rate.
func TestSensitivity_TheRateRows(t *testing.T) {
	out, _ := runCmd(t, workedTTFT("slo-to-rate", "--ttft", "400ms@p95", "-o", "json")...)
	w := decodeOutput(t, out)
	seen := 0
	for _, r := range w.Roles {
		if r.Role != rolePrefill && r.Role != "fleet" {
			continue
		}
		seen++
		a := r.Results.Rate.Answer
		require.NotNil(t, a, out)
		require.NotNil(t, a.Sensitivity, r.Role)
		assert.Less(t, a.Sensitivity.Lo, a.Value, r.Role)
		assert.Greater(t, a.Sensitivity.Hi, a.Value, r.Role)
	}
	assert.Equal(t, 2, seen, out)

	text, _ := runCmd(t, workedTTFT("slo-to-rate", "--ttft", "400ms@p95")...)
	assert.Regexp(t, `UNVALIDATED; V_P ASSUMED \[\d+\.\d\d \.\. \d+\.\d\d\] at V_P \+-15%`, text)
}

// Where the prefill knee sets the operating rate the rate rests on V_P: it
// carries the knee's span, and the worse corner is the point. At the page's 0.85
// of the knee, V_P 15% down is the knee itself, and the TTFT there refuses.
func TestSensitivity_ThePrefillKneeBindsTheRate(t *testing.T) {
	q := append(workedTTFT("replicas-to-slo", "--ttft-percentile", "90"), "--assume-vp", "8000", "--headroom", "0.85")
	text, _ := runCmd(t, q...)
	t.Log(text)
	out, _ := runCmd(t, append(q, "-o", "json")...)
	w := decodeOutput(t, out)
	var decode, pre *wireRole
	for i := range w.Roles {
		switch w.Roles[i].Role {
		case "decode":
			decode = &w.Roles[i]
		case rolePrefill:
			pre = &w.Roles[i]
		}
	}
	require.NotNil(t, decode, out)
	require.NotNil(t, pre, out)
	assert.Equal(t, sourcePrefillKnee, decode.Inputs.RateSource)

	rate := decode.Results.OperatingRate.Answer
	require.NotNil(t, rate.Sensitivity)
	k := pre.Results.Knee.Answer.Sensitivity
	assert.InDelta(t, 0.85*k.Lo, rate.Sensitivity.Lo, 1e-9)
	assert.InDelta(t, 0.85*k.Hi, rate.Sensitivity.Hi, 1e-9)

	v := pre.Results.TTFT.Answer
	require.NotNil(t, v, out)
	assert.Equal(t, string(whatif.Saturated), v.Sensitivity.Refused)
	assert.Equal(t, v.Value, v.Sensitivity.Hi, "a refused corner gives no figure")
}

// Where V_P sits just under the decode knee, V_P up does not lift the rate past
// the decode knee's: the operating rate is the headroom on the lower knee.
func TestSensitivity_TheSpanStopsAtTheDecodeKnee(t *testing.T) {
	q := append(workedTTFT("replicas-to-slo", "--ttft-percentile", "90"), "--assume-vp", "12500", "--headroom", "0.85", "-o", "json")
	out, _ := runCmd(t, q...)
	w := decodeOutput(t, out)
	require.Equal(t, "decode", w.Roles[0].Role)
	decode := w.Roles[0]
	assert.Equal(t, sourcePrefillKnee, decode.Inputs.RateSource)
	dk := decode.Results.Knee.Answer.Value
	require.Less(t, 12.5, dk)
	s := decode.Results.OperatingRate.Answer.Sensitivity
	require.NotNil(t, s)
	assert.Less(t, dk, 12.5*1.15, "uncapped it would pass the decode knee")
	assert.InDelta(t, 0.85*dk, s.Hi, 1e-9)
	assert.InDelta(t, 0.85*12.5*0.85, s.Lo, 1e-9)
}

// A decode knee that binds leaves the operating rate without a span: it does
// not rest on V_P.
func TestSensitivity_ADecodeKneeRateHasNone(t *testing.T) {
	out, _ := runCmd(t, workedTTFT("replicas-to-slo", "--ttft-percentile", "90", "-o", "json")...)
	w := decodeOutput(t, out)
	require.Equal(t, "decode", w.Roles[0].Role)
	assert.Nil(t, w.Roles[0].Results.OperatingRate.Answer.Sensitivity)
}

// A refused fleet has no figure to sweep, and no decode answer has a span.
func TestSensitivity_ARefusalHasNone(t *testing.T) {
	out, _ := runCmd(t, "replicas-to-slo", "--recording", shortRun, "--ttft-percentile", "90", "-o", "json")
	w := decodeOutput(t, out)
	for _, r := range w.Roles {
		if r.Role == rolePrefill {
			require.NotNil(t, r.Results.TTFT)
			assert.Nil(t, r.Results.TTFT.Answer)
		}
	}
}

// spanText puts a refused corner on the worse side, the higher TTFT or the
// lower rate, and prints nothing with no span.
func TestSensitivity_SpanText(t *testing.T) {
	s := &whatif.Sensitivity{Lo: 0.1, Hi: 0.2}
	assert.Equal(t, "[100.0 .. 200.0]", spanText(s, "%.1f", 1000, true))
	s.Refused = whatif.Saturated
	assert.Equal(t, "[100.0 .. Saturated]", spanText(s, "%.1f", 1000, true))
	assert.Equal(t, "[Saturated .. 0.20]", spanText(s, "%.2f", 1, false))
	assert.Empty(t, spanText(nil, "%.2f", 1, false))
}
