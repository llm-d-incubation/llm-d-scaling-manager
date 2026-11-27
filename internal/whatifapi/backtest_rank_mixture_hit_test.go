package whatifapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// With the prefix hit at count on, a test step on another prefill count
// draws the fit window's computed tokens at that count's hit, the move the
// planner and the main view price (whatif.ComputedAtCount): a miss mixture
// at fewer pods, held at more without the reuse-gap law, scaled down with
// it. On the fit's own count, or with the switch off, the step draws its
// own histogram.
func TestRankMixtureQ_ComputedAtTheTestCount(t *testing.T) {
	r := &stubResolver{}
	fit := rmFit(t, r, 480)
	idx := make([]int, len(fit.series))
	for i := range idx {
		idx[i] = i
	}
	// The step's prompts are the fit's, so only the hit moves its draw.
	own := whatif.MixtureStep{Computed: map[float64]float64{500: 0, 9000: 1}}

	off := (&rankMixtureQ{fitPrefill: 2}).withHitAtCount(fit, idx)
	assert.False(t, off.hitAtCount, "the fit source was read with the switch off")
	assert.Equal(t, own.Computed, off.computedAt(own, 1))

	fit.doc.PrefillHitAtCount = true
	on := (&rankMixtureQ{fitPrefill: 2}).withHitAtCount(fit, idx)
	require.True(t, on.hitAtCount)
	require.NotEmpty(t, on.fitPooled.Computed)
	own.Prompt = on.fitPooled.Prompt
	assert.Equal(t, own.Computed, on.computedAt(own, 2), "the fit's own count")

	// A step whose prompts are twice the fit's computes twice the tokens
	// at the same hit.
	long := whatif.MixtureStep{Prompt: map[float64]float64{}}
	for le, c := range on.fitPooled.Prompt {
		long.Prompt[2*le] = c
	}
	m1, _ := whatif.CumulativeMean(on.computedAt(own, 1))
	m2, _ := whatif.CumulativeMean(on.computedAt(long, 1))
	assert.InEpsilon(t, 2*m1, m2, 0.02)

	at1 := on.computedAt(own, 1)
	_, want, ok := whatif.ComputedAtCount(on.fitPooled.Computed, on.fitPooled.Prompt, 2, 1, nil)
	require.True(t, ok)
	assert.Equal(t, want, at1, "fewer pods: the fit's histogram, moved")
	fitMean, _ := whatif.CumulativeMean(on.fitPooled.Computed)
	mAt1, _ := whatif.CumulativeMean(at1)
	assert.Greater(t, mAt1, fitMean)
	assert.Equal(t, on.fitPooled.Computed, on.computedAt(own, 4), "more pods, no law: the fit's hit held")

	on.hitModel = &prefill.HitModel{HInf: 1, Mu: 4.143, Sigma: 1.454, HorizonSeconds: 69.29, GapMedianSeconds: 25.07, FitCount: 2,
		TotalHit: 0.526, GPUHit: 0.526, CeilingAssumed: true, K: 1, A: 1}
	m4, _ := whatif.CumulativeMean(on.computedAt(own, 4))
	assert.Less(t, m4, fitMean, "more pods, with the law: fewer computed tokens")
}
