package allocation

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// TestRescaleTargetsStayBoundedWithAHugeWeight: a weight whose product
// overflows still takes the budget, and every other model keeps a sane
// target. Unbounded, totalW was +Inf, every share NaN, and the rounding turned
// NaN into a huge negative target for every model in the budget.
func TestRescaleTargetsStayBoundedWithAHugeWeight(t *testing.T) {
	in := []rescaleInput{
		{ID: "A", Priority: 1, Demand: 8, CapGPUs: 8},
		{ID: "B", Priority: 1e308, Demand: 10, CapGPUs: 8},
		{ID: "C", Priority: 1e308, Demand: 10, CapGPUs: 8},
	}
	got, over := computeRescaleTargets(in, 8)
	assert.False(t, over)
	total := 0
	for _, m := range in {
		assert.GreaterOrEqual(t, got[m.ID], 0, m.ID)
		assert.LessOrEqual(t, got[m.ID], m.CapGPUs, m.ID)
		total += got[m.ID]
	}
	assert.Equal(t, 8, total)
	assert.Equal(t, 0, got["A"], "the two huge weights take the budget")
	assert.Equal(t, 4, got["B"])
}

// TestFairShareValueStaysFinite: the fair share averages these values across
// models, so one infinite value made a model's target NaN and it got nothing.
func TestFairShareValueStaysFinite(t *testing.T) {
	e := NamedAnalyzerResult{Score: 1e308, Result: &domain.AnalyzerResult{}}
	v := fairShareValue(1e308, e, RolePairedState{domain.RoleBoth: 10}, []string{domain.RoleBoth})
	assert.False(t, math.IsInf(v, 0) || math.IsNaN(v), "fair-share value %v", v)
	assert.Positive(t, v)
}
