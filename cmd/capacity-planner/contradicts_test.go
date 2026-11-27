package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// contradictsReporter is a reporter on a window of 40 cycles 15 s apart, each
// cycle's clock recorded, all
// at 1 r/s on 2 prefill (p) + 2 decode (d) at the shape priced, 1000/1000,
// but for the cycles in hot, served at rate on prefill replicas pre and
// decode replicas dec. The arrivals are the served rate.
func contradictsReporter(hot []int, rate float64, pre, dec int) *reporter {
	const n = 40
	at := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	st := &learnreplay.State{FirstAt: at, At: at.Add((n - 1) * 15 * time.Second),
		ArrivalRates: make([]float64, n), ServingReplicas: map[string][]int{"p": make([]int, n), "d": make([]int, n)},
		CycleTimes: make([]time.Time, n)}
	series := make([]learnreplay.CycleShape, n)
	for i := range n {
		st.CycleTimes[i] = at.Add(time.Duration(i) * 15 * time.Second)
		st.ArrivalRates[i], st.ServingReplicas["p"][i], st.ServingReplicas["d"][i] = 1, 2, 2
		series[i] = learnreplay.CycleShape{Cycle: i + 1, InputTokens: 1000, OutputTokens: 1000, RequestRate: 1}
	}
	for _, i := range hot {
		st.ArrivalRates[i], st.ServingReplicas["p"][i], st.ServingReplicas["d"][i] = rate, pre, dec
		series[i].RequestRate = rate
	}
	st.Variants = []learnreplay.VariantState{{Variant: "p", Role: "prefill"}, {Variant: "d", Role: "decode", ShapeSeries: series}}
	return &reporter{st: st, sh: whatifapi.ReportShapeAsk{Measured: true}, s: &servedState{st: st, decodeVariant: "d"},
		pv: pricedVariant{state: st.Variants[1], shape: shape.Shape{AvgInputTokens: 1000, AvgOutputTokens: 1000}}, pfVariant: "p"}
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
// served more than 1.25x the fleet knee at the shape priced for 3 minutes
// without a break: 12 cycles 15 s apart. At the edges: 11 cycles, a rate at
// 1.25x, a break, a larger fleet, a stated shape, no per-cycle fleet, no
// knee (#297).
func TestReport_WindowContradictsTheKnee(t *testing.T) {
	base := fleetEval{kneeOK: true, knee: 4, binds: roleDecodeWord, decodeKnee: whatif.Answer{Value: 4}}
	for _, c := range []struct {
		name     string
		r        *reporter
		np, nd   int
		want     bool
		contains string
	}{
		{"12 cycles at 3x", contradictsReporter(cycles(10, 22), 12, 2, 2), 2, 2, true, "2 prefill + 2 decode or fewer served above 1.25x the fleet knee of 4.0 r/s at the shape priced for 3 min without a break (12 cycles), up to 12.0 r/s (3.0x)"},
		{"11 cycles", contradictsReporter(cycles(10, 21), 12, 2, 2), 2, 2, false, ""},
		{"at 1.25x is not above", contradictsReporter(cycles(10, 22), 5, 2, 2), 2, 2, false, ""},
		{"just above 1.25x", contradictsReporter(cycles(10, 22), 5.01, 2, 2), 2, 2, true, "(1.3x)"},
		{"a fewer decode carried it", contradictsReporter(cycles(10, 22), 12, 2, 1), 2, 2, true, ""},
		{"a larger prefill carried it", contradictsReporter(cycles(10, 22), 12, 3, 2), 2, 2, false, ""},
		{"a larger decode carried it", contradictsReporter(cycles(10, 22), 12, 2, 3), 2, 2, false, ""},
		{"no serving replica", contradictsReporter(cycles(10, 22), 12, 2, 0), 2, 2, false, ""},
		{"a break", contradictsReporter(append(cycles(10, 16), cycles(17, 23)...), 12, 2, 2), 2, 2, false, ""},
		{"decode alone priced", contradictsReporter(cycles(10, 22), 12, 3, 2), 0, 2, true, "2 decode or fewer served"},
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

// A run past the knee breaks at a gap in the recording: two 2-minute runs
// (8 cycles 15 s apart each) on either side of a 10-minute gap do not
// contradict, while 4 minutes without one do. Counted at the mean cycle
// time, as before per-cycle clocks, the 16 cycles read 8 minutes (the gap
// lifts the mean to 30 s). A recording with no per-cycle clock keeps the
// count at the mean cycle time and says so (#358).
func TestReport_ARecordingGapBreaksTheRun(t *testing.T) {
	base := fleetEval{kneeOK: true, knee: 4, binds: roleDecodeWord, decodeKnee: whatif.Answer{Value: 4}}
	gapped := func() *reporter {
		r := contradictsReporter(cycles(12, 28), 12, 2, 2)
		for i := 20; i < len(r.st.CycleTimes); i++ {
			r.st.CycleTimes[i] = r.st.CycleTimes[i].Add(10 * time.Minute)
		}
		r.st.At = r.st.CycleTimes[len(r.st.CycleTimes)-1]
		return r
	}

	assert.Empty(t, gapped().contradicted(2, 2, base), "two 2-minute runs across a gap are not one")

	got := contradictsReporter(cycles(12, 28), 12, 2, 2).contradicted(2, 2, base)
	assert.Contains(t, got, "for 4 min without a break (16 cycles), up to 12.0 r/s")

	r := gapped()
	r.st.CycleTimes = nil
	got = r.contradicted(2, 2, base)
	assert.Contains(t, got, "for 8 min without a break (16 cycles, "+untimedRunWords+")",
		"no per-cycle clock: the gap cannot be seen, and the words say how the run was counted")
}

// Only what the window served at the shape priced judges the knee: the q32
// warm-up's hot cycles ran at 1000/1000 while its knee is priced at the
// last cycles' 8k/26, a different phase; a burst of arrivals the fleet did
// not serve is not carried; a lower-bound prefill knee reads low by
// construction; a decode knee set by the band's top stops short of a full
// replica, so the share is over the band's top; and every variant of a
// role counts toward its fleet (#297).
func TestReport_ContradictsOnlyAtTheShapePriced(t *testing.T) {
	base := fleetEval{kneeOK: true, knee: 4, binds: roleDecodeWord, decodeKnee: whatif.Answer{Value: 4}}

	r := contradictsReporter(cycles(10, 22), 12, 2, 2)
	r.pv.shape = shape.Shape{AvgInputTokens: 8016, AvgOutputTokens: 26}
	assert.Empty(t, r.contradicted(2, 2, base), "hot cycles of another shape say nothing of this knee")
	r.pv.shape = shape.Shape{AvgInputTokens: 1150, AvgOutputTokens: 900}
	assert.NotEmpty(t, r.contradicted(2, 2, base), "within 20% of each length is the shape priced")

	r = contradictsReporter(cycles(10, 22), 12, 2, 2)
	for _, i := range cycles(10, 22) {
		r.st.Variants[1].ShapeSeries[i].RequestRate = 4.5
	}
	assert.Empty(t, r.contradicted(2, 2, base), "arrivals of 12 r/s served at 4.5 r/s carried no more than 1.25x")

	pre := fleetEval{kneeOK: true, knee: 4, binds: rolePrefill, prefillKnee: whatif.Answer{Value: 4, Flags: []whatif.Flag{whatif.VPLowerBound}}}
	assert.Empty(t, contradictsReporter(cycles(10, 22), 12, 2, 2).contradicted(2, 2, pre), "a lower-bound knee")
	pre.prefillKnee = whatif.Answer{Value: 4}
	assert.NotEmpty(t, contradictsReporter(cycles(10, 22), 12, 2, 2).contradicted(2, 2, pre))

	top := fleetEval{kneeOK: true, knee: 4, binds: roleDecodeWord,
		decodeKnee: whatif.Answer{Value: 4, Basis: whatif.Basis{BoundBy: []whatif.Bound{whatif.BoundBandTop}}}}
	r = contradictsReporter(cycles(10, 22), 6, 2, 2)
	r.pv.decode.Band = whatif.FillBand{Lo: 0.2, Hi: 0.8}
	assert.Empty(t, r.contradicted(2, 2, top), "6 r/s is under 1.25/0.8 = 1.56x a band-top knee of 4")
	assert.NotEmpty(t, r.contradicted(2, 2, base), "but above 1.25x a knee the band's top does not set")

	r = contradictsReporter(cycles(10, 22), 12, 2, 2)
	r.st.Variants = append(r.st.Variants, learnreplay.VariantState{Variant: "d2", Role: "decode"})
	r.st.ServingReplicas["d2"] = make([]int, 40)
	for _, i := range cycles(10, 22) {
		r.st.ServingReplicas["d2"][i] = 1
	}
	assert.Empty(t, r.contradicted(2, 2, base), "2 + 1 decode across the role's variants is more than the 2 priced")

	// A variant not named at a cycle (-1) leaves the fleet there unknown:
	// 2 + (-1) is no fleet of 1.
	r = contradictsReporter(cycles(10, 22), 12, 2, 3)
	r.st.Variants = append(r.st.Variants, learnreplay.VariantState{Variant: "d2", Role: "decode"})
	r.st.ServingReplicas["d2"] = make([]int, 40)
	for _, i := range cycles(10, 22) {
		r.st.ServingReplicas["d2"][i] = -1
	}
	assert.Empty(t, r.contradicted(2, 2, base), "3 decode served; the second variant's -1 does not make it 2")

	// Each cycle is read at its own index: with a larger fleet right after
	// the 12 hot cycles, the run is 12, not 11.
	r = contradictsReporter(cycles(10, 22), 12, 2, 2)
	r.st.ServingReplicas["d"][22] = 3
	assert.Contains(t, r.contradicted(2, 2, base), "(12 cycles)")
	r = contradictsReporter(cycles(10, 22), 12, 2, 2)
	r.st.Variants[1].ShapeSeries[9].InputTokens = 8000
	assert.Contains(t, r.contradicted(2, 2, base), "(12 cycles)", "a cycle of another shape just before")

	// Two decode variants serving in one cycle leave the fleet unknown: the
	// knee and the shape are the priced variant's alone. 1 + 1 is within
	// the 2 priced, so only the unknown mark keeps it from contradicting.
	r = contradictsReporter(cycles(10, 22), 12, 2, 1)
	d2 := make([]learnreplay.CycleShape, 40)
	r.st.ServingReplicas["d2"] = make([]int, 40)
	for i := range d2 {
		d2[i] = learnreplay.CycleShape{Cycle: i + 1, InputTokens: 1000, OutputTokens: 1000, RequestRate: 1}
	}
	for _, i := range cycles(10, 22) {
		r.st.ServingReplicas["d2"][i], d2[i].RequestRate = 1, 6
	}
	r.st.Variants = append(r.st.Variants, learnreplay.VariantState{Variant: "d2", Role: "decode", ShapeSeries: d2})
	assert.Empty(t, r.contradicted(2, 2, base), "a second decode variant served")

	// A cycle lighter than the shape priced, within 20%, has a knee higher
	// by at least that much: 5.2 r/s at OL 820 is under 4.3 r/s at OL 1000
	// (4.0, with the shorter output's KV), under 1.25x 4.
	r = contradictsReporter(cycles(10, 22), 5.2, 2, 2)
	for _, i := range cycles(10, 22) {
		r.st.Variants[1].ShapeSeries[i].OutputTokens = 820
	}
	assert.Empty(t, r.contradicted(2, 2, base), "lighter output, decode binds")
	pre = fleetEval{kneeOK: true, knee: 4, binds: rolePrefill, prefillKnee: whatif.Answer{Value: 4}}
	assert.NotEmpty(t, r.contradicted(2, 2, pre), "the output does not move a prefill knee")
	for _, i := range cycles(10, 22) {
		r.st.Variants[1].ShapeSeries[i].InputTokens = 820
	}
	assert.Empty(t, r.contradicted(2, 2, pre), "lighter prompt, prefill binds")
}

// Only a load past the knee is withheld for a window served past it, and
// only a shape priced heavier than the load's: a lighter one's knee holds
// an UNDER the more (#297).
func TestReport_NotJudgedUnderOnlyWhereTheKneeReadsLow(t *testing.T) {
	past := fleetEval{kneeOK: true, knee: 4, binds: roleDecodeWord, decodeKnee: whatif.Answer{Value: 4}, under: true, pastOperating: true}
	r := contradictsReporter(cycles(10, 22), 12, 2, 2)
	require.True(t, r.notJudgedUnder(2, 2, past, &whatifapi.ReportVerdict{}))
	missed := past
	missed.pastOperating = false
	assert.False(t, r.notJudgedUnder(2, 2, missed, &whatifapi.ReportVerdict{}), "a latency target missed under the knee")

	r = contradictsReporter(nil, 1, 2, 2)
	r.pv.shape = shape.Shape{AvgInputTokens: 700, AvgOutputTokens: 700}
	r.loadShape = &whatifapi.ReportLoadShape{InputTokens: 1000, OutputTokens: 1000}
	assert.False(t, r.notJudgedUnder(2, 2, fleetEval{}, &whatifapi.ReportVerdict{}), "priced lighter than the load")
	r.pv.shape = shape.Shape{AvgInputTokens: 1000, AvgOutputTokens: 1250}
	assert.True(t, r.notJudgedUnder(2, 2, fleetEval{}, &whatifapi.ReportVerdict{}), "priced 25% heavier in output")
}

// A would-be UNDER on a load whose cycles ran at another shape than the
// measured shape priced is NOT JUDGED, and says both shapes; a stated shape
// is the question's, and keeps its verdict (#294, #297).
func TestReport_NotJudgedWhereTheLoadRanAtAnotherShape(t *testing.T) {
	r := contradictsReporter(nil, 1, 2, 2)
	r.pv.shape = shape.Shape{AvgInputTokens: 8016, AvgOutputTokens: 26}
	r.loadShape = &whatifapi.ReportLoadShape{InputTokens: 1000, OutputTokens: 1000}
	var v whatifapi.ReportVerdict
	require.True(t, r.notJudgedUnder(2, 2, fleetEval{}, &v))
	assert.Equal(t, whatifapi.VerdictNotJudged, v.Verdict)
	assert.Contains(t, v.Summary, phasesWords+" the load's cycles ran at IL 1000 OL 1000, the shape priced is IL 8016 OL 26")
	r.sh.Measured = false
	assert.False(t, r.notJudgedUnder(2, 2, fleetEval{}, &whatifapi.ReportVerdict{}))
	r.sh.Measured = true
	r.loadShape = &whatifapi.ReportLoadShape{InputTokens: 8500, OutputTokens: 28}
	assert.False(t, r.notJudgedUnder(2, 2, fleetEval{}, &whatifapi.ReportVerdict{}), "the same traffic")

	// The 32B staircase warm-ups: 8 output tokens against 21 priced, 8k
	// prompts. Where prefill sets the knee, fewer than phasesOutputFloor
	// tokens longer does not withhold; where decode sets it, it does; and
	// a longer output withholds either way.
	r.loadShape = &whatifapi.ReportLoadShape{InputTokens: 7995, OutputTokens: 8}
	r.pv.shape = shape.Shape{AvgInputTokens: 8005, AvgOutputTokens: 21}
	assert.False(t, r.notJudgedUnder(2, 2, fleetEval{binds: rolePrefill}, &whatifapi.ReportVerdict{}), "13 tokens longer, prefill binds")
	assert.True(t, r.notJudgedUnder(2, 2, fleetEval{binds: roleDecodeWord}, &whatifapi.ReportVerdict{}), "13 tokens longer, decode binds")
	r.pv.shape = shape.Shape{AvgInputTokens: 8005, AvgOutputTokens: 8 + phasesOutputFloor}
	assert.True(t, r.notJudgedUnder(2, 2, fleetEval{binds: rolePrefill}, &whatifapi.ReportVerdict{}), "the floor longer, prefill binds")
}

// The q32 warm-up's hot cycles (5-34, 65-90) ran at 1000/1000, up to 3.8x
// the knee its last cycles' 8k/26 prices: they say nothing of that knee. A
// stated load past the knee, which no load shape withholds, is not
// contradicted (v0.12.3's first #297 check called it contradicted). That
// knee rests on a lower bound of V_P (VPLowerBound), so the load past it is
// NOT JUDGED with the knee as at least its rate, not UNDER-PROVISIONED
// (#280).
func TestAtReplicas_Q32HotCyclesOfAnotherShapeDoNotContradict(t *testing.T) {
	out, code := runCmd(t, "at-replicas", "--recording", q32Recording, "--ttft", "2s@p90", "--load-seen", "12", "-o", "json")
	var rep whatifapi.ReportResponse
	require.NoError(t, json.Unmarshal([]byte(out), &rep), "exit %d: %s", code, out)
	require.NotEmpty(t, rep.Shapes)
	v := rep.Shapes[0].Verdict
	assert.NotContains(t, v.Summary, contradictsWords)
	assert.Equal(t, whatifapi.VerdictNotJudged, v.Verdict, v.Summary)
	// 4.1, not the 4.04 V_P gives: the knee is raised to the work the
	// prefill pods sustained over five minutes with their backlog steady
	// (KneeAtServedWork); a two-minute peak read 4.2 (#477).
	assert.Contains(t, v.Summary, "2.9x a prefill knee of at least 4.1 r/s: "+lowerBoundKneeWords)
}

// On the ols-sweep the load's cycles ran at about 1000/500 while the shape
// priced is the last cycles' 4569/1855: v0.12.2 called the window's own
// 1 + 1 UNDER-PROVISIONED at 14.8x that shape's knee, needing 18 decode.
// The verdict is NOT JUDGED and says the load and the shape priced are of
// different traffic, and the command exits refused (#294, #297). Asked for
// no shape, the report prices the load at its own cycles' shape and judges.
func TestAtReplicas_TheOLSSweepLoadIsOfAnotherShape(t *testing.T) {
	def := atReplicasJSON(t, "at-replicas", "--recording", olsSweep)
	require.NotEmpty(t, def.Shapes)
	assert.Equal(t, whatifapi.ShapeLoad, def.Shapes[0].Label, "the default shape is the load's own")
	assert.NotEqual(t, whatifapi.VerdictNotJudged, def.Shapes[0].Verdict.Verdict, def.Shapes[0].Verdict.Summary)
	assert.NotContains(t, def.Shapes[0].Verdict.Summary, phasesWords)

	out, code := runCmd(t, "at-replicas", "--recording", olsSweep, "--shape", "measured", "-o", "json")
	require.Equal(t, whatif.ExitRefused, code, out)
	var rep whatifapi.ReportResponse
	require.NoError(t, json.Unmarshal([]byte(out), &rep))
	require.NotEmpty(t, rep.Shapes)
	v := rep.Shapes[0].Verdict
	assert.Equal(t, whatifapi.VerdictNotJudged, v.Verdict, v.Summary)
	assert.Contains(t, v.Summary, phasesWords+" the load's cycles ran at IL ")
	assert.Contains(t, v.Summary, "the shape priced is IL 4569 OL 1855")
	assert.Nil(t, v.Needed, "no fleet is sized on a knee of other traffic")
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
// 99.1% outside every row, never below the point answer nor past the
// search's bound, and none for a row unbounded above (#297).
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
		// 17 x 1.991 / 8.5 = 3.98: 4.
		{"outside, the goldens' 99.1%", outside, 17, 2, 4, decodeKneeGoldensHigh, ", 4 decode if the decode knee reads 99% high"},
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

	// A row with no upper error bounds nothing: no count, not the point.
	open := whatif.Validation{Row: &whatif.EnvelopeRow{}}
	r = &reporter{load: 17, headroom: 0.85, kneeV: open}
	out = &whatifapi.ReportNeeded{Decode: 2}
	assert.Empty(t, r.atBound(base, 2, out))
	assert.Zero(t, out.DecodeAtErrorBound, "unbounded above")

	// Past the search's bound the count reads the bound, and says more.
	r = &reporter{load: 17, headroom: 0.85, kneeV: outside}
	out = &whatifapi.ReportNeeded{Decode: 2, MaxReplicas: 3}
	assert.Equal(t, ", more than 3 decode if the decode knee reads 99% high", r.atBound(base, 2, out))
	assert.Equal(t, 3, out.DecodeAtErrorBound)
	tiny := fleetEval{decodeKnee: whatif.Answer{Value: 1e-300}}
	out = &whatifapi.ReportNeeded{Decode: 2, MaxReplicas: 32}
	r.atBound(tiny, 2, out)
	assert.Equal(t, 32, out.DecodeAtErrorBound, "a tiny knee does not overflow the count")
}

// The 0.6B goldens' range the planner says and sizes at is the lowest and
// the highest of the decode knees that read high in
// testdata/decode-validation.golden and past its bound (OUT): a knee read
// within the bound is not a knee that read high. The pinned-B lines of e1,
// e1-tuned and the staircase B phases price no knee in the planner (the
// floor's intercept is off in its replay, issue
// llm-d-incubation/llm-d-scaling-manager#318), so the OUT knees are 43.0%,
// 67.9% and 99.1%.
func TestDecodeKneeGoldensMatchTheTable(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "decode-validation.golden"))
	require.NoError(t, err)
	re := regexp.MustCompile(`knee \+([0-9.]+)% over \d+ rows, OUT of`)
	ms := re.FindAllStringSubmatch(string(b), -1)
	high := make([]float64, 0, len(ms))
	for _, m := range ms {
		x, err := strconv.ParseFloat(m[1], 64)
		require.NoError(t, err)
		high = append(high, x/100)
	}
	require.NotEmpty(t, high, "no decode knee read high in the table")
	assert.InDelta(t, slices.Min(high), decodeKneeGoldensLow, 1e-9)
	assert.InDelta(t, slices.Max(high), decodeKneeGoldensHigh, 1e-9)
}
