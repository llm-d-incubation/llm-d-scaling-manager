package main

import (
	"encoding/json"
	"flag"
	"io"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// The main view's model reads the switch too: the inputs a server answers
// with ttftModel rank-mixture, and at-replicas --experimental-ttft=v2, fit
// it stratified by default, and from each rank's own draws where the
// request, serve or at-replicas says false. On the q32 staircase the model
// abstains on the fit window's length, after the fit it reports.
func TestRankMixtureStratified_MainView(t *testing.T) {
	src := &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: filepath.Base(q32Recording)}
	inputs := func(served, asked *bool) bool {
		f := serveFlags{recordings: q32Dir, listen: "127.0.0.1:0", auth: whatifapi.AuthNone, noSelfCheck: true, rankMixtureStratified: served}
		h, err := f.handler(newFlags("serve", io.Discard), io.Discard)
		require.NoError(t, err)
		code, body := loopbackPost(t, h, "/v1/inputs", whatifapi.InputsRequest{Source: src, TTFTModel: string(whatif.QueueRankMixture),
			RankMixtureStratified: asked})
		require.Equal(t, 200, code, "%s", body)
		var in whatifapi.InputsResponse
		require.NoError(t, json.Unmarshal(body, &in))
		require.NotNil(t, in.TTFTModel)
		require.NotNil(t, in.TTFTModel.Fit, in.TTFTModel.Abstained)
		return in.TTFTModel.Fit.Stratified
	}
	off, on := false, true
	assert.True(t, inputs(nil, nil), "inputs: on by default")
	assert.False(t, inputs(nil, &off), "inputs: the request's false turns it off")
	assert.False(t, inputs(&off, nil), "inputs: serve's =false is the default")
	assert.True(t, inputs(&off, &on), "inputs: the request overrides serve")

	atReplicas := func(args ...string) bool {
		out, _ := runCmd(t, append([]string{"at-replicas", "--recording", q32Recording, "--ttft", "2s@p90", "-o", "json",
			"--" + experimentalTTFTFlag + "=" + experimentalTTFTV2}, args...)...)
		var rep whatifapi.ReportResponse
		require.NoError(t, json.Unmarshal([]byte(out), &rep), out)
		require.NotNil(t, rep.TTFTModel)
		require.NotNil(t, rep.TTFTModel.Fit, rep.TTFTModel.Abstained)
		return rep.TTFTModel.Fit.Stratified
	}
	assert.True(t, atReplicas(), "at-replicas: on by default")
	assert.False(t, atReplicas("--"+rankMixtureStratifiedFlag+"=false"), "at-replicas: =false turns it off")
}

// --experimental-ttft=v2 prices with the stratified service moments by
// default, on backtest and on serve; --rank-mixture-stratified=false reads
// each rank's moments from its own draws.
func TestRankMixtureStratified_OnByDefault(t *testing.T) {
	v2 := append(slices.Clone(backtestQ32), "--"+experimentalTTFTFlag+"="+experimentalTTFTV2, "-o", "json")
	cli := func(args ...string) *whatifapi.RankMixtureReport {
		out, _ := runCmd(t, append(slices.Clone(v2), args...)...)
		var bt whatifapi.BacktestResponse
		require.NoError(t, jsonStrict([]byte(out), &bt), out)
		require.NotNil(t, bt.RankMixture)
		return bt.RankMixture
	}
	assert.True(t, cli().Stratified, "backtest: on by default")
	assert.True(t, cli("--"+rankMixtureStratifiedFlag).Stratified)
	assert.False(t, cli("--"+rankMixtureStratifiedFlag+"=false").Stratified, "backtest: =false turns it off")

	served := func(f serveFlags) *whatifapi.RankMixtureReport {
		require.NoError(t, experimentalTTFTValue{switches: &f.experimentalTTFT, v2: &f.rankMixtureTTFT}.Set(experimentalTTFTV2))
		bt := servedV2Backtest(t, f, "", "")
		require.NotNil(t, bt.RankMixture)
		return bt.RankMixture
	}
	off := false
	assert.True(t, served(serveFlags{}).Stratified, "serve: on by default")
	assert.False(t, served(serveFlags{rankMixtureStratified: &off}).Stratified, "serve: =false turns it off")
}

// The switch parses the same on every command that takes it: absent or
// alone it is on, =false off.
func TestRankMixtureStratified_Flag(t *testing.T) {
	for _, c := range []struct {
		args []string
		off  bool
	}{{nil, false}, {[]string{"--" + rankMixtureStratifiedFlag}, false}, {[]string{"--" + rankMixtureStratifiedFlag + "=true"}, false},
		{[]string{"--" + rankMixtureStratifiedFlag + "=false"}, true}} {
		var bf backtestFlags
		fs := flag.NewFlagSet("backtest", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		bf.registerAssume(fs)
		require.NoError(t, fs.Parse(c.args))
		assert.Equal(t, !c.off, stratifiedOr(bf.rankMixtureStratified), "backtest %v", c.args)

		var af answerFlags
		fs = flag.NewFlagSet("at-replicas", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		af.register(fs)
		require.NoError(t, fs.Parse(c.args))
		assert.Equal(t, !c.off, stratifiedOr(af.rankMixtureStratified), "at-replicas %v", c.args)
	}
	var given *bool
	assert.Error(t, optionalBool{&given, true}.Set("maybe"))
	assert.Nil(t, given)
}
