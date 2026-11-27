package whatifapi

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// On a 2 h fit window both models abstain at the default 12 h minimum, for
// the window; with rankMixtureMinFitWindow 1h neither abstains for it, the
// report says the window was short and what was asked, and every TTFT the
// model prices carries RankMixtureShortFitWindow. On a 13 h window the
// same 1h minimum flags nothing: the window is not short.
func TestBacktest_MinFitWindow(t *testing.T) {
	r := &stubResolver{}
	short := rmFit(t, r, 480)
	ts, tl := btSeries(240, 0, 1)
	test := btEntry(t, r, "test", ts, tl, btConfig("H100"))
	test.mixture = btMixture(ts)
	for _, m := range []whatif.QueueModel{whatif.QueueRankMixture, whatif.QueueRankMixtureNLLine} {
		t.Run(string(m), func(t *testing.T) {
			def := btRun(t, short, test, BacktestRequest{TTFTModel: string(m), PrefillSlots: 4})
			require.NotNil(t, def.RankMixture)
			assert.Contains(t, def.RankMixture.Abstained, "under 12h0m0s", "the default holds")
			assert.False(t, def.RankMixture.ShortFitWindow)
			assert.Empty(t, def.RankMixture.MinFitWindow)

			out := btRun(t, short, test, BacktestRequest{TTFTModel: string(m), PrefillSlots: 4, RankMixtureMinFitWindow: "1h"})
			rm := out.RankMixture
			require.NotNil(t, rm)
			assert.NotContains(t, rm.Abstained, "under 1h0m0s")
			assert.NotContains(t, rm.Abstained, "under 12h0m0s")
			assert.True(t, rm.ShortFitWindow)
			assert.Equal(t, "1h0m0s", rm.MinFitWindow)
			assert.False(t, rm.SpreadDefaulted, "the window is at least the minimum asked")
			priced := 0
			for _, st := range out.Steps {
				for _, p := range st.TTFT {
					if slices.Contains(p.Predicted.Flags, string(whatif.RankMixtureTTFT)) {
						priced++
						assert.Contains(t, p.Predicted.Flags, string(whatif.RankMixtureShortFitWindow))
					}
				}
			}
			t.Logf("%s on a 2 h window at a 1h minimum: abstained %q, %d TTFT figures priced", m, rm.Abstained, priced)
			if rm.Abstained == "" {
				assert.Positive(t, priced)
				assert.Contains(t, strings.Join(out.Assumed, "; "), string(whatif.RankMixtureShortFitWindow))
			}
		})
	}
	long := rmFit(t, r, 3120)
	out := btRun(t, long, test, BacktestRequest{TTFTModel: string(whatif.QueueRankMixture), PrefillSlots: 4, RankMixtureMinFitWindow: "1h"})
	require.NotNil(t, out.RankMixture)
	assert.False(t, out.RankMixture.ShortFitWindow, "a 13 h window is not short")
	for _, st := range out.Steps {
		for _, p := range st.TTFT {
			assert.NotContains(t, p.Predicted.Flags, string(whatif.RankMixtureShortFitWindow))
		}
	}
}

// The main view fits with the shortest fit window asked, as the backtest
// does: on the 2 h window the default abstains for it, and 1h does not.
func TestMainView_MinFitWindow(t *testing.T) {
	e := rmFit(t, &stubResolver{}, 480)
	e.engine.PrefillSlots = 4
	for _, m := range []whatif.QueueModel{whatif.QueueRankMixture, whatif.QueueRankMixtureNLLine} {
		def := fitMainMixture(e, TTFTModelOptions{Model: string(m), Stratified: true}.switches())
		assert.Contains(t, def.info.Abstained, "under 12h0m0s", m)
		mm := fitMainMixture(e, TTFTModelOptions{Model: string(m), Stratified: true, MinFitWindow: time.Hour}.switches())
		require.NotNil(t, mm.info.Fit, m)
		assert.NotContains(t, mm.info.Abstained, "under", m)
		assert.True(t, mm.info.Fit.ShortFitWindow, m)
		if mm.priced() {
			assert.Equal(t, []whatif.Flag{whatif.RankMixtureTTFT, whatif.RankMixtureShortFitWindow}, mm.modelFlags(), m)
		}
	}
}

// The server's defaults for the window and the variant's modes reach a
// source that does not set them, and change its inputsId; a request's own
// window wins. A window that is no duration, or the modes asked with v2,
// is the caller's error. 12h is the default, the same entry.
func TestServer_RankMixtureDefaults(t *testing.T) {
	src := &SourceRequest{Kind: SourceRecording, Name: "run.jsonl.gz"}
	inputs := func(c Config, req InputsRequest) (int, InputsResponse, string) {
		c.Resolver, c.WhatifSchema = &stubResolver{}, "schema-v3"
		req.Source = src
		code, body := post(t, New(c).Handler(), "/v1/inputs", req)
		var in InputsResponse
		if code == http.StatusOK {
			require.NoError(t, json.Unmarshal(body, &in))
		}
		return code, in, string(body)
	}
	nl := string(whatif.QueueRankMixtureNLLine)
	_, plain, _ := inputs(Config{TTFTModel: nl}, InputsRequest{})
	_, served, _ := inputs(Config{TTFTModel: nl, RankMixtureMinFitWindow: 2 * time.Hour, RankMixtureKVHold: true, RankMixtureRemainderSpread: true},
		InputsRequest{})
	_, asked, _ := inputs(Config{TTFTModel: nl}, InputsRequest{RankMixtureMinFitWindow: "2h", RankMixtureKVHold: true, RankMixtureRemainderSpread: true})
	_, twelve, _ := inputs(Config{TTFTModel: nl}, InputsRequest{RankMixtureMinFitWindow: "12h"})
	assert.NotEqual(t, plain.InputsID, served.InputsID)
	assert.Equal(t, asked.InputsID, served.InputsID, "the server's defaults are the same as asking")
	assert.Equal(t, plain.InputsID, twelve.InputsID, "12h is the default")

	code, _, body := inputs(Config{}, InputsRequest{TTFTModel: nl, RankMixtureMinFitWindow: "two hours"})
	assert.Equal(t, http.StatusBadRequest, code, body)
	code, _, body = inputs(Config{}, InputsRequest{RankMixtureMinFitWindow: "2h"})
	assert.Equal(t, http.StatusBadRequest, code, body)
	code, _, body = inputs(Config{}, InputsRequest{TTFTModel: string(whatif.QueueRankMixture), RankMixtureKVHold: true})
	assert.Equal(t, http.StatusBadRequest, code, body)
	code, _, body = inputs(Config{TTFTModel: string(whatif.QueueRankMixture), RankMixtureKVHold: true}, InputsRequest{})
	assert.Equal(t, http.StatusOK, code, "a server's modes are the variant's: a v2 source takes none: %s", body)
}

// The main view defaults the variant's modes before it checks them, as a
// backtest does: the remainder server asked for on a server whose remainder
// spread is on by default is the server's spread's, not refused for want of
// the request's own; without a spread on either side it is still refused.
func TestMainTTFTOptions_RemainderServerTakesTheServersSpread(t *testing.T) {
	nl := string(whatif.QueueRankMixtureNLLine)
	o, err := New(Config{RankMixtureRemainderSpread: true}).mainTTFTOptions(InputsRequest{TTFTModel: nl,
		RankMixtureRemainderServer: true})
	require.NoError(t, err)
	assert.True(t, o.RemainderSpread)
	assert.True(t, o.RemainderServer)

	_, err = New(Config{}).mainTTFTOptions(InputsRequest{TTFTModel: nl, RankMixtureRemainderServer: true})
	assert.Error(t, err, "no spread asked and none served")
}
