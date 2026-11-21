package whatif

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// vLLM's scheduler takes 1 + k tokens of the step budget per running
// request: the batch a rank runs is the smaller of its cap and budget / (1+k).
func TestStepBudgetCap(t *testing.T) {
	for _, c := range []struct {
		name            string
		perRank, budget int64
		k               int
		want            int64
	}{
		{"wide EP, MTP k=3", 256, 256, 3, 64},
		{"wide EP, k=1", 256, 256, 1, 128},
		{"wide EP, no spec", 256, 256, 0, 256},
		{"dense, the cap binds", 256, 8192, 3, 256},
		{"budget unknown", 256, 0, 3, 256},
		{"no cap stays no cap", 0, 256, 3, 0},
		{"negative k is none", 256, 128, -2, 128},
		{"the last request is cut, not skipped", 256, 250, 3, 63},
		{"k=2 at 256", 256, 256, 2, 86},
	} {
		assert.Equal(t, c.want, StepBudgetCap(c.perRank, c.budget, c.k), c.name)
	}
}
