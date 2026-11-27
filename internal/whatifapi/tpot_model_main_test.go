package whatifapi

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi/apitest"
)

// tpotEntry is a 4 h source with its decode series (two replicas, a fifth
// apart in token time, two of three drafts accepted a step, output lengths
// from two tokens to a thousand) and router TPOT histograms drawn from the
// distribution (tdEntries' fit), recorded at two decode replicas, whose
// stub refuses every TPOT percentile (no R_q). With model, the entry is
// resolved with the experimental TPOT model, its decode engine read at
// slots max-num-seqs and three speculative tokens.
func tpotEntry(t *testing.T, model bool, slots int) (*Server, *inputsEntry) {
	t.Helper()
	e, _ := tdEntries(t, 960)
	e.doc.Fleet.Decode.Replicas = 2
	e.engine = EngineSlots{DecodeSlots: slots, SpecTokens: 3}
	e.id = "sha256:main-tpot"
	if model {
		e.tpotModel = fitMainTPOT(e, "", 0)
		info := e.tpotModel.info
		e.doc.TPOTModel = &info
		e.id += "-v2"
	}
	s := New(Config{Resolver: &stubResolver{}, WhatifSchema: "schema-v3"})
	s.inputs.add(e.id, e)
	return s, e
}

// tpotTarget is the TPOT target the plans of tpotEntry are asked at.
const tpotTarget = 0.02

// tpotPlan is the plan of the entry at two prefill and two decode replicas
// with a TPOT target of tpotTarget at statistic.
func tpotPlan(t *testing.T, s *Server, id, statistic string, mix *Mix) (PlanResponse, []byte) {
	t.Helper()
	req := PlanRequest{InputsID: id, Fleet: &FleetKnob{Prefill: intp(2), Decode: intp(2)},
		SLO: &SLORequest{TPOT: &Objective{Statistic: statistic, Target: f64(tpotTarget)}}}
	if mix != nil {
		req.Traffic = &Traffic{Mix: mix}
	}
	return planBody(t, s, req)
}

// On a fleet whose planner refuses every TPOT percentile, the experimental
// TPOT model, fitted on the source's window, prices the p90 curve, point A
// and decode's limit at the target: each point is the planner's own mean
// times the model's ratio at the window's output lengths and decode
// spread, flagged TPOTDistribution, the points above the highest step rate
// the window served (per decode replica) marked extrapolated and those
// under it not. Decode's limit is the rate whose mean meets the target over
// the ratio. Without the model the same fleet's percentile is refused
// throughout, and the plan names no part of the model.
func TestMainViewTPOT_ExperimentalModelPricesThePercentileCurve(t *testing.T) {
	const target = tpotTarget
	s0, e0 := tpotEntry(t, false, 64)
	base, baseBody := tpotPlan(t, s0, e0.id, "p90", nil)
	for _, p := range base.TPOT.Curve {
		require.NotNil(t, p.Figure.Refused, "the default refuses the p90 at %g r/s", p.Rate)
	}
	require.NotNil(t, base.TPOT.AtTarget.Rate.Refused)
	assert.Nil(t, base.TPOT.Model)
	for _, word := range []string{"extrapolated", "TPOTDistribution", "tpotModel", tpotModelSource} {
		assert.NotContains(t, string(baseBody), word)
	}

	s, e := tpotEntry(t, true, 64)
	mt := e.tpotModel
	require.Empty(t, mt.info.Abstained)
	fit := mt.info.Fit
	require.NotNil(t, fit)
	assert.Greater(t, fit.C, fit.Renewal, "C is fitted above its speculative-decoding floor")
	assert.Positive(t, fit.Renewal)
	h := mt.info.HeldOut
	require.NotNil(t, h)
	assert.Positive(t, h.Steps)
	assert.LessOrEqual(t, h.P90Inside, h.Steps)
	assert.LessOrEqual(t, h.P95Inside, h.Steps)
	assert.Equal(t, 2, h.Decode)
	assert.Regexp(t, `^held-out steps, at the measured mean: p90 in the measured p90's joint bracket on \d+ of \d+, p95 on \d+$`, mt.info.Evidence)
	require.NotNil(t, mt.info.P95Ratio)
	top := *mt.info.MeasuredUpTo
	assert.InDelta(t, 13.5, top, 0.5, "the highest step rate the window served")

	// The ratio is the lognormal's at the window's spread: above one, and
	// the p95's above the p90's.
	s2 := mt.dist.GroupCV2 + mt.dist.C*mt.inv
	r90 := math.Exp(math.Sqrt(s2)*1.2815515655446004 - s2/2)
	assert.InEpsilon(t, r90, mt.ratio(0.9), 1e-9)
	assert.Greater(t, mt.ratio(0.9), 1.0)
	assert.Greater(t, mt.ratio(0.95), mt.ratio(0.9))

	out, body := tpotPlan(t, s, e.id, "p90", nil)
	assert.Empty(t, apitest.CheckFigures(body), "%s", body)
	priced, beyond := 0, 0
	for _, p := range out.TPOT.Curve {
		require.NotNil(t, p.Figure.Value, "the model prices %g r/s", p.Rate)
		mean := 0.005 + p.Rate/1000
		assert.InEpsilon(t, mt.ratio(0.9)*mean, *p.Figure.Value, 1e-9, "the planner's mean times the ratio at %g r/s", p.Rate)
		assert.Contains(t, p.Figure.Flags, string(whatif.TPOTDistribution))
		assert.Equal(t, p.Rate > top, p.Figure.Extrapolated, "extrapolated at %g r/s (top %g)", p.Rate, top)
		priced++
		if p.Figure.Extrapolated {
			beyond++
		}
	}
	assert.Greater(t, priced, 30)
	assert.Positive(t, beyond)
	assert.Less(t, beyond, priced)

	v := out.TPOT.Model
	require.NotNil(t, v)
	assert.True(t, v.Priced)
	assert.True(t, v.Experimental)
	assert.Equal(t, TPOTModelLabel, v.Label)
	assert.Equal(t, TTFTBeyondMeasured, v.Beyond)
	assert.Equal(t, mt.info.Evidence, v.Evidence)
	assert.InDelta(t, top, *v.MeasuredUpTo, 1e-9)
	require.NotNil(t, out.TPOT.Ratio)
	assert.Equal(t, tpotModelSource, out.TPOT.Ratio.Source)
	assert.InEpsilon(t, mt.ratio(0.9), *out.TPOT.Ratio.Value, 1e-12)

	a := out.TPOT.AtRate.Figure
	require.NotNil(t, a.Value, "point A is priced")
	assert.Contains(t, a.Flags, string(whatif.TPOTDistribution))
	limit := out.TPOT.AtTarget.Rate
	require.NotNil(t, limit.Value, "decode's limit at the target is priced")
	assert.InEpsilon(t, (target/mt.ratio(0.9)-0.005)*1000, *limit.Value, 1e-9, "the mean's limit at the target over the ratio")
	assert.Contains(t, limit.Flags, string(whatif.TPOTDistribution))
	assert.False(t, limit.Extrapolated, "a limit under the load served is not extrapolated")
	assert.InEpsilon(t, target, mt.ratio(0.9)*(0.005+*limit.Value/1000), 1e-9, "the p90 at the limit is the target")
	assert.Contains(t, strings.Join(out.Assumed, "; "), "experimental TPOT distribution")
}

// The per-length mixture (whatif.TPOTPerLengthMixture) is fitted and named as
// the one lognormal is, and prices each rate at the source window's pooled
// output lengths: its ratio is the mixture's quantile at a mean of one, not
// the lognormal's at the pooled E[1/(L-1)]. An unknown model is refused.
func TestMainViewTPOT_PerLengthMixture(t *testing.T) {
	e, _ := tdEntries(t, 960)
	e.doc.Fleet.Decode.Replicas = 2
	e.engine = EngineSlots{DecodeSlots: 64, SpecTokens: 3}
	one, mix := fitMainTPOT(e, "", 0), fitMainTPOT(e, whatif.TPOTPerLengthMixture, 0)
	require.Empty(t, one.info.Abstained)
	require.Empty(t, mix.info.Abstained)
	assert.Equal(t, whatif.TPOTRenewalLognormal, one.info.Model)
	assert.Equal(t, whatif.TPOTPerLengthMixture, mix.info.Model)
	assert.Empty(t, one.dist.PerLength)
	require.NotEmpty(t, mix.dist.PerLength)
	require.NotNil(t, mix.info.P90Ratio)
	assert.InDelta(t, mix.dist.Quantile(1, 0, 0.9), *mix.info.P90Ratio, 1e-12)
	assert.NotEqual(t, *one.info.P90Ratio, *mix.info.P90Ratio)
	assert.Contains(t, mix.tq.report.Assumed, "one lognormal per length")
	_, err := tpotModelOf("renewal-lognormal-mix", "")
	require.Error(t, err)
	m, err := tpotModelOf("", whatif.TPOTPerLengthMixture)
	require.NoError(t, err)
	assert.Equal(t, whatif.TPOTPerLengthMixture, m)
}

// The p90 and the p95 are priced, each at its own ratio: the curve is the
// planner's mean times it, flagged, and decode's limit at the target the
// mean's over it. Any other percentile, and the mean, are the planner's: the
// plan's TPOT view, limits and marginal are the flag-off plan's, but for
// the view saying why.
func TestMainViewTPOT_PricesP90AndP95Only(t *testing.T) {
	s, e := tpotEntry(t, true, 64)
	s0, e0 := tpotEntry(t, false, 64)
	for st, q := range map[string]float64{"p90": 0.9, "p95": 0.95} {
		got, _ := tpotPlan(t, s, e.id, st, nil)
		require.True(t, got.TPOT.Model.Priced, st)
		for _, p := range got.TPOT.Curve {
			require.NotNil(t, p.Figure.Value, "%s at %g r/s", st, p.Rate)
			assert.InEpsilon(t, e.tpotModel.ratio(q)*(0.005+p.Rate/1000), *p.Figure.Value, 1e-9, "%s at %g r/s", st, p.Rate)
			assert.Contains(t, p.Figure.Flags, string(whatif.TPOTDistribution))
		}
		require.NotNil(t, got.TPOT.AtTarget.Rate.Value, st)
		assert.InEpsilon(t, (tpotTarget/e.tpotModel.ratio(q)-0.005)*1000, *got.TPOT.AtTarget.Rate.Value, 1e-9, st)
	}
	assert.Greater(t, e.tpotModel.ratio(0.95), e.tpotModel.ratio(0.9))

	for _, st := range []string{"p50", "p99", "mean"} {
		base, _ := tpotPlan(t, s0, e0.id, st, nil)
		got, _ := tpotPlan(t, s, e.id, st, nil)
		require.NotNil(t, got.TPOT.Model, st)
		assert.False(t, got.TPOT.Model.Priced, st)
		assert.Contains(t, got.TPOT.Model.NotPriced, "p90 and p95", st)
		got.TPOT.Model = nil
		assert.Equal(t, base.TPOT, got.TPOT, "%s: the planner's TPOT view stands", st)
		assert.Equal(t, base.Limits, got.Limits, "%s: the planner's limits stand", st)
		assert.Equal(t, base.Marginal, got.Marginal, st)
	}
}

// Negative controls: a stated output length, or a stated R_q, is the
// planner's alone; and where the model abstains (max-num-seqs not read, a
// slot queue, too short a window) the plan's TPOT is the default's, byte
// for byte, with the view saying why.
func TestMainViewTPOT_DefaultStandsWhereTheModelDoesNotPrice(t *testing.T) {
	s, e := tpotEntry(t, true, 64)
	stated, _ := tpotPlan(t, s, e.id, "p90", &Mix{OutputTokens: 500})
	assert.False(t, stated.TPOT.Model.Priced)
	assert.Contains(t, stated.TPOT.Model.NotPriced, "output lengths")
	for _, p := range stated.TPOT.Curve {
		assert.NotNil(t, p.Figure.Refused)
	}
	ratio := 1.5
	req := PlanRequest{InputsID: e.id, Fleet: &FleetKnob{Prefill: intp(2), Decode: intp(2)},
		SLO: &SLORequest{TPOT: &Objective{Statistic: "p90", Target: f64(0.02)}}, Assume: &Assume{TPOTRatio: &ratio}}
	assumed, _ := planBody(t, s, req)
	assert.False(t, assumed.TPOT.Model.Priced)
	assert.Contains(t, assumed.TPOT.Model.NotPriced, "ratio stated")
	assert.Equal(t, "assumed", assumed.TPOT.Ratio.Source)

	s0, e0 := tpotEntry(t, false, 64)
	base, _ := tpotPlan(t, s0, e0.id, "p90", nil)
	want, err := json.Marshal(base.TPOT)
	require.NoError(t, err)
	for name, slots := range map[string]int{"max-num-seqs": 0, "would wait for one of its": 4} {
		sa, ea := tpotEntry(t, true, slots)
		require.Contains(t, ea.tpotModel.info.Abstained, name)
		got, _ := tpotPlan(t, sa, ea.id, "p90", nil)
		require.NotNil(t, got.TPOT.Model)
		assert.False(t, got.TPOT.Model.Priced)
		assert.Contains(t, got.TPOT.Model.NotPriced, "abstained")
		got.TPOT.Model = nil
		b, err := json.Marshal(got.TPOT)
		require.NoError(t, err)
		assert.JSONEq(t, string(want), string(b), name)
	}
	short, _ := tdEntries(t, 200)
	short.engine = EngineSlots{DecodeSlots: 64, SpecTokens: 3}
	assert.Contains(t, fitMainTPOT(short, "", 0).info.Abstained, "fit steps")
	nodecode, _ := tdEntries(t, 960)
	nodecode.decode = nil
	assert.Contains(t, fitMainTPOT(nodecode, "", 0).info.Abstained, "decode series")
}

// An inputs request names the model, and gets an entry of its own; on the
// stub, which gives no decode series, the model abstains. An unknown
// model is the caller's error.
func TestMainViewTPOT_InputsRequest(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}, WhatifSchema: "schema-v3"}).Handler()
	code, body := post(t, h, "/v1/inputs", InputsRequest{Source: &SourceRequest{Kind: SourceRecording, Name: "run.jsonl.gz"},
		TPOTModel: whatif.TPOTRenewalLognormal})
	require.Equal(t, http.StatusOK, code, string(body))
	var in InputsResponse
	require.NoError(t, json.Unmarshal(body, &in))
	require.NotNil(t, in.TPOTModel)
	assert.Nil(t, in.TTFTModel)
	assert.Contains(t, in.TPOTModel.Abstained, "decode series")
	plain := inputsOf(t, h)
	assert.NotEqual(t, plain, in.InputsID, "the model names its own entry")
	code, body = post(t, h, "/v1/inputs", InputsRequest{Source: &SourceRequest{Kind: SourceRecording, Name: "run.jsonl.gz"},
		TTFTModel: string(whatif.QueueRankMixture), TPOTModel: whatif.TPOTRenewalLognormal})
	require.Equal(t, http.StatusOK, code, string(body))
	var both InputsResponse
	require.NoError(t, json.Unmarshal(body, &both))
	assert.NotEqual(t, in.InputsID, both.InputsID)
	assert.NotNil(t, both.TTFTModel)
	assert.NotNil(t, both.TPOTModel)

	code, body = post(t, h, "/v1/inputs", InputsRequest{TPOTModel: "ratio"})
	assert.Equal(t, http.StatusBadRequest, code, string(body))
}

// Where the planner refuses every TPOT percentile, so that no fleet is
// sized at a p90 target, the experimental model sizes one: decode's limit
// is the model's inversion, flagged, and the plan the answer carries reads
// the same.
func TestMainViewTPOT_SizesWhereThePlannerRefuses(t *testing.T) {
	size := func(s *Server, id string) SizeResponse {
		body, err := s.size(context.Background(), SizeRequest{InputsID: id, Traffic: &Traffic{Classes: &Classes{Critical: &ClassRate{Rate: f64(3)}}},
			SLO: &SLORequest{TPOT: &Objective{Statistic: "p90", Target: f64(0.02)}}})
		require.NoError(t, err)
		var out SizeResponse
		require.NoError(t, json.Unmarshal(body, &out))
		return out
	}
	s0, e0 := tpotEntry(t, false, 64)
	assert.Nil(t, size(s0, e0.id).Answer, "the planner sizes no fleet without R_q")

	s, e := tpotEntry(t, true, 64)
	out := size(s, e.id)
	require.NotNil(t, out.Answer, "%+v", out.Unmet)
	d := out.Answer.Plan.Limits.Decode
	require.NotNil(t, d)
	require.NotNil(t, d.Value)
	assert.Contains(t, d.Flags, string(whatif.TPOTDistribution))
	assert.GreaterOrEqual(t, *d.Value, 3*(1-reachTolerance))
	assert.InEpsilon(t, (0.02/e.tpotModel.ratio(0.9)-0.005)*1000, *d.Value, 1e-9)
}

// The report's measured shape has its TPOT percentile repriced around each
// row's own mean, flagged and marked past the load served; another
// shape, and a stated R_q, are the planner's.
func TestMainViewTPOT_Report(t *testing.T) {
	_, e := tpotEntry(t, true, 64)
	mt := e.tpotModel
	top := *mt.info.MeasuredUpTo
	row := func(rate float64) ReportRow {
		return ReportRow{Rate: rate, TPOTMean: Figure{Value: f64(0.01), Unit: unitSeconds},
			TPOTPercentile: &ReportStat{Statistic: "p90", Figure: Figure{Refused: &Refusal{Reason: string(whatif.NoTPOTHistogram)}}}}
	}
	rep := func() *ReportResponse {
		p95 := row(top / 2)
		p95.TPOTPercentile.Statistic = "p95"
		return &ReportResponse{Fleet: ReportFleet{Decode: 2}, Shapes: []ReportShape{
			{Label: "measured", Table: []ReportRow{row(top / 2), row(top * 1.2), p95}},
			{Label: "long", Table: []ReportRow{row(top / 2)}}}}
	}
	out := rep()
	mt.applyToReport(out, ReportQuery{})
	m := out.Shapes[0]
	for i, want := range []bool{false, true} {
		st := m.Table[i].TPOTPercentile
		require.NotNil(t, st.Value)
		assert.InEpsilon(t, mt.ratio(0.9)*0.01, *st.Value, 1e-12)
		assert.Contains(t, st.Flags, string(whatif.TPOTDistribution))
		assert.Equal(t, want, st.Extrapolated, "row %d", i)
	}
	assert.Contains(t, m.Flags, string(whatif.TPOTDistribution))
	assert.Contains(t, strings.Join(m.Assumed, "; "), "summary cells are the planner's own")
	p95 := m.Table[2].TPOTPercentile
	require.NotNil(t, p95.Value, "a p95 row is the model's")
	assert.InEpsilon(t, mt.ratio(0.95)*0.01, *p95.Value, 1e-12)
	assert.Contains(t, p95.Flags, string(whatif.TPOTDistribution))
	assert.NotNil(t, out.Shapes[1].Table[0].TPOTPercentile.Refused, "another shape is the planner's")

	stated := rep()
	r := 1.5
	mt.applyToReport(stated, ReportQuery{Assume: Assume{TPOTRatio: &r}})
	assert.Equal(t, rep(), stated, "a stated R_q is the planner's")

	_, ea := tpotEntry(t, true, 0)
	abst := rep()
	ea.tpotModel.applyToReport(abst, ReportQuery{})
	assert.Equal(t, rep(), abst, "an abstained model prices nothing")
}

// At twice the recorded decode replicas each replica takes half the rate:
// the load measured doubles, and a point is extrapolated only past it.
func TestMainViewTPOT_MeasuredScalesWithDecodeReplicas(t *testing.T) {
	s, e := tpotEntry(t, true, 64)
	top := *e.tpotModel.info.MeasuredUpTo
	req := PlanRequest{InputsID: e.id, Fleet: &FleetKnob{Prefill: intp(4), Decode: intp(4)},
		SLO: &SLORequest{TPOT: &Objective{Statistic: "p90", Target: f64(0.02)}}}
	out, _ := planBody(t, s, req)
	require.True(t, out.TPOT.Model.Priced)
	assert.InDelta(t, 2*top, *out.TPOT.Model.MeasuredUpTo, 1e-9)
	beyond := 0
	for _, p := range out.TPOT.Curve {
		require.NotNil(t, p.Figure.Value)
		assert.Equal(t, p.Rate > 2*top, p.Figure.Extrapolated, "extrapolated at %g r/s (measured %g at 4 decode)", p.Rate, 2*top)
		if p.Figure.Extrapolated {
			beyond++
		}
	}
	assert.Positive(t, beyond)
	assert.Contains(t, strings.Join(out.Assumed, "; "), "read at 2 decode replicas and taken at 4")
}

// A limit the model set past the load served, per decode replica, is
// marked extrapolated wherever it is shown, as the plan's own limits are:
// the plan's one-more-replica steps and a size's one-fewer steps too. Each
// is marked exactly where it is the model's and past the load served.
func TestMainViewTPOT_LimitsPastTheLoadServedAreMarked(t *testing.T) {
	s, e := tpotEntry(t, true, 64)
	top := *e.tpotModel.info.MeasuredUpTo
	// The stub's mean is 0.005 + rate/1000 at any decode count: at this
	// target decode's TPOT limit is 25 r/s, past the load served at 3
	// decode replicas (25*2/3 > top) and under 4 prefill replicas' limit at
	// a loose TTFT target.
	loose := e.tpotModel.ratio(0.9) * (0.005 + 0.025)
	require.Less(t, top, 25*2/3.0)
	past := func(f Figure, decode int) bool {
		return f.Value != nil && slices.Contains(f.Flags, string(whatif.TPOTDistribution)) && *f.Value*2/float64(decode) > top*(1+1e-9)
	}
	// A loose TTFT target, so that prefill does not bind first.
	slo := &SLORequest{TTFT: &Objective{Statistic: "mean", Target: f64(10)}, TPOT: &Objective{Statistic: "p90", Target: f64(loose)}}
	marked := map[string]bool{}
	check := func(name string, f Figure, decode int) {
		assert.Equal(t, past(f, decode), f.Extrapolated, "%s at %d decode (measured up to %g at 2): %+v", name, decode, top, f)
		marked[name] = f.Extrapolated
	}
	out, _ := planBody(t, s, PlanRequest{InputsID: e.id, Fleet: &FleetKnob{Prefill: intp(4), Decode: intp(2)},
		SLO: slo})
	require.NotNil(t, out.Limits.Decode)
	check("limits.decode", *out.Limits.Decode, 2)
	check("limits.fleet", out.Limits.Fleet.Figure, 2)
	check("marginal.prefillPlusOne", out.Marginal.PrefillPlusOne.Limit, out.Marginal.PrefillPlusOne.Fleet.Decode)
	check("marginal.decodePlusOne", out.Marginal.DecodePlusOne.Limit, out.Marginal.DecodePlusOne.Fleet.Decode)
	assert.True(t, marked["marginal.decodePlusOne"], "the one-more-decode step is past the load served")

	body, err := s.size(context.Background(), SizeRequest{InputsID: e.id, Traffic: &Traffic{Classes: &Classes{Critical: &ClassRate{Rate: f64(20)}}},
		SLO: slo})
	require.NoError(t, err)
	var sz SizeResponse
	require.NoError(t, json.Unmarshal(body, &sz))
	require.NotNil(t, sz.Answer, "%s", body)
	a := sz.Answer
	check("size.limit", a.Limit, a.Fleet.Decode)
	for name, st := range map[string]*SizeStep{"size.fewerPrefill": a.FewerPrefill, "size.fewerDecode": a.FewerDecode} {
		if st != nil {
			check(name, st.Limit, st.Fleet.Decode)
		}
	}

	// A size's step at 4 prefill and 3 decode, as the search prices it
	// (sizer.step): its limit is decode's TPOT limit, past the load served.
	req := SizeRequest{InputsID: e.id, Traffic: &Traffic{Classes: &Classes{Critical: &ClassRate{Rate: f64(20)}}}, SLO: slo}
	sc, err := scenarioOf(&e.doc, req.planAt(FleetCounts{Prefill: 1, Decode: 1}))
	require.NoError(t, err)
	k, err := sc.knobs()
	require.NoError(t, err)
	z := &sizer{e: e, sc: sc, k: k, rate: *sc.Rate, bound: FleetCounts{Prefill: 32, Decode: 32}, models: e.models(), seen: map[FleetCounts]sizeEval{},
		plain: map[FleetCounts]sizeEval{}}
	st, err := z.step(t.Context(), FleetCounts{Prefill: 4, Decode: 3})
	require.NoError(t, err)
	check("size.step", st.Limit, 3)
	assert.True(t, marked["size.step"], "the size's step is past the load served")
}

// With the model on, the self-check prices its TPOT percentiles as the
// answer does: each fold's backtest fits the model on the fold's own window
// and prices the p90 with it, so a p90 the stub refuses (no R_q) is scored.
// Without it the self-check scores no p90 step, as before.
func TestMainViewTPOT_SelfCheckPricesWithTheModel(t *testing.T) {
	e, _ := tdEntries(t, 1920)
	e.engine = EngineSlots{DecodeSlots: 64, SpecTokens: 3}
	e.doc.Source.Cycle = len(e.series)
	cycles := SelfCheckCycles(len(e.series))
	folds := make([]FoldFit, 0, len(cycles))
	for _, c := range cycles {
		folds = append(folds, FoldFit{Cycle: c, At: e.series[c-1].At, Pricer: e.pricer})
	}
	off := selfCheckEntry(t.Context(), e, folds, SelfCheckInfo{PlannerVersion: "vtest"})
	on := selfCheckEntry(t.Context(), e, folds, SelfCheckInfo{PlannerVersion: "vtest", TPOTModel: whatif.TPOTRenewalLognormal})
	require.NotEmpty(t, off.Folds, off.Line)
	require.NotEmpty(t, on.Folds, on.Line)
	require.NotNil(t, off.TPOT)
	require.NotNil(t, on.TPOT)
	assert.Equal(t, off.TPOT.Statistic, on.TPOT.Statistic, "the figure judged, the mean, is the planner's either way")
	assert.Equal(t, off.TPOT.Bias, on.TPOT.Bias)
	assert.Zero(t, off.TPOT.p90Steps, "without the model the stub's p90 is refused")
	assert.Positive(t, on.TPOT.p90Steps, "with it the p90 is the model's, scored")
}

// Unasked (no ExperimentalModels), a day plan is the default's also on inputs resolved with the
// experimental TPOT model, alone or beside the TTFT model: its buckets,
// whose cycles read no lengths, sit at the window's own shape, where the
// models price, yet every figure, refusal and count is the default's, so
// that no plan sizes decode on the model's ratio. The same inputs' size at
// a p90 TPOT target is the model's, so the check is not empty.
func TestDayPlan_ExperimentalTPOTModelLeavesTheDayPlanToTheDefault(t *testing.T) {
	slo := &SLORequest{TTFT: &Objective{Statistic: "p90", Target: f64(3)}, TPOT: &Objective{Statistic: "p90", Target: f64(tpotTarget)}}
	plan := func(s *Server, e *inputsEntry) DayPlanResponse {
		t.Helper()
		for i := range e.series {
			e.series[i].HasShape = false
		}
		body, err := s.dayPlan(context.Background(), DayPlanRequest{InputsID: e.id, SLO: slo})
		require.NoError(t, err)
		var out DayPlanResponse
		require.NoError(t, json.Unmarshal(body, &out))
		return out
	}
	size := func(s *Server, id string) SizeResponse {
		t.Helper()
		body, err := s.size(context.Background(), SizeRequest{InputsID: id, Traffic: &Traffic{Classes: &Classes{Critical: &ClassRate{Rate: f64(3)}}},
			SLO: slo})
		require.NoError(t, err)
		var out SizeResponse
		require.NoError(t, json.Unmarshal(body, &out))
		return out
	}
	// sized is a size's answer, its fleet and limit, or why none: the
	// inputs' ids differ either way.
	sized := func(r SizeResponse) any {
		if r.Answer == nil {
			return r.Unmet
		}
		return []any{r.Answer.Fleet, r.Answer.Limit}
	}
	same := func(name string, base, got DayPlanResponse) {
		t.Helper()
		for _, b := range base.Buckets {
			assert.Equal(t, "window", b.Shape.Source, "%s: the bucket at %s is at the window's shape", name, b.Start)
		}
		assert.Equal(t, base.Refused, got.Refused, name)
		assert.Equal(t, base.Summary, got.Summary, name)
		assert.Equal(t, base.Buckets, got.Buckets, name)
	}

	// The TPOT model alone.
	s0, e0 := tpotEntry(t, false, 64)
	s, e := tpotEntry(t, true, 64)
	require.True(t, e.tpotModel.priced())
	assert.Nil(t, size(s0, e0.id).Answer, "the default sizes no fleet at a p90 TPOT target: no R_q")
	require.NotNil(t, size(s, e.id).Answer, "the model sizes the same inputs")
	base := plan(s0, e0)
	require.NotEmpty(t, base.Buckets)
	same("tpot", base, plan(s, e))

	// Both models, on inputs that hold what each fits on.
	both := func(model bool) (*Server, *inputsEntry) {
		s, e := mainEntry(t, model)
		e.decode = tdDecode(e.series)
		tdRouter(e.latency, e.decode, len(e.series)*3/4)
		e.engine.DecodeSlots, e.engine.SpecTokens = 64, 3
		if model {
			e.tpotModel = fitMainTPOT(e, "", 0)
			require.True(t, e.tpotModel.priced(), e.tpotModel.info.Abstained)
			require.NotNil(t, e.ttftModel)
		}
		return s, e
	}
	sb0, eb0 := both(false)
	sb, eb := both(true)
	require.NotEqual(t, sized(size(sb0, eb0.id)), sized(size(sb, eb.id)), "the models size the same inputs otherwise")
	same("ttft+tpot", plan(sb0, eb0), plan(sb, eb))
}

// A day plan asked for the experimental models prices each bucket at its
// own rate and, where its cycles read lengths of their own (as a
// recording's do) inside what the model's fit served, at its own shape:
// the model draws from the bucket's own histograms, here the window's
// shape throughout, so it prices the buckets it would on a window that
// read no lengths, need for need, and a note says how. Unasked, each
// bucket keeps its own shape, the planner's own. A stated shape stays the
// planner's own, and so does a TPOT target at the mean, which the TPOT
// model does not price; the note says why for each.
func TestDayPlan_ExperimentalModelsSayWhyABucketIsThePlanners(t *testing.T) {
	dayPlan := func(s *Server, e *inputsEntry, req DayPlanRequest) (DayPlanResponse, string) {
		t.Helper()
		req.InputsID = e.id
		body, err := s.dayPlan(context.Background(), req)
		require.NoError(t, err)
		var out DayPlanResponse
		require.NoError(t, json.Unmarshal(body, &out))
		return out, strings.Join(out.Notes, "\n")
	}
	ttft := &SLORequest{TTFT: &Objective{Statistic: "p90", Target: f64(3)}}
	const modelNote = "a bucket is priced at its own rate and its own request shape"

	s, e := mainEntry(t, true)
	require.True(t, slices.ContainsFunc(e.series, func(x SeriesSample) bool { return x.HasShape }), "the cycles read their lengths")
	out, text := dayPlan(s, e, DayPlanRequest{ExperimentalModels: true, SLO: ttft})
	require.Positive(t, out.Summary.Counts.Sized)
	require.NotNil(t, out.TTFTModel)
	require.Empty(t, out.TTFTModel.Abstained)
	assert.Contains(t, text, modelNote)
	priced := 0
	for _, b := range out.Buckets {
		if b.Status == BucketNoData {
			continue
		}
		if b.Needed != nil {
			assert.NotContains(t, b.Flags, FlagWindowShape, "the bucket at %s is priced at its own shape", b.Start)
			assert.Equal(t, "bucket", b.Shape.Source)
			assert.NotNil(t, b.Shape.Model)
		}
		if b.Limit != nil && slices.Contains(b.Limit.Flags, string(whatif.RankMixtureTTFT)) {
			priced++
		}
	}
	require.Positive(t, priced, "the model prices buckets whose cycles read their own shape")
	t.Logf("shapes kept: the TTFT model priced %d of the %d sized buckets", priced, out.Summary.Counts.Sized)
	assert.Contains(t, text, fmt.Sprintf("priced %d of the %d sized buckets", priced, out.Summary.Counts.Sized))

	// Need for need, the plan a window that read no lengths gets.
	s1, e1 := mainEntry(t, true)
	for i := range e1.series {
		e1.series[i].HasShape = false
	}
	noShape, _ := dayPlan(s1, e1, DayPlanRequest{ExperimentalModels: true, SLO: ttft})
	require.Len(t, noShape.Buckets, len(out.Buckets))
	for i, b := range out.Buckets {
		assert.Equal(t, noShape.Buckets[i].Needed, b.Needed, "bucket %s", b.Start)
		assert.Equal(t, noShape.Buckets[i].Limit, b.Limit, "bucket %s", b.Start)
	}
	assert.Equal(t, noShape.Summary, out.Summary)

	// Unasked, each bucket is priced at its own shape, as before.
	plain, plainText := dayPlan(s, e, DayPlanRequest{SLO: ttft})
	assert.NotContains(t, plainText, modelNote)
	for _, b := range plain.Buckets {
		if b.Status != BucketNoData {
			assert.NotContains(t, b.Flags, FlagWindowShape)
			assert.Equal(t, "bucket", b.Shape.Source)
		}
	}

	// A stated shape keeps today's rule: every bucket at it, the planner's own.
	stated, text := dayPlan(s, e, DayPlanRequest{ExperimentalModels: true, SLO: ttft, Traffic: &Traffic{Mix: &Mix{PromptTokens: 4000}}})
	require.Positive(t, stated.Summary.Counts.Sized)
	assert.NotContains(t, text, modelNote)
	for _, b := range stated.Buckets {
		if b.Status != BucketNoData {
			assert.Equal(t, dayShapeStated, b.Shape.Source)
			assert.NotContains(t, b.Flags, FlagWindowShape)
		}
	}
	assert.Contains(t, text, fmt.Sprintf("priced 0 of the %d sized buckets", stated.Summary.Counts.Sized))
	assert.Contains(t, text, fmt.Sprintf("the model prices the source's own request shape, not a stated one (%d)", stated.Summary.Counts.Sized))

	s, e = tpotEntry(t, true, 64)
	out, text = dayPlan(s, e, DayPlanRequest{ExperimentalModels: true,
		SLO: &SLORequest{TTFT: &Objective{Statistic: "p90", Target: f64(3)}, TPOT: &Objective{Statistic: "mean", Target: f64(tpotTarget)}}})
	require.Positive(t, out.Summary.Counts.Sized)
	require.NotNil(t, out.TPOTModel)
	require.Empty(t, out.TPOTModel.Abstained)
	assert.Contains(t, text, fmt.Sprintf("priced 0 of the %d sized buckets", out.Summary.Counts.Sized), "a mean TPOT is the planner's")
	assert.Contains(t, text, fmt.Sprintf("the model prices p90 and p95, not mean (%d)", out.Summary.Counts.Sized))
	// A model that prices no target the plan asks reads no bucket's shape:
	// each bucket keeps its own, the planner's.
	require.True(t, slices.ContainsFunc(e.series, func(x SeriesSample) bool { return x.HasShape }), "the cycles read their lengths")
	assert.NotContains(t, text, modelNote)
	for _, b := range out.Buckets {
		if b.Status != BucketNoData {
			assert.Equal(t, "bucket", b.Shape.Source, "bucket %s", b.Start)
			assert.NotContains(t, b.Flags, FlagWindowShape, "bucket %s", b.Start)
		}
	}
	// At a p90 TPOT target it prices each bucket at its own shape, its ratio
	// the window's.
	out, text = dayPlan(s, e, DayPlanRequest{ExperimentalModels: true,
		SLO: &SLORequest{TTFT: &Objective{Statistic: "p90", Target: f64(3)}, TPOT: &Objective{Statistic: "p90", Target: f64(tpotTarget)}}})
	assert.Contains(t, text, "With the experimental TPOT model, a bucket is priced at its own rate and its own request shape")
	priced = 0
	for _, b := range out.Buckets {
		if b.Needed != nil {
			assert.Equal(t, "bucket", b.Shape.Source, "bucket %s", b.Start)
			assert.NotContains(t, b.Flags, FlagWindowShape, "bucket %s", b.Start)
			if slices.Contains(b.Limit.Flags, string(whatif.TPOTDistribution)) {
				priced++
			}
		}
	}
	assert.Positive(t, priced, "the TPOT model prices buckets at their own shape")
}
