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

// The planner API states flow control and the router's wait as the CLI's
// flags do: assume.flowControl is --flow-control, assume.routerWait
// inPrefill is --router-wait-in-prefill (the default), gateStage
// --price-router-wait, and none --router-wait-in-prefill=false. The fleet the API prices is the CLI's to the bit, and past
// the prefill onset its flag is the one the switch leaves: FlowControlWaitPriced
// with either way of pricing the wait, AboveFlowControlOnset with none. The
// recording has no flow-control series, so the API's flow control is the
// stated one. The fleet is the Qwen3-32B warm-up's at bucket C's shape
// (8000/200 tokens, the spec's prompt cv, Ca^2 4), whose prefill onset sits
// under whatif.MaxRouterWaitKneeShare of its knee. A value no flag takes is
// the caller's error.
func TestTTFT_TheAPIStatesTheRouterWaitAsTheCLI(t *testing.T) {
	_, src := servedAPI(t)
	res, err := src.Resolve(context.Background(), whatifapi.InputsRequest{
		Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: "q32.jsonl.gz"}})
	require.NoError(t, err)
	ss, ok := res.Pricer.(*servedState)
	require.True(t, ok)
	require.False(t, ss.st.FlowControlSeries, "precondition: the flow control the API prices is the stated one")

	cv, ca2 := 0.0577, 4.0
	cases := []struct {
		routerWait string
		flags      []string
		gate, in   bool
		priced     bool
	}{
		{"", nil, whatif.DefaultPriceRouterWait, plannerRouterWaitInPrefill, whatif.DefaultPriceRouterWait || plannerRouterWaitInPrefill},
		{whatifapi.RouterWaitNone, []string{"--router-wait-in-prefill=false"}, false, false, false},
		{whatifapi.RouterWaitInPrefill, []string{"--router-wait-in-prefill"}, false, true, true},
		{whatifapi.RouterWaitGateStage, []string{"--price-router-wait"}, true, false, true},
	}
	for _, c := range cases {
		served, err := ss.Fleet(whatifapi.Knobs{Prefill: 2, Decode: 2, PromptTokens: 8000, OutputTokens: 200, Headroom: 0.9,
			TTFTPercentile: 0.95, ArrivalCV2: &ca2,
			Assume: whatifapi.Assume{PromptCV: &cv, FlowControl: whatifapi.FlowControlOn, RouterWait: c.routerWait}})
		require.NoError(t, err, "routerWait %q", c.routerWait)
		api := served.(*servedFleet)

		var f answerFlags
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		f.register(fs)
		args := q32TTFT("slo-to-rate", "--ttft", "22.71s@p95", "--assume-prompt-cv", "0.0577", "--arrival-cv2", "4", "--flow-control", "on")
		require.NoError(t, fs.Parse(append(args[1:], c.flags...)))
		f.question = rateQuestion
		require.NoError(t, f.check(fs))
		variants, err := f.decodeVariants(ss.st)
		require.NoError(t, err)
		cli, err := f.ttftFleets(ss.st, ttftQuestion{q: 0.95, seconds: 22.71}, variants)
		require.NoError(t, err)
		require.Len(t, cli, 1)
		require.Nil(t, cli[0].refused)

		assert.Equal(t, cli[0].fleet, api.pf.fleet, "routerWait %q: one fleet", c.routerWait)
		assert.Equal(t, cli[0].stated, api.pf.stated, "routerWait %q: the same inputs stated, so the same envelope class", c.routerWait)
		assert.Equal(t, c.gate, api.pf.fleet.PriceRouterWait, "routerWait %q", c.routerWait)
		assert.Equal(t, c.in, api.pf.fleet.RouterWaitInPrefill, "routerWait %q", c.routerWait)
		require.Equal(t, whatif.FlowControlOn, api.pf.fleet.FlowControl.State, "stated")

		on, ok := api.Onsets().(whatif.Onsets)
		require.True(t, ok)
		knee, ok := api.PrefillKnee().(whatif.Answer)
		require.True(t, ok)
		inside := min(whatif.MaxRouterWaitKneeShare*knee.Value, on.Decode)
		require.Less(t, on.Prefill, inside, "precondition: the prefill onset binds first, inside the share")
		above := (on.Prefill + inside) / 2
		got, ok := api.TTFTAt(above).(whatif.TTFT)
		require.True(t, ok, "routerWait %q", c.routerWait)
		fromCLI := cli[0].priced(func(fl whatif.TTFTFleet) whatif.Result { return whatif.TTFTAt(fl, above, 0.95) }).(whatif.TTFT)
		assert.Equal(t, fromCLI.Seconds, got.Seconds, "routerWait %q", c.routerWait)
		assert.Equal(t, c.priced, got.HasFlag(whatif.FlowControlWaitPriced), "routerWait %q: %v", c.routerWait, got.Flags)
		assert.Equal(t, !c.priced, got.HasFlag(whatif.AboveFlowControlOnset), "routerWait %q: %v", c.routerWait, got.Flags)
	}

	for field, bad := range map[string]whatifapi.Assume{"assume.flowControl": {FlowControl: "maybe"}, "assume.routerWait": {RouterWait: "both"}} {
		_, err := ss.Fleet(whatifapi.Knobs{Prefill: 2, Headroom: 0.9, Assume: bad})
		require.Error(t, err, "%+v", bad)
		assert.True(t, whatifapi.IsInputError(err), "%+v: %v", bad, err)
		assert.Contains(t, err.Error(), field)
		require.Error(t, bad.CheckFlowControl(), "the validator backtest and predict call")
	}
	_, err = ss.Fleet(whatifapi.Knobs{Prefill: 2, Headroom: 0.9, Assume: whatifapi.Assume{FlowControl: whatifapi.FlowControlOff, RouterWait: whatifapi.RouterWaitNone}})
	require.NoError(t, err, "a valid assume on the same knobs")
}

// backtest's --flow-control, --router-wait-in-prefill, --price-router-wait
// and --assume-prompt-cv reach the request as assume.flowControl,
// assume.routerWait and assume.promptCV, nothing at the defaults (the default
// --router-wait-in-prefill restated is the default); the two ways of pricing
// the wait are refused together, as is a cv out of range.
func TestBacktest_FlowControlFlagsAreAssumed(t *testing.T) {
	for _, c := range []struct {
		f    backtestFlags
		want *whatifapi.Assume
	}{
		{backtestFlags{routerWaitInPrefill: plannerRouterWaitInPrefill, priceRouterWait: whatif.DefaultPriceRouterWait}, nil},
		{backtestFlags{flowControl: "on", routerWaitInPrefill: true}, &whatifapi.Assume{FlowControl: "on"}},
		{backtestFlags{flowControl: "on", routerWaitInPrefill: false}, &whatifapi.Assume{FlowControl: "on", RouterWait: whatifapi.RouterWaitNone}},
		{backtestFlags{flowControl: "off", priceRouterWait: true}, &whatifapi.Assume{FlowControl: "off", RouterWait: whatifapi.RouterWaitGateStage}},
		{backtestFlags{routerWaitInPrefill: plannerRouterWaitInPrefill, set: map[string]bool{"assume-prompt-cv": true}}, &whatifapi.Assume{PromptCV: ptrTo(0.0)}},
	} {
		req, err := c.f.request()
		require.NoError(t, err)
		assert.Equal(t, c.want, req.Assume, "%+v", c.f)
	}
	_, err := (&backtestFlags{routerWaitInPrefill: true, priceRouterWait: true}).request()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "set one")
	_, err = (&backtestFlags{flowControl: "maybe"}).request()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--flow-control")
	_, err = (&backtestFlags{set: map[string]bool{"assume-prompt-cv": true}, assumePromptCV: whatif.PromptCVStop + 1}).request()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--assume-prompt-cv")
}
