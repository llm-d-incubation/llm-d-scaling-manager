package capacity

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Iterations near the budget, but none above it, do not bind: with vLLM's
// power-of-two bounds the bucket under a budget of 8192 is (4096, 8192], and
// a pool whose prompts of 5k to 8k tokens each complete in one step, or whose
// prompts fit together under the budget, counts all its prefill there without
// a single chunk. Only an iteration above the budget is a prompt it chunked.
func TestReadStepBudgetBinding_NearTheBudgetIsNotBinding(t *testing.T) {
	near := stepHistogram(0, 0, 0, 0, 50, 50, 50, 50, 50, 50, 100, 600)
	got := ReadStepBudgetBinding(near, 8192)
	assert.Equal(t, StepBudgetDoesNotBind, got.Class, "60 percent of the iterations in (4096, 8192], none above")
}
