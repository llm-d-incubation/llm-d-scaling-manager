package whatif

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RankMixtureNLHeldOut scores the variant on v2's folds, each fitted with
// FitRankMixtureNL on the steps before its cut and priced as fitted: on
// steps priced by the model the folds recover, the means land within the
// tolerance. It is not v2's score: v2's folds fit A0 to the steps and
// price at the spread given, the variant's price its measured parts.
func TestRankMixtureNLHeldOut(t *testing.T) {
	truth := RankMixture{S0: 0.1, V: 8000, Shares: []float64{0.3, 0.25, 0.25, 0.2}, Spread: 1, Slots: 4,
		BetaOverhead: 3e-6, BetaDecode: 2e-6, Beta: 5e-6}
	steps := syntheticSteps(t, truth, 48)
	for i := range steps {
		steps[i].RouterMean += 0.7
		steps[i].RouterP90 += 0.7
	}
	o := RankMixtureOptions{Slots: 4, MinSpreadSpan: MinSpreadFitWindow}
	h := RankMixtureNLHeldOut(steps, o, 0.2)
	require.Positive(t, h.Steps)
	assert.Equal(t, h.Steps, h.Within, "every held-out mean within 20%")
	assert.InDelta(t, 1, h.MedianRatio, 0.1)
	assert.Less(t, h.RateLo, h.RateHi)

	v2 := RankMixtureHeldOut(steps, o, 1, 0.2)
	assert.NotEqual(t, v2, h, "the variant's own folds, not v2's")

	assert.Zero(t, RankMixtureNLHeldOut(steps[:7], o, 0.2), "under 8 steps nothing is scored")
}
