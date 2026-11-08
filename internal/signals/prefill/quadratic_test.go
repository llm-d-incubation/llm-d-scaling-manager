package prefill

import (
	"encoding/csv"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle/settlerow"
)

// The curved fit of a (fixed.go): the quadratic s0 + b*I + c*I*I through the
// prompt-length clusters' medians, weighted by relative error, where the
// minutes carry it; the line otherwise; either refused when its intercept is
// extrapolated from prompts all longer than MaxAShortestPrompt; and the
// curve's refused when its standard error is above MaxAInterceptRelSE of
// it.

// curveMinutes is one low-load cycle per prompt length, each minute, on a
// pod whose service time is s0 + b*I + c*I*I and whose overhead is 2.5 ms,
// at 0.02 requests/s so that a second-long prompt is still low load.
func curveMinutes(s0, b, c float64, prompts ...float64) []Cycle {
	out := make([]Cycle, len(prompts))
	for i, p := range prompts {
		out[i] = cycle(time.Time{}, lowLoad(p, s0+b*p+c*p*p, 0.02, 0.0025))
	}
	return out
}

// steadyLevels is each prompt length held for n minutes: steadyPrompts with
// a longer hold. The steady gate keeps n-2 of each level after the first,
// and n-1 of the first.
func steadyLevels(n int, levels ...float64) []float64 {
	out := make([]float64, 0, n*len(levels))
	for _, l := range levels {
		for range n {
			out = append(out, l)
		}
	}
	return out
}

// lineOnly is cs with the quadratic off.
func lineOnly(cs []Cycle) []Cycle {
	for i := range cs {
		cs[i].ALineOnly = true
	}
	return cs
}

// exactA, exactB and exactC are the weighted least-squares curve of
// TestQuadratic_TheSolveIsExactAtEveryScale's four clusters, worked in
// rational arithmetic (scratch check2.py).
const (
	exactA = 0.008226419570117233
	exactB = 6.434897146988352e-07
	exactC = 2.0049477264189214e-10
)

// runs 3 and 4's pod, as the pooled fit of the TTFT recordings measured it:
// Qwen3-0.6B at TP1, about 8 ms + 7.2e-7 s/token + 2.0e-10 s/token^2.
const (
	podS0 = 0.008
	podB  = 7.2e-7
	podC  = 2.0e-10
)

// Known truth: a pod whose service time is a convex quadratic, swept over
// four prompt levels 1k to 37k, gives back s0, b and c, and a adds the
// least overhead. The line through the same minutes meets the axis below
// zero and is refused, as P1 shipped refused run 3.
func TestQuadratic_RecoversTheCurve(t *testing.T) {
	prompts := steadyLevels(5, 1000, 8000, 30000, 37000)
	est := run(NewObserver(), curveMinutes(podS0, podB, podC, prompts...)...)
	require.Empty(t, est.ARefused)
	assert.Equal(t, AModelQuadratic, est.AModel)
	assert.Empty(t, est.ALineReason)
	assert.Equal(t, 4, est.AClusters)
	assert.Equal(t, 1000.0, est.AShortestPrompt)
	assert.False(t, est.AExtrapolated)
	assert.InDelta(t, podS0, est.AIntercept, 1e-9)
	assert.InEpsilon(t, podB, est.ASlope, 1e-6)
	assert.InEpsilon(t, podC, est.ACurve, 1e-6)
	assert.Greater(t, est.AInterceptSE, 0.0, "clusters with no spread read the floor's error")
	assert.LessOrEqual(t, est.AInterceptSE, MaxAInterceptRelSE*est.AIntercept)
	assert.InDelta(t, podS0+0.0025, est.A, 1e-9)
	// No V_P, so no prompt length to read the curve's bound at: none.
	assert.Zero(t, est.SlopeBound)
	lineIntercept, _ := ols(lowLoadXs(prompts), lowLoadYs(prompts, podS0, podB, podC))

	off := run(NewObserver(), lineOnly(curveMinutes(podS0, podB, podC, prompts...))...)
	assert.Equal(t, AModelLinear, off.AModel)
	assert.Equal(t, LineOnly, off.ALineReason)
	assert.Equal(t, NonPositiveIntercept, off.ARefused, "the line's intercept %.4f", lineIntercept)
	assert.Zero(t, off.A)
}

// lowLoadXs is the prompt lengths of steadyLevels the steady gate admits,
// and lowLoadYs their service times on the curve.
func lowLoadXs(prompts []float64) []float64 {
	var out []float64
	for i, p := range prompts {
		if i == 0 || p != prompts[i-1] || (i > 1 && p != prompts[i-2]) {
			continue
		}
		out = append(out, p)
	}
	return out
}

func lowLoadYs(prompts []float64, s0, b, c float64) []float64 {
	xs := lowLoadXs(prompts)
	out := make([]float64, len(xs))
	for i, p := range xs {
		out[i] = s0 + b*p + c*p*p
	}
	return out
}

// On random convex pods swept over four or five levels from a shortest at
// most MaxAShortestPrompt, spanning at least MinQuadraticSpread, the curve
// gives back s0, b and c.
func TestQuadratic_PropertyRecoversRandomCurves(t *testing.T) {
	for seed := range uint64(200) {
		rng := rand.New(rand.NewPCG(seed, 23))
		s0 := 0.001 + 0.02*rng.Float64()
		b := 1e-7 + 5e-6*rng.Float64()
		c := 1e-12 + 5e-10*rng.Float64()
		lo := 500 + 2000*rng.Float64()
		// Each level more than MaxPromptDrift from the one before it.
		levels := []float64{lo, lo * (1.3 + 1.2*rng.Float64()), lo * (MinQuadraticSpread + 5*rng.Float64())}
		levels = append(levels, levels[2]*(1.3+rng.Float64()))
		if rng.IntN(2) == 0 {
			levels = append(levels, levels[3]*(1.3+rng.Float64()))
		}
		est := run(NewObserver(), curveMinutes(s0, b, c, steadyLevels(5, levels...)...)...)
		require.Empty(t, est.ARefused, "seed %d", seed)
		require.Equal(t, AModelQuadratic, est.AModel, "seed %d", seed)
		require.InDelta(t, s0, est.AIntercept, 1e-9, "seed %d", seed)
		require.InEpsilon(t, b, est.ASlope, 1e-5, "seed %d", seed)
		require.InEpsilon(t, c, est.ACurve, 1e-5, "seed %d", seed)
	}
}

// A straight-line pod still uses the line where the minutes cannot carry
// the curve: two prompt levels (ols-sweep's 1k and 6k), three levels, four
// spanning under MinQuadraticSpread, and three whose middle level holds too
// few minutes to count. Each gives the line's a exactly.
func TestQuadratic_TheLineWhereTheClustersCannotCarryIt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		prompts  []float64
		why      LineReason
		clusters int
	}{
		{"two levels", steadyLevels(5, 1000, 6000), TooFewPromptClusters, 2},
		{"three levels", steadyLevels(5, 1000, 3000, 6000), TooFewPromptClusters, 3},
		{"four levels under 3x", steadyLevels(5, 1000, 1400, 2000, 2900), NarrowPromptClusters, 4},
		// The 3000-token level held three minutes: the steady gate keeps
		// one, under MinQuadraticClusterMinutes.
		{"a thin middle level", append(append(steadyLevels(5, 1000), steadyLevels(3, 3000)...), steadyLevels(5, 6000)...),
			TooFewPromptClusters, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			est := run(NewObserver(), curveMinutes(0.008, 2e-6, 0, tc.prompts...)...)
			require.Empty(t, est.ARefused)
			assert.Equal(t, AModelLinear, est.AModel)
			assert.Equal(t, tc.why, est.ALineReason)
			assert.Equal(t, tc.clusters, est.AClusters)
			assert.Equal(t, 1000.0, est.AShortestPrompt, "the line's second shortest minute")
			assert.InDelta(t, 0.008, est.AIntercept, 1e-12)
			assert.InDelta(t, 2e-6, est.ASlope, 1e-15)
			assert.Zero(t, est.ACurve)
			assert.Zero(t, est.AInterceptSE)
			assert.InDelta(t, 0.0105, est.A, 1e-12)
		})
	}
}

// A fitted c not above zero is not convex: the line. A pod whose per-token
// cost falls with the prompt (concave) reads the line's intercept.
func TestQuadratic_ANonConvexFitIsTheLine(t *testing.T) {
	prompts := steadyLevels(5, 1000, 2000, 8000, 16000)
	est := run(NewObserver(), curveMinutes(0.010, 4e-6, -5e-11, prompts...)...)
	require.Empty(t, est.ARefused)
	assert.Equal(t, AModelLinear, est.AModel)
	assert.Equal(t, ConcaveFit, est.ALineReason)
	assert.Equal(t, 4, est.AClusters)
	assert.Zero(t, est.ACurve)
	want, _ := ols(lowLoadXs(prompts), lowLoadYs(prompts, 0.010, 4e-6, -5e-11))
	assert.InDelta(t, want, est.AIntercept, 1e-12)
}

// A quadratic whose intercept is not above zero is refused, not handed to
// the line: a convex pod with s0 of -1 ms reads AModel quadratic and
// NonPositiveIntercept, with its curve reported. Two slow minutes at the
// shortest level hold the line's intercept above zero and leave that
// cluster's median on the curve, so a fallback to the line would publish an
// a.
func TestQuadratic_ANonPositiveInterceptIsRefused(t *testing.T) {
	const s0, b, c = -0.001, 2e-6, 1e-10
	prompts := append(steadyLevels(7, 2000), steadyLevels(5, 6000, 10000, 16000)...)
	cs := curveMinutes(s0, b, c, prompts...)
	for _, i := range []int{5, 6} {
		cs[i].Rows[0] = lowLoad(2000, 0.03, 0.02, 0.0025)
	}
	var xs, ys []float64
	for i, cy := range cs {
		if i == 0 || (i >= 7 && (i-7)%5 < 2) {
			continue // not steady (steadyLevels)
		}
		xs, ys = append(xs, cy.Rows[0].PrefillComputedTokenRate/cy.Rows[0].RequestRate), append(ys, cy.Rows[0].AvgServiceTime)
	}
	lineIntercept, _ := ols(xs, ys)
	require.Greater(t, lineIntercept, 0.0, "the premise: the line would publish an a")

	est := run(NewObserver(), cs...)
	require.Equal(t, 15, est.ASamples)
	assert.Equal(t, NonPositiveIntercept, est.ARefused)
	assert.Equal(t, AModelQuadratic, est.AModel)
	assert.Empty(t, est.ALineReason)
	assert.False(t, est.AExtrapolated)
	assert.Zero(t, est.AIntercept)
	assert.Zero(t, est.A)
	assert.InEpsilon(t, b, est.ASlope, 1e-6, "the curve is reported with the refusal")
	assert.InEpsilon(t, c, est.ACurve, 1e-6, "the curve is reported with the refusal")
}

// An intercept extrapolated from prompts all longer than MaxAShortestPrompt
// is refused, NoShortPrompts and AExtrapolated, for the curve and the line
// alike; at the threshold it is published. With the quadratic off, the line
// is not held to it, as P1 shipped.
func TestQuadratic_NoShortPromptsIsRefused(t *testing.T) {
	require.Equal(t, 2500.0, MaxAShortestPrompt)
	for _, tc := range []struct {
		name     string
		prompts  []float64
		model    AModel
		shortest float64
		refused  Refusal
	}{
		{"a curve from 2500", steadyLevels(5, 2500, 8000, 16000, 30000), AModelQuadratic, 2500, ""},
		{"a curve from 2600", steadyLevels(5, 2600, 8000, 16000, 30000), AModelQuadratic, 2600, NoShortPrompts},
		{"a line from 2500", steadyLevels(5, 2500, 8000), AModelLinear, 2500, ""},
		{"a line from 2501", steadyLevels(5, 2501, 8000), AModelLinear, 2501, NoShortPrompts},
		{"a line from 3000", steadyLevels(5, 3000, 9000), AModelLinear, 3000, NoShortPrompts},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := podC
			if tc.model == AModelLinear {
				c = 0
			}
			est := run(NewObserver(), curveMinutes(podS0, podB, c, tc.prompts...)...)
			assert.Equal(t, tc.model, est.AModel)
			assert.Equal(t, tc.shortest, est.AShortestPrompt)
			assert.Equal(t, tc.refused, est.ARefused)
			assert.Equal(t, tc.refused != "", est.AExtrapolated)
			if tc.refused == "" {
				assert.InDelta(t, podS0, est.AIntercept, 1e-9)
				return
			}
			assert.Zero(t, est.A)
			assert.Zero(t, est.AIntercept)

			off := run(NewObserver(), lineOnly(curveMinutes(podS0, podB, c, tc.prompts...))...)
			assert.False(t, off.AExtrapolated)
			assert.NotEqual(t, NoShortPrompts, off.ARefused)
		})
	}
}

// The shortest is read past one outlier at each fit's own end. The line's is
// its second shortest minute, as its spread test reads it, so one short
// minute does not open the line to an extrapolated intercept. The curve's is
// its shortest counted cluster's median, not the line's minute: a cluster
// too thin to count does not make the curve's intercept look observed.
func TestQuadratic_TheShortestIsPastOneOutlier(t *testing.T) {
	// One 1000-token minute, steady after two loaded minutes at its length
	// (their occupancy is not low load), before a 3000 to 9000 sweep: the
	// line's second shortest minute is 3000.
	loaded := lowLoad(1000, 0.02, 30, 0.0025)
	cs := []Cycle{cycle(time.Time{}, loaded), cycle(time.Time{}, loaded),
		cycle(time.Time{}, lowLoad(1000, podS0+podB*1000, 0.02, 0.0025))}
	cs = append(cs, curveMinutes(podS0, podB, 0, steadyLevels(5, 3000, 9000)...)...)
	est := run(NewObserver(), cs...)
	assert.Equal(t, 1000.0, est.APromptMin, "the short minute is admitted")
	assert.Equal(t, AModelLinear, est.AModel)
	assert.Equal(t, 3000.0, est.AShortestPrompt)
	assert.Equal(t, NoShortPrompts, est.ARefused)

	// A 1000-token level of two minutes (too thin to count) before a curve
	// over 3000 to 30000: the curve's shortest is the 3000 cluster.
	cs = curveMinutes(podS0, podB, podC, steadyLevels(4, 1000)...)
	cs = append(cs, curveMinutes(podS0, podB, podC, steadyLevels(5, 3000, 6000, 12000, 30000)...)...)
	est = run(NewObserver(), cs...)
	require.Equal(t, AModelQuadratic, est.AModel)
	assert.Equal(t, 5, est.AClusters, "the 1000 cluster, three minutes kept, counts")
	assert.Equal(t, 1000.0, est.AShortestPrompt)
	assert.Empty(t, est.ARefused)
	cs = curveMinutes(podS0, podB, podC, steadyLevels(3, 1000)...)
	cs = append(cs, curveMinutes(podS0, podB, podC, steadyLevels(5, 3000, 6000, 12000, 30000)...)...)
	est = run(NewObserver(), cs...)
	require.Equal(t, AModelQuadratic, est.AModel)
	assert.Equal(t, 4, est.AClusters, "the 1000 cluster, two minutes kept, does not count")
	assert.Equal(t, 3000.0, est.AShortestPrompt)
	assert.Equal(t, NoShortPrompts, est.ARefused)
}

// The thresholds, each at its edge: four clusters are the fewest, three
// minutes the fewest a cluster counts with, and a spread of exactly
// MinQuadraticSpread is enough.
func TestQuadratic_TheThresholds(t *testing.T) {
	require.Equal(t, 4, MinQuadraticClusters)
	require.Equal(t, 3, MinQuadraticClusterMinutes)
	require.Equal(t, 3.0, MinQuadraticSpread)

	pts := func(levels []float64, per []int) ([]float64, []float64) {
		xs, ys := make([]float64, 0, 16), make([]float64, 0, 16)
		for i, l := range levels {
			for range per[i] {
				xs, ys = append(xs, l), append(ys, podS0+podB*l+podC*l*l)
			}
		}
		return xs, ys
	}
	for _, tc := range []struct {
		name   string
		levels []float64
		per    []int
		why    LineReason
	}{
		{"four clusters of three, exactly 3x", []float64{1000, 1500, 2250, 3000}, []int{3, 3, 3, 3}, ""},
		{"just under 3x", []float64{1000, 1500, 2250, 2990}, []int{3, 3, 3, 3}, NarrowPromptClusters},
		{"a cluster of two", []float64{1000, 1500, 2250, 3000}, []int{3, 2, 3, 3}, TooFewPromptClusters},
		{"three clusters", []float64{1000, 2000, 3000}, []int{5, 5, 5}, TooFewPromptClusters},
		// The thin cluster is dropped and the other four still carry it.
		{"five clusters, one thin", []float64{1000, 1500, 2000, 2500, 3000}, []int{3, 1, 3, 3, 3}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			xs, ys := pts(tc.levels, tc.per)
			q, why := fitQuadratic(xs, ys)
			assert.Equal(t, tc.why, why)
			if why == "" {
				assert.InDelta(t, podS0, q.a, 1e-9)
				assert.InEpsilon(t, podC, q.c, 1e-6)
				assert.Equal(t, 1000.0, q.shortest)
			}
		})
	}

	// Clusters break at MaxPromptDrift between neighbours.
	t.Run("cluster edges", func(t *testing.T) {
		cl := promptClusters([]float64{1000, 1050, 1155, 1271}, []float64{1, 2, 3, 4})
		assert.Len(t, cl, 2, "1155 is within 10 percent of 1050, 1271 is not of 1155")
		assert.Equal(t, 3, cl[0].n)
		assert.Equal(t, 1050.0, cl[0].x)
		assert.Equal(t, 2.0, cl[0].y)
	})
}

// One slow minute in a cluster of seven does not move the curve: each
// cluster is its median. Run 3's 22:28 minute read 0.375 s at 38k against
// about 0.30 s at its length.
func TestQuadratic_OneSlowMinuteIsTheMediansNot(t *testing.T) {
	prompts := steadyLevels(7, 1000, 8000, 30000, 37000)
	cs := curveMinutes(podS0, podB, podC, prompts...)
	last := cs[len(cs)-1].Rows[0]
	cs[len(cs)-1].Rows[0] = lowLoad(37000, 1.25*last.AvgServiceTime, 0.02, 0.0025)
	est := run(NewObserver(), cs...)
	require.Empty(t, est.ARefused)
	assert.Equal(t, AModelQuadratic, est.AModel)
	assert.InDelta(t, podS0, est.AIntercept, 1e-9)
}

// The weight is relative error: medians each 5% off, alternately high and
// low, over 1k to 37k. Unweighted, the 37k median's absolute residual (15 ms)
// sets the curve, and its intercept reads 10.9 ms against 8 ms; weighted
// 1/s^2, the intercept stays within the 5% the short medians carry, 8.3 ms
// (worked exactly, scratch check.py).
func TestQuadratic_TheFitIsByRelativeError(t *testing.T) {
	xs := []float64{1000, 2000, 4000, 8000, 16000, 30000, 37000}
	ys := make([]float64, len(xs))
	ones := make([]float64, len(xs))
	ws := make([]float64, len(xs))
	for i, x := range xs {
		f := 1.05
		if i%2 == 1 {
			f = 0.95
		}
		ys[i] = f * (podS0 + podB*x + podC*x*x)
		ones[i], ws[i] = 1, 1/(ys[i]*ys[i])
	}
	weighted, _, _, _, _ := quadratic(xs, ys, ws)
	unweighted, _, _, _, _ := quadratic(xs, ys, ones)
	assert.InDelta(t, 0.008271, weighted, 1e-6, "weighted")
	assert.InDelta(t, 0.010871, unweighted, 1e-6, "unweighted, the premise")
	// Three minutes at each median: fitQuadratic's clusters are the medians.
	mx, my := make([]float64, 0, 3*len(xs)), make([]float64, 0, 3*len(xs))
	for i := range xs {
		mx, my = append(mx, xs[i], xs[i], xs[i]), append(my, ys[i], ys[i], ys[i])
	}
	q, why := fitQuadratic(mx, my)
	require.Empty(t, why)
	assert.InDelta(t, weighted, q.a, 1e-12, "fitQuadratic weights by 1/s^2")
}

// The quadratic solve is exact on a large scale and a small one, prompt
// lengths of a few thousandths to eight million; on four clusters of 1k to
// 40k weighted 1/y^2 with noisy y it agrees with the solve in the scaled
// coordinate, worked in exact arithmetic (scratch check2.py), to 1e-12
// relative; and k, the intercept's weight on each point, gives a back.
func TestQuadratic_TheSolveIsExactAtEveryScale(t *testing.T) {
	for _, scale := range []float64{1e-3, 1, 1e3, 1e6} {
		xs := []float64{1 * scale, 2 * scale, 4 * scale, 8 * scale}
		ys := make([]float64, len(xs))
		ws := []float64{1, 1, 1, 1}
		for i, x := range xs {
			u := x / scale
			ys[i] = 0.5 + 0.25*u + 0.125*u*u
		}
		a, b, c, k, ok := quadratic(xs, ys, ws)
		require.True(t, ok, "scale %g", scale)
		assert.InDelta(t, 0.5, a, 1e-12, "scale %g", scale)
		assert.InDelta(t, 0.25, b*scale, 1e-12, "scale %g", scale)
		assert.InDelta(t, 0.125, c*scale*scale, 1e-12, "scale %g", scale)
		var again float64
		for i := range xs {
			again += k[i] * ys[i]
		}
		assert.InDelta(t, a, again, 1e-12, "scale %g: a from k", scale)
	}

	// Four clusters at 1k, 4k, 16k and 40k, each median 3% off the pod's
	// curve (+, -, +, -), weighted 1/y^2: the exact weighted least-squares
	// answer, from scratch check2.py in rational arithmetic.
	xs := []float64{1000, 4000, 16000, 40000}
	ys := make([]float64, len(xs))
	ws := make([]float64, len(xs))
	for i, x := range xs {
		f := 1.03
		if i%2 == 1 {
			f = 0.97
		}
		ys[i] = f * (podS0 + podB*x + podC*x*x)
		ws[i] = 1 / (ys[i] * ys[i])
	}
	a, b, c, k, ok := quadratic(xs, ys, ws)
	require.True(t, ok)
	assert.InEpsilon(t, exactA, a, 1e-12)
	assert.InEpsilon(t, exactB, b, 1e-12)
	assert.InEpsilon(t, exactC, c, 1e-12)
	var again float64
	for i := range xs {
		again += k[i] * ys[i]
	}
	assert.InEpsilon(t, a, again, 1e-12, "a from k")

	// Singular: two distinct lengths, one, or none.
	for _, xs := range [][]float64{{1, 1, 2, 2}, {5, 5, 5}, {0, 0, 0}} {
		_, _, _, _, ok := quadratic(xs, xs, []float64{1, 1, 1, 1}[:len(xs)])
		assert.False(t, ok, "%v", xs)
	}
}

// The line stands, each for its own reason, where the curve is not one: a
// singular solve (SingularFit), a fitted c not above zero (ConcaveFit),
// and a fitted b not above zero (NegativeSlopeFit). Each is checked through
// fitQuadratic, so the reason is the fit's own.
func TestQuadratic_EachNonCurveHasItsReason(t *testing.T) {
	three := func(levels []float64, f func(float64) float64) ([]float64, []float64) {
		xs, ys := make([]float64, 0, 3*len(levels)), make([]float64, 0, 3*len(levels))
		for _, l := range levels {
			xs, ys = append(xs, l, l, l), append(ys, f(l), f(l), f(l))
		}
		return xs, ys
	}
	// c below zero.
	xs, ys := three([]float64{1000, 2000, 8000, 16000}, func(x float64) float64 { return 0.010 + 4e-6*x - 5e-11*x*x })
	_, why := fitQuadratic(xs, ys)
	assert.Equal(t, ConcaveFit, why)
	// b below zero, c above: a curve whose time per token at short prompts is
	// below zero.
	xs, ys = three([]float64{1000, 2000, 8000, 16000}, func(x float64) float64 { return 0.010 - 1e-7*x + 3e-10*x*x })
	q, why := fitQuadratic(xs, ys)
	assert.Equal(t, NegativeSlopeFit, why)
	assert.Less(t, q.b, 0.0)
	assert.Greater(t, q.c, 0.0)
	// Singular: the solve in the scaled coordinate cannot tell four lengths
	// within 1e-5 of each other apart (the spread test passes them only
	// because its threshold is 0; set by hand here, past the clusters).
	_, _, _, _, ok := quadratic([]float64{1, 1 + 1e-6, 1 + 2e-6, 1 + 3e-6}, []float64{1, 2, 3, 4}, []float64{1, 1, 1, 1})
	assert.False(t, ok, "a near-singular solve is not read")
	// Through fitQuadratic, clusters 10% apart over 3x always solve; the
	// reason is reached when the 1/s^2 weights leave one cluster carrying
	// the solve alone: a 1 ms cluster beside three of 1000 s weighs 1e12
	// times each of them, and three parameters rest on one point.
	xs, ys = three([]float64{1000, 2000, 4000, 8000}, func(x float64) float64 {
		if x == 1000 {
			return 0.001
		}
		return 1000
	})
	_, why = fitQuadratic(xs, ys)
	assert.Equal(t, SingularFit, why)

	// Through the observer: b below zero reads the line.
	est := run(NewObserver(), curveMinutes(0.010, -1e-7, 3e-10, steadyLevels(5, 1000, 2000, 8000, 16000)...)...)
	assert.Equal(t, AModelLinear, est.AModel)
	assert.Equal(t, NegativeSlopeFit, est.ALineReason)
}

// The intercept's standard error, worked on an exact curve where one
// cluster has spread: the within part is the minutes' relative noise pooled
// over the clusters, pi/(2n) for each median, and no lack of fit, since
// that cluster's median is on the curve.
func TestQuadratic_TheStandardErrorIsTheClustersSpread(t *testing.T) {
	levels := []float64{1000, 2000, 4000, 8000}
	xs, ys := make([]float64, 0, 3*len(levels)), make([]float64, 0, 3*len(levels))
	for _, l := range levels {
		y := podS0 + podB*l + podC*l*l
		xs, ys = append(xs, l, l, l), append(ys, y, y, y)
	}
	// The 2000-token cluster reads y(1-d), y, y(1+d): its median y, and a
	// relative sum of squares 2 d^2.
	const d = 0.2
	y2 := ys[3]
	ys[3], ys[5] = y2*(1-d), y2*(1+d)
	q, why := fitQuadratic(xs, ys)
	require.Empty(t, why)
	assert.InDelta(t, podS0, q.a, 1e-12)

	mx, my, mw := make([]float64, 4), make([]float64, 4), make([]float64, 4)
	for i, l := range levels {
		mx[i], my[i] = l, podS0+podB*l+podC*l*l
		mw[i] = 1 / (my[i] * my[i])
	}
	_, _, _, k, ok := quadratic(mx, my, mw)
	require.True(t, ok)
	noise := 2 * d * d / 8 // pooled over 4 clusters of 3: 8 degrees of freedom
	var v float64
	for i := range mx {
		v += k[i] * k[i] * math.Pi / 6 * noise * my[i] * my[i]
	}
	assert.InEpsilon(t, math.Sqrt(v), q.se, 1e-9)
}

// Identical minutes do not give a standard error of zero: each median is
// read with at least MinMedianRelSD of noise. Four levels whose minutes all
// read exactly the curve publish the intercept with an error, and the error
// is the floor's.
func TestQuadratic_IdenticalMinutesHaveAnError(t *testing.T) {
	require.Equal(t, 0.01, MinMedianRelSD)
	est := run(NewObserver(), curveMinutes(podS0, podB, podC, steadyLevels(5, 1000, 4000, 16000, 37000)...)...)
	require.Empty(t, est.ARefused)
	assert.Greater(t, est.AInterceptSE, 0.0)

	xs, ys := make([]float64, 0, 12), make([]float64, 0, 12)
	for _, l := range []float64{1000, 4000, 16000, 37000} {
		y := podS0 + podB*l + podC*l*l
		xs, ys = append(xs, l, l, l), append(ys, y, y, y)
	}
	q, why := fitQuadratic(xs, ys)
	require.Empty(t, why)
	mx, my, mw := []float64{1000, 4000, 16000, 37000}, make([]float64, 4), make([]float64, 4)
	for i, l := range mx {
		my[i] = podS0 + podB*l + podC*l*l
		mw[i] = 1 / (my[i] * my[i])
	}
	_, _, _, k, _ := quadratic(mx, my, mw)
	var v float64
	for i := range mx {
		v += k[i] * k[i] * math.Pi / 6 * MinMedianRelSD * MinMedianRelSD * my[i] * my[i]
	}
	assert.InEpsilon(t, math.Sqrt(v), q.se, 1e-9, "the floor's error")
}

// Noise a whole cluster shares (each level's minutes offset together) is
// seen by the lack of fit, not the clusters' spread: the standard error
// with it is larger than with the clusters' spread alone.
func TestQuadratic_SharedClusterNoiseRaisesTheError(t *testing.T) {
	levels := []float64{1000, 2000, 4000, 8000, 16000, 30000, 37000}
	offsets := []float64{0.03, -0.03, 0.03, -0.03, 0.03, -0.03, 0.03}
	xs, ys := make([]float64, 0, 21), make([]float64, 0, 21)
	for i, l := range levels {
		y := (podS0 + podB*l + podC*l*l) * (1 + offsets[i])
		xs, ys = append(xs, l, l, l), append(ys, y*0.99, y, y*1.01)
	}
	q, why := fitQuadratic(xs, ys)
	require.Empty(t, why)
	// The within part alone: the clusters' pooled noise, no lack of fit.
	mx, my, mw := make([]float64, len(levels)), make([]float64, len(levels)), make([]float64, len(levels))
	for i, l := range levels {
		mx[i], my[i] = l, ys[3*i+1]
		mw[i] = 1 / (my[i] * my[i])
	}
	_, _, _, k, _ := quadratic(mx, my, mw)
	noise := math.Max(2*0.01*0.01/2, MinMedianRelSD*MinMedianRelSD)
	var within float64
	for i := range mx {
		within += k[i] * k[i] * math.Pi / 6 * noise * my[i] * my[i]
	}
	assert.Greater(t, q.se, 2*math.Sqrt(within), "the lack of fit is at least double the within part here")
}

// Known truth with noise, its calibration: random convex pods swept over
// four levels from 1k, each minute's service time off by a seeded
// log-normal noise of 1% to 20%. Over these 2000 seeds the standard error
// is conservative, about 1.2 times the error it stands for: the root mean
// square of |a - s0|/SE is 0.83 (1 would be exact), and one standard error
// covers 77.9% of the published intercepts (68.3% would be exact). The
// excess is chance lack of fit read as shared noise, the safe side.
//
// The band is set about that measurement, with the unsafe side tight: the
// coverage must be at least 0.70, which is the standard error scaled by
// 0.85 (0.82 reads the exact 0.683); a standard error smaller than the
// error it stands for fails. At most 0.85: scaled by 1.2 (0.871 at 1.25),
// so a standard error grown to refuse far more than it should fails too.
// The root mean square ratio is between 0.6 and 1. Three standard errors
// cover at least 99%.
func TestQuadratic_TheStandardErrorIsCalibrated(t *testing.T) {
	var published, in1, in3 int
	var z2 float64
	for seed := range uint64(2000) {
		rng := rand.New(rand.NewPCG(seed, 77))
		noise := 0.01 * math.Pow(20, rng.Float64())
		levels := []float64{1000, 2000 + 1000*rng.Float64(), 8000 + 8000*rng.Float64(), 30000 + 8000*rng.Float64()}
		prompts := steadyLevels(7, levels...)
		cs := make([]Cycle, len(prompts))
		for i, p := range prompts {
			svc := (podS0 + podB*p + podC*p*p) * math.Exp(noise*rng.NormFloat64())
			cs[i] = cycle(time.Time{}, lowLoad(p, svc, 0.02, 0.0025))
		}
		est := run(NewObserver(), cs...)
		if est.AModel != AModelQuadratic || est.ARefused != "" {
			continue
		}
		published++
		d := math.Abs(est.AIntercept - podS0)
		if d <= est.AInterceptSE {
			in1++
		}
		if d <= 3*est.AInterceptSE {
			in3++
		}
		z2 += (d / est.AInterceptSE) * (d / est.AInterceptSE)
	}
	c1, c3 := float64(in1)/float64(published), float64(in3)/float64(published)
	rms := math.Sqrt(z2 / float64(published))
	t.Logf("published %d of 2000: 1 SE covers %.3f, 3 SE %.3f; rms |error|/SE %.3f", published, c1, c3, rms)
	require.Greater(t, published, 1900)
	assert.GreaterOrEqual(t, c1, 0.70, "1-SE coverage: the standard error is not smaller than the error")
	assert.LessOrEqual(t, c1, 0.85, "1-SE coverage: nor much larger")
	assert.GreaterOrEqual(t, c3, 0.99, "3-SE coverage")
	assert.Less(t, rms, 1.0, "rms |error|/SE")
	assert.Greater(t, rms, 0.6, "rms |error|/SE")
}

// Known truth with noise, its refusals: noise up to 60% a minute. A
// published intercept has a standard error at most half of it; one refused
// AUncertain publishes nothing.
func TestQuadratic_PropertyNoisyCurvesAreRefusedWhenUncertain(t *testing.T) {
	var published, refused int
	for seed := range uint64(400) {
		rng := rand.New(rand.NewPCG(seed, 31))
		noise := 0.01 * math.Pow(60, rng.Float64())
		levels := []float64{1000, 2000 + 1000*rng.Float64(), 8000 + 8000*rng.Float64(), 30000 + 8000*rng.Float64()}
		prompts := steadyLevels(7, levels...)
		cs := make([]Cycle, len(prompts))
		for i, p := range prompts {
			svc := (podS0 + podB*p + podC*p*p) * math.Exp(noise*rng.NormFloat64())
			cs[i] = cycle(time.Time{}, lowLoad(p, svc, 0.02, 0.0025))
		}
		est := run(NewObserver(), cs...)
		if est.AModel != AModelQuadratic {
			continue
		}
		switch est.ARefused {
		case "":
			published++
			require.LessOrEqual(t, est.AInterceptSE, MaxAInterceptRelSE*est.AIntercept, "seed %d", seed)
		case AUncertain:
			refused++
			require.Greater(t, est.AInterceptSE, 0.0, "seed %d", seed)
			require.Zero(t, est.A, "seed %d", seed)
			require.Zero(t, est.AIntercept, "seed %d", seed)
		}
	}
	t.Logf("published %d, refused AUncertain %d, of 400", published, refused)
	require.Greater(t, published, 250, "most are published")
	require.GreaterOrEqual(t, refused, 10, "the noisiest are refused")
}

// A positive intercept whose standard error is above half of it is refused,
// AUncertain, with the curve and the error reported; one at or under half
// is published.
func TestQuadratic_AnUncertainInterceptIsRefused(t *testing.T) {
	// The shortest cluster's four admitted minutes at -k, 0, +k and 0 of its
	// service time (a minute at 1 - k below zero is clamped by its sign, so
	// k above 1 is a minute read slow by k): its median is on the curve, and
	// its spread, pooled with the other clusters', sets the standard error.
	// At k 3 that is above 4 ms against a of 8 ms; at 5%, under it.
	build := func(k float64) []Cycle {
		cs := curveMinutes(podS0, podB, podC, steadyLevels(5, 1000, 4000, 16000, 37000)...)
		for i, f := range map[int]float64{1: -math.Min(k, 0.5), 2: 0, 3: k, 4: 0} {
			rm := cs[i].Rows[0]
			cs[i].Rows[0] = lowLoad(1000, rm.AvgServiceTime*(1+f), 0.02, 0.0025)
		}
		return cs
	}
	loose := run(NewObserver(), build(3)...)
	assert.Equal(t, AModelQuadratic, loose.AModel)
	assert.Equal(t, AUncertain, loose.ARefused)
	assert.Greater(t, loose.AInterceptSE, MaxAInterceptRelSE*podS0)
	assert.Zero(t, loose.A)
	assert.Zero(t, loose.AIntercept)

	tight := run(NewObserver(), build(0.05)...)
	assert.Empty(t, tight.ARefused)
	assert.InDelta(t, podS0, tight.AIntercept, 1e-9)
	assert.Greater(t, tight.AInterceptSE, 0.0)
	assert.LessOrEqual(t, tight.AInterceptSE, MaxAInterceptRelSE*tight.AIntercept)
}

// A cluster is at most MaxClusterWidth wide: a slow drift of 3% a minute
// from 8k to 37k links every neighbour under MaxPromptDrift, and without the
// cap is one cluster; with it, it is cut every 1.2x. A level's own minutes,
// within a few percent, stay one cluster.
func TestQuadratic_AClusterIsAtMostMaxClusterWidthWide(t *testing.T) {
	require.Equal(t, 1.2, MaxClusterWidth)
	var xs, ys []float64
	for x := 8000.0; x <= 37000; x *= 1.03 {
		xs, ys = append(xs, x), append(ys, podS0+podB*x+podC*x*x)
	}
	cl := promptClusters(xs, ys)
	require.Greater(t, len(cl), 5, "the drift is cut")
	start := 0
	for _, c := range cl {
		lo, hi := xs[start], xs[start+c.n-1]
		assert.LessOrEqual(t, hi, MaxClusterWidth*lo, "cluster %.0f..%.0f", lo, hi)
		start += c.n
	}
	assert.Equal(t, len(xs), start)

	level := promptClusters([]float64{8000, 8100, 8250, 8400, 8600}, []float64{1, 2, 3, 4, 5})
	assert.Len(t, level, 1, "a level's minutes, 8000 to 8600, are one cluster")
	edge := promptClusters([]float64{1000, 1090, 1180, 1200, 1210}, []float64{1, 2, 3, 4, 5})
	assert.Len(t, edge, 2, "1210 is over 1.2x of 1000")
	assert.Equal(t, 4, edge[0].n, "1200 is exactly 1.2x of 1000")
}

// With the curve, the slope bound on V_P is 1/(b + c*I) at the prompt
// length V_P was read at: a convex pod's busy minutes at 30k read 1/(b +
// c*30000) on the curve the low-load minutes fit, which the line's 1/slope
// is below; so the line flags it above the bound and the curve does not.
// With no prompt length the curve sets no bound and flags nothing.
func TestQuadratic_TheSlopeBoundIsTheCurvesAtVPsLength(t *testing.T) {
	low := curveMinutes(podS0, podB, podC, steadyLevels(5, 1000, 4000, 16000, 37000)...)
	vp := 1 / (podB + podC*30000) // the curve's marginal rate at 30k
	busy := func(prompt float64) Cycle {
		rm := base("p-0")
		rm.PrefillComputedTokenRate, rm.RequestRate = vp, vp/prompt
		rm.QueueLength, rm.QueueMin, rm.QueueMinKnown = 3, 1, true
		rm.AvgServiceTime, rm.AvgTTFT = 0.3, 1
		c := cycle(time.Time{}, rm)
		c.QueueMin = map[string]float64{"p-0": 1}
		return c
	}
	feed := func(lineOnly bool, prompt float64) Estimate {
		cs := append([]Cycle{}, low...)
		for range MinBusyMinutes {
			cs = append(cs, busy(prompt))
		}
		for i := range cs {
			cs[i].ALineOnly = lineOnly
		}
		return run(NewObserver(), cs...)
	}

	on := feed(false, 30000)
	require.Equal(t, SourceBusyMinutes, on.VPSource)
	require.InDelta(t, vp, on.VP, 1e-6)
	assert.InDelta(t, 30000, on.VPPromptTokens, 1e-6)
	assert.Equal(t, AModelQuadratic, on.AModel)
	assert.InEpsilon(t, vp, on.SlopeBound, 1e-9, "the curve's marginal rate at 30k")
	// V_P is the bound itself, to rounding: not above it by more.
	assert.InEpsilon(t, on.SlopeBound, on.VP, 1e-9)

	off := feed(true, 30000)
	assert.Less(t, off.SlopeBound, vp, "the line's chord reads under the curve at 30k")
	assert.True(t, off.VPAboveSlopeBound, "the spurious flag the curve removes")

	// A V_P at 1k is far under the curve's bound there.
	short := feed(false, 1000)
	assert.InEpsilon(t, 1/(podB+podC*1000), short.SlopeBound, 1e-9)
	assert.False(t, short.VPAboveSlopeBound)

	// No prompt length: no bound and no flag.
	nolen := feed(false, 30000)
	nolen.VPPromptTokens = 0
	nolen.curveBound()
	assert.Zero(t, nolen.SlopeBound)
	assert.False(t, nolen.VPAboveSlopeBound)
}

// VPPromptTokens is the mean prompt length of the minutes V_P rests on, per
// source, and zero when one of them read none.
func TestCapacity_VPPromptTokensIsTheMeanOfItsMinutes(t *testing.T) {
	t.Run("meanPrompt", func(t *testing.T) {
		assert.Equal(t, 2000.0, meanPrompt([]float64{1000, 2000, 3000}))
		assert.Zero(t, meanPrompt([]float64{1000, 0, 3000}), "a minute that read none")
		assert.Zero(t, meanPrompt([]float64{0}), "the only minute read none")
	})

	// queuedAt is a queued row computing rate tokens/s of prompt-token
	// prompts, with its queue minimum read above zero (a busy minute).
	queuedAt := func(rate, prompt float64) Cycle {
		rm := base("p-0")
		rm.PrefillComputedTokenRate, rm.RequestRate = rate, rate/prompt
		rm.QueueLength, rm.QueueMin, rm.QueueMinKnown = 3, 1, true
		rm.AvgServiceTime, rm.AvgTTFT = 0.05, 0.5
		c := cycle(time.Time{}, rm)
		c.QueueMin = map[string]float64{"p-0": 1}
		return c
	}

	t.Run("busy minutes", func(t *testing.T) {
		cs := []Cycle{queuedAt(24000, 1000), queuedAt(24000, 2000), queuedAt(24000, 3000),
			queuedAt(24000, 4000), queuedAt(24000, 5000)}
		o := NewObserver()
		est := run(o, cs...)
		require.Equal(t, SourceBusyMinutes, est.VPSource)
		assert.Equal(t, 3000.0, est.VPPromptTokens, "the mean of the five busy minutes")
		bm := busyMedian(o.keys["k"], t0.Add(4*minute))
		assert.Equal(t, busy{rate: 24000, n: 5, prompt: 3000}, bm)
	})

	t.Run("the peak's last PeakRun minutes only", func(t *testing.T) {
		// Five queued minutes in a row, no queue minimum (no busy minutes):
		// the peak is the least of the best three, 22000, from the last three
		// at 3000, 4000 and 5000 tokens, not the first two at 1000 and 2000.
		cs := make([]Cycle, 0, 5)
		for i, r := range []float64{20000, 21000, 22000, 23000, 24000} {
			c := queuedAt(r, float64(1000*(i+1)))
			c.QueueMin = nil
			cs = append(cs, c)
		}
		est := run(NewObserver(), cs...)
		require.Equal(t, SourcePeakUnderQueue, est.VPSource)
		require.Equal(t, 22000.0, est.VP)
		assert.Equal(t, 4000.0, est.VPPromptTokens, "the mean of the run's last PeakRun minutes")
	})

	t.Run("the back-pressure bound records none", func(t *testing.T) {
		c := cycle(t0, lowLoad(1000, 0.01, 1, 0.003))
		c.Bound = Bound{Tokens: 30000, Samples: MinBoundSamples}
		est := NewObserver().Observe(c)
		require.Equal(t, SourceBackPressureBound, est.VPSource)
		assert.Zero(t, est.VPPromptTokens)
	})
}

// runRow is one row of testdata/run34-prefill-rows.csv: every prefill row
// runs 3 and 4 of the TTFT track carry (one P/D prefill pod of Qwen3-0.6B at
// TP1 on an H200, at low load, run 3 over 8k, 30k and 37k prompts and run 4
// over 1k, 2k, 4k and 16k, a cycle every 15 s), with the fields the observer
// and the settle tracker read. The recordings themselves stay out of the
// tree.
type runRow struct {
	run string
	now time.Time
	rm  domain.ReplicaMetrics
}

// runRows is the fixture's rows of the runs named, in order.
func runRows(t *testing.T, runs ...string) []runRow {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "run34-prefill-rows.csv"))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	recs, err := csv.NewReader(f).ReadAll()
	require.NoError(t, err)
	require.Equal(t, []string{"run", "now", "ready", "ready_since", "kv", "queue", "request_rate", "computed_rate",
		"avg_input_recent", "service_time", "ttft"}, recs[0])
	num := func(s string) float64 {
		v, err := strconv.ParseFloat(s, 64)
		require.NoError(t, err)
		return v
	}
	var out []runRow
	for _, name := range runs {
		for _, r := range recs[1:] {
			if r[0] != name {
				continue
			}
			now, err := time.Parse(time.RFC3339Nano, r[1])
			require.NoError(t, err)
			since, err := time.Parse(time.RFC3339Nano, r[3])
			require.NoError(t, err)
			rm := base("prefill-" + name)
			rm.Ready, rm.ReadySince = r[2] == "true", since
			rm.KvCacheUsage = num(r[4])
			rm.QueueLength = int(num(r[5]))
			rm.RequestRate, rm.PrefillComputedTokenRate = num(r[6]), num(r[7])
			rm.AvgInputTokensRecent = num(r[8])
			rm.AvgServiceTime, rm.AvgTTFT = num(r[9]), num(r[10])
			out = append(out, runRow{run: name, now: now, rm: rm})
		}
	}
	return out
}

// replayRuns feeds the runs' rows to one observer through one settle
// tracker at the recordings' 15 s interval, as the controller observed
// them, and returns the last estimate. Each run's pod is its own replica of
// one key. A later run is moved in time to start five minutes after the
// one before it ends, so that the earlier run's minutes are inside
// Retention when it ends (the two were recorded ten hours apart) and the
// pooled fit is over both.
func replayRuns(t *testing.T, lineOnly bool, runs ...string) Estimate {
	t.Helper()
	tr := settle.NewTracker()
	tr.SetInterval(15 * time.Second)
	roles := map[string]string{"p": domain.RolePrefill}
	o := NewObserver()
	var est Estimate
	var shift time.Duration
	var last time.Time
	prev := ""
	for _, r := range runRows(t, runs...) {
		if r.run != prev {
			if !last.IsZero() {
				shift = last.Add(5 * time.Minute).Sub(r.now)
			}
			prev = r.run
		}
		now := r.now.Add(shift)
		r.rm.ReadySince = r.rm.ReadySince.Add(shift)
		tr.Observe("ns/m", settlerow.Of([]domain.ReplicaMetrics{r.rm}, roles), now)
		est = o.Observe(Cycle{
			Key: "k", Engine: inferenceengine.EngineVLLM, Disaggregated: true, Rows: []domain.ReplicaMetrics{r.rm},
			Settle: &Settle{View: tr, Model: "ns/m"}, ALineOnly: lineOnly, Now: now,
		})
		last = now
	}
	return est
}

// Runs 3 and 4 pooled (31 admitted minutes in 7 clusters, 1k to 38k): the
// curve gives a's intercept at 7.95 ms (standard error 0.47 ms), against
// 7.98 ms for the relative-error fit of all 185 low-load rows (fit.py) and
// 8.36 ms for ols-sweep's line. The tolerance is 1.5 ms either side of
// 8.0 ms. The line P1 shipped refuses the same minutes
// (NonPositiveIntercept).
func TestQuadratic_Runs3And4PooledGiveA(t *testing.T) {
	require.Len(t, runRows(t, "run3"), 93, "run 3's prefill rows")
	require.Len(t, runRows(t, "run4"), 105, "run 4's prefill rows")

	off := replayRuns(t, true, "run3", "run4")
	assert.Equal(t, NonPositiveIntercept, off.ARefused, "the line, as P1 shipped")
	assert.Equal(t, 31, off.ASamples)

	est := replayRuns(t, false, "run3", "run4")
	require.Empty(t, est.ARefused)
	assert.Equal(t, AModelQuadratic, est.AModel)
	assert.Equal(t, 31, est.ASamples)
	assert.Equal(t, 7, est.AClusters)
	assert.False(t, est.AExtrapolated)
	assert.InDelta(t, 1000, est.AShortestPrompt, 10)
	assert.InDelta(t, 0.008, est.AIntercept, 0.0015, "a's intercept")
	assert.InDelta(t, 0.00047, est.AInterceptSE, 0.00003, "its standard error")
	assert.LessOrEqual(t, est.AInterceptSE, MaxAInterceptRelSE*est.AIntercept)
	assert.InEpsilon(t, podB, est.ASlope, 0.1, "b")
	assert.InEpsilon(t, podC, est.ACurve, 0.05, "c")
	assert.True(t, est.AOverheadKnown)
	assert.InDelta(t, est.AIntercept+est.AOverhead, est.A, 1e-12)
	// No V_P, so the curve sets no bound; the line's is 1/slope.
	assert.Zero(t, est.SlopeBound)
	assert.Greater(t, off.SlopeBound, 0.0)
	t.Logf("runs 3+4: a %.5f s = intercept %.5f (SE %.5f) + overhead %.5f; b %.4e s/token, c %.4e s/token^2; %d minutes in %d clusters, shortest %.0f, prompts %.0f..%.0f",
		est.A, est.AIntercept, est.AInterceptSE, est.AOverhead, est.ASlope, est.ACurve, est.ASamples, est.AClusters,
		est.AShortestPrompt, est.APromptMin, est.APromptMax)

	// Run 4 alone, 1k to 16k in four clusters, agrees: 8.67 ms (SE 0.52).
	r4 := replayRuns(t, false, "run4")
	require.Empty(t, r4.ARefused)
	assert.Equal(t, AModelQuadratic, r4.AModel)
	assert.Equal(t, 4, r4.AClusters)
	assert.InDelta(t, 0.008, r4.AIntercept, 0.0015, "run 4's intercept")
	assert.InDelta(t, 0.00052, r4.AInterceptSE, 0.00003, "run 4's standard error")
	assert.LessOrEqual(t, r4.AInterceptSE, MaxAInterceptRelSE*r4.AIntercept)
	t.Logf("run 4: intercept %.5f (SE %.5f)", r4.AIntercept, r4.AInterceptSE)
}

// Run 3 alone (8k to 37k) is refused, not published: three clusters is the
// line, and the line's intercept is extrapolated from 8k, past
// MaxAShortestPrompt. Fitted through its three medians the curve gave
// 3.69 ms, half the pooled 8 ms, the unsafe side.
func TestQuadratic_Run3AloneIsRefused(t *testing.T) {
	est := replayRun3Alone(t)
	assert.Equal(t, NoShortPrompts, est.ARefused)
	assert.True(t, est.AExtrapolated)
	assert.Equal(t, AModelLinear, est.AModel)
	assert.Equal(t, TooFewPromptClusters, est.ALineReason)
	assert.Equal(t, 3, est.AClusters)
	assert.Equal(t, 15, est.ASamples)
	assert.Greater(t, est.AShortestPrompt, 7900.0)
	assert.Zero(t, est.A)
	assert.Zero(t, est.AIntercept)
}

// replayRun3Alone is run 3's rows replayed alone, the quadratic on.
func replayRun3Alone(t *testing.T) Estimate {
	t.Helper()
	return replayRuns(t, false, "run3")
}
