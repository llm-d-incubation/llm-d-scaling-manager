package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// loopbackPost is apiPost to a handler serve built, which answers a
// loopback Host only.
func loopbackPost(t *testing.T, h http.Handler, path string, body any) (int, []byte) {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080"+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// servedV2Backtest is the q32 split backtest asked of a server serve built
// from f, with req's model keys: the answer, ids cleared.
func servedV2Backtest(t *testing.T, f serveFlags, ttftModel, tpotModel string) whatifapi.BacktestResponse {
	t.Helper()
	f.recordings, f.listen, f.auth = q32Dir, "127.0.0.1:0", whatifapi.AuthNone
	f.noSelfCheck = true
	h, err := f.handler(newFlags("serve", io.Discard), io.Discard)
	require.NoError(t, err)
	code, body := loopbackPost(t, h, "/v1/inputs", whatifapi.InputsRequest{Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording,
		Name: filepath.Base(q32Recording)}})
	require.Equal(t, 200, code, "%s", body)
	var in whatifapi.InputsResponse
	require.NoError(t, json.Unmarshal(body, &in))
	cv2 := 1.0
	code, body = loopbackPost(t, h, "/v1/backtest", whatifapi.BacktestRequest{InputsID: in.InputsID, Split: "50%", ArrivalCV2: &cv2,
		TTFTModel: ttftModel, TPOTModel: tpotModel})
	require.Equal(t, 200, code, "%s", body)
	var bt whatifapi.BacktestResponse
	require.NoError(t, jsonStrict(body, &bt))
	bt.BacktestID, bt.Fit.InputsID, bt.Test.InputsID = "", "", ""
	return bt
}

// serve --experimental-ttft=v2 --experimental-tpot=v2 prices every backtest
// with both experimental models, so a page user need not ask: a request that
// names no model answers what the CLI's backtest with both flags answers,
// and what a request naming both answers.
func TestServeV2Models_EveryBacktestRunsThem(t *testing.T) {
	var cli whatifapi.BacktestResponse
	out, _ := runCmd(t, append(slices.Clone(backtestQ32), "--"+experimentalTTFTFlag+"="+experimentalTTFTV2,
		"--"+experimentalTPOTFlag+"="+experimentalTPOTV2, "-o", "json")...)
	require.NoError(t, jsonStrict([]byte(out), &cli), out)
	cli.BacktestID, cli.Fit.InputsID, cli.Test.InputsID = "", "", ""
	require.NotNil(t, cli.RankMixture)
	require.NotNil(t, cli.TPOTDistribution)

	on := serveFlags{experimentalTPOT: experimentalTPOTV2}
	require.NoError(t, experimentalTTFTValue{switches: &on.experimentalTTFT, v2: &on.rankMixtureTTFT}.Set(experimentalTTFTV2))
	served := servedV2Backtest(t, on, "", "")
	require.NotNil(t, served.RankMixture, "the served default asks for the TTFT model")
	require.NotNil(t, served.TPOTDistribution, "the served default asks for the TPOT distribution")
	assert.NotContains(t, served.RankMixture.Abstained, "did not give", "the server reads the series the TTFT model needs")
	assert.NotContains(t, served.TPOTDistribution.Abstained, "did not give", "the server reads the series the TPOT model needs")
	assert.Equal(t, cli, served, "the served default is the CLI's backtest with both flags")

	asked := servedV2Backtest(t, on, string(whatif.QueueRankMixture), whatif.TPOTRenewalLognormal)
	assert.Equal(t, served, asked, "the served default is the same as asking")
}

// Without the flags nothing changes: the served backtest is the CLI's
// golden (TestBacktestAPIParity), no source reads the models' series, and
// a request that does not ask carries neither model.
func TestServeV2Models_OffByDefault(t *testing.T) {
	off := servedV2Backtest(t, serveFlags{}, "", "")
	assert.Nil(t, off.RankMixture)
	assert.Nil(t, off.TPOTDistribution)
	res, err := (&servedSources{warn: io.Discard, recordings: q32Dir}).Resolve(t.Context(), whatifapi.InputsRequest{
		Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: filepath.Base(q32Recording)}})
	require.NoError(t, err)
	assert.Nil(t, res.Mixture)
	assert.Nil(t, res.Decode)
	assert.Zero(t, res.Engine)
}

// serve takes the flags as the CLI's backtest spells them, says so in its
// help, and refuses a TPOT model it does not have before it serves.
func TestServeV2Models_Flags(t *testing.T) {
	out, _ := runCmd(t, "serve", "-h")
	assert.Contains(t, out, "-"+experimentalTTFTFlag)
	assert.Contains(t, out, "-"+experimentalTPOTFlag)
	assert.Contains(t, out, "=v2 instead prices every backtest with the rank-mixture TTFT model")

	f := serveFlags{recordings: t.TempDir(), listen: "127.0.0.1:0", auth: whatifapi.AuthNone, experimentalTPOT: "v3"}
	_, err := f.handler(newFlags("serve", io.Discard), io.Discard)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "--experimental-tpot=v2"), err.Error())
}
