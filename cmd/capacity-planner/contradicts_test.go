package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// contradictsReporter is a reporter on a window of 40 cycles 15 s apart, all
// at 1 r/s on 2 prefill (p) + 2 decode (d), but for the cycles in hot, at
// rate on prefill replicas pre and decode replicas dec.
func contradictsReporter(hot []int, rate float64, pre, dec int) *reporter {
	const n = 40
	at := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	st := &learnreplay.State{FirstAt: at, At: at.Add((n - 1) * 15 * time.Second),
		ArrivalRates: make([]float64, n), ServingReplicas: map[string][]int{"p": make([]int, n), "d": make([]int, n)}}
	for i := range n {
		st.ArrivalRates[i], st.ServingReplicas["p"][i], st.ServingReplicas["d"][i] = 1, 2, 2
	}
	for _, i := range hot {
		st.ArrivalRates[i], st.ServingReplicas["p"][i], st.ServingReplicas["d"][i] = rate, pre, dec
	}
	return &reporter{st: st, sh: whatifapi.ReportShapeAsk{Measured: true},
		pv: pricedVariant{state: learnreplay.VariantState{Variant: "d"}}, pfVariant: "p"}
}

// cycles is the indexes from..to-1.
func cycles(from, to int) []int {
	var out []int
	for i := from; i < to; i++ {
		out = append(out, i)
	}
	return out
}

// The window contradicts the knee when the fleet priced, or a smaller one,
// carried more than 1.25x the fleet knee for 3 minutes without a break: 12
// cycles 15 s apart. At the edges: 11 cycles, a rate at 1.25x, a break, a
// larger fleet, a stated shape, no per-cycle fleet, no knee (#297).
func TestReport_WindowContradictsTheKnee(t *testing.T) {
	base := fleetEval{kneeOK: true, knee: 4}
	for _, c := range []struct {
		name     string
		r        *reporter
		np, nd   int
		want     bool
		contains string
	}{
		{"12 cycles at 3x", contradictsReporter(cycles(10, 22), 12, 2, 2), 2, 2, true, "2 prefill + 2 decode or fewer carried above 1.25x the fleet knee of 4.0 r/s for 3 min without a break (12 cycles), up to 12.0 r/s (3.0x)"},
		{"11 cycles", contradictsReporter(cycles(10, 21), 12, 2, 2), 2, 2, false, ""},
		{"at 1.25x is not above", contradictsReporter(cycles(10, 22), 5, 2, 2), 2, 2, false, ""},
		{"just above 1.25x", contradictsReporter(cycles(10, 22), 5.01, 2, 2), 2, 2, true, "(1.3x)"},
		{"a fewer decode carried it", contradictsReporter(cycles(10, 22), 12, 2, 1), 2, 2, true, ""},
		{"a larger prefill carried it", contradictsReporter(cycles(10, 22), 12, 3, 2), 2, 2, false, ""},
		{"a larger decode carried it", contradictsReporter(cycles(10, 22), 12, 2, 3), 2, 2, false, ""},
		{"no serving replica", contradictsReporter(cycles(10, 22), 12, 2, 0), 2, 2, false, ""},
		{"a break", contradictsReporter(append(cycles(10, 16), cycles(17, 23)...), 12, 2, 2), 2, 2, false, ""},
		{"decode alone priced", contradictsReporter(cycles(10, 22), 12, 3, 2), 0, 2, true, "2 decode or fewer carried"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := c.r.contradicted(c.np, c.nd, base)
			assert.Equal(t, c.want, got != "", got)
			assert.Contains(t, got, c.contains)
		})
	}
	r := contradictsReporter(cycles(10, 22), 12, 2, 2)
	r.sh.Measured = false
	assert.Empty(t, r.contradicted(2, 2, base), "a stated shape is not the window's")
	r = contradictsReporter(cycles(10, 22), 12, 2, 2)
	r.st.ServingReplicas = nil
	assert.Empty(t, r.contradicted(2, 2, base), "no per-cycle fleet recorded")
	r = contradictsReporter(cycles(10, 22), 12, 2, 2)
	assert.Empty(t, r.contradicted(2, 2, fleetEval{}), "no knee")
}

// On the ols-sweep the window's own 1 + 1 carried 14.8x the knee the model
// prices; v0.12.2 called it UNDER-PROVISIONED, needing 18 decode. The
// verdict is NOT JUDGED and says the window contradicts the model, and the
// command exits refused (#297).
func TestAtReplicas_TheOLSSweepContradictsTheModel(t *testing.T) {
	out, code := runCmd(t, "at-replicas", "--recording", olsSweep, "-o", "json")
	require.Equal(t, whatif.ExitRefused, code, out)
	var rep whatifapi.ReportResponse
	require.NoError(t, json.Unmarshal([]byte(out), &rep))
	require.NotEmpty(t, rep.Shapes)
	v := rep.Shapes[0].Verdict
	assert.Equal(t, whatifapi.VerdictNotJudged, v.Verdict, v.Summary)
	assert.Contains(t, v.Summary, contradictsWords)
	assert.Contains(t, v.Summary, "(14.8x)")
	assert.Nil(t, v.Needed, "no fleet is sized on a knee the window contradicts")
	for _, g := range rep.Shapes[0].Growth {
		assert.Equal(t, whatifapi.VerdictNotJudged, g.Verdict.Verdict, g.Verdict.Summary)
	}
	// The decode knee carries its own label; outside its region the 0.6B
	// range is said once under not validated.
	k := rep.Shapes[0].Capacity.DecodeKnee
	require.NotNil(t, k.Validation)
	assert.False(t, k.Validation.Validated, k.Validation.Label)
	assert.Contains(t, rep.NotValidated, decodeKneeOutside())
}

// Decode sized at the knee's error bound: ceil(load x (1 + e) / (headroom x
// knee per replica)), e the validating row's upper error, the 0.6B goldens'
// 103% outside every row, never below the point answer (#297).
func TestReport_DecodeAtTheErrorBound(t *testing.T) {
	hi := 0.03
	inside := whatif.Validation{Row: &whatif.EnvelopeRow{ErrorHi: &hi}}
	outside := whatif.Validation{Outside: "elsewhere"}
	base := fleetEval{decodeKnee: whatif.Answer{Value: 20}} // 2 decode: 10 r/s each
	for _, c := range []struct {
		name  string
		v     whatif.Validation
		load  float64
		point int
		want  int
		e     float64
		words string
	}{
		// 17 r/s at 0.85 x 10 per replica: 2 at the point, 2.06 at +3% is 3.
		{"inside, +3% crosses a replica", inside, 17, 2, 3, 0.03, ", 3 decode if the decode knee reads 3% high"},
		{"inside, +3% stays", inside, 16, 2, 2, 0.03, ""},
		// 17 x 2.03 / 8.5 = 4.06: 5.
		{"outside, the goldens' 103%", outside, 17, 2, 5, decodeKneeGoldensHigh, ", 5 decode if the decode knee reads 103% high"},
		{"never below the point", inside, 1, 4, 4, 0.03, ""},
		{"exactly at the operating point", inside, 8.5 / 1.03, 1, 1, 0.03, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := &reporter{load: c.load, headroom: 0.85, kneeV: c.v}
			out := &whatifapi.ReportNeeded{Decode: c.point}
			assert.Equal(t, c.words, r.atBound(base, 2, out))
			assert.Equal(t, c.want, out.DecodeAtErrorBound)
			assert.InDelta(t, c.e, out.DecodeKneeError, 1e-12)
		})
	}
	r := &reporter{load: 17, headroom: 0.85, kneeV: inside}
	out := &whatifapi.ReportNeeded{Decode: 2}
	assert.Empty(t, r.atBound(fleetEval{decodeKnee: whatif.Unanswerable{Reason: whatif.NoLine}}, 2, out))
	assert.Zero(t, out.DecodeAtErrorBound, "no knee, no bound")
}
