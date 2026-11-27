package whatifapi

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// rankMixtureStratified is on by default: a request that does not name it
// fits with the stratified service moments, and the report says so; false
// turns them off. The request names it only when set.
func TestBacktest_RankMixtureStratified(t *testing.T) {
	require.True(t, whatif.DefaultRankMixtureStratified, "the stratified service moments are the default")
	r := &stubResolver{}
	fit := rmFit(t, r, 3120)
	ts, tl := btSeries(240, 0, 1)
	test := btEntry(t, r, "test", ts, tl, btConfig("H100"))
	test.mixture = btMixture(ts)
	req := BacktestRequest{TTFTModel: string(whatif.QueueRankMixture), PrefillSlots: 4}
	q, err := backtestQuestionOf(fit, test, req, time.Time{}, "")
	require.NoError(t, err)
	assert.True(t, q.mixture.m.Stratified, "absent, the default")
	assert.True(t, q.mixture.report.Stratified)
	for _, on := range []bool{false, true} {
		req.RankMixtureStratified = &on
		q, err = backtestQuestionOf(fit, test, req, time.Time{}, "")
		require.NoError(t, err)
		assert.Equal(t, on, q.mixture.m.Stratified)
		assert.Equal(t, on, q.mixture.report.Stratified)
	}
	b, err := json.Marshal(BacktestRequest{TTFTModel: string(whatif.QueueRankMixture)})
	require.NoError(t, err)
	assert.NotContains(t, string(b), "rankMixtureStratified")
	off := false
	b, err = json.Marshal(BacktestRequest{TTFTModel: string(whatif.QueueRankMixture), RankMixtureStratified: &off})
	require.NoError(t, err)
	assert.Contains(t, string(b), `"rankMixtureStratified":false`)
}

// The two fits of one backtest, stratified and not, answer under different
// ids.
func TestBacktest_RankMixtureStratifiedNamesItsID(t *testing.T) {
	r := &stubResolver{}
	fit := rmFit(t, r, 3120)
	ts, tl := btSeries(240, 0, 1)
	test := btEntry(t, r, "test", ts, tl, btConfig("H100"))
	test.mixture = btMixture(ts)
	off := false
	on := btRun(t, fit, test, BacktestRequest{TTFTModel: string(whatif.QueueRankMixture), PrefillSlots: 4})
	draws := btRun(t, fit, test, BacktestRequest{TTFTModel: string(whatif.QueueRankMixture), PrefillSlots: 4, RankMixtureStratified: &off})
	require.NotNil(t, on.RankMixture)
	require.NotNil(t, draws.RankMixture)
	assert.True(t, on.RankMixture.Stratified)
	assert.False(t, draws.RankMixture.Stratified)
	assert.NotEqual(t, on.BacktestID, draws.BacktestID)
}

// The server's default is on; Config.NoRankMixtureStratified (serve
// --rank-mixture-stratified=false) turns it off, and a request's own
// rankMixtureStratified overrides either.
func TestServer_RankMixtureStratifiedDefault(t *testing.T) {
	assert.True(t, New(Config{Resolver: &stubResolver{}}).stratified())
	assert.False(t, New(Config{Resolver: &stubResolver{}, NoRankMixtureStratified: true}).stratified())
	off, on := false, true
	assert.False(t, stratifiedOf(&off, true))
	assert.True(t, stratifiedOf(&on, false))
	assert.True(t, stratifiedOf(nil, true))
	assert.False(t, stratifiedOf(nil, false))
}

// The main view's model read from each rank's own draws is another entry:
// rankMixtureStratified false changes the inputsId, and absent or true
// leave it as the model alone names it.
func TestInputs_RankMixtureStratifiedNamesItsEntry(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}, WhatifSchema: "schema-v3"}).Handler()
	id := func(stratified *bool) string {
		code, body := post(t, h, "/v1/inputs", InputsRequest{Source: &SourceRequest{Kind: SourceRecording, Name: "run.jsonl.gz"},
			TTFTModel: string(whatif.QueueRankMixture), RankMixtureStratified: stratified})
		require.Equal(t, http.StatusOK, code, string(body))
		var in InputsResponse
		require.NoError(t, json.Unmarshal(body, &in))
		return in.InputsID
	}
	off, on := false, true
	def := id(nil)
	assert.Equal(t, def, id(&on))
	assert.NotEqual(t, def, id(&off))
}
