package whatifapi

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// overReport is a report of one shape judged OVER-PROVISIONED at a p95 load
// of 10 r/s, against a TTFT p90 target of 1 s.
func overReport() *ReportResponse {
	seen := 10.0
	return &ReportResponse{
		Load:   ReportLoad{Seen: &seen, Source: "observed", Statistic: LoadP95},
		SLO:    ReportSLO{TTFT: &ReportObjective{Statistic: "p90", Target: 1}},
		Shapes: []ReportShape{{Label: "load", Verdict: ReportVerdict{Verdict: VerdictOver, Summary: "OVER-PROVISIONED: ...", Shrink: []string{"prefill"}}}},
	}
}

// busyAndQuiet is 10 quiet cycles at 2 r/s whose TTFT is fast and 10 busy
// ones at 12 r/s with the histogram busy.
func busyAndQuiet(busy map[float64]float64) ([]SeriesSample, []LatencySample) {
	t0 := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	inf := math.Inf(1)
	fast := map[float64]float64{0.1: 10, 0.5: 10, 2: 10, inf: 10}
	s := make([]SeriesSample, 0, 20)
	l := make([]LatencySample, 0, 20)
	for i := range 20 {
		at := t0.Add(time.Duration(i) * 15 * time.Second)
		rate, h := 2.0, fast
		if i >= 10 {
			rate, h = 12, busy
		}
		s = append(s, SeriesSample{At: at, Rate: rate, RateKnown: true})
		l = append(l, routerTTFT(at, h, 0.3))
	}
	return s, l
}

// An OVER-PROVISIONED verdict on a fleet whose TTFT the router measured over
// its target at the load judged is withheld: NOT JUDGED, flagged, and no role
// named to shrink. The quiet cycles under the load do not count.
func TestReportWithholdsOverOnAMeasuredTTFTMiss(t *testing.T) {
	inf := math.Inf(1)
	slow := map[float64]float64{0.1: 1, 0.5: 2, 2: 10, inf: 10}
	s, l := busyAndQuiet(slow)
	r := overReport()
	r.WithholdOverOnMeasured(s, l)
	v := r.Shapes[0].Verdict
	assert.Equal(t, VerdictNotJudged, v.Verdict)
	assert.Contains(t, v.Summary, "nothing to spare")
	assert.Empty(t, v.Shrink)
	assert.Contains(t, r.Shapes[0].Flags, FlagMeasuredTTFTOverTarget)
	assert.Equal(t, whatif.ExitRefused, r.ExitCode)

	// Measured within its target at the busy cycles: the over stands.
	fast := map[float64]float64{0.1: 10, 0.5: 10, 2: 10, inf: 10}
	s, l = busyAndQuiet(fast)
	ok := overReport()
	ok.WithholdOverOnMeasured(s, l)
	assert.Equal(t, VerdictOver, ok.Shapes[0].Verdict.Verdict)
	assert.Equal(t, whatif.ExitAnswered, ok.ExitCode)

	// A stated load was read at no cycle: nothing is measured there.
	s, l = busyAndQuiet(slow)
	stated := overReport()
	stated.Load.Source = "asked"
	stated.WithholdOverOnMeasured(s, l)
	assert.Equal(t, VerdictOver, stated.Shapes[0].Verdict.Verdict)

	// A shape the question typed is other traffic than the router measured.
	typed := overReport()
	typed.Shapes[0].Source = shapeSourceAsked
	typed.WithholdOverOnMeasured(s, l)
	assert.Equal(t, VerdictOver, typed.Shapes[0].Verdict.Verdict)
}

// The miss counts only where the cycles served with at least the fleet
// judged: a 2 + 2 fleet that missed says nothing of a 4 + 4 one, and a 4 + 4
// fleet that missed says a 2 + 2 one would miss too.
func TestReportMeasuredMissIsTheFleetJudged(t *testing.T) {
	inf := math.Inf(1)
	slow := map[float64]float64{0.1: 1, 0.5: 2, 2: 10, inf: 10}
	s, l := busyAndQuiet(slow)
	for i := range s {
		s[i].Prefill, s[i].Decode = 2, 2
	}
	larger := overReport()
	larger.Fleet = ReportFleet{Prefill: 4, Decode: 4}
	larger.WithholdOverOnMeasured(s, l)
	assert.Equal(t, VerdictOver, larger.Shapes[0].Verdict.Verdict, "the cycles ran a smaller fleet")

	same := overReport()
	same.Fleet = ReportFleet{Prefill: 2, Decode: 2}
	same.WithholdOverOnMeasured(s, l)
	assert.Equal(t, VerdictNotJudged, same.Shapes[0].Verdict.Verdict)

	smaller := overReport()
	smaller.Fleet = ReportFleet{Prefill: 1, Decode: 2}
	smaller.WithholdOverOnMeasured(s, l)
	assert.Equal(t, VerdictNotJudged, smaller.Shapes[0].Verdict.Verdict, "a larger fleet missed")
}

// The TTFT measured at a load pools the router's histograms of the cycles
// at or above it; the mean's cycles are every one with traffic.
func TestMeasuredTTFTAtLoad(t *testing.T) {
	inf := math.Inf(1)
	slow := map[float64]float64{0.1: 1, 0.5: 2, 2: 10, inf: 10}
	s, l := busyAndQuiet(slow)
	p90, ok := MeasuredTTFTAtLoad(s, l, FleetCounts{}, LoadP95, 10, 0.9)
	require.True(t, ok)
	assert.Greater(t, p90, 1.0, "the busy cycles alone")
	all, ok := MeasuredTTFTAtLoad(s, l, FleetCounts{}, LoadMean, 10, 0.9)
	require.True(t, ok)
	assert.Less(t, all, p90, "the quiet cycles pull the pooled p90 down")
	_, ok = MeasuredTTFTAtLoad(s, l[:3], FleetCounts{}, LoadP95, 10, 0.9)
	assert.False(t, ok, "latency not aligned with the series")
}
