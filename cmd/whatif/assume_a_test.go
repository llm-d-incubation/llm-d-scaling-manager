package main

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// A fleet of one prompt length gives P1 no fit of a, so it refuses
// NoFixedLatency: run 2 sent 30k and 37k prompts under load. --assume-a gives
// it a flagged answer, and the refusal says so.
func TestAssumeA_AnswersWhatARefusalLeftOpen(t *testing.T) {
	q := []string{"replicas-to-slo", "--recording", saturatedRun, "--ttft-percentile", "95"}

	refused, code := runCmd(t, q...)
	require.Equal(t, 3, code, refused)
	assert.Contains(t, refused, "refused: NoFixedLatency")
	assert.Contains(t, refused, "pass --assume-a for a flagged answer")

	text, _ := runCmd(t, slices.Concat(q, []string{"--assume-a", "11ms"})...)
	assert.NotContains(t, text, "NoFixedLatency", text)
	assert.Contains(t, text, "end to end; UNVALIDATED; a ASSUMED")
	assert.Contains(t, text, "a 11.00 ms ASSUMED;", "no figure was learned to print beside it")
	assert.Contains(t, text, "a 11ms (--assume-a)")
	assert.Contains(t, text, string(assumedA))
	assert.Contains(t, text, " a ASSUMED.", "the closing note")

	out, _ := runCmd(t, slices.Concat(q, []string{"--assume-a", "11ms", "-o", "json"})...)
	w := decodeOutput(t, out)
	var priced bool
	for _, r := range w.Roles {
		if r.Role != rolePrefill {
			continue
		}
		priced = true
		require.NotNil(t, r.TTFT)
		assert.InDelta(t, 0.011, r.TTFT.FixedLatencySeconds, 1e-12)
		assert.Equal(t, shapeAssumed, r.TTFT.FixedLatencySource)
		assert.Nil(t, r.TTFT.FixedLatencyLearned)
		require.NotNil(t, r.Results.TTFT)
		require.NotNil(t, r.Results.TTFT.Answer, out)
		assert.Contains(t, r.Results.TTFT.Answer.Flags, string(assumedA))
	}
	assert.True(t, priced, out)
}

// With a learned figure the assumed one wins, as for V_P, and the learned one
// prints beside it. The answer moves by what the assumption moved.
func TestAssumeA_OverridesTheLearnedFigureAndPrintsIt(t *testing.T) {
	q := []string{"replicas-to-slo", "--recording", shortRun, "--assume-vp", "130000", "--ttft-percentile", "90"}
	ttftOf := func(args ...string) (whatif.Result, string) {
		t.Helper()
		out, _ := runCmd(t, slices.Concat(q, args, []string{"-o", "json"})...)
		for _, r := range decodeOutput(t, out).Roles {
			if r.Role == rolePrefill {
				return whatif.Answer{Value: r.Results.TTFT.Answer.Value}, out
			}
		}
		t.Fatal("no prefill role: " + out)
		return nil, ""
	}
	learned, _ := ttftOf()
	assumed, out := ttftOf("--assume-a", "111ms")

	w := decodeOutput(t, out)
	for _, r := range w.Roles {
		if r.Role != rolePrefill {
			continue
		}
		require.NotNil(t, r.TTFT.FixedLatencyLearned)
		a := *r.TTFT.FixedLatencyLearned
		assert.Greater(t, a, 0.0)
		// a enters the request's own latency only: one for one on the figure.
		assert.InDelta(t, 0.111-a, assumed.(whatif.Answer).Value-learned.(whatif.Answer).Value, 1e-3)
	}

	text, _ := runCmd(t, slices.Concat(q, []string{"--assume-a", "111ms"})...)
	assert.Contains(t, text, "a 111.00 ms ASSUMED, learned ")
	assert.Contains(t, text, "UNVALIDATED; V_P ASSUMED; a ASSUMED")
}

// --assume-a asks about TTFT, as every TTFT flag does on question 2, and with
// no target is a usage error on question 1.
func TestAssumeA_UsageAndAsking(t *testing.T) {
	text, _ := runCmd(t, "replicas-to-slo", "--recording", shortRun, "--assume-a", "11ms")
	assert.Contains(t, text, "TTFT p90", text)

	q2 := []string{"replicas-to-slo", "--recording", saturatedRun}
	q1 := []string{"slo-to-rate", "--recording", saturatedRun, "--tpot", "4ms"}
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"a of 0", append(slices.Clone(q2), "--assume-a", "0s"), "--assume-a"},
		{"a below 0", append(slices.Clone(q2), "--assume-a", "-1ms"), "--assume-a"},
		{"a with no unit", append(slices.Clone(q2), "--assume-a", "11"), "assume-a"},
		{"no target", append(slices.Clone(q1), "--assume-a", "11ms"), "need a --ttft target"},
		{"question 2 at the knee", append(slices.Clone(q2), "--assume-a", "11ms", "--headroom", "1"), "at the knee no TTFT exists"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := run(context.Background(), c.args, io.Discard, io.Discard)
			require.Error(t, err)
			assert.Equal(t, 2, exitCode(err), "%v", err)
			assert.True(t, strings.Contains(err.Error(), c.want), err.Error())
		})
	}
}

// The output with a assumed, pinned in text and in JSON on both questions.
func TestAssumeA_Golden(t *testing.T) {
	q2 := []string{"replicas-to-slo", "--recording", saturatedRun, "--ttft-percentile", "95", "--assume-a", "11ms"}
	q1 := []string{"slo-to-rate", "--recording", saturatedRun, "--ttft", "10s@p95", "--assume-a", "11ms"}
	checkGolden(t, "ttft-assume-a", 3, q2)
	checkGolden(t, "ttft-assume-a-json", 3, append(slices.Clone(q2), "-o", "json"))
	checkGolden(t, "ttft-assume-a-rate", 0, q1)
}
