package capacity

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// stepHistogram is a cumulative tokens-per-iteration histogram on vLLM's
// default bounds (1 to 16384, then +Inf), from the iterations in each bucket,
// lowest first: per[i] is the iterations in (bound[i-1], bound[i]].
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

// bindsHistogram is a chunked-prefill pool under load, 1000 iterations: 10%
// of them count more than 8192 tokens, 30% more than 4096.
func bindsHistogram() []domain.LengthBucket {
	return stepHistogram(0, 0, 0, 0, 100, 200, 100, 50, 50, 100, 100, 200, 60, 40)
}

// freeHistogram is a decode-heavy pool, 1000 iterations: 0.04% of them above
// 8192 (the aggregated DEV day's shape), 0.24% above 4096.
func freeHistogram() []domain.LengthBucket {
	return stepHistogram(0, 0, 0, 0, 400, 300, 150, 80, 40, 20, 7.6, 2, 0.3, 0.1)
}

// The classes, against a budget of 8192 unless stated: the class is the
// share of iterations above 8192, the smallest bound at or over the budget.
func TestReadStepBudgetBinding_TheClasses(t *testing.T) {
	for _, c := range []struct {
		name   string
		steps  []domain.LengthBucket
		budget int64
		class  StepBudgetClass
		reason string
		over   float64
	}{
		{"binds", bindsHistogram(), 8192, StepBudgetBinds, "", 0.1},
		{"does not bind", freeHistogram(), 8192, StepBudgetDoesNotBind, "", 0.0004},
		{"no series", nil, 8192, StepBudgetUnknown, StepBudgetNoSeries, 0},
		{"no iteration counted", stepHistogram(), 8192, StepBudgetUnknown, StepBudgetNoSteps, 0},
		// 100 iterations, every tenth above the budget: too few to read.
		{"too few iterations", stepHistogram(0, 0, 0, 0, 10, 20, 10, 5, 5, 10, 10, 20, 6, 4), 8192,
			StepBudgetUnknown, StepBudgetTooFew, 0},
		{"budget not known", bindsHistogram(), 0, StepBudgetUnknown, StepBudgetNotKnown, 0},
		// The largest finite bound is 16384: an iteration above 32768 cannot
		// be told from one under it.
		{"no finite bound at or over the budget", bindsHistogram(), 32768, StepBudgetUnknown, StepBudgetTooCoarse, 0},
		// A smaller budget chunks more: 40% of the iterations above 2048.
		{"a smaller budget", bindsHistogram(), 2048, StepBudgetBinds, "", 0.4},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := ReadStepBudgetBinding(c.steps, c.budget)
			assert.Equal(t, c.class, got.Class)
			assert.Equal(t, c.reason, got.Reason)
			assert.InDelta(t, c.over, got.OverShare, 1e-9)
		})
	}
}

// The bound the class is read at, the diagnostic near bound and its share,
// and the observed top; the top is read with no budget known too.
func TestReadStepBudgetBinding_TheFigures(t *testing.T) {
	got := ReadStepBudgetBinding(bindsHistogram(), 8192)
	assert.Equal(t, 8192.0, got.BoundTokens)
	assert.Equal(t, 4096.0, got.NearTokens)
	assert.InDelta(t, 0.3, got.Share, 1e-9)
	assert.InDelta(t, 1000.0, got.Iterations, 1e-9)
	assert.Equal(t, 16384.0, got.TopTokens)
	assert.True(t, got.TopOpen)

	got = ReadStepBudgetBinding(freeHistogram(), 0)
	assert.Equal(t, StepBudgetNotKnown, got.Reason)
	assert.Equal(t, 8192.0, got.TopTokens, "99.9% of the iterations at or under 8192")
	assert.False(t, got.TopOpen)
}

// rate() extrapolates each bucket on its own: a bound that reads a hair
// under the one below is read as the one below, row by row.
func TestReadStepBudgetBinding_NonMonotoneBuckets(t *testing.T) {
	steps := bindsHistogram()
	steps[9].Rate = steps[8].Rate - 0.01
	got := ReadStepBudgetBinding(steps, 8192)
	assert.Equal(t, StepBudgetBinds, got.Class)
	assert.InDelta(t, 0.1, got.OverShare, 1e-9)
}

// A pool weights each row's rates by the seconds it stands for and adds them
// bound by bound; an empty row adds nothing, and a row of no seconds adds no
// iteration.
func TestStepTokens_PoolsRowsOnTheSameBounds(t *testing.T) {
	perSecond := func(h []domain.LengthBucket) []domain.LengthBucket {
		for i := range h {
			h[i].Rate /= 100
		}
		return h
	}
	var s StepTokens
	s.Add(perSecond(bindsHistogram()), 100)
	s.Add(nil, 15)
	s.Add(perSecond(freeHistogram()), 100)
	s.Add(perSecond(freeHistogram()), 0)
	assert.Equal(t, 3, s.Samples)
	assert.False(t, s.MixedBounds)
	got := s.Binding(8192)
	assert.InDelta(t, 2000.0, got.Iterations, 1e-9)
	assert.InDelta(t, 100.4/2000, got.OverShare, 1e-9)
	assert.Equal(t, StepBudgetBinds, got.Class)

	c := s.Clone()
	c.Buckets[0].Rate = -1
	assert.NotEqual(t, -1.0, s.Buckets[0].Rate, "a clone shares no bucket")
}

// Rows read on other bounds (an engine of another release, a bucket missing)
// do not add as cumulative counts: the pool is not read.
func TestStepTokens_MixedBoundsAreUnknown(t *testing.T) {
	missing := bindsHistogram()
	missing = append(missing[:11:11], missing[12:]...) // no 8192 bucket
	var s StepTokens
	s.Add(bindsHistogram(), 1)
	s.Add(missing, 1)
	assert.True(t, s.MixedBounds)
	got := s.Binding(8192)
	assert.Equal(t, StepBudgetUnknown, got.Class)
	assert.Equal(t, StepBudgetMixedBounds, got.Reason)
}

// The budget is the engine's as resolved (StepBudget): the flag, else the
// release's default; a nil engine has none.
func TestEngineParams_StepBudgetBinding(t *testing.T) {
	var s StepTokens
	s.Add(bindsHistogram(), 1)
	assert.Equal(t, StepBudgetBinds, (&EngineParams{MaxNumBatchedTokens: 8192}).StepBudgetBinding(s).Class)
	assert.Equal(t, StepBudgetBinds, (&EngineParams{EffectiveMaxBatchedTokens: 8192}).StepBudgetBinding(s).Class)
	var none *EngineParams
	assert.Equal(t, StepBudgetNotKnown, none.StepBudgetBinding(s).Reason)
}
