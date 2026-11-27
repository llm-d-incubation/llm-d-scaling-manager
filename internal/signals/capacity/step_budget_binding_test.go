package capacity

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// stepHistogram is a cumulative tokens-per-step histogram on vLLM's default
// bounds (1 to 16384, then +Inf), steps a second, from the steps a second in
// each bucket, lowest first: per[i] is the steps in (bound[i-1], bound[i]].
func stepHistogram(per ...float64) []domain.LengthBucket {
	bounds := []float64{1, 8, 16, 32, 64, 128, 256, 512, 1024, 2048, 4096, 8192, 16384, math.Inf(1)}
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

// The three classes, from the iterations alone, against a budget of 8192:
// the near bound is 4096, the largest bound under 90% of it.
func TestReadStepBudgetBinding_TheClasses(t *testing.T) {
	// A chunked-prefill pool under load: 30% of its iterations above 4096.
	binds := stepHistogram(0, 0, 0, 0, 10, 20, 10, 5, 5, 10, 10, 30)
	// A decode-heavy pool: 0.2% of its steps above 4096, 0.04% above 8192
	// (the aggregated DEV day's shape).
	free := stepHistogram(0, 0, 0, 0, 40, 30, 15, 8, 4, 2, 0.76, 0.2, 0.03, 0.01)
	for _, c := range []struct {
		name   string
		steps  []domain.LengthBucket
		budget int64
		class  StepBudgetClass
		reason string
		share  float64
	}{
		{"binds", binds, 8192, StepBudgetBinds, "", 0.3},
		{"does not bind", free, 8192, StepBudgetDoesNotBind, "", 0.0024},
		{"no series", nil, 8192, StepBudgetUnknown, StepBudgetNoSeries, 0},
		{"no step counted", stepHistogram(), 8192, StepBudgetUnknown, StepBudgetNoSteps, 0},
		{"budget not known", binds, 0, StepBudgetUnknown, StepBudgetNotKnown, 0},
		{"budget under every bound", binds, 1, StepBudgetUnknown, StepBudgetTooCoarse, 0},
		// Iterations count a chunked prompt whole when it completes: 40% of
		// them above a budget of 2048 are prompts the budget chunked, a
		// binding budget, not a wrong one.
		{"iterations above the budget", binds, 2048, StepBudgetBinds, "", 0.5},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := ReadStepBudgetBinding(c.steps, c.budget)
			assert.Equal(t, c.class, got.Class)
			assert.Equal(t, c.reason, got.Reason)
			assert.InDelta(t, c.share, got.Share, 1e-9)
		})
	}
	assert.InDelta(t, 0.4, ReadStepBudgetBinding(binds, 2048).OverShare, 1e-9, "the chunked completions")
}

// The near bound, the share above the budget and the observed top are read
// as stated; the top is read with no budget known too.
func TestReadStepBudgetBinding_TheFigures(t *testing.T) {
	free := stepHistogram(0, 0, 0, 0, 40, 30, 15, 8, 4, 2, 0.76, 0.2, 0.03, 0.01)
	got := ReadStepBudgetBinding(free, 8192)
	assert.Equal(t, 4096.0, got.NearTokens)
	assert.InDelta(t, 0.0004, got.OverShare, 1e-9)
	assert.InDelta(t, 100.0, got.StepRate, 1e-9)
	assert.Equal(t, 8192.0, got.TopTokens, "99.9% of the steps at or under 8192")
	assert.False(t, got.TopOpen)

	open := stepHistogram(0, 0, 0, 0, 50, 0, 0, 0, 0, 0, 0, 0, 0, 50)
	got = ReadStepBudgetBinding(open, 0)
	assert.Equal(t, StepBudgetNotKnown, got.Reason)
	assert.Equal(t, 16384.0, got.TopTokens, "the last finite bound, a floor")
	assert.True(t, got.TopOpen)
}

// rate() extrapolates each bucket on its own: a bound that reads a hair
// under the one below is read as the one below, not as negative steps.
func TestReadStepBudgetBinding_NonMonotoneBuckets(t *testing.T) {
	steps := stepHistogram(0, 0, 0, 0, 10, 20, 10, 5, 5, 10, 10, 30)
	steps[9].Rate = steps[8].Rate - 0.01
	got := ReadStepBudgetBinding(steps, 8192)
	assert.Equal(t, StepBudgetBinds, got.Class)
	assert.InDelta(t, 0.3, got.Share, 1e-9)
}

// Pools add each bound's rate over the sets, and nothing where every set is
// empty; an unusable rate adds nothing.
func TestPoolStepTokens(t *testing.T) {
	a := []domain.LengthBucket{{UpperBound: 4096, Rate: 1}, {UpperBound: math.Inf(1), Rate: 2}}
	b := []domain.LengthBucket{{UpperBound: math.Inf(1), Rate: 3}, {UpperBound: 4096, Rate: math.NaN()}, {UpperBound: 1024, Rate: 0.5}}
	assert.Equal(t, []domain.LengthBucket{{UpperBound: 1024, Rate: 0.5}, {UpperBound: 4096, Rate: 1}, {UpperBound: math.Inf(1), Rate: 5}},
		PoolStepTokens(a, b))
	assert.Nil(t, PoolStepTokens(nil, []domain.LengthBucket{}))
}

// The budget is the engine's as resolved (StepBudget): the flag, else the
// release's default; a nil engine has none.
func TestEngineParams_StepBudgetBinding(t *testing.T) {
	steps := stepHistogram(0, 0, 0, 0, 10, 20, 10, 5, 5, 10, 10, 30)
	assert.Equal(t, StepBudgetBinds, (&EngineParams{MaxNumBatchedTokens: 8192}).StepBudgetBinding(steps).Class)
	assert.Equal(t, StepBudgetBinds, (&EngineParams{EffectiveMaxBatchedTokens: 8192}).StepBudgetBinding(steps).Class)
	var none *EngineParams
	assert.Equal(t, StepBudgetNotKnown, none.StepBudgetBinding(steps).Reason)
}
