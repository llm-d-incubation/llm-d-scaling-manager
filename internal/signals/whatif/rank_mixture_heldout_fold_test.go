package whatif

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The held-out folds score the mean of the steps after each cut. The second
// fold's scored steps, the window's last quarter, are no fold's training
// steps, so with the spread calibrated inside each fold their p90s cannot
// move the score. At the window's spread they do: the window calibrated it
// on those p90s, so the score is not out of sample (the leak this fixes).
func TestRankMixtureHeldOutFoldSpread_NoScoredP90SetsTheSpread(t *testing.T) {
	truth := RankMixture{S0: 0.25, V: 20000, Shares: []float64{0.3, 0.25, 0.25, 0.2}, A0: 0.4, Spread: 1.5,
		KV: KVDeferral{P0: 0.02, P1: 0.3, M0: 0.3, M1: 1.2, Known: true, RhoLo: 0, RhoHi: 1}}
	truth.BetaOverhead, truth.BetaDecode = 4e-6, 3e-6
	truth.Beta = truth.BetaOverhead + truth.BetaDecode
	steps := syntheticSteps(t, truth, 96)
	o := RankMixtureOptions{SpreadFloor: MinSpread}

	// The same window with the last quarter's measured p90 read higher.
	moved := slices.Clone(steps)
	start := steps[0].At.Add(-time.Nanosecond)
	lastQuarter := start.Add(3 * steps[len(steps)-1].At.Sub(start) / 4)
	n := 0
	for i := range moved {
		if moved[i].At.After(lastQuarter) {
			moved[i].RouterP90 *= 1.6
			n++
		}
	}
	require.Positive(t, n)

	win, _, err := FitRankMixture(steps, o)
	require.NoError(t, err)
	winMoved, _, err := FitRankMixture(moved, o)
	require.NoError(t, err)
	require.Greater(t, winMoved.Spread, win.Spread, "the window's spread reads the last quarter's p90")

	assert.NotEqual(t, RankMixtureHeldOut(steps, o, win.Spread, 0.2), RankMixtureHeldOut(moved, o, winMoved.Spread, 0.2),
		"at the window's spread, a p90 the folds score moves their score")

	h := RankMixtureHeldOutFoldSpread(steps, o, win.Spread, true, 0.2)
	require.Positive(t, h.Steps)
	assert.Equal(t, h, RankMixtureHeldOutFoldSpread(moved, o, winMoved.Spread, true, 0.2),
		"calibrated inside each fold, no p90 the folds score moves their score")
	assert.Positive(t, h.Within, "the folds still score the model within the tolerance")
}

// A window whose spread was not calibrated is scored at it, as before: no
// scored step set it. A floor above every fold's own spread prices each fold
// at the floor, which is the window's spread too.
func TestRankMixtureHeldOutFoldSpread_UncalibratedAndFlooredAsBefore(t *testing.T) {
	truth := RankMixture{S0: 0.25, V: 20000, Shares: []float64{0.3, 0.25, 0.25, 0.2}, A0: 0.4, Spread: 1.5,
		KV: KVDeferral{P0: 0.02, P1: 0.3, M0: 0.3, M1: 1.2, Known: true, RhoLo: 0, RhoHi: 1}}
	truth.BetaOverhead, truth.BetaDecode = 4e-6, 3e-6
	truth.Beta = truth.BetaOverhead + truth.BetaDecode
	steps := syntheticSteps(t, truth, 96)

	o := RankMixtureOptions{SpreadFloor: MinSpread, MinSpreadSpan: 24 * time.Hour}
	m, diag, err := FitRankMixture(steps, o)
	require.NoError(t, err)
	require.True(t, diag.SpreadDefaulted)
	assert.Equal(t, RankMixtureHeldOut(steps, o, m.Spread, 0.2), RankMixtureHeldOutFoldSpread(steps, o, m.Spread, false, 0.2))

	floored := RankMixtureOptions{SpreadFloor: 2}
	h := RankMixtureHeldOutFoldSpread(steps, floored, 2, true, 0.2)
	require.Positive(t, h.Steps)
	assert.Equal(t, RankMixtureHeldOut(steps, floored, 2, 0.2), h)
}
