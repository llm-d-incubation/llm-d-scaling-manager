package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// backtestQ32 is the backtest of the q32 staircase's warm-up recording
// split at its middle: fitted on its first half, tested on the rest.
var backtestQ32 = []string{"backtest", "--recording", q32Recording, "--split", "50%", "--arrival-cv2", "1"}

// The backtest's output is pinned, as text and as JSON.
func TestBacktestGolden(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		code int
	}{
		{"backtest-q32-split", backtestQ32, 0},
		{"backtest-q32-split-json", append(slices.Clone(backtestQ32), "-o", "json"), 0},
	} {
		t.Run(c.name, func(t *testing.T) { checkGolden(t, c.name, c.code, c.args) })
	}
}

// A recording split in two is scored: the fit is read at the split, the
// test is every cycle after it. It reads the JSON golden TestBacktestGolden
// holds the command to.
func TestBacktestSplitsOneRecording(t *testing.T) {
	out, err := os.ReadFile(filepath.Join("testdata", "backtest-q32-split-json.golden"))
	require.NoError(t, err)
	var bt whatifapi.BacktestResponse
	require.NoError(t, jsonStrict(out, &bt), string(out))
	assert.Equal(t, "50%", bt.Split)
	assert.True(t, bt.Fit.To.Before(bt.Test.From), "the fit ends before the test starts")
	assert.True(t, bt.Config.Match)
	assert.Equal(t, whatifapi.MeasuredRouter, bt.Measured)
	assert.Positive(t, bt.Summary.Judged)
	scored := 0
	for _, sc := range bt.Summary.TPOT {
		scored += sc.All.Steps
	}
	assert.Positive(t, scored, "no TPOT step scored")
	assert.True(t, strings.HasPrefix(bt.Verdict, "Over the "), bt.Verdict)
}

// Two recordings of different configurations are refused, exit 2, naming
// what differs. The q32 staircase and the 0.6B staircase share the
// pseudonym model-02: the parallelism tells them apart. (Priced anyway with
// --allow-config-mismatch: TestBacktest_RefusesAnotherConfiguration.)
func TestBacktestRefusesAnotherConfiguration(t *testing.T) {
	var out strings.Builder
	err := run(t.Context(), []string{"backtest", "--fit", q32Recording, "--test", stair("ttft-staircase-b-off")}, &out, &out)
	require.Error(t, err)
	assert.Equal(t, 2, exitCode(err), out.String())
	assert.Contains(t, err.Error(), "decode tensor-parallel-size is 2 in the fit and 1 in the test")
	assert.Contains(t, err.Error(), "--allow-config-mismatch")
}

// backtestParity is the backtest's JSON goldens TestBacktestAPIParity
// holds POST /v1/backtest to.
var backtestParity = []string{"backtest-q32-split-json"}

// POST /v1/backtest, splitting the served recording, answers the CLI's JSON
// golden, but for the ids: one code path.
func TestBacktestAPIParity(t *testing.T) {
	src := &servedSources{warn: io.Discard, recordings: q32Dir}
	h := whatifapi.New(whatifapi.Config{Resolver: src, WhatifSchema: "schema-v" + strconv.Itoa(schemaVersion)}).Handler()
	code, body := apiPost(t, h, "/v1/inputs", whatifapi.InputsRequest{Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording,
		Name: filepath.Base(q32Recording)}})
	require.Equal(t, 200, code, "%s", body)
	var in whatifapi.InputsResponse
	require.NoError(t, json.Unmarshal(body, &in))
	cv2 := 1.0
	code, body = apiPost(t, h, "/v1/backtest", whatifapi.BacktestRequest{InputsID: in.InputsID, Split: "50%", ArrivalCV2: &cv2})
	require.Equal(t, 200, code, "%s", body)
	var api, cli whatifapi.BacktestResponse
	require.NoError(t, jsonStrict(body, &api))
	b, err := os.ReadFile(filepath.Join("testdata", "backtest-q32-split-json.golden"))
	require.NoError(t, err)
	require.NoError(t, jsonStrict(b, &cli))
	for _, x := range []*whatifapi.BacktestResponse{&api, &cli} {
		x.BacktestID, x.Fit.InputsID, x.Test.InputsID = "", "", ""
	}
	assert.Equal(t, cli, api)
	assert.NotEmpty(t, api.Steps)
}

// The KV-deferral hold given without the remainder spread is warned of, not
// refused: each mode alone is how the two are compared. Given with it, or
// not given, there is no warning.
func TestBacktestFlags_KVHoldAloneWarns(t *testing.T) {
	base := []string{"backtest", "--recording", "a", "--experimental-ttft=v2nl-line", "--ttft-target", "fast"}
	for name, c := range map[string]struct {
		extra []string
		warns bool
	}{
		"hold alone":  {[]string{"--rank-mixture-kv-hold"}, true},
		"both":        {[]string{"--rank-mixture-kv-hold", "--rank-mixture-remainder-spread"}, false},
		"spread only": {[]string{"--rank-mixture-remainder-spread"}, false},
		"neither":     {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			var out, warn strings.Builder
			err := run(t.Context(), append(slices.Clone(base), c.extra...), &out, &warn)
			require.Error(t, err, "the bad target stops it after the modes are checked")
			assert.Contains(t, err.Error(), "--ttft-target")
			if c.warns {
				assert.Contains(t, warn.String(), "--rank-mixture-kv-hold without --rank-mixture-remainder-spread")
			} else {
				assert.NotContains(t, warn.String(), "--rank-mixture-kv-hold")
			}
		})
	}
}

// The flags are checked before any recording is read.
func TestBacktestFlags(t *testing.T) {
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"no source":        {[]string{"backtest"}, "give --recording"},
		"both ways":        {[]string{"backtest", "--recording", "a", "--fit", "b"}, "give one way"},
		"split with two":   {[]string{"backtest", "--fit", "a", "--test", "b", "--split", "50%"}, "split one --recording"},
		"bad target":       {[]string{"backtest", "--recording", "a", "--ttft-target", "fast"}, "--ttft-target"},
		"bad output":       {[]string{"backtest", "--recording", "a", "-o", "yaml"}, "-o"},
		"fit without test": {[]string{"backtest", "--fit", "a"}, "give --recording"},
		"fit steps alone":  {[]string{"backtest", "--recording", "a", "--rank-mixture-fit-steps"}, "--experimental-ttft=v2"},
		"kv hold alone":    {[]string{"backtest", "--recording", "a", "--rank-mixture-kv-hold"}, "--experimental-ttft=v2nl-line"},
		"kv hold with v2": {[]string{"backtest", "--recording", "a", "--experimental-ttft=v2", "--rank-mixture-kv-hold"},
			"--experimental-ttft=v2nl-line"},
		"spread with v2": {[]string{"backtest", "--recording", "a", "--experimental-ttft=v2", "--rank-mixture-remainder-spread"},
			"--experimental-ttft=v2nl-line"},
		"server alone": {[]string{"backtest", "--recording", "a", "--" + rankMixtureRemainderServerFlag},
			"--rank-mixture-remainder-spread"},
		"server without the spread": {[]string{"backtest", "--recording", "a", "--experimental-ttft=v2nl-line",
			"--" + rankMixtureRemainderServerFlag}, "--rank-mixture-remainder-spread"},
		"server with v2": {[]string{"backtest", "--recording", "a", "--experimental-ttft=v2", "--rank-mixture-remainder-spread",
			"--" + rankMixtureRemainderServerFlag}, "--experimental-ttft=v2nl-line"},
	} {
		t.Run(name, func(t *testing.T) {
			var out strings.Builder
			err := run(t.Context(), c.args, &out, &out)
			require.Error(t, err)
			assert.Equal(t, 2, exitCode(err))
			assert.Contains(t, err.Error(), c.want)
		})
	}
}

// The remainder server on a recording made before the pods' API-server
// series were recorded (every public one): the backtest sends it, the model
// reports why it did not apply, and every figure is the spread's own.
func TestBacktest_RemainderServerWithoutTheSeries(t *testing.T) {
	base := []string{"backtest", "--recording", q32Recording, "--split", "50%", "--experimental-ttft=v2nl-line",
		"--rank-mixture-min-fit-window", "2h", "--rank-mixture-kv-hold", "--rank-mixture-remainder-spread", "-o", "json"}
	type answer struct {
		Steps []struct {
			TTFT json.RawMessage `json:"ttft"`
		} `json:"steps"`
		RankMixture struct {
			ZParts *struct {
				RemainderServer    json.RawMessage `json:"remainderServer"`
				RemainderServerOff string          `json:"remainderServerOff"`
			} `json:"zParts"`
		} `json:"rankMixture"`
	}
	read := func(args []string) answer {
		var out, warn strings.Builder
		require.NoError(t, run(t.Context(), args, &out, &warn), warn.String())
		var a answer
		require.NoError(t, json.Unmarshal([]byte(out.String()), &a))
		require.NotNil(t, a.RankMixture.ZParts)
		return a
	}
	spread := read(base)
	server := read(append(slices.Clone(base), "--"+rankMixtureRemainderServerFlag))
	assert.Empty(t, spread.RankMixture.ZParts.RemainderServerOff, "not asked, nothing said")
	// The public recordings hold no router processing either, so no spread
	// to draw on: the first reason it did not apply.
	assert.Equal(t, "no remainder spread to draw it on", server.RankMixture.ZParts.RemainderServerOff)
	assert.Nil(t, server.RankMixture.ZParts.RemainderServer)
	require.NotEmpty(t, spread.Steps)
	require.Len(t, server.Steps, len(spread.Steps))
	for i := range spread.Steps {
		assert.JSONEq(t, string(spread.Steps[i].TTFT), string(server.Steps[i].TTFT), "step %d", i)
	}
}
