package whatif

import (
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cleanFitSteps leaves out a step whose busiest rank is at or over
// MaxCleanFitRho, unless too few would be left.
func TestCleanFitSteps(t *testing.T) {
	mk := func(rate float64) MixtureStep {
		return MixtureStep{Ranks: []MixtureRank{{Rate: rate, Computed: 4000}, {Rate: rate / 2, Computed: 4000}}}
	}
	// s0 0.1, V 10000: es 0.5 s, rho = rate/2 on the busiest rank.
	steps := []MixtureStep{mk(1), mk(1.2), mk(1.6), mk(1.79), mk(1.8), mk(1.9)}
	on, clean, over := cleanFitSteps(steps, 0.1, 10000)
	assert.Len(t, on, 4)
	assert.Equal(t, [2]int{4, 2}, [2]int{clean, over})
	on, clean, over = cleanFitSteps(steps[2:], 0.1, 10000)
	assert.Len(t, on, 4, "with fewer than minCleanFitSteps clean, every step")
	assert.Equal(t, [2]int{2, 2}, [2]int{clean, over})
	// A service line that is not set reads every step as overloaded, and
	// the fits read every step.
	on, clean, _ = cleanFitSteps(steps, math.NaN(), 10000)
	assert.Len(t, on, len(steps))
	assert.Zero(t, clean)
}

// The assumed line says which fit-step fixes priced, and is the default's
// without them.
func TestRankMixture_AssumedNamesFitSteps(t *testing.T) {
	m := RankMixture{S0: 0.1, V: 10000, Shares: []float64{0.6, 0.4}, Spread: 1}
	base := m.Assumed()
	assert.NotContains(t, base, "busiest-rank utilization")
	assert.NotContains(t, base, "priced rate")
	m.CleanSteps, m.OverloadedSteps = 10, 3
	assert.Contains(t, m.Assumed(), "fitted on the 10 steps under busiest-rank utilization 0.9 (3 left out)")
	m.CleanSteps = 2
	assert.Contains(t, m.Assumed(), "fitted on every step: 2 under")
	m.ShareRows = make([]ShareRow, 3)
	assert.Contains(t, m.Assumed(), "too few to read them at the load")
	m.ShareRows = make([]ShareRow, 12)
	assert.Contains(t, m.Assumed(), "the mean of the 5 of 12 fit steps nearest it")
	assert.True(t, strings.HasPrefix(m.Assumed(), base), "the fixes only add to the default's line")
}

// With CleanFitSteps the KV-deferral lines are fitted without the
// overloaded steps: a burst of overload queue time on a few steps moves the
// default fit's lines and leaves the switched fit's on the truth.
func TestFitCore_CleanFitSteps(t *testing.T) {
	s0, v, ranks := 0.2, 10000.0, 4
	steps := make([]MixtureStep, 0, 14)
	for i := range 14 {
		rate := 2 + float64(i%10)
		rho := rate / float64(ranks) * (s0 + 2000/v)
		p, m := 0.05+0.3*rho, 0.4+0.6*rho
		if i >= 10 {
			// overloaded: the busiest rank over 0.9, a queue of tens of seconds
			rate = 16
			p, m = 0.95, 30
		}
		q := map[float64]float64{kvEdgeLo: 1 - p*math.Exp(-kvEdgeLo/m), kvEdgeHi: 1 - p*math.Exp(-kvEdgeHi/m), math.Inf(1): 1}
		rs := make([]MixtureRank, 0, ranks)
		for range ranks {
			rs = append(rs, MixtureRank{Rate: rate / float64(ranks), PrefillTime: 0.5, QueueTime: 0.1, Computed: 2000 + 100*float64(i%3)})
		}
		steps = append(steps, MixtureStep{Rate: rate, Ranks: rs, Queue: q})
	}
	base, _, err := FitCore(steps, RankMixtureOptions{NoPrompt: true})
	require.NoError(t, err)
	clean, _, err := FitCore(steps, RankMixtureOptions{NoPrompt: true, CleanFitSteps: true})
	require.NoError(t, err)
	require.True(t, base.KV.Known)
	require.True(t, clean.KV.Known)
	assert.Greater(t, base.KV.RhoHi, clean.KV.RhoHi, "the overloaded steps are in the default fit only")
	assert.Less(t, clean.KV.RhoHi, MaxCleanFitRho)
	assert.Nil(t, base.ShareRows)
	assert.Nil(t, clean.ShareRows, "the shares switch is its own")
	assert.Zero(t, base.CleanSteps+base.OverloadedSteps)
	assert.Equal(t, 4, clean.OverloadedSteps)
}

// sharesAt averages the profiles of the fit steps nearest the priced rate;
// without the switch (or with too few rows) it prices the window's profile,
// and the draws are the default's.
func TestSharesAtLoad(t *testing.T) {
	m := RankMixture{Shares: []float64{0.7, 0.3}}
	assert.Equal(t, m.Shares, m.sharesAt(10))
	for i := range 10 {
		top := 0.8 - 0.03*float64(i)
		m.ShareRows = append(m.ShareRows, ShareRow{Rate: float64(i + 1), Shares: []float64{top, 1 - top}})
	}
	assert.InDelta(t, 0.59, m.sharesAt(10)[0], 1e-9)
	assert.InDelta(t, 0.74, m.sharesAt(1)[0], 1e-9)
	rows := shareRowsOf([]MixtureStep{{Rate: 2, Ranks: []MixtureRank{{Rate: 0.5}, {Rate: 1.5}}}, {Rate: 1, Ranks: []MixtureRank{{Rate: 1}}},
		// a quiet step, as ShareProfile leaves it out: a handful of requests
		{Rate: 0.5, Ranks: []MixtureRank{{Rate: 0.5}, {Rate: 0}}},
		{Rate: 3, Ranks: []MixtureRank{{Rate: math.Inf(1)}, {Rate: 1}}}}, 2)
	require.Len(t, rows, 1)
	assert.Equal(t, []float64{0.75, 0.25}, rows[0].Shares)
	// The draws follow the shares at the rate: a more even profile prices
	// the busiest rank lower.
	line := RankMixture{S0: 0.1, V: 10000, Shares: []float64{0.7, 0.3}}
	d0, u := line.Draws(1.5, expHist(2000), pointHist(40000), 4000)
	require.Nil(t, u)
	even := line
	for range 5 {
		even.ShareRows = append(even.ShareRows, ShareRow{Rate: 1.5, Shares: []float64{0.5, 0.5}})
	}
	d1, u := even.Draws(1.5, expHist(2000), pointHist(40000), 4000)
	require.Nil(t, u)
	assert.Less(t, d1.Checks.Rho[0], d0.Checks.Rho[0])
	// FitCore sets the rows only with the switch.
	st := syntheticSteps(t, RankMixture{S0: 0.05, V: 20000, Shares: []float64{0.6, 0.4}, Spread: 1}, 12)
	m1, _, err := FitCore(st, RankMixtureOptions{SharesAtLoad: true})
	require.NoError(t, err)
	assert.Len(t, m1.ShareRows, 12)
}
