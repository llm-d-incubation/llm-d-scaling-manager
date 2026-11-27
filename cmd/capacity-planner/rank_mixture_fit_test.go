package main

import (
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// The fit flags parse the same on every command, default to the model's
// own fit, and are refused where the model they change is not asked.
func TestRankMixtureFit_Flags(t *testing.T) {
	fs := newFlags("x", io.Discard)
	var r rankMixtureFit
	r.registerModes(fs)
	r.registerWindow(fs)
	require.NoError(t, fs.Parse(nil))
	assert.Zero(t, r.window(), "12h is the default")
	assert.Empty(t, r.windowField())
	require.NoError(t, r.check(false, false))
	require.NoError(t, fs.Parse([]string{"--rank-mixture-kv-hold", "--rank-mixture-remainder-spread", "--" + rankMixtureMinFitWindowFlag, "2h"}))
	assert.Equal(t, 2*time.Hour, r.window())
	assert.Equal(t, "2h0m0s", r.windowField())
	assert.Equal(t, whatifapi.TTFTModelOptions{Model: "m", Stratified: true, KVHold: true, RemainderSpread: true, MinFitWindow: 2 * time.Hour},
		r.options("m", true))
	require.NoError(t, r.check(true, true))
	assert.Error(t, r.check(true, false), "the modes are the variant's")
	assert.Error(t, rankMixtureFit{minFitWindow: time.Hour}.check(false, false), "the window is the model's")
	assert.NoError(t, rankMixtureFit{minFitWindow: time.Hour}.check(true, false), "v2 takes the window")
	assert.Error(t, rankMixtureFit{minFitWindow: -time.Hour}.check(true, true))

	require.NoError(t, fs.Parse([]string{"--" + rankMixtureRemainderServerFlag}))
	assert.True(t, r.options("m", true).RemainderServer)
	require.NoError(t, r.check(true, true))
	assert.Error(t, rankMixtureFit{remainderServer: true}.check(true, true), "the server draws on the spread")
	assert.Error(t, rankMixtureFit{remainderServer: true, remainderSpread: true}.check(true, false), "the variant's")
}

// Each command that fits the model takes the flags, and refuses them
// without the model, as a usage error.
func TestRankMixtureFit_RefusedWithoutTheModel(t *testing.T) {
	for _, args := range [][]string{
		{"at-replicas", "--recording", q32Recording, "--rank-mixture-kv-hold"},
		{"at-replicas", "--recording", q32Recording, "--experimental-ttft=v2", "--rank-mixture-remainder-spread"},
		{"at-replicas", "--recording", q32Recording, "--" + rankMixtureMinFitWindowFlag, "2h"},
		{"day-plan", "--recording", q32Recording, "--rank-mixture-kv-hold"},
		{"day-plan", "--recording", q32Recording, "--experimental-ttft=v2nl-line", "--" + rankMixtureRemainderServerFlag},
		{"at-replicas", "--recording", q32Recording, "--experimental-ttft=v2nl-line", "--" + rankMixtureRemainderServerFlag},
		{"serve", "--recordings", t.TempDir(), "--experimental-ttft=v2nl-line", "--" + rankMixtureRemainderServerFlag},
		{"backtest", "--recording", q32Recording, "--" + rankMixtureMinFitWindowFlag, "2h"},
		{"serve", "--recordings", t.TempDir(), "--rank-mixture-kv-hold"},
		{"serve", "--recordings", t.TempDir(), "--" + rankMixtureMinFitWindowFlag, "2h"},
	} {
		out, code := runCmd(t, args...)
		assert.Equal(t, 2, code, "%v: %s", args, out)
	}
}

// serve's fit flags are the server's defaults (whatifapi.Config), 12h
// the planner's own.
func TestServeRankMixtureFit_Config(t *testing.T) {
	f := serveFlags{recordings: t.TempDir(), listen: "127.0.0.1:0", auth: whatifapi.AuthNone, noSelfCheck: true,
		rankMixtureTTFT: true, rankMixtureNLLine: true,
		rankMixtureFit: rankMixtureFit{kvHold: true, remainderSpread: true, minFitWindow: 2 * time.Hour}}
	_, err := f.handler(newFlags("serve", io.Discard), io.Discard)
	require.NoError(t, err)
	f.rankMixtureNLLine = false
	_, err = f.handler(newFlags("serve", io.Discard), io.Discard)
	require.Error(t, err, "the modes need =v2nl-line")
}

// --tpot-dist-min-fit-steps defaults to the distribution's own 24, needs
// --experimental-tpot=v2 below or above it, and is at least 4.
func TestTPOTDistFit_Flags(t *testing.T) {
	fs := newFlags("x", io.Discard)
	var d tpotDistFit
	d.register(fs)
	require.NoError(t, fs.Parse(nil))
	assert.Zero(t, d.steps(), "24 is the default")
	require.NoError(t, d.check(false))
	require.NoError(t, fs.Parse([]string{"--" + tpotDistMinFitStepsFlag, "8"}))
	assert.Equal(t, 8, d.steps())
	require.NoError(t, d.check(true))
	assert.Error(t, d.check(false), "the count is the distribution's")
	assert.Error(t, tpotDistFit{minFitSteps: 3}.check(true))
	for _, args := range [][]string{
		{"at-replicas", "--recording", q32Recording, "--" + tpotDistMinFitStepsFlag, "8"},
		{"day-plan", "--recording", q32Recording, "--" + tpotDistMinFitStepsFlag, "8"},
		{"backtest", "--recording", q32Recording, "--" + tpotDistMinFitStepsFlag, "8"},
		{"serve", "--recordings", t.TempDir(), "--" + tpotDistMinFitStepsFlag, "8"},
	} {
		out, code := runCmd(t, args...)
		assert.Equal(t, 2, code, "%v: %s", args, out)
	}
}
