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
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// experimentalArgs prices the staircase's b-on fleet with the assumed
// prefill and the targets of the at-replicas goldens.
var experimentalArgs = slices.Concat([]string{"--recording", stairBOn}, stairAssumed)

// --experimental-ttft turns on exactly the TTFT model's seven switches that
// are off by default, and nothing else; without it the replay's options are
// the zero options, as before the flag.
func TestExperimentalTTFT_TurnsOnExactlyItsSwitches(t *testing.T) {
	assert.Equal(t, learnreplay.Options{}, (&answerFlags{}).replayOptions(), "off, the options are as before")

	want := map[string]map[string]bool{
		learnreplay.PackageSaturation: {
			saturation.SwitchPrefillVPWindowBest:   true,
			saturation.SwitchPrefillVPAtServedWork: true,
		},
		learnreplay.PackageSLO: {
			slo.SwitchTTFTComputedHit:    true,
			slo.SwitchTTFTTierLoad:       true,
			slo.SwitchTTFTTierKVWait:     true,
			slo.SwitchTTFTPSByEngine:     true,
			slo.SwitchTTFTHopWindowMeans: true,
		},
	}
	got := (&answerFlags{experimentalTTFT: true}).replayOptions()
	assert.Equal(t, want, got.Switches)

	// Each is a switch its package accepts, off in the planner by default,
	// so the flag moves what the default does not.
	planner := learnreplay.PlannerSwitches()
	for pkg, set := range want {
		for name := range set {
			assert.NotContains(t, planner[pkg], name, "%s is on by default: the flag would add nothing", name)
		}
	}
	require.NoError(t, saturation.NewSaturationAnalyzer(capacity.NewStore()).SetSwitchesForReplay(got.Switches[learnreplay.PackageSaturation]))
	require.NoError(t, slo.New(nil, nil).SetSwitchesForReplay(got.Switches[learnreplay.PackageSLO]))

	// The plateau flag adds its own switch beside them.
	both := (&answerFlags{experimentalTTFT: true, vpPlateau: true}).replayOptions()
	assert.True(t, both.Switches[learnreplay.PackageSaturation][saturation.SwitchPrefillVPOccupancyPlateau])
	assert.Len(t, both.Switches[learnreplay.PackageSaturation], 3)
}

// at-replicas on the b-on staircase with --experimental-ttft: every prefill
// and TTFT figure is flagged ExperimentalTTFT and reads OUTSIDE VALIDATED
// REGION, and the assumed line lists the switches.
func TestExperimentalTTFT_AtReplicasGolden(t *testing.T) {
	checkGolden(t, "at-replicas-experimental-ttft", 0,
		slices.Concat([]string{"at-replicas"}, experimentalArgs, []string{"--" + experimentalTTFTFlag}))
}

// Off, no figure says experimental and the output is the default's; on,
// the prefill role's TTFT answer carries ExperimentalTTFT, reads OUTSIDE VALIDATED
// REGION by it, and the inputs and the footer say so.
func TestExperimentalTTFT_OffByDefaultOnWhenAsked(t *testing.T) {
	args := slices.Concat([]string{"slo-to-rate", "-o", "json"}, experimentalArgs)
	off, _ := runCmd(t, args...)
	assert.NotContains(t, off, string(whatif.ExperimentalTTFT))
	assert.NotContains(t, off, experimentalTTFTFlag)
	again, _ := runCmd(t, args...)
	assert.Equal(t, off, again, "the default's output is stable")

	on, code := runCmd(t, slices.Concat(args, []string{"--" + experimentalTTFTFlag})...)
	require.NotEqual(t, off, on)
	w := decodeOutput(t, on)
	answered := 0
	for _, r := range w.Roles {
		if r.TTFT != nil {
			assert.Contains(t, r.TTFT.Assumed, experimentalTTFTNote(), "role %s", r.Role)
		}
		// The prefill role's rate is the one at the TTFT target; decode's
		// is at the TPOT target, which the switches do not touch.
		if r.Role != rolePrefill || r.Results.Rate == nil || r.Results.Rate.Answer == nil {
			continue
		}
		answered++
		a := r.Results.Rate.Answer
		assert.Contains(t, a.Flags, string(whatif.ExperimentalTTFT), "role %s", r.Role)
		require.NotNil(t, a.Validation, "role %s", r.Role)
		assert.False(t, a.Validation.Validated, "role %s", r.Role)
		assert.True(t, strings.HasPrefix(a.Validation.Label, "OUTSIDE VALIDATED REGION (experimental TTFT model"),
			"role %s: %s", r.Role, a.Validation.Label)
	}
	assert.Positive(t, answered, "no TTFT answer priced (exit %d):\n%s", code, on)
	assert.Contains(t, w.Note, "--"+experimentalTTFTFlag)
	for _, s := range experimentalTTFT() {
		assert.Contains(t, experimentalTTFTNote(), s.name)
	}
}

// Every command that replays a recording takes the flag, with one help
// that says it is experimental and how it read.
func TestExperimentalTTFT_EveryReplayingCommandTakesIt(t *testing.T) {
	for _, cmd := range []string{"at-replicas", "slo-to-rate", "replicas-to-slo", "backtest", "predict", "day-plan"} {
		out, _ := runCmd(t, cmd, "-h")
		assert.Contains(t, out, "-"+experimentalTTFTFlag, cmd)
		assert.Contains(t, out, "for demonstration, not validated", cmd)
		assert.Contains(t, out, "for demonstration only", cmd)
	}
}

// The server reads the request's experimentalTTFT and keeps its inputs
// apart from the default's; backtest, predict and day-plan resolve through
// it with their flag, and a backtest's refit keeps it.
func TestExperimentalTTFT_TheServerAndTheResolvingCommandsReadIt(t *testing.T) {
	ctx := context.Background()
	src := &servedSources{warn: io.Discard, recordings: filepath.Dir(stairBOn)}
	req := whatifapi.InputsRequest{Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: filepath.Base(stairBOn)}}
	off, err := src.Resolve(ctx, req)
	require.NoError(t, err)
	req.ExperimentalTTFT = true
	on, err := src.Resolve(ctx, req)
	require.NoError(t, err)
	assert.False(t, off.Inputs.ExperimentalTTFT)
	assert.NotContains(t, off.Inputs.Assumed, experimentalTTFTNote())
	assert.True(t, on.Inputs.ExperimentalTTFT)
	assert.Contains(t, on.Inputs.Assumed, experimentalTTFTNote())
	assert.NotEqual(t, off.Content, on.Content, "the experimental inputs share the default's id")

	bt := backtestFlags{experimentalTTFT: true}
	res, err := bt.resolve(ctx, stairBOn, 0, io.Discard)
	require.NoError(t, err)
	assert.True(t, res.Inputs.ExperimentalTTFT, "backtest and predict")
	dp := dayPlanFlags{recording: stairBOn, experimentalTTFT: true, noSelfCheck: true}
	res, err = dp.resolve(ctx, io.Discard)
	require.NoError(t, err)
	assert.True(t, res.Inputs.ExperimentalTTFT, "day-plan")
}

// serve --experimental-ttft prices every request with the experimental model,
// so a page user need not ask: a request that does not ask reads as one that
// does, under the same inputs id; the flag is in serve's help.
func TestExperimentalTTFT_ServeFlagTurnsItOnForEveryRequest(t *testing.T) {
	ctx := context.Background()
	req := whatifapi.InputsRequest{Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: filepath.Base(stairBOn)}}
	asked := req
	asked.ExperimentalTTFT = true
	on, err := (&servedSources{warn: io.Discard, recordings: filepath.Dir(stairBOn)}).Resolve(ctx, asked)
	require.NoError(t, err)

	served := &servedSources{warn: io.Discard, recordings: filepath.Dir(stairBOn), experimentalTTFT: true}
	got, err := served.Resolve(ctx, req)
	require.NoError(t, err)
	assert.True(t, got.Inputs.ExperimentalTTFT)
	assert.Contains(t, got.Inputs.Assumed, experimentalTTFTNote())
	assert.Equal(t, on.Content, got.Content, "the served default and the asked-for model share inputs")

	out, _ := runCmd(t, "serve", "-h")
	assert.Contains(t, out, "-"+experimentalTTFTFlag)
}
