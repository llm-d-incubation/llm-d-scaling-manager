package whatif

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nlSteps is a fit window for the variant: syntheticSteps with a fitted
// KV-deferral wait, the router mean carrying Z's constants.
func nlSteps(t *testing.T) []MixtureStep {
	t.Helper()
	truth := RankMixture{S0: 0.1, V: 8000, Shares: []float64{0.3, 0.25, 0.25, 0.2}, Spread: 1, Slots: 4,
		BetaOverhead: 3e-6, BetaDecode: 2e-6, Beta: 5e-6, KV: KVDeferral{P0: 0.02, P1: 0.3, M0: 0.2, M1: 1, Known: true}}
	steps := syntheticSteps(t, truth, 48)
	for i := range steps {
		steps[i].RouterMean += 0.7
		steps[i].RouterP90 += 0.7
	}
	return steps
}

// nlOptions are the variant's options in the tests, both modes off.
func nlOptions() RankMixtureOptions {
	return RankMixtureOptions{Slots: 4, MinSpreadSpan: MinSpreadFitWindow}
}

// processingHist is a router request-processing histogram, seconds.
func processingHist() map[float64]float64 {
	return map[float64]float64{0.01: 0.2, 0.05: 0.6, 0.2: 0.9, 1: 0.98, math.Inf(1): 1}
}

// The held lines read no further than RhoHold: past it they are read at it,
// under it as they were. Unheld, they are extrapolated to MaxKVRho.
func TestKVDeferral_HoldReadsNoFurther(t *testing.T) {
	k := KVDeferral{P0: 0.05, P1: 0.6, M0: 0.2, M1: 1.5, RhoLo: 0.2, RhoHi: 0.5, Known: true}
	held := k
	held.RhoHold = 0.5
	for _, c := range []struct{ rho, readAt float64 }{{0.8, 0.5}, {0.99, 0.5}, {0.5, 0.5}, {0.3, 0.3}} {
		p, m := held.at(c.rho)
		wp, wm := k.at(c.readAt)
		assert.Equal(t, wp, p, "rho %g", c.rho)
		assert.Equal(t, wm, m, "rho %g", c.rho)
	}
	p8, m8 := k.at(0.8)
	p5, m5 := k.at(0.5)
	assert.Greater(t, p8, p5, "unheld, the line is extrapolated")
	assert.Greater(t, m8, m5)
}

// Priced past the hold, the KV-deferral wait is lower with it; priced where
// every rank is under it, the draws are the same draws. The figure is
// flagged outside the fitted range either way.
func TestRankMixture_KVHoldLowersTheWaitPastIt(t *testing.T) {
	m := RankMixture{S0: 0.05, V: 10000, Shares: []float64{0.6, 0.4}, Stratified: true,
		KV: KVDeferral{P0: 0.05, P1: 0.8, M0: 0.2, M1: 2, RhoLo: 0.1, RhoHi: 0.4, Known: true}}
	held := m
	held.KV.RhoHold = 0.4
	comp, prom := expHist(2000), expHist(40000)

	d, u := m.Draws(4, comp, prom, 8000)
	require.Nil(t, u)
	dh, u := held.Draws(4, comp, prom, 8000)
	require.Nil(t, u)
	require.Greater(t, d.Checks.Rho[0], held.KV.RhoHold, "the busy rate is past the hold")
	mean, _ := d.Stats(0, 0)
	meanHeld, _ := dh.Stats(0, 0)
	assert.Less(t, meanHeld, mean)
	assert.Equal(t, d.Checks, dh.Checks, "the checks, KVOutside among them, do not move")
	assert.True(t, dh.Checks.KVOutside)

	d, u = m.Draws(1, comp, prom, 8000)
	require.Nil(t, u)
	dh, u = held.Draws(1, comp, prom, 8000)
	require.Nil(t, u)
	require.Less(t, slices.Max(d.Checks.Rho), held.KV.RhoHold, "the quiet rate is inside the hold")
	assert.Equal(t, d.QP, dh.QP)
}

// FitRankMixtureNL with KVHold holds the lines at the KVHoldQuantile of the
// per-rank utilizations its own fit steps price, and changes nothing else:
// not the fit window's checks, not any other parameter.
func TestFitRankMixtureNL_KVHoldAtTheFitQuantile(t *testing.T) {
	steps := nlSteps(t)
	o := nlOptions()
	off, offDiag, err := FitRankMixtureNL(steps, o)
	require.NoError(t, err)
	require.True(t, off.KV.Known)
	assert.Zero(t, off.KV.RhoHold)
	assert.False(t, off.KVHold)

	o.KVHold = true
	on, onDiag, err := FitRankMixtureNL(steps, o)
	require.NoError(t, err)
	rhos := make([]float64, 0, 4*len(steps))
	for _, p := range drawSteps(off, steps) {
		require.False(t, p.priceError)
		rhos = append(rhos, p.d.Checks.Rho...)
	}
	assert.InDelta(t, sampleQuantile(rhos, KVHoldQuantile), on.KV.RhoHold, 1e-12)
	assert.Less(t, on.KV.RhoHold, slices.Max(rhos))
	assert.Greater(t, on.KV.RhoHold, sampleQuantile(rhos, 0.5))
	assert.Equal(t, offDiag, onDiag, "the hold never moves the fit window's checks")
	same := on
	same.KV.RhoHold, same.KVHold = 0, false
	assert.Equal(t, off, same, "the hold is the only difference")
	assert.Contains(t, on.Assumed(), "KV-deferral lines held past rho")
	assert.Contains(t, on.Assumed(), "KV-deferral hold without the remainder spread", "held alone, the line says so")
	assert.NotContains(t, off.Assumed(), "held past")
	o.RemainderSpread = true
	both, _, err := FitRankMixtureNL(steps, o)
	require.NoError(t, err)
	assert.Contains(t, both.Assumed(), "KV-deferral lines held past rho")
	assert.NotContains(t, both.Assumed(), "without the remainder spread", "with the spread asked for, no caveat")
	o.RemainderSpread = false

	o.NoKV = true
	noKV, _, err := FitRankMixtureNL(steps, o)
	require.NoError(t, err)
	assert.Zero(t, noKV.KV.RhoHold)
	assert.Contains(t, noKV.Assumed(), "KV-deferral hold not applied: no KV-deferral line fitted")
}

// sampleQuantile is linear between order statistics, as numpy's default.
func TestSampleQuantile(t *testing.T) {
	xs := []float64{3, 1, 4, 2}
	assert.InDelta(t, 3.85, sampleQuantile(xs, 0.95), 1e-12)
	assert.Equal(t, 1.0, sampleQuantile(xs, 0))
	assert.Equal(t, 4.0, sampleQuantile(xs, 1))
	assert.Equal(t, 2.5, sampleQuantile(xs, 0.5))
	assert.Equal(t, []float64{3, 1, 4, 2}, xs, "the sample is not sorted in place")
	assert.Zero(t, sampleQuantile(nil, 0.95))
}

// A time histogram's first bucket runs from 0, uniform; a bucket above it is
// log-uniform; the +Inf bucket runs to twice its lower edge.
func TestTimeHist(t *testing.T) {
	h, ok := newTimeHist(processingHist())
	require.True(t, ok)
	assert.InDelta(t, 0.005, h.ppf(0.1), 1e-12)
	assert.InDelta(t, math.Sqrt(0.01*0.05), h.ppf(0.4), 1e-12)
	assert.InDelta(t, math.Sqrt2, h.ppf(0.99), 1e-12)
	_, ok = newTimeHist(map[float64]float64{1: 0, math.Inf(1): 0})
	assert.False(t, ok)
}

// One histogram's Vincentized shape is its own law over its mean: mean 1,
// its quantiles the histogram's, its p95 inside the histogram's bucket.
func TestVincentShape_OneHistogramIsItsOwnLaw(t *testing.T) {
	shape, n := vincentShape([]map[float64]float64{processingHist()})
	require.Equal(t, 1, n)
	require.Len(t, shape, shapeGrid)
	var s float64
	for _, x := range shape {
		s += x
	}
	assert.InDelta(t, 1, s/shapeGrid, 1e-12)
	h, _ := newTimeHist(processingHist())
	var raw float64
	for i := range shapeGrid {
		raw += h.ppf((float64(i) + 0.5) / shapeGrid)
	}
	raw /= shapeGrid
	for _, u := range []float64{0.1, 0.5, 0.9, 0.95, 0.99} {
		assert.InEpsilon(t, h.ppf(u)/raw, shapeAt(shape, u), 2e-3, "u %g", u)
	}
	p95 := shapeAt(shape, 0.95) * raw
	assert.Greater(t, p95, 0.2)
	assert.Less(t, p95, 1.0)
	// Two copies average to the same shape; none reads none.
	two, n := vincentShape([]map[float64]float64{processingHist(), processingHist()})
	assert.Equal(t, 2, n)
	assert.InDeltaSlice(t, shape, two, 1e-12)
	none, n := vincentShape(nil)
	assert.Nil(t, none)
	assert.Zero(t, n)
}

// A part drawn from a shape keeps its line's mean, and its p95 is the line
// less the shape's mean plus the shape's p95; a line under the shape's mean
// is held at 0, so the part is the shape alone.
func TestDrawParts_ShapeKeepsTheMeanAndReadsItsP95(t *testing.T) {
	shape, _ := vincentShape([]map[float64]float64{processingHist()})
	part := ZPart{A: 1, Shape: shape, ShapeMean: 0.4}
	l := make([]float64, 100000)
	z := drawParts(l, []ZPart{part})
	var mean float64
	for _, x := range z {
		mean += x
	}
	mean /= float64(len(z))
	assert.InEpsilon(t, 1, mean, 0.01)
	assert.InEpsilon(t, 0.6+0.4*shapeAt(shape, 0.95), sampleQuantile(z, 0.95), 0.02)
	assert.InDelta(t, 0.4*shapeAt(shape, 0.95), part.ShapeMean*part.ShapeQuantile(0.95), 1e-12)

	part.A = 0.1
	z = drawParts(l, []ZPart{part})
	mean = 0
	for _, x := range z {
		mean += x
	}
	mean /= float64(len(z))
	assert.InEpsilon(t, 0.4, mean, 0.01)
	assert.Zero(t, ZPart{A: 1}.ShapeQuantile(0.95))
}

// RemainderSpread draws the remainder from the router's measured processing
// where at least MinRemainderSpreadSteps fit steps hold its histogram; with
// fewer the remainder is its line, the assumed line says so, and the model
// is the one fitted without the mode. Neither moves the fit window's checks.
func TestFitRankMixtureNL_RemainderSpread(t *testing.T) {
	steps := nlSteps(t)
	o := nlOptions()
	off, offDiag, err := FitRankMixtureNL(steps, o)
	require.NoError(t, err)
	o.RemainderSpread = true
	comp, prom := expHist(1700), expHist(44000)
	dOff, u := off.Draws(5, comp, prom, DefaultMixtureDraws)
	require.Nil(t, u)

	check := func(name string, steps []MixtureStep) RankMixture {
		t.Helper()
		m, diag, err := FitRankMixtureNL(steps, o)
		require.NoError(t, err, name)
		assert.Equal(t, offDiag, diag, "%s: the fit window's checks do not move", name)
		return m
	}
	line := check("none recorded", steps)
	assert.Nil(t, line.Parts.Remainder.Shape)
	assert.Contains(t, line.Assumed(), "router remainder drawn as its line")
	same := line
	same.RemainderSpread = false
	assert.Equal(t, off, same, "with no processing recorded the mode changes nothing")

	few := slices.Clone(steps)
	for i := range MinRemainderSpreadSteps - 1 {
		few[i].RouterProcessingHist, few[i].RouterProcessingMean = processingHist(), 0.3
	}
	assert.Nil(t, check("too few", few).Parts.Remainder.Shape)

	all := slices.Clone(steps)
	for i := range all {
		all[i].RouterProcessingHist, all[i].RouterProcessingMean = processingHist(), 0.3+0.01*float64(i%3)
	}
	on := check("every step", all)
	r := on.Parts.Remainder
	require.NotNil(t, r.Shape)
	assert.InDelta(t, 0.31, r.ShapeMean, 1e-12, "the median of the steps' means")
	want, _ := vincentShape([]map[float64]float64{processingHist()})
	assert.InDeltaSlice(t, want, r.Shape, 1e-12)
	assert.Contains(t, on.Assumed(), "router remainder spread as the router's measured request processing")
	dOn, u := on.Draws(5, comp, prom, DefaultMixtureDraws)
	require.Nil(t, u)
	require.NotNil(t, dOn.Z)
	_, qOff := dOff.Stats(off.A0, off.Beta, 0.95)
	_, qOn := dOn.Stats(on.A0, on.Beta, 0.95)
	assert.Greater(t, qOn[0], qOff[0], "the measured spread lengthens the tail")
	assert.Equal(t, dOff.QP, dOn.QP, "the queue part does not move")
}

// MixtureStepOf pools the router's processing histogram as the engines'
// and takes the mean of the cycles that read a processing mean.
func TestMixtureStepOf_PoolsRouterProcessing(t *testing.T) {
	c1 := MixtureCycle{RouterProcessingHist: map[float64]float64{0.1: 1, math.Inf(1): 2}, RouterProcessingMean: 0.2}
	c2 := MixtureCycle{RouterProcessingHist: map[float64]float64{0.1: 3, math.Inf(1): 4}}
	s := MixtureStepOf([]MixtureCycle{c1, c2}, time.Unix(100, 0))
	assert.Equal(t, map[float64]float64{0.1: 2, math.Inf(1): 3}, s.RouterProcessingHist)
	assert.Equal(t, 0.2, s.RouterProcessingMean)
	s = MixtureStepOf([]MixtureCycle{{}}, time.Unix(100, 0))
	assert.Nil(t, s.RouterProcessingHist)
	assert.Zero(t, s.RouterProcessingMean)
}
