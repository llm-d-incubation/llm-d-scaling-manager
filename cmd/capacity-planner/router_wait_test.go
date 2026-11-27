package main

import (
	"context"
	"flag"
	"io"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// --price-router-wait is whatif.TTFTFleet.PriceRouterWait: opt-in, off by
// default, at the SLO analyzer's default, and on with the flag. The fleet
// carries the flag as asked; whatif applies it only on processor sharing
// (TestFlowControlWait_FlagFollowsTheSwitch, "first come").
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
	require.False(t, whatif.DefaultPriceRouterWait, "opt-in: off by default")

	pf := fleetOf(t, learnedState(), answerFlags{})
	require.Nil(t, pf.refused)
	assert.True(t, pf.fleet.ProcessorSharing)
	assert.False(t, pf.fleet.PriceRouterWait, "the default")
	pf = fleetOf(t, learnedState(), answerFlags{priceRouterWait: true})
	assert.True(t, pf.fleet.PriceRouterWait, "with the flag")
}

// --router-wait-in-prefill is whatif.TTFTFleet.RouterWaitInPrefill: on by
// default (issue #287), off with =false, and refused beside
// --price-router-wait when both are set; --price-router-wait set alone turns
// it off, so router v0.11.0's gate stage needs one flag. The output names it,
// so it is told apart from --price-router-wait, which leaves the same flag.
func TestTTFT_RouterWaitInPrefillFlag(t *testing.T) {
	for _, c := range []struct {
		args          []string
		inPrefill     bool
		priceGateWait bool
	}{
		{nil, true, false},
		{[]string{"--router-wait-in-prefill"}, true, false},
		{[]string{"--router-wait-in-prefill=false"}, false, false},
		{[]string{"--price-router-wait"}, false, true},
	} {
		var f answerFlags
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		f.register(fs)
		require.NoError(t, fs.Parse(append([]string{"--recording", outputSpreadRun}, c.args...)))
		require.NoError(t, f.check(fs), "%v", c.args)
		assert.Equal(t, c.inPrefill, f.routerWaitInPrefill, "%v", c.args)
		assert.Equal(t, c.priceGateWait, f.priceRouterWait, "%v", c.args)
	}

	pf := fleetOf(t, learnedState(), answerFlags{})
	require.Nil(t, pf.refused)
	assert.False(t, pf.fleet.RouterWaitInPrefill, "the struct's zero value")
	pf = fleetOf(t, learnedState(), answerFlags{routerWaitInPrefill: true})
	assert.True(t, pf.fleet.RouterWaitInPrefill, "with the flag")

	// Bucket C past the prefill onset: the default and the flag answer alike,
	// FlowControlWaitPriced; =false caps the rate answer at the onset.
	args := q32TTFT("slo-to-rate", "--ttft", "22.71s@p95", "--assume-prompt-cv", "0.0577", "--arrival-cv2", "4", "--flow-control", "on")
	def, code := runCmd(t, args...)
	require.Equal(t, 0, code, def)
	assert.Contains(t, def, "(--router-wait-in-prefill)", "named where it takes, unasked")
	assert.Contains(t, def, "FlowControlWaitPriced")
	out, code := runCmd(t, slices.Concat(args, []string{"--router-wait-in-prefill"})...)
	require.Equal(t, 0, code, out)
	assert.Equal(t, def, out, "the flag restates the default")
	off, code := runCmd(t, slices.Concat(args, []string{"--router-wait-in-prefill=false"})...)
	require.Equal(t, 0, code, off)
	assert.NotContains(t, off, "FlowControlWaitPriced")
	assert.Contains(t, off, "prefill flow-control onset", "off, the rate answer stops at the onset")
	gate, code := runCmd(t, slices.Concat(args, []string{"--price-router-wait"})...)
	require.Equal(t, 0, code, gate)
	assert.NotContains(t, gate, "(--router-wait-in-prefill)", "the gate stage alone")
	err := run(context.Background(), slices.Concat(args, []string{"--router-wait-in-prefill", "--price-router-wait"}), io.Discard, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "set one")
	out, code = runCmd(t, q32TTFT("slo-to-rate", "--ttft", "22.71s@p95", "--assume-prompt-cv", "0.0577", "--flow-control", "off", "--router-wait-in-prefill")...)
	require.Equal(t, 0, code, out)
	assert.Contains(t, out, "--router-wait-in-prefill not applied: flow control is not on")
	out, code = runCmd(t, q32TTFT("slo-to-rate", "--ttft", "22.71s@p95", "--assume-prompt-cv", "0.0577", "--flow-control", "off")...)
	require.Equal(t, 0, code, out)
	assert.NotContains(t, out, "--router-wait-in-prefill", "the default is not named where it does not take")
}

// The default is pinned: --router-wait-in-prefill is on (issue #287, the
// router v1.0.0 staircase), and --price-router-wait stays off.
func TestTTFT_RouterWaitInPrefillIsTheDefault(t *testing.T) {
	assert.True(t, plannerRouterWaitInPrefill, "on by default since #287")
	assert.Equal(t, whatif.DefaultRouterWaitInPrefill, plannerRouterWaitInPrefill, "read from learnreplay.PlannerSwitches at the value whatif states")
	assert.False(t, whatif.DefaultPriceRouterWait, "the gate stage stays opt-in")
	var bf backtestFlags
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	bf.registerAssume(fs)
	require.NoError(t, fs.Parse(nil))
	bf.parsed(fs)
	assert.True(t, bf.routerWaitInPrefill, "backtest's flag")
	require.NoError(t, fs.Parse([]string{"--price-router-wait"}))
	bf.parsed(fs)
	assert.False(t, bf.routerWaitInPrefill, "backtest's flag gives way to --price-router-wait")
	req, err := bf.request()
	require.NoError(t, err)
	require.NotNil(t, req.Assume)
	assert.Equal(t, whatifapi.RouterWaitGateStage, req.Assume.RouterWait)
}

// The planner API and the CLI read one fleet: on the KV-pull recording with
// flow control taken as on (no committed recording has it on) and V_P
// assumed low enough that the prefill onset binds before the decode one, the API's fleet (servedState.Fleet) is the CLI's
// (answerFlags parsed from the same knobs, ttftFleets), the router's wait at
// the shared default. So their figures, flags and rate answers are one: at a
// rate past the prefill onset both carry AboveFlowControlOnset, a miss
// (whatif.TTFTMeets), or with the switch on FlowControlWaitPriced inside
// whatif.MaxRouterWaitKneeShare of the knee, judged by the figure; and the
// rate question 1 answers is under the cap, bound by it, its own verdict met.
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
		got := fromAPI.(whatif.TTFT)
		assert.Equal(t, fromCLI.(whatif.TTFT).Flags, got.Flags, "at %g r/s", rate)
		assert.Equal(t, fromCLI.(whatif.TTFT).Seconds, got.Seconds, "at %g r/s", rate)
		assert.Equal(t, fromCLI.(whatif.TTFT).Parts, got.Parts, "at %g r/s", rate)
		priced := whatif.DefaultPriceRouterWait && rate >= on.Prefill && rate <= whatif.MaxRouterWaitKneeShare*knee.Value
		assert.Equal(t, priced, got.HasFlag(whatif.FlowControlWaitPriced), "at %g r/s: %v", rate, got.Flags)
		met, known := whatif.TTFTMeets(fromAPI, target)
		assert.True(t, known)
		assert.Equal(t, rate < on.Prefill || priced, met, "at %g r/s (onset %g): %v", rate, on.Prefill, got.Flags)
	}

	_, prefill, _ := api.PlainLimits(target.Seconds, 0, 1)
	r, ok := prefill.(whatif.Answer)
	require.True(t, ok, "%v", prefill)
	cliRate, ok := whatif.RateForTTFT(cli[0].fleet, target).(whatif.Answer)
	require.True(t, ok)
	assert.Equal(t, cliRate.Value, r.Value, "one rate")
	limit := on.Prefill
	if whatif.DefaultPriceRouterWait {
		limit = max(limit, whatif.MaxRouterWaitKneeShare*knee.Value)
	}
	assert.Less(t, r.Value, limit)
	assert.Contains(t, r.Basis.BoundBy, whatif.BoundPrefillOnset)
	met, known := whatif.TTFTMeets(api.TTFTAt(r.Value), target)
	assert.True(t, met && known, "the rate answered meets its own verdict")
}
