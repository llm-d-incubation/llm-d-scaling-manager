package whatifapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi/apitest"
)

// mainEntry is a 13 h source with its per-rank series (rmFit's) whose
// prefill V_P the planner refuses (stubResolver.refusePrefill), as a P/D
// fleet whose prefill never queued: the default TTFT is refused at every
// rate. With model, the entry is resolved with the experimental TTFT model.
func mainEntry(t *testing.T, model bool) (*Server, *inputsEntry) {
	t.Helper()
	r := &stubResolver{refusePrefill: true}
	e := rmFit(t, r, 3120)
	e.engine.PrefillSlots = 4
	e.id = "sha256:main-ttft"
	if model {
		e.ttftModel = fitMainMixture(e, rankMixtureSwitches{stratified: whatif.DefaultRankMixtureStratified})
		info := e.ttftModel.info
		e.doc.TTFTModel = &info
		e.id += "-v2"
	}
	s := New(Config{Resolver: r, WhatifSchema: "schema-v3"})
	s.inputs.add(e.id, e)
	return s, e
}

// planBody prices req on s and decodes it.
func planBody(t *testing.T, s *Server, req PlanRequest) (PlanResponse, []byte) {
	t.Helper()
	body, err := s.plan(context.Background(), req)
	require.NoError(t, err)
	var out PlanResponse
	require.NoError(t, json.Unmarshal(body, &out))
	return out, body
}

// On a fleet whose default TTFT model refuses every rate, the experimental
// model, fitted on the source's window, prices the TTFT curve and point A:
// every point up to the planner's own lower knee (decode's, here) is
// answered, flagged RankMixtureTTFT, and the points above the highest step
// rate the window served are marked extrapolated, those under it not. The
// view names the model, its held-out evidence, the label for the dashed
// part, and, the prefill knee refused, that the right end is an estimate.
// Without the model the same fleet's curve is refused throughout.
func TestMainView_ExperimentalModelPricesTheRefusedCurve(t *testing.T) {
	s0, e0 := mainEntry(t, false)
	two := &FleetKnob{Decode: intp(2)}
	base, baseBody := planBody(t, s0, PlanRequest{InputsID: e0.id, Fleet: two})
	for _, p := range base.TTFT.Line.Curve {
		require.NotNil(t, p.Figure.Refused, "the default refuses the TTFT at %g r/s", p.Rate)
	}
	assert.Nil(t, base.TTFT.Model)
	assert.NotContains(t, string(baseBody), "extrapolated")

	s, e := mainEntry(t, true)
	mm := e.ttftModel
	require.Empty(t, mm.info.Abstained)
	require.NotNil(t, mm.info.HeldOut)
	h := mm.info.HeldOut
	assert.Positive(t, h.Steps)
	assert.Equal(t, "mean", h.Statistic)
	assert.LessOrEqual(t, h.Within+h.Low, h.Steps)
	assert.Positive(t, h.MedianRatio)
	assert.Less(t, h.RateFrom, h.RateTo)
	assert.Equal(t, 1, h.Prefill)
	top := *mm.info.MeasuredUpTo
	assert.InDelta(t, 2+0.05*239, top, 0.5, "the highest step rate the window served")

	out, body := planBody(t, s, PlanRequest{InputsID: e.id, Fleet: two})
	assert.Empty(t, apitest.CheckFigures(body), "%s", body)
	knee := *out.TTFT.Markers.DecodeKnee.Value
	require.Less(t, top, knee, "the fixture's knee is above its top rate")
	priced, beyond, refused := 0, 0, false
	for _, p := range out.TTFT.Line.Curve {
		if p.Rate > knee {
			assert.NotNil(t, p.Figure.Refused, "past the planner's knee the curve stops: %g r/s", p.Rate)
			continue
		}
		// Where a rank of the model saturates under the planner's knee, the
		// planner's refusal stands, at that rate and every one above it.
		if p.Figure.Value == nil {
			refused = true
			continue
		}
		require.False(t, refused, "a priced point above a refused one at %g r/s", p.Rate)
		assert.Contains(t, p.Figure.Flags, string(whatif.RankMixtureTTFT))
		assert.Equal(t, p.Rate > top, p.Figure.Extrapolated, "extrapolated at %g r/s (top %g)", p.Rate, top)
		priced++
		if p.Figure.Extrapolated {
			beyond++
		}
	}
	assert.Greater(t, priced, 30)
	assert.Positive(t, beyond)
	assert.Less(t, beyond, priced)
	// A rising curve.
	c := out.TTFT.Line.Curve
	assert.Less(t, *c[0].Figure.Value, *c[priced-1].Figure.Value)

	v := out.TTFT.Model
	require.NotNil(t, v)
	assert.True(t, v.Priced)
	assert.True(t, v.Experimental)
	assert.Equal(t, "Experimental TTFT model", v.Label)
	assert.Equal(t, "beyond the highest load measured", v.Beyond)
	assert.Equal(t, TTFTRightEndEstimate, v.RightEnd)
	assert.Regexp(t, `^held-out steps: \d+ of \d+ within 20%$`, v.Evidence)
	assert.InDelta(t, top, *v.MeasuredUpTo, 1e-9)
	require.NotNil(t, out.TTFT.Line.AtRate.Figure.Value, "point A is priced")
	assert.Contains(t, strings.Join(out.Assumed, "; "), "OUTSIDE VALIDATED REGION")
}

// At twice the recorded prefill replicas each replica's ranks take half the
// rate: the curve at 2r reads as the recorded fleet's at r, and what is
// measured scales with it.
func TestMainView_ExperimentalModelScalesWithPrefillReplicas(t *testing.T) {
	s, e := mainEntry(t, true)
	one, _ := planBody(t, s, PlanRequest{InputsID: e.id})
	two, _ := planBody(t, s, PlanRequest{InputsID: e.id, Fleet: &FleetKnob{Prefill: intp(2), Decode: intp(2)}})
	require.True(t, two.TTFT.Model.Priced)
	assert.InDelta(t, 2**one.TTFT.Model.MeasuredUpTo, *two.TTFT.Model.MeasuredUpTo, 1e-9)
	f1 := e.ttftModel.fleet(stubFleetFor(t, e, 1), Knobs{Prefill: 1, Decode: 1, TTFTPercentile: 0.9}).TTFTAt(3)
	f2 := e.ttftModel.fleet(stubFleetFor(t, e, 2), Knobs{Prefill: 2, Decode: 1, TTFTPercentile: 0.9}).TTFTAt(6)
	a, ok1 := valueOf(f1)
	b, ok2 := valueOf(f2)
	require.True(t, ok1 && ok2)
	assert.InDelta(t, a, b, 1e-12)
}

// stubFleetFor is the entry's pricer's fleet at n prefill replicas.
func stubFleetFor(t *testing.T, e *inputsEntry, n int) Fleet {
	t.Helper()
	fl, err := e.pricer.Fleet(Knobs{Prefill: n, Decode: 1})
	require.NoError(t, err)
	return fl
}

// A stated prompt shape is the planner's alone: the model prices the
// source's own traffic, and the view says why it did not price.
func TestMainView_ExperimentalModelLeavesAStatedShapeToThePlanner(t *testing.T) {
	s, e := mainEntry(t, true)
	out, _ := planBody(t, s, PlanRequest{InputsID: e.id, Traffic: &Traffic{Mix: &Mix{PromptTokens: 4000}}})
	require.NotNil(t, out.TTFT.Model)
	assert.False(t, out.TTFT.Model.Priced)
	assert.Contains(t, out.TTFT.Model.NotPriced, "request shape")
	for _, p := range out.TTFT.Line.Curve {
		assert.NotNil(t, p.Figure.Refused)
	}
}

// Where the model abstains (a source with no per-rank series, as the stub
// gives) the planner's answer stands: the plan's curve, limits and points
// are the default's, byte for byte, the view says why, and the inputs say
// the model abstained. An unknown model is the caller's error.
func TestMainView_ExperimentalModelAbstainsAndTheDefaultStands(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}, WhatifSchema: "schema-v3"}).Handler()
	code, body := post(t, h, "/v1/inputs", InputsRequest{Source: &SourceRequest{Kind: SourceRecording, Name: "run.jsonl.gz"},
		TTFTModel: string(whatif.QueueRankMixture)})
	require.Equal(t, http.StatusOK, code, string(body))
	var in InputsResponse
	require.NoError(t, json.Unmarshal(body, &in))
	require.NotNil(t, in.TTFTModel)
	assert.Contains(t, in.TTFTModel.Abstained, "per-rank series")
	plain := inputsOf(t, h)
	assert.NotEqual(t, plain, in.InputsID, "the model names its own entry")

	base, _ := plan(t, h, PlanRequest{InputsID: plain})
	got, _ := plan(t, h, PlanRequest{InputsID: in.InputsID})
	a, err := json.Marshal(base.TTFT.Line)
	require.NoError(t, err)
	b, err := json.Marshal(got.TTFT.Line)
	require.NoError(t, err)
	assert.JSONEq(t, string(a), string(b))
	require.NotNil(t, got.TTFT.Model)
	assert.False(t, got.TTFT.Model.Priced)
	assert.Contains(t, got.TTFT.Model.NotPriced, "abstained")

	code, body = post(t, h, "/v1/inputs", InputsRequest{TTFTModel: "v9"})
	assert.Equal(t, http.StatusBadRequest, code, string(body))
}

// The curve stops at the lower of the planner's knees; a prefill knee that
// is only a lower bound is raised to the load the fleet served, never
// past decode's knee; a refused prefill knee leaves decode's.
func TestCurveCap(t *testing.T) {
	for _, c := range []struct {
		name       string
		pv         float64
		pok, lower bool
		dv         float64
		dok        bool
		served     float64
		want       float64
		ok         bool
	}{
		{"prefill binds", 10, true, false, 20, true, 15, 10, true},
		{"a lower bound raised to the load served", 10, true, true, 20, true, 15, 15, true},
		{"a lower bound over the load served stays", 10, true, true, 20, true, 8, 10, true},
		{"never past decode's knee", 10, true, true, 12, true, 15, 12, true},
		{"prefill refused: decode's", 0, false, true, 12, true, 15, 12, true},
		{"decode refused: prefill's", 10, true, true, 0, false, 15, 15, true},
		{"none", 0, false, false, 0, false, 15, 0, false},
	} {
		got, ok := curveCap(c.pv, c.pok, c.lower, c.dv, c.dok, c.served)
		assert.Equal(t, c.ok, ok, c.name)
		assert.InDelta(t, c.want, got, 1e-12, c.name)
	}
}

// v2SizeOf sizes the entry's fleet for rate with the policy's TTFT target.
func v2SizeOf(t *testing.T, s *Server, id string, rate float64) SizeResponse {
	t.Helper()
	body, err := s.size(context.Background(), SizeRequest{InputsID: id, Traffic: &Traffic{Classes: &Classes{Critical: &ClassRate{Rate: f64(rate)}}},
		SLO: &SLORequest{TTFT: &Objective{Statistic: "p90", Target: f64(3)}}})
	require.NoError(t, err)
	var out SizeResponse
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

// Where the planner refuses prefill, so that no fleet is sized, the
// experimental model sizes one: its prefill limit is the rate whose TTFT
// meets the target, and the fleet's the lowest of that, headroom times the
// curve's end and decode's. The same rate grown by a quarter and by half is
// sized too, never on fewer replicas. Each limit the model set is flagged
// RankMixtureTTFT, and the plan the answer carries reads the same.
func TestMainView_ExperimentalModelSizesWhereThePlannerRefuses(t *testing.T) {
	s0, e0 := mainEntry(t, false)
	base := v2SizeOf(t, s0, e0.id, 4)
	assert.Nil(t, base.Answer, "the planner sizes no fleet without V_P")

	s, e := mainEntry(t, true)
	var prev *SizeAnswer
	for _, x := range []float64{1, 1.25, 1.5} {
		out := v2SizeOf(t, s, e.id, 4*x)
		require.NotNil(t, out.Answer, "sized at %g r/s: %+v", 4*x, out.Unmet)
		a := out.Answer
		require.NotNil(t, a.Limit.Value)
		assert.GreaterOrEqual(t, *a.Limit.Value, 4*x*(1-reachTolerance))
		assert.Contains(t, a.Limit.Flags, string(whatif.RankMixtureTTFT))
		top := *e.ttftModel.info.MeasuredUpTo * float64(a.Fleet.Prefill) / float64(e.ttftModel.prefill)
		assert.Equal(t, *a.Limit.Value > top, a.Limit.Extrapolated, "a limit past the load served at %d prefill is extrapolated", a.Fleet.Prefill)
		require.NotNil(t, a.Plan.Limits.Prefill.Value)
		assert.Contains(t, a.Plan.Limits.Prefill.Flags, string(whatif.RankMixtureTTFT))
		if prev != nil {
			assert.GreaterOrEqual(t, a.Fleet.Prefill+a.Fleet.Decode, prev.Fleet.Prefill+prev.Fleet.Decode)
		}
		prev = a
	}
}

// The model's prefill limit is where its curve meets the target: the TTFT
// just under it meets the target, and just over it does not.
func TestMixtureFleet_PrefillLimitIsWhereTheCurveMeetsTheTarget(t *testing.T) {
	_, e := mainEntry(t, true)
	k := Knobs{Prefill: 1, Decode: 2, TTFTPercentile: 0.9}
	fl, ok := e.ttftModel.fleet(stubFleetFor(t, e, 1), k).(*mixtureFleet)
	require.True(t, ok)
	lo, _ := fl.price(1)
	hi, okHi := fl.price(fl.knee * 0.8)
	require.True(t, okHi)
	target := (lo + hi) / 2
	r, atEnd, ok := fl.rateAtTarget(target)
	require.True(t, ok)
	assert.False(t, atEnd)
	// The draws are fixed but finite: the curve is monotone to within a
	// percent or so, not exactly.
	at, _ := fl.price(r)
	assert.LessOrEqual(t, at, target)
	assert.InEpsilon(t, target, at, 0.02)
	assert.Less(t, r, fl.knee*0.8, "under the rate whose TTFT is over the target")
	_, atEnd, ok = fl.rateAtTarget(1e6)
	assert.True(t, ok)
	assert.True(t, atEnd, "a target the whole curve meets is met at its end")
	_, _, ok = fl.rateAtTarget(lo / 10)
	assert.False(t, ok, "a target under the floor is met nowhere")
}

// A limit the curve's end sets is "at least" (VPLowerBound) only where that
// end is a prefill knee known as a lower bound; decode's knee, or a priced
// prefill knee, is no lower bound.
func TestMixtureFleet_AtLeastOnlyOnALowerBoundPrefillKnee(t *testing.T) {
	for _, c := range []struct {
		name string
		f    mixtureFleet
		want bool
	}{
		{"decode's knee ends the curve", mixtureFleet{lowerBound: true, endAtLeast: false}, false},
		{"a lower-bound prefill knee ends it", mixtureFleet{lowerBound: true, endAtLeast: true}, true},
	} {
		got := slices.Contains(c.f.limitFlags(true), whatif.VPLowerBound)
		assert.Equal(t, c.want, got, c.name)
		assert.NotContains(t, c.f.limitFlags(false), whatif.VPLowerBound, "%s: a limit the target set is no bound", c.name)
	}
	_, e := mainEntry(t, true)
	fl, ok := e.ttftModel.fleet(stubFleetFor(t, e, 1), Knobs{Prefill: 1, Decode: 2, TTFTPercentile: 0.9}).(*mixtureFleet)
	require.True(t, ok)
	assert.True(t, fl.lowerBound, "the stub refuses the prefill knee")
	assert.False(t, fl.endAtLeast, "the curve ends at decode's knee there")
}

// The curve's end is "at least" only where a prefill knee that is only a
// lower bound ends it: not decode's knee, not a tie with it (the fleet stops
// at decode's), and not a refused or a priced prefill knee.
func TestEndAtLeast(t *testing.T) {
	for _, c := range []struct {
		name       string
		pv         float64
		pok, lower bool
		dv         float64
		dok        bool
		served     float64
		want       bool
	}{
		{"a lower-bound prefill knee under decode's", 10, true, true, 20, true, 8, true},
		{"raised to the load served, still under decode's", 10, true, true, 20, true, 15, true},
		{"raised past decode's: decode's ends it", 10, true, true, 12, true, 15, false},
		{"a tie with decode's: decode's ends it", 12, true, true, 12, true, 8, false},
		{"a priced prefill knee", 10, true, false, 20, true, 8, false},
		{"a refused prefill knee: decode's", 0, false, true, 12, true, 15, false},
		{"decode refused: the lower-bound prefill knee", 10, true, true, 0, false, 15, true},
	} {
		assert.Equal(t, c.want, endAtLeast(c.pv, c.pok, c.lower, c.dv, c.dok, c.served), c.name)
	}
}

// The model's fleet limit is the lowest of headroom times the curve's end,
// the prefill limit and decode's, named by what binds; a refused decode or
// prefill limit is the fleet's. Decode's bound is named as the planner names
// it, and only a binding curve's end that is "at least" carries
// VPLowerBound.
func TestMixtureFleet_FleetLimit(t *testing.T) {
	f := &mixtureFleet{knee: 10, kneeOK: true, endAtLeast: true}
	prefill := whatif.Answer{Value: 7, Flags: []whatif.Flag{whatif.RankMixtureTTFT}}
	prefill.Basis.BoundBy = []whatif.Bound{"TTFT"}
	decode := whatif.Answer{Value: 6}
	decode.Basis.BoundBy = []whatif.Bound{"TPOT p90"}

	a, ok := f.fleetLimit(0.85, decode, prefill).(whatif.Answer)
	require.True(t, ok)
	assert.InDelta(t, 6, a.Value, 1e-12, "decode binds")
	assert.Equal(t, []whatif.Bound{"decode TPOT p90"}, a.Basis.BoundBy)
	assert.Equal(t, []whatif.Flag{whatif.RankMixtureTTFT}, a.Flags)

	a, ok = f.fleetLimit(0.5, decode, prefill).(whatif.Answer)
	require.True(t, ok)
	assert.InDelta(t, 5, a.Value, 1e-12, "the cap binds")
	assert.Equal(t, []whatif.Bound{whatif.BoundOperatingPoint}, a.Basis.BoundBy)
	assert.Contains(t, a.Flags, whatif.VPLowerBound, "a lower-bound end is at least")

	a, ok = f.fleetLimit(1, nil, prefill).(whatif.Answer)
	require.True(t, ok)
	assert.InDelta(t, 7, a.Value, 1e-12, "no TPOT asked: prefill binds")
	assert.NotContains(t, a.Flags, whatif.VPLowerBound)

	refused := whatif.Unanswerable{Reason: whatif.NoPrefillCapacity}
	assert.Equal(t, whatif.Result(refused), f.fleetLimit(1, refused, prefill), "a refused decode limit is the fleet's")
	assert.Equal(t, whatif.Result(refused), f.fleetLimit(1, decode, refused), "a refused prefill limit is the fleet's")
}

// With no TPOT asked, so that the fleet grows with the rate past the load the
// source served, every fleet limit the model set reads extrapolated exactly
// where it passes that load at its own prefill count: the answer's, the
// plan's +1 steps' and the one-fewer steps'. Some of them do, so the check is
// not empty.
func TestMainView_ExperimentalModelMarksEveryExtrapolatedLimit(t *testing.T) {
	s, e := mainEntry(t, true)
	none := "none"
	var checked, extrapolated int
	check := func(what string, n FleetCounts, f Figure) {
		t.Helper()
		if f.Value == nil {
			return
		}
		assert.Contains(t, f.Flags, string(whatif.RankMixtureTTFT), what)
		top := *e.ttftModel.info.MeasuredUpTo * float64(n.Prefill) / float64(e.ttftModel.prefill)
		assert.Equal(t, *f.Value > top, f.Extrapolated, "%s at %+v: %g r/s against %g served", what, n, *f.Value, top)
		checked++
		if f.Extrapolated {
			extrapolated++
		}
	}
	var prev *SizeAnswer
	for _, rate := range []float64{4, 12, 24} {
		body, err := s.size(context.Background(), SizeRequest{InputsID: e.id, Traffic: &Traffic{Classes: &Classes{Critical: &ClassRate{Rate: f64(rate)}}},
			SLO: &SLORequest{TTFT: &Objective{Statistic: "p90", Target: f64(3)}, TPOT: &Objective{Statistic: none}}})
		require.NoError(t, err)
		var out SizeResponse
		require.NoError(t, json.Unmarshal(body, &out))
		require.NotNil(t, out.Answer, "sized at %g r/s: %+v", rate, out.Unmet)
		a := out.Answer
		check("the answer", a.Fleet, a.Limit)
		check("+1 prefill", a.Plan.Marginal.PrefillPlusOne.Fleet, a.Plan.Marginal.PrefillPlusOne.Limit)
		check("+1 decode", a.Plan.Marginal.DecodePlusOne.Fleet, a.Plan.Marginal.DecodePlusOne.Limit)
		for _, st := range []*SizeStep{a.FewerPrefill, a.FewerDecode} {
			if st != nil {
				check("one fewer", st.Fleet, st.Limit)
			}
		}
		if prev != nil {
			assert.Greater(t, a.Fleet.Prefill+a.Fleet.Decode, prev.Fleet.Prefill+prev.Fleet.Decode, "%g r/s needs more than the rate before", rate)
		}
		prev = a
	}
	assert.Positive(t, extrapolated, "some limit is past the load served")
	assert.Less(t, extrapolated, checked, "and some is not")
}

// Unasked (no ExperimentalModels), a day plan is the default TTFT model's, also on inputs resolved with the
// experimental one: its buckets, whose cycles read no lengths, sit at the
// window's own shape, where the model prices, yet every figure, refusal and
// count is the default's, so that no plan mixes a model-priced prefill with
// a default-priced decode. The same inputs' size is the model's, so the
// check is not empty.
func TestDayPlan_ExperimentalModelLeavesTheDayPlanToTheDefault(t *testing.T) {
	plan := func(model bool) DayPlanResponse {
		t.Helper()
		s, e := mainEntry(t, model)
		for i := range e.series {
			e.series[i].HasShape = false
		}
		body, err := s.dayPlan(context.Background(), DayPlanRequest{InputsID: e.id,
			SLO: &SLORequest{TTFT: &Objective{Statistic: "p90", Target: f64(3)}}})
		require.NoError(t, err)
		var out DayPlanResponse
		require.NoError(t, json.Unmarshal(body, &out))
		return out
	}
	base, v2 := plan(false), plan(true)
	require.Positive(t, base.Summary.Counts.Sized, "the default sizes some bucket")
	for _, b := range base.Buckets {
		assert.Equal(t, "window", b.Shape.Source, "the bucket at %s is at the window's shape", b.Start)
	}
	assert.Equal(t, base.Summary.Counts.Sized, base.Summary.Counts.PrefillNotPriced, "the default prices no prefill: V_P is refused")

	s, e := mainEntry(t, true)
	require.NotNil(t, v2SizeOf(t, s, e.id, 4).Answer, "the model sizes the same inputs")

	assert.Equal(t, base.Refused, v2.Refused)
	assert.Equal(t, base.Summary, v2.Summary)
	assert.Equal(t, base.Buckets, v2.Buckets)
}

// Asked for (ExperimentalModels), a day plan on inputs resolved with the
// experimental TTFT model is priced with it, as a size on those inputs is:
// where the planner refuses prefill, the model sizes it, each such bucket's
// limit flagged RankMixtureTTFT, and the plan names the model it used. It is
// a plan of its own, under its own id. Asked on inputs resolved with no
// model, it is refused rather than answered as the planner's own.
func TestDayPlan_ExperimentalModelsSizePrefillWhereThePlannerRefuses(t *testing.T) {
	plan := func(model, asked bool) (DayPlanResponse, error) {
		t.Helper()
		s, e := mainEntry(t, model)
		for i := range e.series {
			e.series[i].HasShape = false
		}
		body, err := s.dayPlan(context.Background(), DayPlanRequest{InputsID: e.id, ExperimentalModels: asked,
			SLO: &SLORequest{TTFT: &Objective{Statistic: "p90", Target: f64(3)}}})
		var out DayPlanResponse
		if err == nil {
			require.NoError(t, json.Unmarshal(body, &out))
		}
		return out, err
	}
	base, err := plan(true, false)
	require.NoError(t, err)
	v2, err := plan(true, true)
	require.NoError(t, err)
	require.Positive(t, base.Summary.Counts.Sized)
	assert.Equal(t, base.Summary.Counts.Sized, base.Summary.Counts.PrefillNotPriced, "unasked, the planner's own prices no prefill")
	assert.Nil(t, base.TTFTModel, "unasked, no model is named")

	require.NotNil(t, v2.TTFTModel, "the plan names the model it was asked to price with")
	assert.Empty(t, v2.TTFTModel.Abstained)
	assert.Nil(t, v2.TPOTModel, "the inputs carry no TPOT model")
	assert.NotEqual(t, base.DayPlanID, v2.DayPlanID, "a plan priced with the model is another plan")
	assert.Less(t, v2.Summary.Counts.PrefillNotPriced, v2.Summary.Counts.Sized, "the model prices prefill the planner refuses")
	priced := 0
	for _, b := range v2.Buckets {
		if b.Needed == nil || b.Needed.Prefill == nil {
			continue
		}
		priced++
		require.NotNil(t, b.Limit)
		assert.Contains(t, b.Limit.Flags, string(whatif.RankMixtureTTFT), "the bucket at %s says the model sized it", b.Start)
		assert.NotContains(t, b.Flags, FlagPrefillNotPriced)
	}
	require.Positive(t, priced, "some bucket's prefill is sized")
	t.Logf("sized %d buckets: prefill sized in %d with the model, %d without", v2.Summary.Counts.Sized, priced,
		base.Summary.Counts.Sized-base.Summary.Counts.PrefillNotPriced)
	for i, b := range v2.Buckets[:min(5, len(v2.Buckets))] {
		if b.Needed != nil && b.Needed.Prefill != nil && i < len(base.Buckets) && base.Buckets[i].Needed != nil {
			t.Logf("%s at %.2f r/s: %s -> %d prefill + %d decode, had %v", b.Start.Format("15:04"), *b.SizedRate,
				neededWords(base.Buckets[i].Needed), *b.Needed.Prefill, b.Needed.Decode, b.Actual)
		}
	}
	assert.True(t, slices.ContainsFunc(v2.Notes, func(n string) bool {
		return strings.Contains(n, fmt.Sprintf("priced %d of the %d sized buckets", priced, v2.Summary.Counts.Sized))
	}), "the notes count the buckets it priced: %q", v2.Notes)

	_, err = plan(false, true)
	require.Error(t, err)
	assert.True(t, IsInputError(err))
	assert.Contains(t, err.Error(), "experimentalModels")
}

// neededWords is a bucket's need as the log line says it.
func neededWords(n *DayNeeded) string {
	if n.Prefill == nil {
		return fmt.Sprintf("at least %d decode (prefill not sized)", n.Decode)
	}
	return fmt.Sprintf("%d prefill + %d decode", *n.Prefill, n.Decode)
}
