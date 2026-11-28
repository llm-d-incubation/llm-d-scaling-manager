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
			s.AddAcceptance(steps, 1, 1)
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
	assert.Zero(t, got.PromptBoundTokens, "and the prompts' at 20000")

	mixed := fullPool(steps, promptHistogram(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 500, 0, 500))
	mixed.AddPrompts(stepHistogram(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 500, 0, 500), 1)
	require.True(t, mixed.PromptMixedBounds)
	got = mixed.BindingWith(8192, BindingOptions{Prompts: true})
	assert.Equal(t, StepBudgetDoesNotBind, got.Class)
	assert.Zero(t, got.PromptOverShare)
}

// A speculating decode pool, k = 3 and one draft token accepted a step: a
// step schedules 4 tokens a request and the series counts the 2 it emitted,
// so a step that fills the budget of 8192 counts 4096. Its iterations are
// read against the budget in emitted tokens, 8192 x 2 / 4 = 4096: 10% of
// them reach it, and the budget binds, where the share above 8192 read none.
// Unread, the acceptance is taken at k and the budget is read as it stands.
func TestStepTokens_SpeculatingDecodeReadsEmittedTokens(t *testing.T) {
	// 1000 iterations: 900 in (2048, 4096], 100 in (4096, 8192].
	steps := stepHistogram(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 900, 100)
	var s StepTokens
	s.Add(steps, 1)

	unread := s.BindingWith(8192, BindingOptions{SpeculativeTokens: 3})
	assert.Equal(t, s.Binding(8192), unread, "no acceptance read: the budget as it stands")
	assert.Equal(t, StepBudgetDoesNotBind, unread.Class)

	s.AddAcceptance(steps, 1, 1)
	got := s.BindingWith(8192, BindingOptions{SpeculativeTokens: 3})
	assert.Equal(t, StepBudgetBinds, got.Class)
	assert.InDelta(t, 4096, got.EmittedBudget, 1e-9)
	assert.InDelta(t, 1, got.Acceptance, 1e-9)
	assert.InDelta(t, 4096, got.BoundTokens, 1e-9)
	assert.InDelta(t, 0.1, got.OverShare, 1e-9)
	assert.Equal(t, int64(8192), got.Budget, "the budget is reported as the flags set it")

	// An acceptance read above k is held at k: the budget then stands.
	var over StepTokens
	over.Add(steps, 1)
	over.AddAcceptance(steps, 5, 1)
	got = over.BindingWith(8192, BindingOptions{SpeculativeTokens: 3})
	assert.InDelta(t, 3, got.Acceptance, 1e-9)
	assert.InDelta(t, 8192, got.EmittedBudget, 1e-9)
	assert.Equal(t, StepBudgetDoesNotBind, got.Class)
}

// The acceptance is weighted by the iterations each row counted, and a row
// with none, or a reading that is no count, adds nothing.
func TestStepTokens_AcceptanceIsIterationWeighted(t *testing.T) {
	var s StepTokens
	s.AddAcceptance(stepHistogram(0, 0, 0, 0, 300), 0.5, 1)
	s.AddAcceptance(stepHistogram(0, 0, 0, 0, 100), 2.5, 1)
	s.AddAcceptance(stepHistogram(), 9, 1)
	s.AddAcceptance(stepHistogram(0, 0, 0, 0, 100), math.NaN(), 1)
	s.AddAcceptance(stepHistogram(0, 0, 0, 0, 100), -1, 1)
	s.AddAcceptance(nil, 1, 1)
	assert.InDelta(t, 400, s.AcceptedIterations, 1e-9)
	assert.InDelta(t, 1.0, s.AcceptedSum/s.AcceptedIterations, 1e-9)
}

// Clone copies the prompts too.
func TestStepTokens_CloneCopiesThePrompts(t *testing.T) {
	s := fullPool(bindsHistogram(), promptHistogram(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 500, 0, 500))
	c := s.Clone()
	c.Prompts[0].Rate = 7
	assert.Zero(t, s.Prompts[0].Rate)
}
