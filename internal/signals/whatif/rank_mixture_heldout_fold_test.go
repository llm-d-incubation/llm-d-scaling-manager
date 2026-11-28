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

// A window just over MinSpreadSpan whose p90 was read in its last quarter
// only: the window's spread is calibrated (its second self-check fold
// scores p90s), while each held-out fold trains on a span under
// MinSpreadSpan whose own self-check folds read no p90. Fitted as the
// learner fits a window that short, each fold is priced at the floored
// default spread and scores its steps, as RankMixtureHeldOut scores them at
// that spread. Calibrated on a span under MinSpreadSpan, no fold could fit
// its spread, and the folds scored nothing.
func TestRankMixtureHeldOutFoldSpread_AShortFoldIsPricedAtTheDefault(t *testing.T) {
	truth := RankMixture{S0: 0.25, V: 20000, Shares: []float64{0.3, 0.25, 0.25, 0.2}, A0: 0.4, Spread: 1.5,
		KV: KVDeferral{P0: 0.02, P1: 0.3, M0: 0.3, M1: 1.2, Known: true, RhoLo: 0, RhoHi: 1}}
	truth.BetaOverhead, truth.BetaDecode = 4e-6, 3e-6
	truth.Beta = truth.BetaOverhead + truth.BetaDecode
	steps := syntheticSteps(t, truth, 96)
	start := steps[0].At.Add(-time.Nanosecond)
	lastQuarter := start.Add(3 * steps[len(steps)-1].At.Sub(start) / 4)
	for i := range steps {
		if !steps[i].At.After(lastQuarter) {
			steps[i].RouterP90 = 0
		}
	}
	// The window is 96 five-minute steps, 8 h; the span one step under it.
	o := RankMixtureOptions{SpreadFloor: MinSpread, MinSpreadSpan: 8*time.Hour - 5*time.Minute}
	m, diag, err := FitRankMixture(steps, o)
	require.NoError(t, err)
	require.False(t, diag.SpreadDefaulted, "the window's spread is calibrated")

	// Why the span matters: the first fold's steps calibrated alone.
	firstFold := stepsBetween(steps, start, start.Add(steps[len(steps)-1].At.Sub(start)/2))
	noSpan := o
	noSpan.MinSpreadSpan = 0
	_, _, err = FitRankMixture(firstFold, noSpan)
	require.Error(t, err, "a fold under the span cannot calibrate its own spread")

	h := RankMixtureHeldOutFoldSpread(steps, o, m.Spread, true, 0.2)
	t.Logf("held-out: %d of %d steps within the tolerance (window spread %.2f)", h.Within, h.Steps, m.Spread)
	require.Positive(t, h.Steps, "the folds score steps")
	assert.Equal(t, RankMixtureHeldOut(steps, o, MinSpread, 0.2), h, "each fold priced at the floored default spread")
}

// BenchmarkRankMixtureHeldOut_FoldSpread is the cost of the held-out folds
// on a fit window of 96 five-minute steps, beside the window's own fit:
// at the window's spread (each fold a FitCore), and with
// RankMixtureSwitches.HeldOutFoldSpread (each fold a FitRankMixture),
// calibrated in the folds (a window of twice MinSpreadSpan or more) and at
// the default (folds under the span). Run with
//
//	go test ./internal/signals/whatif/ -run '^$' -bench 'RankMixtureHeldOut_FoldSpread' -benchtime 3x
func BenchmarkRankMixtureHeldOut_FoldSpread(b *testing.B) {
	truth := RankMixture{S0: 0.25, V: 20000, Shares: []float64{0.3, 0.25, 0.25, 0.2}, A0: 0.4, Spread: 1.5,
		KV: KVDeferral{P0: 0.02, P1: 0.3, M0: 0.3, M1: 1.2, Known: true, RhoLo: 0, RhoHi: 1}}
	truth.BetaOverhead, truth.BetaDecode = 4e-6, 3e-6
	truth.Beta = truth.BetaOverhead + truth.BetaDecode
	steps := syntheticSteps(b, truth, 96)
	o := RankMixtureOptions{SpreadFloor: MinSpread}
	m, _, err := FitRankMixture(steps, o)
	require.NoError(b, err)
	underSpan := o
	underSpan.MinSpreadSpan = 8*time.Hour - 5*time.Minute
	b.Run("window-fit", func(b *testing.B) {
		for b.Loop() {
			_, _, _ = FitRankMixture(steps, o)
		}
	})
	b.Run("held-out-window-spread", func(b *testing.B) {
		for b.Loop() {
			RankMixtureHeldOut(steps, o, m.Spread, 0.2)
		}
	})
	b.Run("held-out-fold-spread-calibrated", func(b *testing.B) {
		for b.Loop() {
			RankMixtureHeldOutFoldSpread(steps, o, m.Spread, true, 0.2)
		}
	})
	b.Run("held-out-fold-spread-under-span", func(b *testing.B) {
		for b.Loop() {
			RankMixtureHeldOutFoldSpread(steps, underSpan, m.Spread, true, 0.2)
		}
	})
}

// FitMixtureWindow's held-out score (mixtureHeldOut) takes the folds' own
// spread only with HeldOutFoldSpread, and only where the window's spread
// was calibrated; otherwise the folds are scored at the window's spread.
// The calibration span is a minute, so each fold calibrates its own spread
// and the two scores differ.
func TestRankMixtureHeldOutFoldSpread_TheWindowFitDispatches(t *testing.T) {
	truth := RankMixture{S0: 0.25, V: 20000, Shares: []float64{0.3, 0.25, 0.25, 0.2}, A0: 0.4, Spread: 1.5,
		KV: KVDeferral{P0: 0.02, P1: 0.3, M0: 0.3, M1: 1.2, Known: true, RhoLo: 0, RhoHi: 1}}
	truth.BetaOverhead, truth.BetaDecode = 4e-6, 3e-6
	truth.Beta = truth.BetaOverhead + truth.BetaDecode
	steps := syntheticSteps(t, truth, 96)
	off := RankMixtureSwitches{MinFitWindow: time.Minute}
	on := off
	on.HeldOutFoldSpread = true
	o := off.Options(0)
	m, diag, err := FitRankMixture(steps, o)
	require.NoError(t, err)
	require.False(t, diag.SpreadDefaulted)
	rq := &RankMixtureFitted{M: m, Report: RankMixtureReport{SpreadDefaulted: diag.SpreadDefaulted}}

	atWindow := RankMixtureHeldOut(steps, o, m.Spread, 0.2)
	inFolds := RankMixtureHeldOutFoldSpread(steps, o, m.Spread, true, 0.2)
	require.NotEqual(t, atWindow, inFolds, "the two scores differ on this window")
	assert.Equal(t, atWindow, mixtureHeldOut(steps, rq, off, 0.2), "off: the window's spread")
	assert.Equal(t, inFolds, mixtureHeldOut(steps, rq, on, 0.2), "on: the folds' own spread")

	defaulted := *rq
	defaulted.Report.SpreadDefaulted = true
	assert.Equal(t, atWindow, mixtureHeldOut(steps, &defaulted, on, 0.2),
		"on, a window whose spread defaulted: the window's spread, as before")
}
