package main

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// threePods is the three-prefill-pod golden: P1 keeps 45 minutes of f at 3
// pods, whose f falls with the load, and learns a from none (no prompt
// spread), so a is assumed.
const threePods = "../../internal/engines/analyzers/learnreplay/testdata/goldens/ttft-three-pods/recording.jsonl.gz"

// --prefill-f-load-curve turns on its switch and nothing else, one the
// planner leaves off by default and the saturation analyzer accepts.
func TestFLoadCurve_TurnsOnItsSwitch(t *testing.T) {
	got := (&answerFlags{fLoadCurve: true}).replayOptions()
	assert.Equal(t, map[string]map[string]bool{learnreplay.PackageSaturation: {saturation.SwitchPrefillFLoadCurve: true}}, got.Switches)
	assert.NotContains(t, learnreplay.PlannerSwitches()[learnreplay.PackageSaturation], saturation.SwitchPrefillFLoadCurve)
	require.NoError(t, saturation.NewSaturationAnalyzer(capacity.NewStore()).SetSwitchesForReplay(got.Switches[learnreplay.PackageSaturation]))
	assert.Equal(t, saturation.SwitchPrefillFLoadCurve, fLoadCurveFlag, "the flag is named for its switch")
}

// Every command that replays a recording takes the flag, with one help.
func TestFLoadCurve_EveryReplayingCommandTakesIt(t *testing.T) {
	for _, cmd := range []string{"at-replicas", "slo-to-rate", "replicas-to-slo", "backtest", "predict", "day-plan"} {
		out, _ := runCmd(t, cmd, "-h")
		assert.Contains(t, out, "-"+fLoadCurveFlag, cmd)
		assert.Contains(t, out, "validated on lab recordings only", cmd)
	}
}

// fLoadCurveRead is the prefill knee, the prefill role's inputs, and every
// priced rate's validation label, from slo-to-rate on the three-pod golden
// at prefill pods, with extra flags.
func fLoadCurveRead(t *testing.T, prefillPods string, extra ...string) (knee float64, in *wireTTFT, labels []string) {
	t.Helper()
	args := append([]string{"slo-to-rate", "--recording", threePods, "--ttft", "2s@p90", "--assume-a", "20ms", "--assume-itl-b", "floor",
		"--prefill", prefillPods, "-o", "json"}, extra...)
	out, code := runCmd(t, args...)
	require.Equal(t, 0, code, out)
	w := decodeOutput(t, out)
	var raw struct {
		Roles []struct {
			Role    string `json:"role"`
			Results any    `json:"results"`
		} `json:"roles"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &raw))
	k, ok := 0.0, false
	for _, r := range raw.Roles {
		if r.Role == rolePrefill {
			k, ok = firstNumber(r.Results, "kneeRate")
		}
	}
	require.True(t, ok, out)
	for _, r := range w.Roles {
		if r.Role == rolePrefill {
			in = r.TTFT
		}
		if r.Results.Rate != nil && r.Results.Rate.Answer != nil && r.Results.Rate.Answer.Validation != nil {
			labels = append(labels, r.Results.Rate.Answer.Validation.Label)
		}
	}
	require.NotNil(t, in, "no prefill role: %s", out)
	return k, in, labels
}

// On the recorded count the curve is priced: the inputs carry it and say
// so, f at a light load is the curve's, so the knee is at most the flat
// one, and every priced figure reads OUTSIDE VALIDATED REGION. Off, none of
// it. At a count with no minutes the flat f stands and the inputs say why.
func TestFLoadCurve_PricedAtTheRecordedCountOnly(t *testing.T) {
	offKnee, off, _ := fLoadCurveRead(t, "3")
	onKnee, on, labels := fLoadCurveRead(t, "3", "--"+fLoadCurveFlag)
	t.Logf("knee %.4f -> %.4f; curve %.4g + %.4g/u over %d minutes", offKnee, onKnee, on.ImbalanceCurveFloor, on.ImbalanceCurveExcess, on.ImbalanceCurveMinutes)
	assert.Zero(t, off.ImbalanceCurveExcess)
	assert.NotContains(t, notesText(off.Assumed), fLoadCurveFlag)
	assert.Positive(t, on.ImbalanceCurveFloor)
	assert.Positive(t, on.ImbalanceCurveExcess)
	assert.GreaterOrEqual(t, on.ImbalanceCurveMinutes, 8)
	assert.Contains(t, notesText(on.Assumed), "f at a rate the higher of")
	assert.LessOrEqual(t, onKnee, offKnee)
	require.NotEmpty(t, labels, "a rate is priced")
	for _, l := range labels {
		assert.True(t, strings.HasPrefix(l, "OUTSIDE VALIDATED REGION"), l)
	}

	_, two, _ := fLoadCurveRead(t, "2", "--"+fLoadCurveFlag)
	assert.Zero(t, two.ImbalanceCurveExcess)
	assert.Contains(t, notesText(two.Assumed), "f against the load not identified on 2 prefill pods (0 minutes")

	assert.Equal(t, []string{"--" + fLoadCurveFlag}, (&answerFlags{fLoadCurve: true, set: map[string]bool{}}).statedOutside())
}

// The server reads the request's prefillFLoadCurve, keeps its inputs apart
// from the default's, and states the flag; backtest, predict and day-plan
// resolve through it with their flag.
func TestFLoadCurve_TheServerAndTheResolvingCommandsReadIt(t *testing.T) {
	ctx := context.Background()
	src := &servedSources{warn: io.Discard, recordings: filepath.Dir(threePods)}
	req := whatifapi.InputsRequest{Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: filepath.Base(threePods)}}
	off, err := src.Resolve(ctx, req)
	require.NoError(t, err)
	req.PrefillFLoadCurve = true
	on, err := src.Resolve(ctx, req)
	require.NoError(t, err)
	assert.False(t, off.Inputs.PrefillFLoadCurve)
	assert.True(t, on.Inputs.PrefillFLoadCurve)
	assert.NotEqual(t, off.Content, on.Content, "the curve's inputs share the default's id")
	pricer, ok := on.Pricer.(*servedState)
	require.True(t, ok, "%T", on.Pricer)
	assert.True(t, pricer.fLoadCurve)

	bt := backtestFlags{fLoadCurve: true}
	res, err := bt.resolve(ctx, threePods, 0, io.Discard)
	require.NoError(t, err)
	assert.True(t, res.Inputs.PrefillFLoadCurve, "backtest and predict")
	dp := dayPlanFlags{recording: threePods, fLoadCurve: true, noSelfCheck: true}
	res, err = dp.resolve(ctx, io.Discard)
	require.NoError(t, err)
	assert.True(t, res.Inputs.PrefillFLoadCurve, "day-plan")
}
