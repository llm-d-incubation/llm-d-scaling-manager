package main

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// On a synthetic fleet of multi-Pod groups whose engine rows report one
// rank in four, the rows add up to a quarter of the router's rate and count
// a quarter of the replicas: the planner refuses the recording, naming both,
// rather than price every replica at a quarter of its rate (issue
// llm-d-incubation/llm-d-scaling-manager#331). The same fleet with one row a
// replica resolves.
func TestResolve_RefusesEngineRowsThatCountOneRankInFour(t *testing.T) {
	resolve := func(path string) (*whatifapi.Resolved, error) {
		src := &servedSources{recordings: filepath.Dir(path), warn: io.Discard}
		return src.Resolve(context.Background(), whatifapi.InputsRequest{Model: "m",
			Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: filepath.Base(path)}})
	}
	res, err := resolve(busyFleetRecordingOf(t, 1))
	require.NoError(t, err)
	assert.Empty(t, whatifapi.EngineRowsShort(res.Series))
	compared := 0
	for _, s := range res.Series {
		if s.RateKnown && s.Rate > 0 && s.EngineRate > 0 {
			compared++
		}
	}
	assert.Greater(t, compared, 100, "the rates were compared, and agreed")

	_, err = resolve(busyFleetRecordingOf(t, 4))
	require.Error(t, err)
	assert.True(t, whatifapi.IsInputError(err), err)
	assert.Contains(t, err.Error(), "the engine rows report")
	assert.Contains(t, err.Error(), "(25%)")
	assert.Contains(t, err.Error(), "9 decode engine rows stand for 36 serving replicas")
	assert.Contains(t, err.Error(), "2 prefill engine rows stand for 8 serving replicas")
	assert.Contains(t, err.Error(), "multi-Pod group")
	assert.Contains(t, err.Error(), engineRowsFix, "the refusal says what to do")

	// The CLI's answers read the same rows, and refuse them too.
	err = run(context.Background(), []string{"at-replicas", "--recording", busyFleetRecordingOf(t, 4), "--decode", "9", "--prefill", "2"},
		io.Discard, io.Discard)
	require.Error(t, err)
	assert.Equal(t, 2, exitCode(err), "an input error")
	assert.Contains(t, err.Error(), "9 decode engine rows stand for 36 serving replicas")
	assert.Contains(t, err.Error(), "multi-Pod group")
	assert.Contains(t, err.Error(), engineRowsFix, "the refusal says what to do")
}
