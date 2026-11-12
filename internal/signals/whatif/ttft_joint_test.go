package whatif

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// jointEstimate is the staircase's bucket B as the joint window reads it
// (docs/proposals/slo-capacity-planner-ttft.md, "First, does the model
// explain the split?").
func jointEstimate() prefill.JointEstimate {
	return prefill.JointEstimate{
		PrefillWork: 0.025, PrefillWorkPrompt: 8000, VP: 8000 / 0.025, PrefillWorkSamples: 40,
		Overhead: 0.014, OverheadSamples: 40,
		Pull: []prefill.PullBucket{
			{Utilization: 0.2, Seconds: 0.0244, PromptTokens: 8000, Samples: 10},
			{Utilization: 0.3, Seconds: 0.0246, PromptTokens: 8000, Samples: 10},
			{Utilization: 0.5, Seconds: 0.0250, PromptTokens: 8000, Samples: 10},
		},
		ITLAlpha: 0.00209, ITLBeta: 0.000218, ITLSamples: 40,
		DecodeSteps: 5.18, DecodeFixed: 0.0115, DecodeSamples: 40, OutputTokens: 200,
	}
}

// Each part is its stage at its own pods' load: the prefill pod and the pull
// as processor sharing, the decode batch at the closed loop's k.
func TestJointTTFTMean_TheParts(t *testing.T) {
	est := jointEstimate()
	got, refused := JointTTFTMean(est, JointQuestion{PrefillReplicas: 2, DecodeReplicas: 2, Rate: 30, HopSeconds: 0.01})
	require.Empty(t, refused)

	rho := 15 * 0.025 // 0.375, the bucket from 0.3
	assert.InDelta(t, rho, got.PrefillUtilization, 1e-12)
	assert.InDelta(t, 0.014/(1-rho), got.Overhead, 1e-12, "a(rho) = a0/(1 - rho)")
	assert.InDelta(t, 0.025/(1-rho), got.Prefill, 1e-12, "T_P = S/(1 - rho)")
	assert.InDelta(t, 0.0246, got.PullWork, 1e-12)
	assert.InDelta(t, 15*0.0246, got.PullUtilization, 1e-12, "the pull at lambda/N_d")
	assert.InDelta(t, 0.0246/(1-15*0.0246), got.Pull, 1e-12)
	loop := 200 * 15 * 0.000218
	k := 200 * 15 * 0.00209 / (1 - loop)
	assert.InDelta(t, k, got.Batch, 1e-9, "k = n_out*lambda_d*alpha / (1 - n_out*lambda_d*beta)")
	assert.InDelta(t, 0.00209+0.000218*k, got.ITL, 1e-12)
	assert.InDelta(t, 5.18*got.ITL+0.0115, got.Decode, 1e-12)
	assert.InDelta(t, got.Overhead+got.Prefill+got.Pull+got.Decode, got.Engine, 1e-15)
	assert.InDelta(t, got.Engine+0.01, got.Seconds, 1e-15)
	assert.Equal(t, []Flag{OverheadMeasured}, got.Flags)
	// With these rounded inputs the engines read 144.2 ms; b-off's step at
	// 30.5 r/s on 2+2 measured 148.4 ms there.
	assert.InDelta(t, 0.1442, got.Engine, 0.0005)
}

// The pull table: below the lowest bucket the lowest, in a gap the next one
// up (both read high, since the pull grows with the prefill load), past the
// highest the highest, flagged PullUnseen (which reads low).
func TestJointTTFTMean_ThePullTable(t *testing.T) {
	est := jointEstimate()
	for _, c := range []struct {
		rate   float64
		work   float64
		unseen bool
	}{
		{rate: 4, work: 0.0244},  // rho 0.05: below the lowest
		{rate: 18, work: 0.0244}, // rho 0.225: the bucket from 0.2
		{rate: 34, work: 0.0250}, // rho 0.425: no bucket 0.4, the next one up
		{rate: 52, work: 0.0250, unseen: true},
	} {
		got, refused := JointTTFTMean(est, JointQuestion{PrefillReplicas: 2, DecodeReplicas: 4, Rate: c.rate})
		require.Empty(t, refused, "rate %v", c.rate)
		assert.InDelta(t, c.work, got.PullWork, 1e-12, "rate %v", c.rate)
		assert.Equal(t, c.unseen, got.HasFlag(PullUnseen), "rate %v", c.rate)
	}
}

// The prompt asked about scales the prefill work and the pull, each from the
// prompt it was read at.
func TestJointTTFTMean_ScalesWithThePrompt(t *testing.T) {
	est := jointEstimate()
	at, _ := JointTTFTMean(est, JointQuestion{PrefillReplicas: 4, DecodeReplicas: 4, Rate: 10})
	twice, _ := JointTTFTMean(est, JointQuestion{PrefillReplicas: 4, DecodeReplicas: 4, Rate: 10, PromptTokens: 16000})
	assert.InDelta(t, 2*at.PrefillUtilization, twice.PrefillUtilization, 1e-12)
	assert.InDelta(t, 2*at.PullWork, twice.PullWork, 1e-12)
	assert.Equal(t, at.Decode, twice.Decode, "the decode pod's own part does not read the prompt")
}

// More rate never lowers the TTFT, and more pods of either role never raise it.
func TestJointTTFTMean_Monotone(t *testing.T) {
	est := jointEstimate()
	price := func(np, nd int, rate float64) float64 {
		got, refused := JointTTFTMean(est, JointQuestion{PrefillReplicas: np, DecodeReplicas: nd, Rate: rate})
		require.Empty(t, refused, "%d+%d at %v", np, nd, rate)
		return got.Seconds
	}
	for rate := 1.0; rate < 36; rate++ {
		assert.LessOrEqual(t, price(2, 2, rate), price(2, 2, rate+1), "rate %v", rate)
		assert.GreaterOrEqual(t, price(2, 2, rate), price(3, 2, rate), "prefill pods at %v", rate)
		assert.GreaterOrEqual(t, price(2, 2, rate), price(2, 3, rate), "decode pods at %v", rate)
	}
}

// Each refusal: a missing input, no pods, a stage at its knee, a batch the
// closed loop cannot hold.
func TestJointTTFTMean_Refusals(t *testing.T) {
	q := JointQuestion{PrefillReplicas: 2, DecodeReplicas: 2, Rate: 20}
	for _, c := range []struct {
		name string
		edit func(*prefill.JointEstimate, *JointQuestion)
		want JointRefusal
	}{
		{"no prefill pods", func(_ *prefill.JointEstimate, q *JointQuestion) { q.PrefillReplicas = 0 }, JointNoReplicas},
		{"no decode pods", func(_ *prefill.JointEstimate, q *JointQuestion) { q.DecodeReplicas = 0 }, JointNoReplicas},
		{"a negative rate", func(_ *prefill.JointEstimate, q *JointQuestion) { q.Rate = -1 }, JointInvalidInput},
		{"no work", func(e *prefill.JointEstimate, _ *JointQuestion) { e.PrefillWorkRefused = prefill.NoQueueFreeMinutes },
			JointNoPrefillCapacity},
		{"no a0", func(e *prefill.JointEstimate, _ *JointQuestion) { e.OverheadRefused = prefill.NoOverheadReadings }, JointNoOverhead},
		{"no pull", func(e *prefill.JointEstimate, _ *JointQuestion) { e.Pull = nil }, JointNoPull},
		{"no line", func(e *prefill.JointEstimate, _ *JointQuestion) { e.ITLRefused = prefill.NoITLLine }, JointNoITLLine},
		{"no own line", func(e *prefill.JointEstimate, _ *JointQuestion) { e.DecodeRefused = prefill.NoDecodeOwnLine },
			JointNoDecodeOwnLine},
		{"no output length", func(e *prefill.JointEstimate, _ *JointQuestion) { e.OutputTokens = 0 }, JointNoOutputLength},
		// 2 pods at S 25 ms hold 80 r/s.
		{"the prefill knee", func(_ *prefill.JointEstimate, q *JointQuestion) { q.Rate = 80 }, JointSaturated},
		// rho exactly 1: one pod at S 250 ms and 4 r/s.
		{"rho at 1", func(e *prefill.JointEstimate, q *JointQuestion) {
			e.PrefillWork, q.PrefillReplicas, q.Rate = 0.25, 1, 4
		}, JointSaturated},
		{"a negative prompt", func(_ *prefill.JointEstimate, q *JointQuestion) { q.PromptTokens = -1 }, JointInvalidInput},
		{"a hop that is not a number", func(_ *prefill.JointEstimate, q *JointQuestion) { q.HopSeconds = math.NaN() },
			JointInvalidInput},
		{"the pull's knee", func(e *prefill.JointEstimate, _ *JointQuestion) {
			e.Pull = []prefill.PullBucket{{Utilization: 0, Seconds: 0.1, PromptTokens: 8000, Samples: 4}}
		}, JointSaturated},
		// n_out * lambda_d * beta = 200 * 10 * 0.0005 = 1.
		{"the batch's knee", func(e *prefill.JointEstimate, _ *JointQuestion) { e.ITLBeta = 0.0005 }, JointBatchUnheld},
		// Lines the window refuses, given anyway: no step takes no time, and the
		// decode pod's own part is not negative.
		{"a negative ITL", func(e *prefill.JointEstimate, _ *JointQuestion) { e.ITLAlpha = -0.01 }, JointNoITLLine},
		{"a negative own part", func(e *prefill.JointEstimate, _ *JointQuestion) { e.DecodeFixed = -1 },
			JointNoDecodeOwnLine},
	} {
		est, qq := jointEstimate(), q
		c.edit(&est, &qq)
		got, refused := JointTTFTMean(est, qq)
		assert.Equal(t, c.want, refused, c.name)
		assert.Zero(t, got.Seconds, c.name)
	}
}
