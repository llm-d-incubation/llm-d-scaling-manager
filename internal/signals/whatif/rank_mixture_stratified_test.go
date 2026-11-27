package whatif

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// heavyTailLaw is a computed-token law whose top bucket holds 1e-4 of the
// requests at up to a million tokens: most of the service's second moment.
func heavyTailLaw() map[float64]float64 {
	return map[float64]float64{1000: 0.4, 2000: 0.8, 5000: 0.97, 20000: 0.9999, 1000000: 1, math.Inf(1): 1}
}

// With the switch every rank reads one utilization, at the law's mean
// service, and its cs^2 as a fine quadrature of the law reads them; without
// it the ranks read their own draws and scatter on the same law. The top
// bucket is wide (20k to 1M tokens) and holds most of the second moment: a
// 2^16-point grid of the law reads its cs^2 4 % low.
func TestRankMixture_StratifiedMoments(t *testing.T) {
	shares := []float64{0.25, 0.25, 0.25, 0.25}
	off := RankMixture{S0: 0.1, V: 20000, Shares: shares}
	on := off
	on.Stratified = true
	hx, ok := newCumHist(heavyTailLaw())
	require.True(t, ok)
	es, cs2 := on.serviceMoments(hx)
	var want, want2 float64
	const n = 1 << 20
	for i := range n {
		v := 0.1 + hx.ppf((float64(i)+0.5)/n)/20000
		want += v
		want2 += v * v
	}
	want /= n
	assert.InEpsilon(t, want, es, 1e-5)
	assert.InEpsilon(t, want2/n/(want*want)-1, cs2, 1e-3)
	d, u := on.Draws(4, heavyTailLaw(), pointHist(80000), DefaultMixtureDraws)
	require.Nil(t, u)
	for _, r := range d.Checks.Rho {
		assert.InDelta(t, d.Checks.Rho[0], r, 1e-12, "one law, one utilization")
	}
	assert.InEpsilon(t, es, d.Checks.Rho[0], 1e-9)
	d2, u := off.Draws(4, heavyTailLaw(), pointHist(80000), DefaultMixtureDraws)
	require.Nil(t, u)
	spread := 0.0
	for _, r := range d2.Checks.Rho {
		spread = max(spread, math.Abs(r-d2.Checks.Rho[0]))
	}
	assert.Greater(t, spread, 1e-3, "without the switch the ranks scatter")
}

// FitCore carries the switch onto the model; off by default.
func TestFitCore_StratifiedMoments(t *testing.T) {
	st := syntheticSteps(t, RankMixture{S0: 0.05, V: 20000, Shares: []float64{0.6, 0.4}, Spread: 1}, 12)
	m, _, err := FitCore(st, RankMixtureOptions{})
	require.NoError(t, err)
	assert.False(t, m.Stratified)
	m, _, err = FitCore(st, RankMixtureOptions{StratifiedMoments: true})
	require.NoError(t, err)
	assert.True(t, m.Stratified)
}

// The stratified floor is bounded over the fleet: up to 16 ranks each takes
// minStratifiedDraws, past that the fleet takes maxStratifiedDraws, and a
// fleet whose own draws are more keeps them.
func TestRankMixture_StratifiedDrawsBounded(t *testing.T) {
	for _, c := range []struct{ ranks, want int }{{1, DefaultMixtureDraws}, {4, 4 * minStratifiedDraws},
		{16, maxStratifiedDraws}, {64, maxStratifiedDraws}, {1000, 1000 * 65}} {
		shares := make([]float64, c.ranks)
		for i := range shares {
			shares[i] = 1 / float64(c.ranks)
		}
		m := RankMixture{S0: 0.1, V: 20000, Shares: shares, Stratified: true}
		d, u := m.Draws(0.5*float64(c.ranks), heavyTailLaw(), pointHist(80000), DefaultMixtureDraws)
		require.Nil(t, u)
		assert.Len(t, d.QP, c.want, "%d ranks", c.ranks)
	}
}
