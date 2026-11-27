package whatifapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// A TTFT model fitted on a short window flags every figure it sets
// RankMixtureShortFitWindow: the fleet limit where decode binds, which
// still names the model, and the report's measured shape, whose flags are
// every flag its figures carry.
func TestMainView_ShortFitWindowReachesEveryFigure(t *testing.T) {
	short := &mainMixture{rq: &rankMixtureQ{report: RankMixtureReport{ShortFitWindow: true}}}
	f := &mixtureFleet{mm: short, knee: 10, kneeOK: true}
	prefill := whatif.Answer{Value: 7, Flags: short.modelFlags()}
	decode := whatif.Answer{Value: 6}
	a, ok := f.fleetLimit(0.85, decode, prefill).(whatif.Answer)
	require.True(t, ok)
	assert.InDelta(t, 6, a.Value, 1e-12, "decode binds")
	assert.Contains(t, a.Flags, whatif.RankMixtureShortFitWindow, "the decode-bound limit is the short-fit model's")
	assert.Equal(t, []whatif.Flag{whatif.RankMixtureTTFT}, (*mainMixture)(nil).modelFlags(), "no fit, no window flag")

	_, e := mainEntry(t, true)
	mm := e.ttftModel
	require.True(t, mm.priced(), mm.info.Abstained)
	mm.rq.report.ShortFitWindow = true
	rate := mm.top / 2
	out := &ReportResponse{Fleet: ReportFleet{Prefill: mm.prefill, Decode: 1}, Shapes: []ReportShape{{Label: shapeMeasured,
		Capacity: ReportLimits{PrefillKnee: ReportRate{Figure: Figure{Value: ptr(mm.top)}}, DecodeKnee: ReportRate{Figure: Figure{Value: ptr(mm.top)}}},
		Table:    []ReportRow{{Rate: rate, TTFT: []ReportStat{{Statistic: "p90"}}}}}}}
	mm.applyToReport(out, ReportQuery{})
	sh := out.Shapes[0]
	require.NotNil(t, sh.Table[0].TTFT[0].Value, "the model prices the row")
	assert.Contains(t, sh.Table[0].TTFT[0].Flags, string(whatif.RankMixtureShortFitWindow))
	assert.Contains(t, sh.Flags, string(whatif.RankMixtureTTFT))
	assert.Contains(t, sh.Flags, string(whatif.RankMixtureShortFitWindow), "the shape's flags are its figures'")
}

// A TPOT distribution fitted on fewer steps than the default flags the
// report's measured shape TPOTDistributionShortFit, as it flags each
// percentile.
func TestMainViewTPOT_ShortFitReachesTheShape(t *testing.T) {
	_, e := tpotEntry(t, true, 64)
	mt := e.tpotModel
	require.True(t, mt.priced(), mt.info.Abstained)
	mt.tq.report.ShortFit = true
	out := &ReportResponse{Fleet: ReportFleet{Decode: 2}, Shapes: []ReportShape{{Label: shapeMeasured, Table: []ReportRow{{
		Rate: *mt.info.MeasuredUpTo / 2, TPOTMean: Figure{Value: f64(0.01), Unit: unitSeconds},
		TPOTPercentile: &ReportStat{Statistic: "p90", Figure: Figure{Refused: &Refusal{Reason: string(whatif.NoTPOTHistogram)}}}}}}}}
	mt.applyToReport(out, ReportQuery{})
	sh := out.Shapes[0]
	require.NotNil(t, sh.Table[0].TPOTPercentile.Value)
	assert.Contains(t, sh.Table[0].TPOTPercentile.Flags, string(whatif.TPOTDistributionShortFit))
	assert.Contains(t, sh.Flags, string(whatif.TPOTDistributionShortFit), "the shape's flags are its figures'")
}
