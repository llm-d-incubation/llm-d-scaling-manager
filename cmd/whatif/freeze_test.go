package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// freezeSpecText is a one-bucket spec on the worked example's fleet.
const freezeSpecText = `
operatingFraction: 0.85
percentiles: [90, 95]
targetFloorMargin: 1.2
promptCV: 0.0577
steps: [0.4, 0.7, 1.2]
buckets:
- {name: A, prompt: 1000, output: 1000, ca2: 1, prefill: 1, decode: 1}
`

func writeSpec(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spec.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func freezeArgs(spec string, extra ...string) []string {
	return append([]string{"freeze", "--recording", olsSweep, "--spec", spec, "--assume-vp", "30000"}, extra...)
}

func TestFreeze_SameInputsSameBytes(t *testing.T) {
	spec := writeSpec(t, freezeSpecText)
	one, err := runCLI(freezeArgs(spec)...)
	require.NoError(t, err)
	two, err := runCLI(freezeArgs(spec)...)
	require.NoError(t, err)
	assert.Equal(t, one, two)
	assert.Positive(t, len(one))
}

func TestFreeze_PricesTheBucketAndNamesWhatIsAssumed(t *testing.T) {
	out, err := runCLI(freezeArgs(writeSpec(t, freezeSpecText))...)
	require.NoError(t, err)
	var f frozenFile
	require.NoError(t, json.Unmarshal([]byte(out), &f))
	require.Len(t, f.Buckets, 1)
	b := f.Buckets[0]
	assert.Empty(t, b.Refused)
	assert.Positive(t, b.Knee)
	assert.InDelta(t, 0.85*b.Knee, b.OperatingRate, 1e-9)
	assert.Contains(t, strings.Join(b.Assumed, ";"), "--assume-vp")
	assert.NotContains(t, strings.Join(b.Assumed, ";"), "prompt cv", "the spec settles the spread")
	assert.NotContains(t, strings.Join(b.Assumed, ";"), "Poisson arrivals", "the driver sends them")
	require.Len(t, b.Targets, 2)
	for _, tg := range b.Targets {
		assert.GreaterOrEqual(t, tg.Seconds, 1.2*tg.Floor, "the margin holds a target off the floor")
		assert.Positive(t, tg.PredictedOff)
		assert.Positive(t, tg.PredictedOn)
	}
	require.Len(t, b.Curve, 3)
	assert.Positive(t, b.Curve[0].P90)
	assert.NotEmpty(t, b.Curve[2].Refused, "a step past the knee is a refusal in the curve")
	assert.Equal(t, 64, len(f.RecordingSHA256))
}

func TestFreeze_AMarginAboveThePricedFigureRaisesTheTarget(t *testing.T) {
	high := strings.Replace(freezeSpecText, "targetFloorMargin: 1.2", "targetFloorMargin: 1.9", 1)
	out, err := runCLI(freezeArgs(writeSpec(t, high))...)
	require.NoError(t, err)
	var f frozenFile
	require.NoError(t, json.Unmarshal([]byte(out), &f))
	raised := 0
	for _, tg := range f.Buckets[0].Targets {
		assert.GreaterOrEqual(t, tg.Seconds, 1.9*tg.Floor)
		if tg.Source == "floor margin" {
			raised++
			assert.InDelta(t, 1.9*tg.Floor, tg.Seconds, 1e-12)
		}
	}
	assert.Positive(t, raised, "the case must exercise the margin")
}

func TestFreeze_NeverReplacesAFrozenFile(t *testing.T) {
	spec := writeSpec(t, freezeSpecText)
	path := filepath.Join(t.TempDir(), "frozen.json")
	_, err := runCLI(freezeArgs(spec, "-o", path)...)
	require.NoError(t, err)
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	_, err = runCLI(freezeArgs(spec, "-o", path)...)
	require.Error(t, err)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestFreeze_MeasuredCa2HoldsTheFrozenTargets(t *testing.T) {
	spec := writeSpec(t, freezeSpecText)
	path := filepath.Join(t.TempDir(), "frozen.json")
	_, err := runCLI(freezeArgs(spec, "-o", path)...)
	require.NoError(t, err)
	out, err := runCLI(freezeArgs(spec, "--targets", path, "--ca2", "A=4")...)
	require.NoError(t, err)
	var first, redo frozenFile
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &first))
	require.NoError(t, json.Unmarshal([]byte(out), &redo))
	for i, tg := range redo.Buckets[0].Targets {
		assert.Equal(t, first.Buckets[0].Targets[i].Seconds, tg.Seconds)
		assert.Equal(t, "frozen file", tg.Source)
		assert.Less(t, tg.PredictedOff, first.Buckets[0].Targets[i].PredictedOff, "bursts lower the rate at one target")
	}
	assert.InDelta(t, 4, redo.Buckets[0].Ca2, 0)
}

func TestFreeze_ARefusalIsInTheFileAndExits3(t *testing.T) {
	// Run 4 learns a and refuses V_P: with no --assume-vp the bucket has no prediction.
	spec := writeSpec(t, freezeSpecText)
	out, err := runCLI("freeze", "--recording", shortRun, "--spec", spec)
	require.Error(t, err)
	assert.Equal(t, 3, exitCode(err))
	var f frozenFile
	require.NoError(t, json.Unmarshal([]byte(out), &f))
	assert.Contains(t, f.Buckets[0].Refused, "V_P")
	assert.False(t, f.Gate.VP)
}

func TestFreeze_RefusesABadSpec(t *testing.T) {
	cases := map[string]string{
		"operatingFraction": strings.Replace(freezeSpecText, "operatingFraction: 0.85", "operatingFraction: 1.5", 1),
		"targetFloorMargin": strings.Replace(freezeSpecText, "targetFloorMargin: 1.2", "targetFloorMargin: 0.9", 1),
		"percentile":        strings.Replace(freezeSpecText, "[90, 95]", "[90, 100]", 1),
		"prefill":           strings.Replace(freezeSpecText, "prefill: 1", "prefill: 0", 1),
		"prompt":            strings.Replace(freezeSpecText, "prompt: 1000", "prompt: 0", 1),
		"name":              strings.Replace(freezeSpecText, "name: A", `name: ""`, 1),
	}
	for word, body := range cases {
		_, err := runCLI(freezeArgs(writeSpec(t, body))...)
		require.Error(t, err, word)
		assert.Equal(t, 2, exitCode(err), word)
		assert.Contains(t, err.Error(), word)
	}
}

func TestFreeze_NamesNoClockOrPath(t *testing.T) {
	out, err := runCLI(freezeArgs(writeSpec(t, freezeSpecText))...)
	require.NoError(t, err)
	assert.NotContains(t, out, "recording.jsonl")
	assert.NotContains(t, out, "namespace")
	assert.NotContains(t, out, "\"at\"")
}
