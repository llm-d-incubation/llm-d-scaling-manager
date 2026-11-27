package main

import (
	"context"
	"flag"
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// loopGainVariant is a decode whose intercept is small beside its slope: its
// loop gain passes 0.8 well under its knee.
func loopGainVariant(on bool) pricedVariant {
	return pricedVariant{loopGain: on, decode: whatif.Decode{Line: itl.Model{A: 0.040, B: 0.004}, Band: whatif.FillBand{Lo: 0.05, Hi: 0.8},
		CapacityTokens: 400_000, KVPerRequest: 8400, OutputTokens: 200, ReplicaCap: 256, Replicas: 2, Tier: whatif.TierOLS}}
}

// tpotsAt is replicas-to-slo --rate's mean TPOT and p90 on pv.
func tpotsAt(pv pricedVariant, rate float64) (whatif.Answer, whatif.Answer) {
	q := whatif.ReplicasQuery{Rate: rate, PercentileTarget: &whatif.TPOTTarget{Percentile: 0.9, Ratio: whatif.TPOTRatio{Value: 1.3, Assumed: true}}}
	a, _ := replicasAt(pv, nil, nil, q, &rate, 0, resolvedRatio{ratio: q.PercentileTarget.Ratio})
	mean, _ := a.slo.MeanTPOT.(whatif.Answer)
	p90, _ := a.slo.PercentileTPOT.(whatif.Answer)
	return mean, p90
}

// On, every TPOT carries its loop gain, and one at a gain of 0.8 or more is
// flagged DecodeLoopGain and reads outside the envelope; its value does not
// move. Off, the figures are the planner's as before.
func TestDecodeLoopGain_FlagsWithoutMovingTheFigure(t *testing.T) {
	// g = rate/N x S x KVreq/C x A: 0.5 and 0.85.
	perGain := 400_000 / (200 * 8400 * 0.040) * 2
	for _, c := range []struct {
		g    float64
		high bool
	}{{0.5, false}, {0.85, true}} {
		rate := c.g * perGain
		offMean, offP90 := tpotsAt(loopGainVariant(false), rate)
		onMean, onP90 := tpotsAt(loopGainVariant(true), rate)
		require.Positive(t, offMean.Value)
		assert.Nil(t, offMean.Basis.LoopGain, "off: no loop gain")
		assert.NotContains(t, offMean.Flags, whatif.DecodeLoopGain)
		for _, p := range []struct{ off, on whatif.Answer }{{offMean, onMean}, {offP90, onP90}} {
			assert.Equal(t, p.off.Value, p.on.Value, "g %.2f: the switch moves no figure", c.g)
			require.NotNil(t, p.on.Basis.LoopGain, "g %.2f", c.g)
			assert.InDelta(t, c.g, p.on.Basis.LoopGain.Gain, 1e-9)
			assert.Equal(t, c.high, p.on.HasFlag(whatif.DecodeLoopGain), "g %.2f: %v", c.g, p.on.Flags)
		}
		w := toWire(onMean, unitSeconds)
		require.NotNil(t, w.Answer.Basis.LoopGain)
		assert.InDelta(t, c.g, w.Answer.Basis.LoopGain.Gain, 1e-9)
	}
}

// No envelope row allows DecodeLoopGain, and it says why in words.
func TestDecodeLoopGain_EveryRowLeavesItOutside(t *testing.T) {
	e := whatif.DefaultEnvelope()
	for source, flags := range e.AllowedFlags {
		assert.NotContains(t, flags, whatif.DecodeLoopGain, source)
	}
	assert.Contains(t, e.OutsideFlags[whatif.DecodeLoopGain], "loop gain")
}

// The flag is on every command that prices a decode TPOT, and off by default.
func TestDecodeLoopGain_FlagRegisteredOff(t *testing.T) {
	var f answerFlags
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f.register(fs)
	require.NotNil(t, fs.Lookup(decodeLoopGainFlag))
	assert.False(t, f.decodeLoopGain)
	require.NoError(t, fs.Parse([]string{"--" + decodeLoopGainFlag}))
	assert.True(t, f.decodeLoopGain)
}

// Every command that prices a decode TPOT takes the flag.
func TestDecodeLoopGain_EveryCommandTakesIt(t *testing.T) {
	for _, cmd := range []string{"at-replicas", "slo-to-rate", "replicas-to-slo", "backtest", "predict", "day-plan"} {
		out, _ := runCmd(t, cmd, "-h")
		assert.Contains(t, out, "-"+decodeLoopGainFlag, cmd)
	}
}

// loopGainMeanTPOT is replicas-to-slo's decode mean TPOT on the three-pod
// golden, with extra flags.
func loopGainMeanTPOT(t *testing.T, extra ...string) *wireAnswer {
	t.Helper()
	args := append([]string{"replicas-to-slo", "--recording", threePods, "--assume-itl-b", "floor", "-o", "json"}, extra...)
	out, code := runCmd(t, args...)
	require.Equal(t, 0, code, out)
	for _, r := range decodeOutput(t, out).Roles {
		if r.Results.MeanTPOT != nil && r.Results.MeanTPOT.Answer != nil {
			return r.Results.MeanTPOT.Answer
		}
	}
	t.Fatalf("no mean TPOT priced: %s", out)
	return nil
}

// From the command line: on, the mean TPOT carries its loop gain at the same
// value; off, it carries none.
func TestDecodeLoopGain_ReplicasToSLOCarriesIt(t *testing.T) {
	off := loopGainMeanTPOT(t)
	on := loopGainMeanTPOT(t, "--"+decodeLoopGainFlag)
	assert.Nil(t, off.Basis.LoopGain)
	require.NotNil(t, on.Basis.LoopGain)
	assert.Positive(t, on.Basis.LoopGain.Gain)
	assert.GreaterOrEqual(t, on.Basis.LoopGain.KneeGain, on.Basis.LoopGain.Gain)
	assert.Equal(t, off.Value, on.Value)
}

// The server reads the request's decodeLoopGain, keeps its inputs apart from
// the default's, and its fleets price with it; backtest, predict and
// day-plan resolve through it with their flag.
func TestDecodeLoopGain_TheServerAndTheResolvingCommandsReadIt(t *testing.T) {
	ctx := context.Background()
	src := &servedSources{warn: io.Discard, recordings: filepath.Dir(threePods)}
	req := whatifapi.InputsRequest{Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: filepath.Base(threePods)}}
	off, err := src.Resolve(ctx, req)
	require.NoError(t, err)
	req.DecodeLoopGain = true
	on, err := src.Resolve(ctx, req)
	require.NoError(t, err)
	assert.False(t, off.Inputs.DecodeLoopGain)
	assert.True(t, on.Inputs.DecodeLoopGain)
	assert.NotEqual(t, off.Content, on.Content, "the loop gain's inputs share the default's id")
	pricer, ok := on.Pricer.(*servedState)
	require.True(t, ok, "%T", on.Pricer)
	assert.True(t, pricer.decodeLoopGain)
	fleet, err := pricer.fleetAtImbalance(whatifapi.Knobs{Decode: 1, Prefill: 3, Headroom: 0.85}, 0)
	require.NoError(t, err)
	assert.True(t, fleet.pv.loopGain, "a fleet the server prices reads the loop gain")

	bt := backtestFlags{decodeLoopGain: true}
	res, err := bt.resolve(ctx, threePods, 0, io.Discard)
	require.NoError(t, err)
	assert.True(t, res.Inputs.DecodeLoopGain, "backtest and predict")
	dp := dayPlanFlags{recording: threePods, decodeLoopGain: true, noSelfCheck: true}
	res, err = dp.resolve(ctx, io.Discard)
	require.NoError(t, err)
	assert.True(t, res.Inputs.DecodeLoopGain, "day-plan")
}
