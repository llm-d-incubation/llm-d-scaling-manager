package main

import (
	"context"
	"flag"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// --price-router-wait is the experimental whatif.TTFTFleet.PriceRouterWait:
// off by default, at the SLO analyzer's default, and on with the flag. The
// fleet carries the flag as asked; whatif applies it only on processor
// sharing (TestFlowControlWait_FlagFollowsTheSwitch, "first come").
func TestTTFT_PriceRouterWaitFlag(t *testing.T) {
	for _, c := range []struct {
		args []string
		want bool
	}{{nil, whatif.DefaultPriceRouterWait}, {[]string{"--price-router-wait"}, true}, {[]string{"--price-router-wait=false"}, false}} {
		var f answerFlags
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		f.register(fs)
		require.NoError(t, fs.Parse(c.args))
		assert.Equal(t, c.want, f.priceRouterWait, "%v", c.args)
	}
	require.False(t, whatif.DefaultPriceRouterWait, "experimental: off by default")

	pf := fleetOf(t, learnedState(), answerFlags{})
	require.Nil(t, pf.refused)
	assert.True(t, pf.fleet.ProcessorSharing)
	assert.False(t, pf.fleet.PriceRouterWait, "the default")
	pf = fleetOf(t, learnedState(), answerFlags{priceRouterWait: true})
	assert.True(t, pf.fleet.PriceRouterWait, "with the flag")
}

// The planner API and the CLI read one fleet: on the KV-pull recording with
// flow control taken as on (no committed recording has it on) and V_P
// assumed low enough that the prefill onset binds before the decode one, the API's fleet (servedState.Fleet) is the CLI's
// (answerFlags parsed from the same knobs, ttftFleets), the router's wait at
// the shared default. So their figures, flags and rate answers are one: at a
// rate past the prefill onset both carry AboveFlowControlOnset, a miss
// (whatif.TTFTMeets), and the rate question 1 answers is under the onset,
// bound by it, its own verdict met.
func TestTTFT_TheAPIAndTheCLIReadOneFleet(t *testing.T) {
	_, src := servedAPI(t)
	res, err := src.Resolve(context.Background(), whatifapi.InputsRequest{
		Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: "throughput-sweep.jsonl.gz"}})
	require.NoError(t, err)
	ss, ok := res.Pricer.(*servedState)
	require.True(t, ok)
	require.False(t, ss.st.FlowControlSeries, "precondition: no committed recording has flow control on")
	ss.st.FlowControlSeries = true

	vp, a, cv := 20000.0, 0.009, 0.06
	served, err := ss.Fleet(whatifapi.Knobs{Prefill: 2, Headroom: 0.9, Assume: whatifapi.Assume{VP: &vp, A: &a, PromptCV: &cv}, TTFTPercentile: 0.9})
	require.NoError(t, err)
	api := served.(*servedFleet)

	var f answerFlags
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f.register(fs)
	require.NoError(t, fs.Parse([]string{"--recording", outputSpreadRun, "--prefill", "2", "--assume-vp", "20000",
		"--assume-a", "9ms", "--assume-prompt-cv", "0.06", "--ttft", "500ms@p90"}))
	f.question = rateQuestion
	require.NoError(t, f.check(fs))
	variants, err := f.decodeVariants(ss.st)
	require.NoError(t, err)
	cli, err := f.ttftFleets(ss.st, ttftQuestion{q: 0.9, seconds: 0.5}, variants)
	require.NoError(t, err)
	require.Len(t, cli, 1)
	require.Nil(t, cli[0].refused)

	assert.Equal(t, whatif.DefaultPriceRouterWait, api.pf.fleet.PriceRouterWait, "the API at the shared default")
	assert.Equal(t, cli[0].fleet, api.pf.fleet, "one fleet")
	require.Equal(t, whatif.FlowControlOn, api.pf.fleet.FlowControl.State)

	on, ok := api.Onsets().(whatif.Onsets)
	require.True(t, ok)
	knee, ok := api.PrefillKnee().(whatif.Answer)
	require.True(t, ok)
	t.Logf("onsets %+v, prefill knee %g, decode knee %v", on, knee.Value, api.DecodeKnee())
	require.Less(t, on.Prefill, min(knee.Value, on.Decode), "precondition: the prefill onset binds first")
	above := (on.Prefill + min(knee.Value, on.Decode)) / 2
	target := whatif.TTFTTarget{Seconds: 100, Percentile: 0.9}
	for _, rate := range []float64{0.5 * on.Prefill, above} {
		fromAPI := api.TTFTAt(rate)
		fromCLI := cli[0].priced(func(fl whatif.TTFTFleet) whatif.Result { return whatif.TTFTAt(fl, rate, 0.9) })
		require.IsType(t, whatif.TTFT{}, fromAPI)
		assert.Equal(t, fromCLI.(whatif.TTFT).Flags, fromAPI.(whatif.TTFT).Flags, "at %g r/s", rate)
		met, known := whatif.TTFTMeets(fromAPI, target)
		assert.True(t, known)
		assert.Equal(t, rate < on.Prefill, met, "at %g r/s (onset %g): %v", rate, on.Prefill, fromAPI.(whatif.TTFT).Flags)
	}

	_, prefill, _ := api.PlainLimits(target.Seconds, 0, 1)
	r, ok := prefill.(whatif.Answer)
	require.True(t, ok, "%v", prefill)
	cliRate, ok := whatif.RateForTTFT(cli[0].fleet, target).(whatif.Answer)
	require.True(t, ok)
	assert.Equal(t, cliRate.Value, r.Value, "one rate")
	assert.Less(t, r.Value, on.Prefill)
	assert.Contains(t, r.Basis.BoundBy, whatif.BoundPrefillOnset)
	met, known := whatif.TTFTMeets(api.TTFTAt(r.Value), target)
	assert.True(t, met && known, "the rate answered meets its own verdict")
}
