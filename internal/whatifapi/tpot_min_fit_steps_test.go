package whatifapi

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// On 10 fit steps the TPOT distribution abstains at the default 24; with
// tpotDistMinFitSteps 8 it fits, reports the short fit, says so on its
// assumed line, and flags every percentile it prices
// TPOTDistributionShortFit, in the backtest and on the main view. On 64
// steps the same 8 flags nothing.
func TestTPOTDist_MinFitSteps(t *testing.T) {
	fit, test := tdEntries(t, 200)
	fit.engine = EngineSlots{DecodeSlots: 64, SpecTokens: 3}
	req := BacktestRequest{TPOTModel: whatif.TPOTRenewalLognormal, DecodeSlots: 64, SpecTokens: 3}
	def := btRun(t, fit, test, req)
	require.NotNil(t, def.TPOTDistribution)
	assert.Contains(t, def.TPOTDistribution.Abstained, "under 24", "the default holds")

	req.TPOTDistMinFitSteps = 8
	out := btRun(t, fit, test, req)
	d := out.TPOTDistribution
	require.NotNil(t, d)
	require.Empty(t, d.Abstained)
	assert.Less(t, d.FitSteps, MinTPOTDistFitSteps)
	assert.True(t, d.ShortFit)
	assert.Equal(t, 8, d.MinFitSteps)
	assert.Contains(t, strings.Join(out.Assumed, "; "), string(whatif.TPOTDistributionShortFit))
	priced := 0
	for _, st := range out.Steps {
		for _, p := range st.TPOT {
			if slices.Contains(p.Predicted.Flags, string(whatif.TPOTDistribution)) {
				priced++
				assert.Contains(t, p.Predicted.Flags, string(whatif.TPOTDistributionShortFit))
			}
		}
	}
	assert.Positive(t, priced)

	mt := fitMainTPOT(fit, "", 8)
	require.Empty(t, mt.info.Abstained)
	require.NotNil(t, mt.info.Fit)
	assert.True(t, mt.info.Fit.ShortFit)
	assert.Equal(t, []whatif.Flag{whatif.TPOTDistribution, whatif.TPOTDistributionShortFit}, mt.tq.report.Flags())
	assert.Contains(t, fitMainTPOT(fit, "", 0).info.Abstained, "under 24")

	long, _ := tdEntries(t, 960)
	long.engine = fit.engine
	assert.False(t, fitMainTPOT(long, "", 8).info.Fit.ShortFit, "64 steps are not short")
}

// A count under the floor, or one without a TPOT model, is the caller's
// error; 24 is the default entry; the server's default reaches a source.
func TestTPOTDist_MinFitStepsRequest(t *testing.T) {
	assert.Error(t, checkTPOTMinFitSteps(3, whatif.TPOTRenewalLognormal))
	assert.Error(t, checkTPOTMinFitSteps(8, ""))
	assert.NoError(t, checkTPOTMinFitSteps(0, ""))
	assert.NoError(t, checkTPOTMinFitSteps(4, whatif.TPOTRenewalLognormal))
	s := New(Config{Resolver: &stubResolver{}, WhatifSchema: "schema-v3", TPOTDistMinFitSteps: 8})
	n, err := s.tpotMinFitStepsOf(InputsRequest{TPOTModel: whatif.TPOTRenewalLognormal})
	require.NoError(t, err)
	assert.Equal(t, 8, n, "the server's default")
	n, err = s.tpotMinFitStepsOf(InputsRequest{TPOTModel: whatif.TPOTRenewalLognormal, TPOTDistMinFitSteps: 24})
	require.NoError(t, err)
	assert.Zero(t, n, "24 is the default")
	n, err = s.tpotMinFitStepsOf(InputsRequest{})
	require.NoError(t, err)
	assert.Zero(t, n, "no model, no count")
	h := s.Handler()
	code, body := post(t, h, "/v1/inputs", InputsRequest{Source: &SourceRequest{Kind: SourceRecording, Name: "run.jsonl.gz"},
		TPOTModel: whatif.TPOTRenewalLognormal, TPOTDistMinFitSteps: 2})
	assert.Equal(t, http.StatusBadRequest, code, string(body))
}
