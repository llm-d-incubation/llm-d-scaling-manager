package whatifapi

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// hitMixture is a main-view mixture fitted on 2 prefill replicas whose
// pooled window computed about half of each prompt.
func hitMixture(on bool) *mainMixture {
	return &mainMixture{prefill: 2, hitAtCount: on, pooled: whatif.MixtureStep{
		Computed: map[float64]float64{1000: 0, 4000: 40, 8000: 100, math.Inf(1): 100},
		Prompt:   map[float64]float64{8000: 0, 16000: 70, 32000: 100, math.Inf(1): 100},
	}}
}

// With PrefillHitAtCount on, the experimental model draws its computed
// tokens at the prefill count priced: on 1 replica the histogram is a miss
// mixture at the floor's hit, so its mean is E[L] (1 - h/2), and its
// figures are flagged PrefixHitAtCount. At the recorded count, at more
// replicas (held, flagged PrefixHitHeld) and with the switch off, the
// pooled histogram is drawn as it is.
func TestMainMixture_TheComputedTokensAtTheCountPriced(t *testing.T) {
	mm := hitMixture(true)
	ec, _ := whatif.CumulativeMean(mm.pooled.Computed)
	el, _ := whatif.CumulativeMean(mm.pooled.Prompt)
	h := 1 - ec/el
	require.Greater(t, h, 0.3)

	c, flag, note := mm.computedAt(1)
	got, ok := whatif.CumulativeMean(c)
	require.True(t, ok)
	t.Logf("E[L] %.0f, E[computed] %.0f on 2 replicas, %.0f priced on 1 (h %.3f)", el, ec, got, h)
	assert.InDelta(t, el*(1-h/2), got, 0.02*el, "the floor's computed tokens, the cap aside")
	assert.LessOrEqual(t, got, el*(1-h/2)*(1+1e-9), "the cap only lowers it")
	assert.Equal(t, whatif.PrefixHitAtCount, flag)
	assert.Contains(t, note, "on 1 prefill replicas, from")
	assert.Contains(t, note, "a miss mixture")

	for name, c := range map[string]struct {
		mm   *mainMixture
		n    int
		flag whatif.Flag
	}{
		"recorded count": {hitMixture(true), 2, ""},
		"more replicas":  {hitMixture(true), 4, whatif.PrefixHitHeld},
		"switch off":     {hitMixture(false), 1, ""},
	} {
		got, flag, note := c.mm.computedAt(c.n)
		assert.Equal(t, c.mm.pooled.Computed, got, name)
		assert.Equal(t, c.flag, flag, name)
		assert.Empty(t, note, name)
	}

	// With the reuse-gap law the source gave, more replicas move the hit up
	// too, and the histogram's bounds scale down with it.
	law := hitMixture(true)
	law.hitModel = &prefill.HitModel{HInf: 1, Mu: 4.143, Sigma: 1.454, HorizonSeconds: 69.29, GapMedianSeconds: 25.07, FitCount: 2,
		TotalHit: 0.526, GPUHit: 0.526, CeilingAssumed: true, K: 1, A: 1}
	up, flag, note := law.computedAt(4)
	mUp, _ := whatif.CumulativeMean(up)
	assert.Less(t, mUp, ec)
	assert.Equal(t, whatif.PrefixHitAtCount, flag)
	assert.Contains(t, note, "(gap)")
	assert.Contains(t, note, "scaled by")

	// The flag reaches the model's TTFT figures.
	f := &mixtureFleet{hitFlag: whatif.PrefixHitAtCount}
	assert.Equal(t, []whatif.Flag{whatif.RankMixtureTTFT, whatif.PrefixHitAtCount}, f.flags())
	assert.Equal(t, []whatif.Flag{whatif.RankMixtureTTFT}, (&mixtureFleet{}).flags())
}
