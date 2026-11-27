package main

import (
	"encoding/json"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// backtestQ32Load is the q32 staircase's warm-up split by load: fitted on
// its steps below the median rate, tested on its top quartile.
var backtestQ32Load = []string{"backtest", "--recording", q32Recording, "--fit-below", "p50", "--test-above", "p75", "--arrival-cv2", "1"}

// A recording split by load is fitted on its low steps alone and judged on
// its high ones: the fit reads fewer cycles than the recording, every
// judged step is in the top quartile, and the rest of the test's span is
// counted as outside its load. POST /v1/backtest answers the same, but for
// the ids.
func TestBacktestSplitsOneRecordingByLoad(t *testing.T) {
	var out strings.Builder
	err := run(t.Context(), append(append([]string{}, backtestQ32Load...), "-o", "json"), &out, io.Discard)
	require.NoError(t, err, out.String())
	var cli whatifapi.BacktestResponse
	require.NoError(t, jsonStrict([]byte(out.String()), &cli))
	ls := cli.LoadSplit
	require.NotNil(t, ls)
	assert.Equal(t, "p50", ls.FitBelow)
	assert.Equal(t, "p75", ls.TestAbove)
	assert.Positive(t, ls.FitSteps)
	assert.Positive(t, ls.TestSteps)
	assert.Less(t, ls.FitBelowRate, ls.TestAboveRate)
	assert.True(t, strings.HasPrefix(cli.Split, "by load: fit below p50"), cli.Split)

	var whole strings.Builder
	require.NoError(t, run(t.Context(), append(append([]string{}, backtestQ32...), "-o", "json"), &whole, io.Discard))
	var half whatifapi.BacktestResponse
	require.NoError(t, jsonStrict([]byte(whole.String()), &half))
	assert.Less(t, cli.Fit.Cycles, half.Fit.Cycles+half.Test.Cycles, "the fit read every cycle")
	judged := 0
	for _, st := range cli.Steps {
		if st.Excluded == "" {
			judged++
		}
		if st.Excluded == whatifapi.ExcludedLoad {
			assert.Less(t, st.Rate, ls.TestAboveRate, "a step at or above the floor left out: %s", st.Start)
		}
	}
	assert.Equal(t, cli.Summary.Judged, judged)
	assert.LessOrEqual(t, judged, ls.TestSteps)

	src := &servedSources{warn: io.Discard, recordings: q32Dir}
	h := whatifapi.New(whatifapi.Config{Resolver: src, WhatifSchema: "schema-v" + strconv.Itoa(schemaVersion)}).Handler()
	code, body := apiPost(t, h, "/v1/inputs", whatifapi.InputsRequest{Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording,
		Name: filepath.Base(q32Recording)}})
	require.Equal(t, 200, code, "%s", body)
	var in whatifapi.InputsResponse
	require.NoError(t, json.Unmarshal(body, &in))
	cv2 := 1.0
	code, body = apiPost(t, h, "/v1/backtest", whatifapi.BacktestRequest{InputsID: in.InputsID, FitBelow: "p50", TestAbove: "p75", ArrivalCV2: &cv2})
	require.Equal(t, 200, code, "%s", body)
	var api whatifapi.BacktestResponse
	require.NoError(t, jsonStrict(body, &api))
	for _, x := range []*whatifapi.BacktestResponse{&api, &cli} {
		x.BacktestID, x.Fit.InputsID, x.Test.InputsID = "", "", ""
	}
	assert.Equal(t, cli, api)

	var text strings.Builder
	require.NoError(t, run(t.Context(), backtestQ32Load, &text, io.Discard))
	assert.Contains(t, text.String(), "split by load: fit below p50")
	assert.NotContains(t, text.String(), "one recording split")
}

// The load split's flags are checked before any recording is read.
func TestBacktestLoadSplitFlags(t *testing.T) {
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"fit bound alone":  {[]string{"backtest", "--recording", "a", "--fit-below", "p50"}, "give both"},
		"test bound alone": {[]string{"backtest", "--recording", "a", "--test-above", "p75"}, "give both"},
		"with a split":     {[]string{"backtest", "--recording", "a", "--fit-below", "p50", "--test-above", "p75", "--split", "50%"}, "give one"},
		"fit alone":        {[]string{"backtest", "--fit", "a", "--fit-below", "p50", "--test-above", "p75"}, "give --recording"},
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
