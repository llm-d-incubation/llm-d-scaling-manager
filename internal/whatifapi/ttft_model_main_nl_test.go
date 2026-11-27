package whatifapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// mainNLEntry is mainEntry with the variant whatif.QueueRankMixtureNLLine
// fitted for the main view in place of v2.
func mainNLEntry(t *testing.T) (*Server, *inputsEntry) {
	t.Helper()
	s, e := mainEntry(t, false)
	e.ttftModel = fitMainMixture(e, TTFTModelOptions{Model: string(whatif.QueueRankMixtureNLLine), Stratified: true}.switches())
	info := e.ttftModel.info
	e.doc.TTFTModel = &info
	return s, e
}

// The main view fits the variant exactly as a backtest fits it on the same
// window (fitRankMixtureSteps, then its own identifiability checks), names
// it on the inputs and on the plan's TTFT view, and scores it on its own
// held-out folds (whatif.RankMixtureNLHeldOut), not v2's. v2's name and
// label are as before.
func TestMainView_NLLineIsTheBacktestsFit(t *testing.T) {
	_, v2 := mainEntry(t, true)
	assert.Equal(t, string(whatif.QueueRankMixture), v2.ttftModel.info.Model)
	assert.Equal(t, TTFTModelLabel, v2.ttftModel.info.Label)

	s, e := mainNLEntry(t)
	mm := e.ttftModel
	assert.Equal(t, string(whatif.QueueRankMixtureNLLine), mm.info.Model)
	assert.Equal(t, TTFTModelNLLineLabel, mm.info.Label)
	require.NotNil(t, mm.info.Fit)

	idx := make([]int, len(e.series))
	for i := range idx {
		idx[i] = i
	}
	sw := rankMixtureSwitches{stratified: true, nlLine: true}
	rq, steps, err := fitRankMixtureSteps(e, idx, DefaultBacktestStep, e.engine.PrefillSlots, sw)
	require.NoError(t, err)
	want := rq.identified().report
	assert.Equal(t, want, *mm.info.Fit, "the backtest's fit, checks and all")
	require.NotNil(t, want.ZParts, "the variant's Z, measured part by part")
	assert.Equal(t, mm.info.Abstained, want.Abstained)
	if mm.info.Abstained != "" {
		t.Logf("the variant abstained on the test source: %s", mm.info.Abstained)
		return
	}

	h := whatif.RankMixtureNLHeldOut(steps, sw.options(e.engine.PrefillSlots), BacktestTolerance)
	require.Positive(t, h.Steps)
	require.NotNil(t, mm.info.HeldOut)
	assert.Equal(t, h.Steps, mm.info.HeldOut.Steps)
	assert.Equal(t, h.Within, mm.info.HeldOut.Within)
	assert.InDelta(t, h.MedianRatio, mm.info.HeldOut.MedianRatio, 1e-12)

	got, _ := planBody(t, s, PlanRequest{InputsID: e.id, Fleet: &FleetKnob{Decode: intp(2)}})
	require.NotNil(t, got.TTFT.Model)
	assert.Equal(t, string(whatif.QueueRankMixtureNLLine), got.TTFT.Model.Model)
	assert.Equal(t, TTFTModelNLLineLabel, got.TTFT.Model.Label)
	assert.True(t, got.TTFT.Model.Priced)
}

// POST /v1/inputs takes the variant by name, as a backtest does: the source
// priced with it is its own entry, apart from v2's and the planner's own.
// Where it abstains (the stub gives no per-rank series) it says so on the
// inputs and on the plan, labelled as the variant, and the planner's own
// TTFT stands, byte for byte. A model it does not have is still a 400.
func TestMainView_NLLineByName(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}, WhatifSchema: "schema-v3"}).Handler()
	ids := map[string]string{}
	var nl InputsResponse
	for _, m := range []whatif.QueueModel{whatif.QueueRankMixture, whatif.QueueRankMixtureNLLine} {
		code, body := post(t, h, "/v1/inputs", InputsRequest{Source: &SourceRequest{Kind: SourceRecording, Name: "run.jsonl.gz"},
			TTFTModel: string(m)})
		require.Equal(t, http.StatusOK, code, string(body))
		var in InputsResponse
		require.NoError(t, json.Unmarshal(body, &in))
		require.NotNil(t, in.TTFTModel)
		assert.Equal(t, string(m), in.TTFTModel.Model)
		assert.Contains(t, in.TTFTModel.Abstained, "per-rank series")
		ids[string(m)] = in.InputsID
		nl = in
	}
	assert.Equal(t, TTFTModelNLLineLabel, nl.TTFTModel.Label)
	plain := inputsOf(t, h)
	assert.NotEqual(t, ids[string(whatif.QueueRankMixture)], ids[string(whatif.QueueRankMixtureNLLine)], "each model names its own entry")
	assert.NotEqual(t, plain, nl.InputsID)

	base, _ := plan(t, h, PlanRequest{InputsID: plain})
	got, _ := plan(t, h, PlanRequest{InputsID: nl.InputsID})
	a, err := json.Marshal(base.TTFT.Line)
	require.NoError(t, err)
	b, err := json.Marshal(got.TTFT.Line)
	require.NoError(t, err)
	assert.JSONEq(t, string(a), string(b))
	require.NotNil(t, got.TTFT.Model)
	assert.Equal(t, string(whatif.QueueRankMixtureNLLine), got.TTFT.Model.Model)
	assert.Equal(t, TTFTModelNLLineLabel, got.TTFT.Model.Label)
	assert.False(t, got.TTFT.Model.Priced)
	assert.Contains(t, got.TTFT.Model.NotPriced, "abstained")

	code, body := post(t, h, "/v1/inputs", InputsRequest{TTFTModel: "rank-mixture-nl"})
	assert.Equal(t, http.StatusBadRequest, code, string(body))
}

// A server whose default is the variant (serve --experimental-ttft=v2nl-line)
// fits it for a source whose request names no model.
func TestMainView_NLLineServerDefault(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}, WhatifSchema: "schema-v3", TTFTModel: string(whatif.QueueRankMixtureNLLine)}).Handler()
	code, body := post(t, h, "/v1/inputs", InputsRequest{Source: &SourceRequest{Kind: SourceRecording, Name: "run.jsonl.gz"}})
	require.Equal(t, http.StatusOK, code, string(body))
	var in InputsResponse
	require.NoError(t, json.Unmarshal(body, &in))
	require.NotNil(t, in.TTFTModel)
	assert.Equal(t, string(whatif.QueueRankMixtureNLLine), in.TTFTModel.Model)
}

// Where the variant prices, every figure it sets names it: the plan's TTFT
// view carries its model and label, the size's prefill limit is bound by
// "TTFT, rank-mixture-nl-line", and its curve is drawn with its own fit,
// not v2's. The stub's window identifies v2 but not the variant (its own
// p90 is not priced), so the variant's fit is placed on v2's priced entry
// with its abstention cleared: what is tested is the naming, not the fit.
func TestMainView_NLLineNamesWhatItPrices(t *testing.T) {
	_, nl := mainNLEntry(t)
	s, e := mainEntry(t, true)
	v2 := *e.ttftModel
	v2Line, _ := planBody(t, s, PlanRequest{InputsID: e.id, Fleet: &FleetKnob{Decode: intp(2)}})

	s, e = mainEntry(t, true)
	mm := e.ttftModel
	rq := *nl.ttftModel.rq
	rq.report.Abstained = ""
	mm.rq = &rq
	mm.info.Model, mm.info.Label = nl.ttftModel.info.Model, nl.ttftModel.info.Label
	require.True(t, mm.priced())

	got, _ := planBody(t, s, PlanRequest{InputsID: e.id, Fleet: &FleetKnob{Decode: intp(2)}})
	require.NotNil(t, got.TTFT.Model)
	assert.True(t, got.TTFT.Model.Priced)
	assert.Equal(t, string(whatif.QueueRankMixtureNLLine), got.TTFT.Model.Model)
	assert.Equal(t, TTFTModelNLLineLabel, got.TTFT.Model.Label)
	assert.NotEqual(t, v2Line.TTFT.Line, got.TTFT.Line, "the variant's fit prices the curve, not v2's")
	assert.Equal(t, TTFTModelLabel, v2.info.Label)

	fl, ok := mm.fleet(stubFleetFor(t, e, 1), Knobs{Prefill: 1, Decode: 2, TTFTPercentile: 0.9}).(*mixtureFleet)
	require.True(t, ok)
	lo, _ := fl.price(1)
	hi, okHi := fl.price(fl.knee * 0.8)
	require.True(t, okHi)
	a, ok := fl.prefillLimit((lo + hi) / 2).(whatif.Answer)
	require.True(t, ok, "a target between the floor and the curve's top is met")
	assert.Equal(t, []whatif.Bound{whatif.Bound("TTFT, " + string(whatif.QueueRankMixtureNLLine))}, a.Basis.BoundBy)
}
