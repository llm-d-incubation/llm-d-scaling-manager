package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// stairBOff is the staircase's bucket B, flow control off: 2+2, 8000-token
// prompts, Poisson arrivals.
const stairBOff = "../../internal/engines/analyzers/learnreplay/testdata/goldens/ttft-staircase-b-off/recording.jsonl.gz"

// jointJSON runs joint-ttft with -o json and decodes its answer: exit 0
// with a mean, 3 with a refusal.
func jointJSON(t *testing.T, args ...string) jointAnswer {
	t.Helper()
	out, code := runCmd(t, append([]string{"joint-ttft", "--recording", stairBOff, "-o", "json"}, args...)...)
	var a jointAnswer
	require.NoError(t, json.Unmarshal([]byte(out), &a), out)
	want := 0
	if a.Refused != "" {
		want = 3
	}
	require.Equal(t, want, code, out)
	return a
}

// The JSON's keys are lowerCamel throughout, schemaVersion's included.
func TestJointTTFT_JSONKeys(t *testing.T) {
	out, code := runCmd(t, "joint-ttft", "--recording", stairBOff, "--cycle", "120", "-o", "json")
	require.Equal(t, 0, code, out)
	var raw map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &raw))
	var check func(prefix string, v any)
	check = func(prefix string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				assert.Regexp(t, `^[a-z][A-Za-z0-9]*$`, k, "%s.%s", prefix, k)
				check(prefix+"."+k, c)
			}
		case []any:
			for _, c := range x {
				check(prefix+"[]", c)
			}
		}
	}
	check("", raw)
	assert.Contains(t, raw, "mean")
	assert.Contains(t, raw["inputs"], "itlBetaSeconds")
}

// At the fleet that ran, at B's third step (30.5 r/s), the joint mean prices
// what the engines measured there, about 148 ms, plus the assumed hop.
func TestJointTTFT_TheFleetThatRan(t *testing.T) {
	a := jointJSON(t, "--cycle", "120")
	require.NotNil(t, a.Mean, a.Refused)
	assert.Equal(t, 2, a.PrefillReplicas)
	assert.Equal(t, 2, a.DecodeReplicas)
	assert.Equal(t, a.RanRate, a.Rate)
	assert.InDelta(t, 0.148, a.Mean.Engine, 0.010)
	assert.InDelta(t, whatif.AssumedRouterHopSeconds, a.Mean.Hop, 1e-12)
	assert.Contains(t, a.Mean.Flags, whatif.RouterHopAssumed)
	assert.Contains(t, a.Mean.Flags, whatif.OverheadMeasured)
	assert.Equal(t, jointSchemaVersion, a.SchemaVersion)
}

// Another fleet: a third pod of either role lowers the mean; a rate past the
// prefill pods' knee is refused.
func TestJointTTFT_AnotherFleet(t *testing.T) {
	ran := jointJSON(t, "--cycle", "120")
	require.NotNil(t, ran.Mean)
	for _, more := range [][]string{{"--prefill", "3"}, {"--decode", "3"}} {
		a := jointJSON(t, append([]string{"--cycle", "120"}, more...)...)
		require.NotNil(t, a.Mean, more)
		assert.Less(t, a.Mean.Seconds, ran.Mean.Seconds, more)
	}
	hop := jointJSON(t, "--cycle", "120", "--hop", "0.01")
	require.NotNil(t, hop.Mean)
	assert.InDelta(t, 0.01, hop.Mean.Hop, 1e-12)
	assert.NotContains(t, hop.Mean.Flags, whatif.RouterHopAssumed)

	past := jointJSON(t, "--cycle", "120", "--rate", "90")
	assert.Nil(t, past.Mean)
	assert.Equal(t, whatif.JointSaturated, past.Refused)
}

// Before any input is learned the answer is a refusal, and the text says why.
func TestJointTTFT_RefusedAndText(t *testing.T) {
	a := jointJSON(t, "--cycle", "1")
	assert.Nil(t, a.Mean)
	assert.Equal(t, whatif.JointNoPrefillCapacity, a.Refused)
	out, code := runCmd(t, "joint-ttft", "--recording", stairBOff, "--cycle", "1")
	assert.Equal(t, 3, code, "a refusal exits 3: %s", out)
	assert.Contains(t, out, "refused: "+whatif.JointNoPrefillCapacity)

	out, code = runCmd(t, "joint-ttft", "--recording", stairBOff, "--cycle", "120")
	require.Equal(t, 0, code, out)
	assert.Contains(t, out, "mean TTFT")
	assert.Contains(t, out, "UNVALIDATED")

	_, code = runCmd(t, "joint-ttft")
	assert.Equal(t, 2, code, "no recording")
	_, code = runCmd(t, "joint-ttft", "--recording", stairBOff, "--rate", "-1")
	assert.Equal(t, 2, code, "a negative rate")
}
