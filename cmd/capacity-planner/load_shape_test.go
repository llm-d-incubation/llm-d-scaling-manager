package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// loadShapeArgs is the guide's sample question (issue #294).
var loadShapeArgs = []string{"at-replicas", "-o", "json", "--recording", olsSweep, "--model", "model-02", "--assume-vp", "30000",
	"--ttft", "500ms@p95", "--tpot", "4ms"}

// reportJSON is an at-replicas command line's JSON report.
func reportJSON(t *testing.T, args ...string) *whatifapi.ReportResponse {
	t.Helper()
	out, _ := runCmd(t, append(append([]string{}, loadShapeArgs...), args...)...)
	var r whatifapi.ReportResponse
	require.NoError(t, json.Unmarshal([]byte(out), &r), out)
	return &r
}

// On the guide's sample the p95 load, 29.7 r/s, ran at 1000-token prompts
// and 500-token outputs (cycles 82-95), while the measured shape is the
// last cycles' 4569/1855. The report says so beside the shape priced and
// flags it; asked the load's own shape, it prices the load at the traffic
// that made it, and needs what day-plan sizes the same rate at (4 + 2), not
// 14 + 23.
func TestAtReplicasLoadShape(t *testing.T) {
	r := reportJSON(t, "--shape", "measured", "--shape", "load")
	require.Len(t, r.Shapes, 2)
	m, l := r.Shapes[0], r.Shapes[1]
	require.NotNil(t, m.LoadShape)
	ls := *m.LoadShape
	assert.Equal(t, whatifapi.LoadP95, ls.Statistic)
	assert.InDelta(t, 1000, ls.InputTokens, 50, "the load's cycles ran 1000-token prompts")
	assert.InDelta(t, 500, ls.OutputTokens, 50)
	assert.Greater(t, m.InputTokens, 4*ls.InputTokens, "the measured shape is the last cycles'")
	assert.True(t, ls.Mismatch)
	assert.Contains(t, m.Flags, whatifapi.FlagShapeLoadMismatch)
	assert.Contains(t, m.Verdict.Summary, "needs 14 prefill + 23 decode")

	assert.Equal(t, ls.InputTokens, l.InputTokens, "--shape load is the load's cycles' shape")
	assert.Equal(t, ls.OutputTokens, l.OutputTokens)
	assert.Equal(t, sourceLoadCycles, l.Source)
	require.NotNil(t, l.LoadShape)
	assert.False(t, l.LoadShape.Mismatch, "a shape asked is never flagged")
	assert.NotContains(t, l.Flags, whatifapi.FlagShapeLoadMismatch)
	assert.Contains(t, l.Verdict.Summary, "needs 4 prefill + 2 decode")
}

// A stated load was read at no cycle: no load shape, and --shape load is
// refused as a usage error.
func TestAtReplicasLoadShapeStatedLoad(t *testing.T) {
	r := reportJSON(t, "--load-seen", "10")
	assert.Nil(t, r.Shapes[0].LoadShape)
	_, code := runCmd(t, append(append([]string{}, loadShapeArgs...), "--load-seen", "10", "--shape", "load")...)
	assert.Equal(t, 2, code)
}

// The p95 less its counting noise is reported, and judged at on asking. On
// the sample, a load swinging from under 1 to 30 r/s, it is all but the p95.
func TestAtReplicasP95Modulation(t *testing.T) {
	r := reportJSON(t)
	require.NotNil(t, r.Load.P95Modulation)
	assert.Less(t, *r.Load.P95Modulation, *r.Load.P95)
	assert.InDelta(t, *r.Load.P95, *r.Load.P95Modulation, 0.01**r.Load.P95, "a swing of 30 r/s is modulation, not noise")
	m := reportJSON(t, "--load-statistic", whatifapi.LoadP95Modulation)
	require.NotNil(t, m.Load.Seen)
	assert.Equal(t, *r.Load.P95Modulation, *m.Load.Seen)
	assert.Equal(t, whatifapi.LoadP95Modulation, m.Load.Statistic)
}

// With no decode variant named, as POST /v1/report asks of a model of one,
// the load's shape is read off the model's only variant that is not
// prefill; with two such and none named, off none.
func TestDecodeShapeSeriesUnnamed(t *testing.T) {
	series := []learnreplay.CycleShape{{Cycle: 1, InputTokens: 1000}}
	st := &learnreplay.State{Variants: []learnreplay.VariantState{{Variant: "p", Role: "prefill", ShapeSeries: []learnreplay.CycleShape{{Cycle: 1}}},
		{Variant: "d", Role: "decode", ShapeSeries: series}}}
	r := &reporter{st: st, s: &servedState{}}
	assert.Equal(t, series, r.decodeShapeSeries())
	r.s.decodeVariant = "d"
	assert.Equal(t, series, r.decodeShapeSeries())
	st.Variants = append(st.Variants, learnreplay.VariantState{Variant: "d2", Role: "decode"})
	r.s.decodeVariant = ""
	assert.Nil(t, r.decodeShapeSeries(), "two decode variants and none named")
}

// The load's cycles: the p95's are every cycle at or above it, the peak's
// its cycle, the mean's every cycle with traffic; numbered from 1.
func TestLoadCycles(t *testing.T) {
	rates := []float64{0, 1, 5, 2, 5, 3}
	stats := map[string]float64{whatifapi.LoadP95: 5, whatifapi.LoadPeak: 5}
	assert.Equal(t, []int{3, 5}, loadCycles(rates, whatifapi.LoadP95, stats))
	assert.Equal(t, []int{3, 5}, loadCycles(rates, whatifapi.LoadPeak, stats))
	assert.Equal(t, []int{2, 3, 4, 5, 6}, loadCycles(rates, whatifapi.LoadMean, stats))
	assert.Nil(t, loadCycles(rates, loadStated, stats))
	series := []learnreplay.CycleShape{{Cycle: 3, InputTokens: 1000, OutputTokens: 100, RequestRate: 3},
		{Cycle: 4, InputTokens: 9000}, {Cycle: 5, InputTokens: 2000, OutputTokens: 300, RequestRate: 1}}
	ls := loadShapeOf(series, []int{3, 5}, whatifapi.LoadP95)
	require.NotNil(t, ls)
	assert.InDelta(t, 1250, ls.InputTokens, 1e-9, "weighted by request rate")
	assert.InDelta(t, 150, ls.OutputTokens, 1e-9)
	assert.Equal(t, 2, ls.Cycles)
	assert.Equal(t, 3, ls.FromCycle)
	assert.Equal(t, 5, ls.ToCycle)
	assert.Nil(t, loadShapeOf(series, []int{1}, whatifapi.LoadP95))
}
