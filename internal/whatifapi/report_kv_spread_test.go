package whatifapi

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// kvCycle is a cycle at rate r/s whose pods decode pods ran at a busiest
// fill of busiest and a mean fill of mean.
func kvCycle(rate float64, pods int, busiest, mean float64) SeriesSample {
	return SeriesSample{Rate: rate, Decode: pods, DecodeKVPods: pods, DecodeKVBusiest: busiest, DecodeKVMean: mean}
}

// kvSeries is 20 cycles of 4 pods at ratios 1.00 to 1.19 and rates 1 to 20,
// one of 6 pods at 1.5 (21 r/s) and one of 2 pods at 2.0 (22 r/s): 22 cycles
// read, so that the p95 (the 21st of 22) is not the peak. Then a cycle at
// the floor exactly, idle, which is read; and cycles that are not: one
// under the floor, one of one pod, a busiest NaN, a mean +Inf, a mean NaN,
// and one with no KV read.
func kvSeries() []SeriesSample {
	s := make([]SeriesSample, 0, 29)
	for i := range 20 {
		s = append(s, kvCycle(float64(i+1), 4, 0.5*(1+float64(i)/100), 0.5))
	}
	s = append(s,
		kvCycle(21, 6, 0.75, 0.5),
		kvCycle(22, 2, 0.98, 0.49),
		kvCycle(0, 4, 0.06, MinKVSpreadFill), // at the floor: read; idle: not at any load
		kvCycle(30, 4, 0.2, 0.0499),          // under the floor
		kvCycle(30, 1, 0.9, 0.9),             // one pod: no spread
		kvCycle(30, 4, math.NaN(), 0.5),
		kvCycle(30, 4, 0.9, math.Inf(1)),
		kvCycle(30, 4, 0.9, math.NaN()),
		SeriesSample{Rate: 30, Decode: 4}, // no KV read, as a recording made before the series kept it
	)
	return s
}

// One hot decode pod is reported: each cycle's busiest fill over the mean,
// its median, p95 and peak, the pods read, the median at the load judged,
// and the fill peaks, over the cycles with two pods or more and a finite mean
// fill of MinKVSpreadFill or more (issue
// llm-d-incubation/llm-d-scaling-manager#483).
func TestDecodeKVSpreadOfOneHotPod(t *testing.T) {
	got := DecodeKVSpreadOf(kvSeries(), LoadP95, 20)
	require.NotNil(t, got)
	assert.Equal(t, 23, got.Cycles, "the 22, and the cycle at the floor exactly")
	// Sorted: 1.00..1.19, 1.2 (the floor's), 1.5, 2.0.
	assert.InDelta(t, 1.11, got.Median, 1e-9)
	assert.InDelta(t, 1.5, got.P95, 1e-9, "the 22nd of 23 by nearest rank")
	assert.InDelta(t, 2.0, got.Peak, 1e-9)
	assert.NotEqual(t, got.P95, got.Peak)
	assert.Equal(t, ReportKVSpreadPods{Median: 4, Min: 2, Max: 6}, got.Pods)
	require.NotNil(t, got.AtLoad)
	assert.Equal(t, 3, got.AtLoadCycles, "the cycles at 20 r/s or more: 20 itself is in")
	assert.InDelta(t, 1.5, *got.AtLoad, 1e-9)
	assert.InDelta(t, 0.98, got.BusiestFillPeak, 1e-9, "past the 0.8 band top")
	assert.InDelta(t, 0.5, got.MeanFillPeak, 1e-9, "which the mean never reaches")

	mean := DecodeKVSpreadOf(kvSeries(), LoadMean, 3)
	require.NotNil(t, mean.AtLoad)
	assert.Equal(t, 22, mean.AtLoadCycles, "every cycle read with traffic: not the idle one")

	stated := DecodeKVSpreadOf(kvSeries(), LoadP95, 0)
	assert.Nil(t, stated.AtLoad, "no observed load")
	assert.Zero(t, stated.AtLoadCycles)

	assert.Nil(t, DecodeKVSpreadOf([]SeriesSample{kvCycle(5, 1, 0.9, 0.9), kvCycle(5, 3, 0.03, 0.02)}, LoadP95, 5),
		"no cycle with two pods filled")
	assert.Nil(t, DecodeKVSpreadOf([]SeriesSample{{Rate: 5, Decode: 3}, {Rate: 6, Decode: 3}}, LoadP95, 5),
		"a recording that kept no fill reports none")
	assert.Nil(t, DecodeKVSpreadOf(nil, LoadP95, 5))
}

// The at-load cycles are the measured TTFT's (atLoadCycle): the same rate
// rule, one selector.
func TestAtLoadCycleIsTheMeasuredTTFTRule(t *testing.T) {
	for _, c := range []struct {
		rate float64
		stat string
		want bool
	}{
		{20, LoadP95, true}, {20.5, LoadPeak, true}, {19.9, LoadP95, false}, {0, LoadP95, false},
		{0.1, LoadMean, true}, {0, LoadMean, false}, {math.NaN(), LoadMean, false},
	} {
		assert.Equal(t, c.want, atLoadCycle(SeriesSample{Rate: c.rate}, c.stat, 20), "%v at %s", c.rate, c.stat)
	}
}

// spreadReport is a report of two shapes at an observed p95 load of 20 r/s,
// with figures set, as the reporter leaves it.
func spreadReport() *ReportResponse {
	seen := 20.0
	use := ReportUse{Decode: ReportDecodeUse{KVFill: FigureOf(whatif.Answer{Value: 0.42}, "", false), BandTop: 0.8,
		SlotsOccupied: FigureOf(whatif.Answer{Value: 12}, "", false), Slots: 64,
		WaitProbability: FigureOf(whatif.Answer{Value: 0.01}, "", false)},
		Prefill: ReportPrefillUse{Pooled: FigureOf(whatif.Answer{Value: 0.3}, "", false), WorstPod: FigureOf(whatif.Answer{Value: 0.4}, "", false), Imbalance: 1.3}}
	return &ReportResponse{
		Load: ReportLoad{Seen: &seen, Source: "observed", Statistic: LoadP95},
		Shapes: []ReportShape{
			{Label: "seen", Utilization: use, Verdict: ReportVerdict{Verdict: VerdictOver, Summary: "OVER-PROVISIONED: ..."}, Flags: []string{"X"}},
			{Label: "asked", Utilization: use, Verdict: ReportVerdict{Verdict: VerdictNotJudged}},
		},
	}
}

// A report carries the spread on every shape's decode utilization and
// nothing else of it moves: the report less the new field is the report
// before, byte for byte.
func TestReportDecodeKVSpreadIsReportOnly(t *testing.T) {
	r := spreadReport()
	before, err := json.Marshal(r)
	require.NoError(t, err)
	r.AttachKVSpread(kvSeries())
	for i := range r.Shapes {
		k := r.Shapes[i].Utilization.Decode.KVSpread
		require.NotNil(t, k, r.Shapes[i].Label)
		assert.Equal(t, 23, k.Cycles)
		require.NotNil(t, k.AtLoad)
		assert.InDelta(t, 1.5, *k.AtLoad, 1e-9)
	}
	assert.NotSame(t, r.Shapes[0].Utilization.Decode.KVSpread, r.Shapes[1].Utilization.Decode.KVSpread)
	for i := range r.Shapes {
		r.Shapes[i].Utilization.Decode.KVSpread = nil
	}
	after, err := json.Marshal(r)
	require.NoError(t, err)
	assert.JSONEq(t, string(before), string(after))

	// A stated load, or none read (Seen nil), has no at-load median.
	r = spreadReport()
	r.Load.Seen, r.Load.Source = nil, "none"
	r.AttachKVSpread(kvSeries())
	require.NotNil(t, r.Shapes[0].Utilization.Decode.KVSpread)
	assert.Nil(t, r.Shapes[0].Utilization.Decode.KVSpread.AtLoad)

	r = spreadReport()
	r.AttachKVSpread([]SeriesSample{kvCycle(10, 1, 0.9, 0.9)})
	assert.Nil(t, r.Shapes[0].Utilization.Decode.KVSpread, "nothing measured, nothing reported")
}
