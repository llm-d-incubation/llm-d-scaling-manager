package main

import (
	"context"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// sweepRestart is the throughput sweep's restart golden: its prefill pod
// never queued, so V_P is the served peak, a lower bound, and its prompts
// spread, so the service line is read there.
const sweepRestart = "../../internal/engines/analyzers/learnreplay/testdata/goldens/throughput-sweep-restart/recording.jsonl.gz"

// --prefill-vp-service-line turns on its switch and nothing else, one the
// planner leaves off by default and the saturation analyzer accepts.
func TestVPServiceLine_TurnsOnItsSwitch(t *testing.T) {
	got := (&answerFlags{vpServiceLine: true}).replayOptions()
	assert.Equal(t, map[string]map[string]bool{learnreplay.PackageSaturation: {saturation.SwitchPrefillVPServiceLine: true}}, got.Switches)
	assert.NotContains(t, learnreplay.PlannerSwitches()[learnreplay.PackageSaturation], saturation.SwitchPrefillVPServiceLine)
	require.NoError(t, saturation.NewSaturationAnalyzer(capacity.NewStore()).SetSwitchesForReplay(got.Switches[learnreplay.PackageSaturation]))
	assert.Equal(t, saturation.SwitchPrefillVPServiceLine, vpServiceLineFlag, "the flag is named for its switch")
}

// Every command that replays a recording takes the flag, with one help.
func TestVPServiceLine_EveryReplayingCommandTakesIt(t *testing.T) {
	for _, cmd := range []string{"at-replicas", "slo-to-rate", "replicas-to-slo", "backtest", "predict", "day-plan"} {
		out, _ := runCmd(t, cmd, "-h")
		assert.Contains(t, out, "-"+vpServiceLineFlag, cmd)
		assert.Contains(t, out, "not validated", cmd)
	}
}

// Off, the restart golden's V_P is the served peak, a lower bound, and no
// TTFT is priced. On, V_P is the line's, every figure priced reads OUTSIDE
// VALIDATED REGION with the flag among its stated inputs, and the
// self-check says it ran with it.
func TestVPServiceLine_OffByDefaultOutsideWhenOn(t *testing.T) {
	args := []string{"slo-to-rate", "-o", "json", "--recording", sweepRestart, "--ttft", "2s@p90"}
	off, _ := runCmd(t, args...)
	assert.NotContains(t, off, string(prefill.SourceServiceLine))
	assert.NotContains(t, off, vpServiceLineFlag)

	on, code := runCmd(t, append(args, "--"+vpServiceLineFlag)...)
	require.Equal(t, 0, code, on)
	w := decodeOutput(t, on)
	i := slices.IndexFunc(w.Roles, func(r wireRole) bool { return r.Role == rolePrefill })
	require.GreaterOrEqual(t, i, 0, "no prefill role")
	require.NotNil(t, w.Roles[i].TTFT)
	assert.Equal(t, string(prefill.SourceServiceLine), w.Roles[i].TTFT.VPOrigin)
	priced := 0
	for _, r := range w.Roles {
		if r.Results.Rate == nil || r.Results.Rate.Answer == nil {
			continue
		}
		a := r.Results.Rate.Answer
		require.NotNil(t, a.Validation, "role %s", r.Role)
		assert.False(t, a.Validation.Validated, "role %s", r.Role)
		assert.True(t, strings.HasPrefix(a.Validation.Label, "OUTSIDE VALIDATED REGION"), "role %s: %s", r.Role, a.Validation.Label)
		priced++
	}
	assert.Positive(t, priced, "on, a rate is priced")

	// The figures' stated inputs name it, from the flag or the server's
	// request alike.
	assert.Equal(t, []string{"--" + vpServiceLineFlag}, (&answerFlags{vpServiceLine: true, set: map[string]bool{}}).statedOutside())
	assert.Empty(t, (&answerFlags{set: map[string]bool{}}).statedOutside())
}

// The server reads the request's prefillVPServiceLine, keeps its inputs
// apart from the default's, and states the flag, so its figures read
// outside as the CLI's do; backtest, predict and day-plan resolve through
// it with their flag.
func TestVPServiceLine_TheServerAndTheResolvingCommandsReadIt(t *testing.T) {
	ctx := context.Background()
	src := &servedSources{warn: io.Discard, recordings: filepath.Dir(sweepRestart)}
	req := whatifapi.InputsRequest{Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: filepath.Base(sweepRestart)}}
	off, err := src.Resolve(ctx, req)
	require.NoError(t, err)
	req.PrefillVPServiceLine = true
	on, err := src.Resolve(ctx, req)
	require.NoError(t, err)
	assert.False(t, off.Inputs.PrefillVPServiceLine)
	assert.True(t, on.Inputs.PrefillVPServiceLine)
	assert.NotEqual(t, off.Content, on.Content, "the line's inputs share the default's id")
	// The fleets the server prices from the inputs state it, as the CLI's do.
	for _, c := range []struct {
		res  *whatifapi.Resolved
		want []string
	}{{off, nil}, {on, []string{"--" + vpServiceLineFlag}}} {
		pricer, ok := c.res.Pricer.(*servedState)
		require.True(t, ok, "%T", c.res.Pricer)
		sf, err := pricer.fleetAt(whatifapi.Knobs{Decode: 1, Prefill: 1, Headroom: 0.85})
		require.NoError(t, err)
		assert.Equal(t, c.want, sf.pv.stated, "decode")
		assert.Equal(t, c.want, sf.pf.stated, "prefill")
	}

	bt := backtestFlags{vpServiceLine: true}
	res, err := bt.resolve(ctx, sweepRestart, 0, io.Discard)
	require.NoError(t, err)
	assert.True(t, res.Inputs.PrefillVPServiceLine, "backtest and predict")
	dp := dayPlanFlags{recording: sweepRestart, vpServiceLine: true, noSelfCheck: true}
	res, err = dp.resolve(ctx, io.Discard)
	require.NoError(t, err)
	assert.True(t, res.Inputs.PrefillVPServiceLine, "day-plan")
}
