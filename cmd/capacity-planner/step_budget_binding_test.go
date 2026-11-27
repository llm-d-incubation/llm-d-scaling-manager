package main

import (
	"fmt"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifsource/promtest"
)

// withIterationTokens adds to the sweep's window each pod's tokens-per-
// iteration histogram, iterations a second in each bucket of vLLM's default
// bounds, lowest first, as the collector's query reads it.
func withIterationTokens(t *testing.T, srv *promtest.Server, per ...float64) {
	t.Helper()
	q := sweepQueries(t, inferenceengine.EngineVLLM)
	bounds := []float64{1, 8, 16, 32, 64, 128, 256, 512, 1024, 2048, 4096, 8192, 16384, math.Inf(1)}
	for j := range sweepPods {
		running := 0.0
		for i, b := range bounds {
			if i < len(per) {
				running += per[i]
			}
			le := "+Inf"
			if !math.IsInf(b, 1) {
				le = strconv.FormatFloat(b, 'f', -1, 64)
			}
			rate := running
			srv.Add(q(registration.QueryIterationTokensBuckets), promtest.Series{Labels: map[string]string{
				"model_name": "m", "pod": fmt.Sprintf("m-dec-7c9d-%d", j), "instance": fmt.Sprintf("10.0.0.%d:8000", j+1),
				"namespace": sweepNS, "le": le},
				Value: func(ts time.Time) (float64, bool) {
					_, ok := sweepStepOf(ts)
					return rate, ok
				}})
		}
	}
}

// The step budget's binding reaches the answer from the window itself: the
// collector reads the tokens per iteration into the capture, the replay pools
// them, and the engine the inputs name carries the class against the budget
// the engine arguments set. A window with no such series carries none, so an
// older recording's answer reads as before.
func TestStepBudgetBinding_FromTheWindowToTheAnswer(t *testing.T) {
	flags := []string{"--accelerator", "NVIDIA-H200", "--vllm-version", "0.34", "--engine-args", "--max-num-batched-tokens=8192"}
	for _, c := range []struct {
		name  string
		per   []float64
		class capacity.StepBudgetClass
		share float64
	}{
		{"binds", []float64{0, 0, 0, 0, 10, 20, 10, 5, 5, 10, 10, 30}, capacity.StepBudgetBinds, 0.3},
		{"does not bind", []float64{0, 0, 0, 0, 40, 30, 15, 8, 4, 2, 0.76, 0.2, 0.03, 0.01}, capacity.StepBudgetDoesNotBind, 0.0024},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := sweepServer(t)
			withIterationTokens(t, srv, c.per...)
			e := engineOfAnswer(t, srv, flags...)
			require.NotNil(t, e.StepBudgetBinding, "no step budget binding in the inputs' engine")
			assert.Equal(t, string(c.class), e.StepBudgetBinding.Class)
			assert.InDelta(t, c.share, e.StepBudgetBinding.Share, 1e-6)
			assert.Equal(t, 4096.0, e.StepBudgetBinding.NearTokens)
		})
	}
	t.Run("no series", func(t *testing.T) {
		assert.Nil(t, engineOfAnswer(t, sweepServer(t), flags...).StepBudgetBinding)
	})
}

// The engine row names the class, its share and the observed top, and says
// it is not priced.
func TestStepBudgetBindingText(t *testing.T) {
	for _, c := range []struct {
		b    whatifapi.StepBudgetBinding
		want string
	}{
		{whatifapi.StepBudgetBinding{Class: "binds", NearTokens: 4096, Share: 0.21, TopTokens: 16384, TopOpen: true},
			"binds (21% of iterations above 4096 tokens; iterations above the top bound, 16384 tokens)"},
		{whatifapi.StepBudgetBinding{Class: "does-not-bind", NearTokens: 128, Share: 0, TopTokens: 32},
			"does not bind (0% of iterations above 128 tokens; 99.9% of iterations at or under 32 tokens)"},
		{whatifapi.StepBudgetBinding{Class: "unknown", Reason: capacity.StepBudgetNotKnown, TopTokens: 8192},
			"unknown (step budget not known; 99.9% of iterations at or under 8192 tokens)"},
	} {
		assert.Equal(t, c.want, stepBudgetBindingText(&c.b))
	}
	e := &whatifapi.EngineInfo{Summary: "vLLM 0.34", StepBudget: 8192, StepBudgetSource: engineSetByFlag,
		StepBudgetBinding: &whatifapi.StepBudgetBinding{Class: "binds", NearTokens: 4096, Share: 0.3, TopTokens: 8192}}
	assert.Contains(t, engineInfoText(e), "; step budget binding binds (30% of iterations above 4096 tokens; 99.9% of iterations at or under 8192 tokens), "+notPricedYetWording)
	e.StepBudgetBinding = nil
	assert.NotContains(t, engineInfoText(e), "binding")
}
