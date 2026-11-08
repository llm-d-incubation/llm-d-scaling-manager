package prefill

import (
	"math"
	"slices"
)

// The fixed cost a (slo-capacity-planner-ttft.md, "What is derivable today"
// and "Prefill capacity").
//
// On a P/D prefill pod the sidecar asks for one token, so the service time
// (AvgServiceTime) is the prefill time. At low load a prompt has a step to
// itself, so a fit of the service time against the effective prompt length
// over low-load minutes gives the fixed part s0 as its intercept. The
// per-token cost rises with the prompt, from attention, so a lone prompt runs
// as T(I) = s0 + b*I + c*I*I with c >= 0. Over short prompts c*I*I is lost in
// the noise and the straight line s0 + b*I fits; over long ones a straight
// line through the curve's chord meets the axis below zero (run 3 of the
// TTFT recordings, 8k to 37k prompts: -52 ms).
//
// So the curve is fitted where the minutes can carry it, and the line
// everywhere else, as P1 shipped:
//
//   - The admitted minutes are grouped into prompt-length clusters
//     (promptClusters). Only a cluster of at least
//     MinQuadraticClusterMinutes minutes counts.
//   - A cluster is at most MaxClusterWidth wide (longest over shortest), so
//     a continuum of lengths, or a slow drift across a sweep, is cut into
//     several clusters instead of chaining into one.
//   - At least MinQuadraticClusters of them, the longest's median prompt at
//     least MinQuadraticSpread times the shortest's: the quadratic is the
//     curve through each counted cluster's median prompt and median service
//     time, weighted by relative error (1/s^2). Otherwise the line
//     (TooFewPromptClusters, NarrowPromptClusters).
//   - A fitted c that is not above zero is not convex, and a fitted b that
//     is not above zero has a time per token that falls below zero at short
//     prompts: the line (ConcaveFit, NegativeSlopeFit). A solve the points
//     cannot carry is the line too (SingularFit).
//   - The intercept is extrapolated from the shortest prompts the fit
//     reads, and reads low the further they are from zero: the shortest
//     must be at most MaxAShortestPrompt, or a is refused (NoShortPrompts,
//     AExtrapolated). For the curve that is its shortest counted cluster's
//     median; for the line the second shortest minute, as the spread test
//     reads it.
//   - The curve's intercept carries a standard error from the clusters'
//     pooled spread and the curve's lack of fit (fitQuadratic), and a
//     curve whose standard error is above
//     MaxAInterceptRelSE of its intercept is refused (AUncertain): three
//     medians amplify their noise into the extrapolated intercept, and a
//     positive intercept that noise could have put at zero is not one.
//   - An intercept that is not above zero is refused (NonPositiveIntercept);
//     the curve's is not replaced by the line's.
//
// The curve is fitted through the clusters' medians, not the minutes, which
// is a value estimator (analyzer-learner.md, 5) for a named contaminant: a
// minute no state identifies whose service time reads slow (run 3's 22:28
// minute at 38k, 0.375 s against 0.30 s for the other four at its length).
// Through run 3's fifteen minutes it takes the curve's intercept, weighted,
// from 4.0 ms to 15.1 ms; one minute 25% off in any position of runs 3 and 4
// pooled moves the fit through the minutes by up to 0.55 ms and the fit
// through the medians by up to 0.14 ms. Each cluster's median tolerates one
// such minute in three. A trimmed mean would cost less precision, but only
// pays from five minutes a cluster, and a cluster here may hold three.
//
// The weight is relative because the noise is: the service time's spread
// grows with it, and a fit by absolute error lets the 37k minutes set the
// intercept of a curve read at 1k. The line keeps P1's untrimmed,
// unweighted fit over every minute.
//
// AvgTTFT - AvgServiceTime holds a constant overhead (tokenization,
// scheduling, the HTTP path) beside the engine wait; with no queue there is
// no wait, and the least of it is added to a.
//
// The low-load fit bounds V_P from above, a validation signal and not a
// source: a step of B tokens of prompts of length I costs about
// s0 + (b + c*I)*B, so a busy pod runs below 1/(b + c*I) at the length it
// is fed. On the line that is 1/slope. On a convex pod the line's slope is
// a chord's, about b + c*(I1 + I2) over prompts I1 to I2, so 1/slope reads
// under the bound at every length the fit spans, and a V_P measured there
// would be flagged above it when it is not. So with the curve the bound is
// 1/(b + c*I) at the prompt length V_P was read at
// (Estimate.VPPromptTokens), and with no such length there is none
// (curveBound).
//
// Low load is a state, not a value: the queue empty over the minute, and
// the pod's occupancy (request rate times service time, Little's law) at or
// under LowLoadOccupancy, so a prompt rarely shares a step. The readings are
// excluded by that state. QueueLength has no presence bit until the TTFT
// recorder fields land; the occupancy test holds a row whose queue was not
// read to low load anyway.
//
// On an aggregated pod the service time includes decode, and a is refused.

// LowLoadOccupancy is the most a pod may be occupied (requests in service on
// average) for its minute to count as low load.
const LowLoadOccupancy = 0.1

// MinFixedSamples is the fewest low-load minutes a is fitted from.
const MinFixedSamples = 5

// MinPromptSpread is the least ratio of the longest to the shortest
// effective prompt the low-load minutes must span for the fit to separate
// the fixed cost from the per-token cost, read between the second shortest
// and the second longest so that each end holds two minutes.
const MinPromptSpread = 1.5

// MinQuadraticClusters is the fewest prompt-length clusters, each of at least
// MinQuadraticClusterMinutes minutes, the quadratic is fitted through: one
// per parameter, and one more so that a fourth length checks the curve the
// other three set.
const MinQuadraticClusters = 4

// MinQuadraticClusterMinutes is the fewest minutes a prompt-length cluster
// holds for the quadratic to count it: its median then tolerates one slow
// minute in three.
const MinQuadraticClusterMinutes = 3

// MinQuadraticSpread is the least ratio of the longest counted cluster's
// median prompt to the shortest's for the quadratic: below it, c*I*I is not
// told apart from b*I.
const MinQuadraticSpread = 3.0

// MaxClusterWidth is the widest a prompt-length cluster may be: its longest
// minute over its shortest. MaxPromptDrift links neighbours, and without a
// cap neighbours a few percent apart chain from 8k to 37k (run 3's 86
// low-load rows at 29k to 38k link into one cluster).
const MaxClusterWidth = 1.2

// MaxAInterceptRelSE is the largest the curve's intercept's standard error
// may be, relative to the intercept, for it to be published as a: two
// standard errors from zero.
const MaxAInterceptRelSE = 0.5

// MaxAShortestPrompt is the longest the shortest prompts a fit reads may be,
// in tokens, for its intercept to be published as a. The intercept is
// extrapolated from them, and reads low the further they are from zero:
// through runs 3 and 4 of the TTFT recordings pooled (1k to 37k), the curve
// gives 7.98 ms from a shortest cluster at 1k, 6.91 ms at 2k and 5.55 ms at
// 4k, against 8.46 ms on ols-sweep's line. Low is the unsafe side: a smaller
// a prices TTFT lower. 2500 admits a cluster at 2k, at most about 14% low
// there, and refuses one at 4k, 30% low.
const MaxAShortestPrompt = 2500.0

// AModel is the curve a's intercept was taken from.
type AModel string

const (
	// AModelLinear is the straight line s0 + b*I through every low-load
	// minute.
	AModelLinear AModel = "linear"
	// AModelQuadratic is the convex curve s0 + b*I + c*I*I through the
	// prompt-length clusters' medians.
	AModelQuadratic AModel = "quadratic"
)

// LineReason is why a was taken from the line and not the quadratic.
type LineReason string

const (
	// LineOnly is the quadratic switched off (Cycle.ALineOnly).
	LineOnly LineReason = "LineOnly"
	// TooFewPromptClusters is fewer than MinQuadraticClusters clusters of
	// MinQuadraticClusterMinutes minutes.
	TooFewPromptClusters LineReason = "TooFewPromptClusters"
	// NarrowPromptClusters is clusters spanning less than
	// MinQuadraticSpread.
	NarrowPromptClusters LineReason = "NarrowPromptClusters"
	// ConcaveFit is a fitted c not above zero.
	ConcaveFit LineReason = "ConcaveFit"
	// NegativeSlopeFit is a fitted b not above zero.
	NegativeSlopeFit LineReason = "NegativeSlopeFit"
	// SingularFit is a solve the clusters cannot carry (quadratic).
	SingularFit LineReason = "SingularFit"
)

// fixed sets a on est from the key's low-load samples; lineOnly fits the
// line alone, as P1 shipped.
func (est *Estimate) fixed(ks *keyState, disaggregated, lineOnly bool) {
	if !disaggregated {
		est.ARefused = AggregatedRefused
		return
	}
	var xs, ys []float64
	overhead, overheadOK := 0.0, false
	for _, name := range ks.podNames() {
		for _, s := range ks.pods[name].samples {
			if !s.lowLoad {
				continue
			}
			xs, ys = append(xs, s.ieff), append(ys, s.svc)
			if s.overheadOK && (!overheadOK || s.overhead < overhead) {
				overhead, overheadOK = s.overhead, true
			}
		}
	}
	est.ASamples = len(xs)
	if len(xs) < MinFixedSamples {
		est.ARefused = TooFewLowLoadMinutes
		return
	}
	sorted := slices.Sorted(slices.Values(xs))
	lo, hi := sorted[0], sorted[len(sorted)-1]
	est.APromptMin, est.APromptMax = lo, hi
	// The spread must hold without the one most extreme minute at either
	// end: at least two minutes at each end of it, so one outlying prompt
	// length cannot set the slope alone.
	if !(sorted[len(sorted)-2] >= MinPromptSpread*sorted[1]) {
		est.ARefused = NoPromptSpread
		return
	}
	intercept, slope := ols(xs, ys)
	if math.IsNaN(slope) {
		// Unreachable past the spread test; kept apart from a fitted slope
		// that is not positive, which is a different finding.
		est.ARefused = NoPromptVariance
		return
	}
	if !(slope > 0) || !isFinite(slope) {
		est.ARefused = NonPositiveSlope
		return
	}
	est.SlopeBound = 1 / slope
	est.AModel, est.ASlope = AModelLinear, slope
	if lineOnly {
		est.ALineReason = LineOnly
	} else {
		shortest := sorted[1]
		q, why := fitQuadratic(xs, ys)
		est.AClusters = q.clusters
		if why != "" {
			est.ALineReason = why
		} else {
			est.AModel, est.ASlope, est.ACurve, est.AInterceptSE = AModelQuadratic, q.b, q.c, q.se
			intercept, shortest = q.a, q.shortest
		}
		est.AShortestPrompt = shortest
		if !(shortest <= MaxAShortestPrompt) {
			est.AExtrapolated = true
			est.ARefused = NoShortPrompts
			return
		}
	}
	if !(intercept > 0) || !isFinite(intercept) {
		est.ARefused = NonPositiveIntercept
		return
	}
	if est.AModel == AModelQuadratic && !(est.AInterceptSE <= MaxAInterceptRelSE*intercept) {
		est.ARefused = AUncertain
		return
	}
	est.AIntercept = intercept
	est.A = intercept
	if overheadOK {
		est.AOverhead, est.AOverheadKnown = overhead, true
		est.A += overhead
	}
}

// quadFit is the quadratic s = a + b*I + c*I*I, the standard error of a, the
// shortest counted cluster's median prompt, and how many clusters of
// MinQuadraticClusterMinutes minutes the minutes held.
type quadFit struct {
	a, b, c  float64
	se       float64
	shortest float64
	clusters int
}

// MinMedianRelSD is the least relative noise each cluster's service time is
// read with, for the intercept's standard error: an SD of 1% of the
// cluster's median. The least any counted cluster of runs 3 and 4 or
// ols-sweep read was 0.74% (run 4 at 2k, four minutes), and their pooled
// value is 5.4%; a cluster of identical minutes, which a replayed
// recording or a cached response can give, would otherwise read none, and
// an intercept with no error is never refused. One percent is under every
// cluster measured, so it raises none of theirs.
const MinMedianRelSD = 0.01

// fitQuadratic is the convex curve through the prompt-length clusters'
// medians, weighted by relative error, or why the line stands instead.
//
// The intercept's standard error is sqrt(sum of k_i^2 var_i), k_i the
// intercept's weight on cluster i's median (a = sum of k_i y_i) and var_i
// that median's variance: pi/(2 n_i) times the noise's variance (the
// median's large-sample variance under normal noise). The noise is read
// relative, as the fit weights it, and pooled over the clusters (each
// minute about its cluster's median), so one quiet cluster of three does
// not set its own variance near zero; it is at least MinMedianRelSD. Four
// clusters leave the curve a residual degree of freedom, and the clusters'
// relative residuals from it (their lack of fit, over n-3) stand for each
// median's variance where they are larger: noise that sits between the
// clusters, not within them, is what the pooled spread cannot see.
func fitQuadratic(xs, ys []float64) (quadFit, LineReason) {
	var cl []cluster
	for _, c := range promptClusters(xs, ys) {
		if c.n >= MinQuadraticClusterMinutes {
			cl = append(cl, c)
		}
	}
	q := quadFit{clusters: len(cl)}
	if len(cl) < MinQuadraticClusters {
		return q, TooFewPromptClusters
	}
	q.shortest = cl[0].x
	if !(cl[len(cl)-1].x >= MinQuadraticSpread*cl[0].x) {
		return q, NarrowPromptClusters
	}
	mx, my, mw := make([]float64, len(cl)), make([]float64, len(cl)), make([]float64, len(cl))
	for i, c := range cl {
		// Every service time admitted is above zero (finitePositive).
		mx[i], my[i], mw[i] = c.x, c.y, 1/(c.y*c.y)
	}
	var k []float64
	var ok bool
	q.a, q.b, q.c, k, ok = quadratic(mx, my, mw)
	if !ok {
		return q, SingularFit
	}
	if !(q.c > 0) {
		return q, ConcaveFit
	}
	if !(q.b > 0) {
		// A negative b is a curve whose time per token falls below zero at
		// short prompts: what a slow minute at the long end does to three
		// medians (run 3 fitted through its minutes read b of -1.2e-6),
		// not a pod. The line is read instead.
		return q, NegativeSlopeFit
	}
	q.se = interceptSE(cl, mx, my, k, q)
	return q, ""
}

// interceptSE is the standard error of the curve q's intercept through the
// clusters cl (medians mx, my; the intercept's weights k), as fitQuadratic
// gives it.
//
// Each median's relative variance has two parts. Within its cluster: the
// minutes' relative noise about their medians, pooled over the clusters
// (rel2 over dof), at least MinMedianRelSD, times the median's pi/(2n).
// Between the clusters: noise a whole cluster shares (a level whose minutes
// all read slow), which no cluster's own spread shows. That is read from
// the curve's lack of fit, the medians' mean squared relative residual over
// n-3, less what the within part already explains of it; a negative excess
// is none (the method-of-moments estimate of a random effect). On 4000
// seeded pods with noise of 1% to 20% within the clusters, 77.8% of
// published intercepts fall within one standard error (68.3% would be
// exact; the excess is chance lack of fit read as a between part, the safe
// side) and 99.9% within three. With a 2% offset shared by each level
// beside 1% within, 63.5% fall within one and 95.8% within three; the
// within part alone gave 31%.
func interceptSE(cl []cluster, mx, my, k []float64, q quadFit) float64 {
	var rel2, dof float64
	for _, c := range cl {
		rel2 += c.relSS
		dof += float64(c.n - 1)
	}
	noise := math.Max(rel2/dof, MinMedianRelSD*MinMedianRelSD)
	var lack, within float64
	for i, c := range cl {
		r := (my[i] - (q.a + q.b*mx[i] + q.c*mx[i]*mx[i])) / my[i]
		lack += r * r
		within += math.Pi / (2 * float64(c.n)) * noise
	}
	between := math.Max(0, lack/float64(len(cl)-3)-within/float64(len(cl)))
	var v float64
	for i, c := range cl {
		v += k[i] * k[i] * (math.Pi/(2*float64(c.n))*noise + between) * my[i] * my[i]
	}
	return math.Sqrt(v)
}

// cluster is one prompt-length cluster: its minutes' median prompt length
// and median service time, how many minutes it holds, and the sum of its
// minutes' squared deviations from the median service time, relative to it.
type cluster struct {
	x, y  float64
	n     int
	relSS float64
}

// promptPair is one minute's prompt length and service time.
type promptPair struct{ x, y float64 }

// promptClusters groups the points by prompt length, shortest first: sorted
// by length, a point within MaxPromptDrift of the one before it, and within
// MaxClusterWidth of its cluster's first, joins its cluster; any other
// starts the next. The steady gate holds a minute to MaxPromptDrift of the
// minutes before it, so one level of a sweep is one cluster.
func promptClusters(xs, ys []float64) []cluster {
	pts := make([]promptPair, len(xs))
	for i := range xs {
		pts[i] = promptPair{xs[i], ys[i]}
	}
	slices.SortStableFunc(pts, func(p, q promptPair) int {
		switch {
		case p.x < q.x:
			return -1
		case p.x > q.x:
			return 1
		}
		return 0
	})
	// One scratch slice for the medians: each cluster's range is copied into
	// it and sorted there.
	scratch := make([]float64, 0, len(pts))
	med := func(lo, hi int, val func(promptPair) float64) float64 {
		scratch = scratch[:0]
		for _, p := range pts[lo:hi] {
			scratch = append(scratch, val(p))
		}
		return median(scratch)
	}
	var out []cluster
	start := 0
	for k := 1; k <= len(pts); k++ {
		if k < len(pts) && pts[k].x <= (1+MaxPromptDrift)*pts[k-1].x && pts[k].x <= MaxClusterWidth*pts[start].x {
			continue
		}
		c := cluster{
			x: med(start, k, func(p promptPair) float64 { return p.x }),
			y: med(start, k, func(p promptPair) float64 { return p.y }),
			n: k - start,
		}
		for _, p := range pts[start:k] {
			r := (p.y - c.y) / c.y
			c.relSS += r * r
		}
		out = append(out, c)
		start = k
	}
	return out
}

// curveBound sets the slope bound from the curve: 1/(b + c*I) at the prompt
// length V_P was read at, or none when that length is unknown, and flags
// V_P above it. On the line, fixed set the bound to 1/slope.
func (est *Estimate) curveBound() {
	if est.AModel == AModelQuadratic {
		est.SlopeBound = 0
		if i := est.VPPromptTokens; i > 0 {
			if m := est.ASlope + est.ACurve*i; m > 0 && isFinite(m) {
				est.SlopeBound = 1 / m
			}
		}
	}
	est.VPAboveSlopeBound = est.VP > 0 && est.SlopeBound > 0 && est.VP > est.SlopeBound
}

// ols is the least-squares line y = intercept + slope*x through the points;
// NaN when x does not vary.
func ols(xs, ys []float64) (intercept, slope float64) {
	n := float64(len(xs))
	var mx, my float64
	for i := range xs {
		mx += xs[i]
		my += ys[i]
	}
	mx, my = mx/n, my/n
	var sxx, sxy float64
	for i := range xs {
		dx := xs[i] - mx
		sxx += dx * dx
		sxy += dx * (ys[i] - my)
	}
	if !(sxx > 0) {
		return math.NaN(), math.NaN()
	}
	slope = sxy / sxx
	return my - slope*mx, slope
}

// minSolveDet is the least determinant the scaled normal matrix may have for
// the solve to be read: below it the points do not hold three distinct
// lengths to the solve's precision.
const minSolveDet = 1e-12

// quadratic is the weighted least-squares curve y = a + b*x + c*x*x through
// the points, each squared residual weighted by ws, and k, the intercept's
// weight on each point (a = sum of k_i y_i). ok is false when the solve is
// singular: fewer than three distinct x, or a determinant under minSolveDet.
//
// The normal equations are built in u = x/max|x|, so their entries are
// within the weights' range whatever the prompt lengths (raw x^4 reaches
// 1e18 at 40k tokens, and 1e22 once weighted 1/s^2 at 10 ms); b and c are
// unscaled after, and k needs none, since a's column is not scaled.
func quadratic(xs, ys, ws []float64) (a, b, c float64, k []float64, ok bool) {
	scale := 0.0
	for _, x := range xs {
		scale = math.Max(scale, math.Abs(x))
	}
	if !(scale > 0) || math.IsInf(scale, 0) {
		return 0, 0, 0, nil, false
	}
	// s[j] is the weighted sum of u^j, t[j] of y*u^j.
	var s [5]float64
	var t [3]float64
	for i := range xs {
		u, p := xs[i]/scale, ws[i]
		for j := range s {
			s[j] += p
			if j < len(t) {
				t[j] += p * ys[i]
			}
			p *= u
		}
	}
	m := [3][3]float64{{s[0], s[1], s[2]}, {s[1], s[2], s[3]}, {s[2], s[3], s[4]}}
	det := det3(m)
	// The determinant is read relative to the matrix's own size, s[0]^3
	// (u is at most 1, so no entry is above s[0]).
	if !(det > minSolveDet*s[0]*s[0]*s[0]) || math.IsInf(det, 0) {
		return 0, 0, 0, nil, false
	}
	var sol [3]float64
	for col := range sol {
		mc := m
		for row := range mc {
			mc[row][col] = t[row]
		}
		sol[col] = det3(mc) / det
	}
	// The first row of the inverse: the first column's cofactors over the
	// determinant (m is symmetric).
	l0 := [3]float64{
		(m[1][1]*m[2][2] - m[1][2]*m[2][1]) / det,
		-(m[0][1]*m[2][2] - m[0][2]*m[2][1]) / det,
		(m[0][1]*m[1][2] - m[0][2]*m[1][1]) / det,
	}
	k = make([]float64, len(xs))
	for i := range xs {
		u := xs[i] / scale
		k[i] = (l0[0] + l0[1]*u + l0[2]*u*u) * ws[i]
	}
	return sol[0], sol[1] / scale, sol[2] / (scale * scale), k, true
}

// det3 is the determinant of m.
func det3(m [3][3]float64) float64 {
	return m[0][0]*(m[1][1]*m[2][2]-m[1][2]*m[2][1]) -
		m[0][1]*(m[1][0]*m[2][2]-m[1][2]*m[2][0]) +
		m[0][2]*(m[1][0]*m[2][1]-m[1][1]*m[2][0])
}
