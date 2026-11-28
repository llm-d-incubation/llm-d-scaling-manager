package capacity

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// promptHistogram is a cumulative computed-prompt histogram on vLLM's 1-2-5
// bounds (1 to 20000, then +Inf), from the prompts in each bucket, lowest
// first.
func promptHistogram(per ...float64) []domain.LengthBucket {
	bounds := []float64{1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10000, 20000, math.Inf(1)}
	out := make([]domain.LengthBucket, len(bounds))
	running := 0.0
	for i, b := range bounds {
		if i < len(per) {
			running += per[i]
		}
		out[i] = domain.LengthBucket{UpperBound: b, Rate: running}
	}
	return out
}

// fullPool is a pool of one row standing for one second, its iterations and
// its computed prompts.
func fullPool(steps, prompts []domain.LengthBucket) StepTokens {
	var s StepTokens
	s.Add(steps, 1)
	s.AddPrompts(prompts, 1)
	return s
}

// With no option, BindingWith is Binding: every figure the same, on every
// pool the class tests read.
func TestStepTokens_BindingWithNoOptionIsBinding(t *testing.T) {
	for _, steps := range [][]domain.LengthBucket{bindsHistogram(), freeHistogram(), nil, stepHistogram(),
		stepHistogram(0, 0, 0, 0, 10, 20, 10, 5, 5, 10, 10, 20, 6, 4)} {
		for _, budget := range []int64{0, 2048, 8192, 40000} {
			s := fullPool(steps, promptHistogram(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 500, 0, 500))
			assert.Equal(t, s.Binding(budget), s.BindingWith(budget, BindingOptions{}))
		}
	}
}

// The limit the series cannot see past: prompts of 2000-5000 tokens, each
// under the budget of 8192, arriving together so that every step is full.
// The scheduler chunks the last prompt it admits each step, and the
// iteration that completes it counts the prompt whole, under the budget; the
// iterations that only ran chunks are not counted. No iteration counts more
// than 8192 and no prompt is longer than it, so both readings say the budget
// does not bind although it binds every step. The class is not evidence that
// a budget is free (step_budget_binding.go).
func TestStepTokens_ConcurrentPromptsUnderTheBudgetReadDoesNotBind(t *testing.T) {
	// 1000 iterations, each completing one or two prompts: all in (4096, 8192].
	steps := stepHistogram(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1000)
	// 1000 prompts in (2000, 5000].
	prompts := promptHistogram(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1000)
	got := fullPool(steps, prompts).BindingWith(8192, BindingOptions{Prompts: true})
	assert.Equal(t, StepBudgetDoesNotBind, got.Class)
	assert.Zero(t, got.OverShare)
	assert.Zero(t, got.PromptOverShare)
	assert.InDelta(t, 1000, got.Prompts, 1e-9, "the prompts were read")
	assert.InDelta(t, 10000.0, got.PromptBoundTokens, 1e-9)
}

// An aggregated pod where half the computed prompts are longer than the
// budget: each completes in one iteration and then decodes for hundreds, so
// the iterations above the budget are 0.5% of them and the iteration share
// reads does not bind. The prompts read binds: every one above 10000 tokens
// was chunked.
func TestStepTokens_PromptsOverTheBudgetBindOnAnAggregatedPod(t *testing.T) {
	// 100000 iterations, 500 of them above 8192 (the long prompts' last
	// chunks), the rest decode steps of 64-128 tokens.
	steps := stepHistogram(0, 0, 0, 0, 0, 99000, 0, 0, 0, 0, 500, 0, 500)
	// 1000 prompts: 500 in (2000, 5000], 500 in (10000, 20000].
	prompts := promptHistogram(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 500, 0, 500)
	s := fullPool(steps, prompts)

	old := s.Binding(8192)
	assert.Equal(t, StepBudgetDoesNotBind, old.Class, "the iterations alone are diluted")
	assert.InDelta(t, 0.005, old.OverShare, 1e-9)
	assert.Zero(t, old.PromptOverShare, "Binding reads no prompts")

	got := s.BindingWith(8192, BindingOptions{Prompts: true})
	assert.Equal(t, StepBudgetBinds, got.Class)
	assert.InDelta(t, 0.005, got.OverShare, 1e-9, "the iteration share is unchanged")
	assert.InDelta(t, 0.5, got.PromptOverShare, 1e-9)
	assert.InDelta(t, 10000.0, got.PromptBoundTokens, 1e-9)
	assert.InDelta(t, 1000, got.Prompts, 1e-9)
}

// The prompt reading needs StepBudgetMinPrompts prompts and a finite bound at
// or over the budget, and a pool whose prompts are on mixed bounds reads
// none; the iterations still decide the class.
func TestStepTokens_PromptReadingNeedsEnoughOnOneBound(t *testing.T) {
	steps := freeHistogram()
	few := fullPool(steps, promptHistogram(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 100, 0, 299))
	got := few.BindingWith(8192, BindingOptions{Prompts: true})
	assert.Equal(t, StepBudgetDoesNotBind, got.Class, "399 prompts are too few")
	assert.Zero(t, got.Prompts)
	assert.Zero(t, got.PromptOverShare)

	coarse := fullPool(steps, promptHistogram(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 500, 0, 500))
	got = coarse.BindingWith(40000, BindingOptions{Prompts: true})
	assert.Equal(t, StepBudgetUnknown, got.Class, "the iterations' bounds stop at 16384")
	assert.Equal(t, StepBudgetTooCoarse, got.Reason)
	assert.Zero(t, got.PromptBoundTokens, "and the prompts' at 20000")

	mixed := fullPool(steps, promptHistogram(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 500, 0, 500))
	mixed.AddPrompts(stepHistogram(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 500, 0, 500), 1)
	require.True(t, mixed.PromptMixedBounds)
	got = mixed.BindingWith(8192, BindingOptions{Prompts: true})
	assert.Equal(t, StepBudgetDoesNotBind, got.Class)
	assert.Zero(t, got.PromptOverShare)
}

// A budget over 16384, the iterations' last bound, as a prefill pod with
// --max-num-batched-tokens 18000 runs: no iteration can be told over the
// budget from under it. The prompts' 1-2-5 bounds follow max_model_len and
// still have one at or over the budget (20000), so where they bind the class
// binds on them alone; where they do not, the class stays unknown, too coarse,
// since the concurrent case the prompts cannot see is not ruled out either.
// Binding, which reads no prompts, is unknown in both.
func TestStepTokens_PromptsBindPastTheIterationsLastBound(t *testing.T) {
	steps := bindsHistogram()
	long := fullPool(steps, promptHistogram(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 500, 0, 0, 500))
	got := long.BindingWith(18000, BindingOptions{Prompts: true})
	assert.Equal(t, StepBudgetBinds, got.Class, "half the prompts are over 20000")
	assert.Empty(t, got.Reason)
	assert.InDelta(t, 20000.0, got.PromptBoundTokens, 1e-9)
	assert.InDelta(t, 0.5, got.PromptOverShare, 1e-9)
	assert.Zero(t, got.BoundTokens, "no iteration bound at or over the budget")
	assert.Zero(t, got.OverShare)
	assert.InDelta(t, 8192.0, got.NearTokens, 1e-9, "the near bound is still read")

	old := long.Binding(18000)
	assert.Equal(t, StepBudgetUnknown, old.Class)
	assert.Equal(t, StepBudgetTooCoarse, old.Reason)

	short := fullPool(steps, promptHistogram(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 990, 0, 0, 10))
	got = short.BindingWith(18000, BindingOptions{Prompts: true})
	assert.Equal(t, StepBudgetUnknown, got.Class, "1% of prompts over 20000: under the share")
	assert.Equal(t, StepBudgetTooCoarse, got.Reason)
	assert.InDelta(t, 0.01, got.PromptOverShare, 1e-9, "the prompts were read")
}

// The prompt share binds from StepBudgetPromptBindsShare, its own threshold:
// just under it does not bind, at it binds.
func TestStepTokens_PromptShareThreshold(t *testing.T) {
	steps := freeHistogram()
	n := 1000.0
	over := StepBudgetPromptBindsShare * n
	at := fullPool(steps, promptHistogram(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, n-over, 0, over))
	assert.Equal(t, StepBudgetBinds, at.BindingWith(8192, BindingOptions{Prompts: true}).Class)
	under := fullPool(steps, promptHistogram(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, n-over+1, 0, over-1))
	assert.Equal(t, StepBudgetDoesNotBind, under.BindingWith(8192, BindingOptions{Prompts: true}).Class)
}

// Clone copies the prompts too.
func TestStepTokens_CloneCopiesThePrompts(t *testing.T) {
	s := fullPool(bindsHistogram(), promptHistogram(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 500, 0, 500))
	c := s.Clone()
	c.Prompts[0].Rate = 7
	assert.Zero(t, s.Prompts[0].Rate)
}
