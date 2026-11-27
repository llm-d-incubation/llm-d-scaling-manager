package main

import (
	"context"
	"flag"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// The planner API states a load-aware router as the CLI's flag does:
// assume.loadAwareRouter is --load-aware-router, the fleet the API prices
// is the CLI's to the bit, and the switch is stated outside the validated
// region alike. On the staircase at three prefill pods with the 1.5 floor
// (TestTTFT_LoadAwareRouterOnTheStaircase) the knee lifts toward the pooled
// one; absent, the fleet is the default's, f held.
func TestTTFT_TheAPIStatesTheLoadAwareRouterAsTheCLI(t *testing.T) {
	dir := t.TempDir()
	b, err := os.ReadFile(stairBOn)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "staircase.jsonl.gz"), b, 0o644))
	src := &servedSources{recordings: dir, warn: io.Discard}
	res, err := src.Resolve(context.Background(), whatifapi.InputsRequest{
		Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: "staircase.jsonl.gz"}})
	require.NoError(t, err)
	served, ok := res.Pricer.(*servedState)
	require.True(t, ok)
	require.Equal(t, whatif.DefaultPlannerLoadAwareRouter, served.loadAwareRouter, "the server prices at the planner's default")
	// The floor at three pods, as the staircase test reads it.
	ss := *served
	ss.imbalanceAcrossCounts = false

	vp, a := 400000.0, 0.02
	knees := map[bool]float64{}
	for _, on := range []bool{false, true} {
		got, err := ss.Fleet(whatifapi.Knobs{Prefill: 3, Headroom: 0.9, TTFTPercentile: 0.9,
			Assume: whatifapi.Assume{VP: &vp, A: &a, LoadAwareRouter: on}})
		require.NoError(t, err, "loadAwareRouter %v", on)
		api := got.(*servedFleet)

		var f answerFlags
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		f.register(fs)
		args := []string{"--recording", stairBOn, "--assume-vp", "400000", "--assume-a", "20ms", "--prefill", "3",
			"--imbalance-across-counts=false", "--ttft", "2s@p90"}
		if on {
			args = append(args, "--load-aware-router")
		}
		require.NoError(t, fs.Parse(args))
		f.question = rateQuestion
		require.NoError(t, f.check(fs))
		variants, err := f.decodeVariants(ss.st)
		require.NoError(t, err)
		cli, err := f.ttftFleets(ss.st, ttftQuestion{q: 0.9, seconds: 2}, variants)
		require.NoError(t, err)
		require.Len(t, cli, 1)
		require.Nil(t, cli[0].refused)

		// --imbalance-across-counts=false is the test's, set on the server's
		// state as at-replicas sets it, which no request states.
		notAcross := func(s []string) []string {
			return slices.DeleteFunc(slices.Clone(s), func(x string) bool { return x == "--imbalance-across-counts" })
		}
		assert.Equal(t, cli[0].fleet, api.pf.fleet, "loadAwareRouter %v: one fleet", on)
		assert.Equal(t, notAcross(cli[0].stated), api.pf.stated, "loadAwareRouter %v: the same inputs stated", on)
		assert.Equal(t, on, api.pf.fleet.Prefill.LoadAwareRouter)
		assert.Equal(t, notAcross(f.statedOutside()), api.f.statedOutside(), "loadAwareRouter %v: one envelope class", on)
		assert.Equal(t, on, slices.Contains(api.f.statedOutside(), "--load-aware-router"), "loadAwareRouter %v", on)
		knee, ok := api.PrefillKnee().(whatif.Answer)
		require.True(t, ok, "loadAwareRouter %v", on)
		knees[on] = knee.Value
	}
	assert.Greater(t, knees[true], 1.2*knees[false], "the knee lifts toward the pooled one")
}

// backtest's --load-aware-router is assume.loadAwareRouter, off by default:
// at the default no assume is sent.
func TestBacktest_LoadAwareRouterFlag(t *testing.T) {
	for _, c := range []struct {
		args []string
		want *whatifapi.Assume
	}{
		{nil, nil},
		{[]string{"--load-aware-router"}, &whatifapi.Assume{LoadAwareRouter: true}},
		{[]string{"--load-aware-router=false"}, nil},
	} {
		var f backtestFlags
		fs := flag.NewFlagSet("backtest", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		f.registerAssume(fs)
		require.NoError(t, fs.Parse(c.args))
		f.parsed(fs)
		req, err := f.request()
		require.NoError(t, err, "%v", c.args)
		assert.Equal(t, c.want, req.Assume, "%v", c.args)
	}
}
