package main

import (
	"encoding/json"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// serve --experimental-ttft=v2nl-line prices with the variant wherever =v2
// prices with v2: every backtest that names no model answers what the CLI's
// backtest with the same flag answers, and every source's main view is
// fitted with the variant, named and labelled as such. at-replicas with the
// flag fits the same model on the same recording as the server. On the q32
// staircase, a fit window under 12 h, the variant abstains and says why, as
// v2 does: the plan's TTFT line is the planner's own.
func TestServeV2NLLine_BacktestAndMainView(t *testing.T) {
	var cli whatifapi.BacktestResponse
	out, _ := runCmd(t, append(slices.Clone(backtestQ32), "--"+experimentalTTFTFlag+"="+experimentalTTFTV2NLLine, "-o", "json")...)
	require.NoError(t, jsonStrict([]byte(out), &cli), out)
	cli.BacktestID, cli.Fit.InputsID, cli.Test.InputsID = "", "", ""
	require.NotNil(t, cli.RankMixture)
	assert.Equal(t, string(whatif.QueueRankMixtureNLLine), cli.RankMixture.Model)

	var on serveFlags
	require.NoError(t, experimentalTTFTValue{switches: &on.experimentalTTFT, v2: &on.rankMixtureTTFT, nlLine: &on.rankMixtureNLLine}.
		Set(experimentalTTFTV2NLLine))
	served := servedV2Backtest(t, on, "", "")
	require.NotNil(t, served.RankMixture)
	assert.Equal(t, cli, served, "the served default is the CLI's backtest with the same flag")

	f := serveFlags{recordings: q32Dir, listen: "127.0.0.1:0", auth: whatifapi.AuthNone, noSelfCheck: true,
		rankMixtureTTFT: on.rankMixtureTTFT, rankMixtureNLLine: on.rankMixtureNLLine}
	h, err := f.handler(newFlags("serve", io.Discard), io.Discard)
	require.NoError(t, err)
	code, body := loopbackPost(t, h, "/v1/inputs", whatifapi.InputsRequest{Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording,
		Name: filepath.Base(q32Recording)}})
	require.Equal(t, 200, code, "%s", body)
	var in whatifapi.InputsResponse
	require.NoError(t, json.Unmarshal(body, &in))
	require.NotNil(t, in.TTFTModel)
	assert.Equal(t, string(whatif.QueueRankMixtureNLLine), in.TTFTModel.Model)
	assert.Equal(t, whatifapi.TTFTModelNLLineLabel, in.TTFTModel.Label)
	assert.NotContains(t, in.TTFTModel.Abstained, "did not give", "the server reads the series the variant needs")
	assert.Contains(t, in.TTFTModel.Abstained, "too short for its own checks", "the 12 h fit-window rule holds")
	assert.Contains(t, in.TTFTModel.Abstained, "the fit window's own mean not priced", "the variant's own check, on the whole window (the backtest fits on its first half)")

	code, body = loopbackPost(t, h, "/v1/plan", whatifapi.PlanRequest{InputsID: in.InputsID})
	require.Equal(t, 200, code, "%s", body)
	var p whatifapi.PlanResponse
	require.NoError(t, json.Unmarshal(body, &p))
	require.NotNil(t, p.TTFT.Model)
	assert.Equal(t, whatifapi.TTFTModelNLLineLabel, p.TTFT.Model.Label)
	assert.False(t, p.TTFT.Model.Priced)
	assert.Contains(t, p.TTFT.Model.NotPriced, "abstained")

	args := []string{"at-replicas", "--recording", q32Recording, "--ttft", "2s@p90", "-o", "json", "--" + experimentalTTFTFlag + "=" + experimentalTTFTV2NLLine}
	rep, _ := runCmd(t, args...)
	var r whatifapi.ReportResponse
	require.NoError(t, json.Unmarshal([]byte(rep), &r), rep)
	require.NotNil(t, r.TTFTModel)
	assert.Equal(t, *in.TTFTModel, *r.TTFTModel, "at-replicas fits the model the server fits")
}

// The answer commands other than at-replicas refuse =v2nl-line, as they
// refuse =v2.
func TestV2NLLine_RefusedOffAtReplicas(t *testing.T) {
	out, code := runCmd(t, "slo-to-rate", "--recording", q32Recording, "--"+experimentalTTFTFlag+"="+experimentalTTFTV2NLLine)
	assert.Equal(t, 2, code, out)
}

// serve, like backtest, warns where the KV hold is turned on without the
// remainder's spread, and is silent with both.
func TestServeV2NLLine_WarnsOnHoldAlone(t *testing.T) {
	for name, c := range map[string]struct {
		spread, warns bool
	}{"hold alone": {false, true}, "both": {true, false}} {
		t.Run(name, func(t *testing.T) {
			f := serveFlags{recordings: q32Dir, listen: "127.0.0.1:0", auth: whatifapi.AuthNone, noSelfCheck: true,
				rankMixtureTTFT: true, rankMixtureNLLine: true,
				rankMixtureFit: rankMixtureFit{kvHold: true, remainderSpread: c.spread, minFitWindow: whatif.MinSpreadFitWindow}}
			var warn strings.Builder
			_, err := f.handler(newFlags("serve", io.Discard), &warn)
			require.NoError(t, err)
			if c.warns {
				assert.Contains(t, warn.String(), "--rank-mixture-kv-hold without --rank-mixture-remainder-spread")
			} else {
				assert.NotContains(t, warn.String(), "--rank-mixture-kv-hold")
			}
		})
	}
}
