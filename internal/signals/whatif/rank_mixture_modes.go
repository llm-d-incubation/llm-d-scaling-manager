package whatif

import (
	"fmt"
	"math"
	"slices"
	"strings"
)

// Two modes of QueueRankMixtureNLLine (rank_mixture_nl.go), each a switch of
// RankMixtureOptions that defaults off. Neither moves the fit window's own
// checks (RankMixtureFit FitMean, FitP90): each is set on the model once
// they are read, so a mode never changes whether the model prices.
//
// KV-deferral hold (KVHold). The KV-deferral lines p_d(rho) and m_d(rho)
// are fitted over the utilizations the fit window held, and read at a
// rank's utilization up to MaxKVRho. Priced at a load past the fit
// window's, the lines are extrapolated, and the queue part reads high: on a
// production P/D recording it read the test half's busier loads well over
// what was measured at p90 and p95. With the mode, the lines are read
// no further than KVDeferral.RhoHold, the KVHoldQuantile of the ranks'
// utilizations over the fit window's steps priced (each step's per-rank
// rho, as Draws computes it): past it the wait is held, not extrapolated.
// A figure priced past the fitted range is still flagged
// KVDeferralOutside. The hold is at a quantile of the ranks' utilizations,
// not at the top of the fitted range (KVDeferral.RhoHi, a mean over the
// ranks), which holds lower still. Held alone, the hold lowers every busier
// step's mean a little: on that recording it took one outlier step's mean,
// already just over 0.8, under it; with the remainder spread on as well, it
// took none.
//
// Remainder spread (RemainderSpread). The router's remainder (its TTFT less
// both engines') is drawn as its line, a constant at each prompt. The router
// measures most of it as its own request processing
// (llm_d_epp_request_processing_duration_seconds, recorded with the router's
// latency): on that recording it read most of the remainder's mean, with a
// heavy tail. With the mode, each request's
// remainder is its line less the processing's mean, held at 0, plus a draw
// of the processing's measured law: R = max(A + B L - m, 0) + m Q(u), u
// uniform and independent of the other parts. m is the median over the fit
// window's steps of the processing's mean, and Q the steps' processing
// quantile functions, each over its own mean, averaged on shapeGrid
// mid-points (Vincentized). Nothing is fitted to the router's quantiles.
// The fit window needs MinRemainderSpreadSteps steps with a processing
// histogram; with fewer (a recording made before the series was recorded,
// or a router that does not export it) the remainder is its line, and the
// assumed line says so.

// KVHoldQuantile is the share of the fit window's rank utilizations the
// KV-deferral hold sits at (RankMixtureOptions.KVHold).
const KVHoldQuantile = 0.95

// MinRemainderSpreadSteps is the fewest fit steps with a router
// request-processing histogram the remainder spread is read from
// (RankMixtureOptions.RemainderSpread).
const MinRemainderSpreadSteps = 4

// shapeGrid is the points a part's shape is held on (ZPart.Shape): the
// mid-points (i + 0.5)/shapeGrid.
const shapeGrid = 2048

// kvHoldOf is the KVHoldQuantile of the ranks' utilizations over the steps
// priced; 0 where none priced.
func kvHoldOf(ps []pricedStep) float64 {
	var rhos []float64
	for _, p := range ps {
		if !p.priceError {
			rhos = append(rhos, p.d.Checks.Rho...)
		}
	}
	return sampleQuantile(rhos, KVHoldQuantile)
}

// sampleQuantile is the q quantile of xs, linear between order statistics
// (the sample's (n - 1) q-th, Hyndman and Fan's type 7); 0 for none.
func sampleQuantile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	h := float64(len(s)-1) * q
	i := int(math.Floor(h))
	if i >= len(s)-1 {
		return s[len(s)-1]
	}
	return s[i] + (h-float64(i))*(s[i+1]-s[i])
}

// remainderShape is the remainder's measured law on steps
// (RankMixtureOptions.RemainderSpread): the steps' request-processing
// quantile functions over their means, averaged (vincentShape), and the
// median of the steps' processing means. nil and 0 where fewer than
// MinRemainderSpreadSteps steps hold a histogram, or none a mean.
func remainderShape(steps []MixtureStep) (shape []float64, mean float64) {
	var hists []map[float64]float64
	var means []float64
	for _, st := range steps {
		if len(st.RouterProcessingHist) > 0 {
			hists = append(hists, st.RouterProcessingHist)
		}
		if st.RouterProcessingMean > 0 && Finite(st.RouterProcessingMean) {
			means = append(means, st.RouterProcessingMean)
		}
	}
	shape, n := vincentShape(hists)
	if n < MinRemainderSpreadSteps || len(means) == 0 {
		return nil, 0
	}
	return shape, medianOf(means)
}

// vincentShape is the histograms' mean-1 quantile function on shapeGrid's
// mid-points: each histogram's quantiles over their own mean, averaged
// (Vincentized), and n the histograms read.
func vincentShape(hists []map[float64]float64) (shape []float64, n int) {
	acc := make([]float64, shapeGrid)
	q := make([]float64, shapeGrid)
	for _, b := range hists {
		h, ok := newTimeHist(b)
		if !ok {
			continue
		}
		var sum float64
		for i := range q {
			q[i] = h.ppf((float64(i) + 0.5) / shapeGrid)
			sum += q[i]
		}
		mean := sum / shapeGrid
		if !(mean > 0) || !Finite(mean) {
			continue
		}
		for i := range q {
			acc[i] += q[i] / mean
		}
		n++
	}
	if n == 0 {
		return nil, 0
	}
	for i := range acc {
		acc[i] /= float64(n)
	}
	return acc, n
}

// ShapeQuantile is the part's shape at cumulative share q, over its mean; 0
// where it has none.
func (z ZPart) ShapeQuantile(q float64) float64 {
	if z.Shape == nil {
		return 0
	}
	return shapeAt(z.Shape, q)
}

// shapeAt is the shape at cumulative share u, linear between its grid's
// mid-points and held at the end ones outside them.
func shapeAt(shape []float64, u float64) float64 {
	n := len(shape)
	x := u*float64(n) - 0.5
	switch {
	case x <= 0:
		return shape[0]
	case x >= float64(n-1):
		return shape[n-1]
	}
	i := int(x)
	return shape[i] + (x-float64(i))*(shape[i+1]-shape[i])
}

// timeHist is a cumulative histogram of seconds as a sampler: the buckets
// that hold requests, each with its range and the cumulative share at its
// end. cumHist reads token counts, whose first bucket starts at 1 and whose
// +Inf bucket is held at its lower edge; a time's first bucket starts at 0,
// and its +Inf bucket runs to twice its lower edge.
type timeHist struct {
	lo, hi, cum []float64
}

// newTimeHist reads b, upper bound to cumulative rate (+Inf the total). ok
// is false where it holds no requests.
func newTimeHist(b map[float64]float64) (timeHist, bool) {
	les := make([]float64, 0, len(b))
	for le := range b {
		if !math.IsNaN(le) {
			les = append(les, le)
		}
	}
	slices.Sort(les)
	var h timeHist
	prevC, prevLe, total := 0.0, 0.0, 0.0
	for _, le := range les {
		c := b[le]
		if !Finite(c) {
			continue
		}
		if k := c - prevC; k > 0 {
			hi := le
			if math.IsInf(le, 1) {
				hi = 2 * prevLe
			}
			h.lo, h.hi = append(h.lo, prevLe), append(h.hi, max(hi, prevLe))
			total += k
			h.cum = append(h.cum, total)
			prevC = c
		}
		if !math.IsInf(le, 1) {
			prevLe = le
		}
	}
	if !(total > 0) {
		return timeHist{}, false
	}
	for i := range h.cum {
		h.cum[i] /= total
	}
	return h, true
}

// ppf is the time at cumulative share u in (0, 1): uniform in a bucket
// starting at 0, log-uniform inside any other.
func (h timeHist) ppf(u float64) float64 {
	b, _ := slices.BinarySearch(h.cum, u)
	b = min(b, len(h.cum)-1)
	lo, hi := h.lo[b], h.hi[b]
	start := 0.0
	if b > 0 {
		start = h.cum[b-1]
	}
	v := min(max((u-start)/(h.cum[b]-start), 0), 1)
	if lo <= 0 {
		return lo + v*(hi-lo)
	}
	return math.Exp(math.Log(lo) + v*(math.Log(hi)-math.Log(lo)))
}

// modesAssumed is the modes in words, for assumedNL; "" where neither was
// asked for.
func (m RankMixture) modesAssumed() string {
	var parts []string
	switch {
	case !m.KVHold:
	case m.KV.RhoHold > 0:
		parts = append(parts, fmt.Sprintf("KV-deferral lines held past rho %.3g (the %g quantile of the fit window's rank "+
			"utilizations), not extrapolated", m.KV.RhoHold, KVHoldQuantile))
		if !m.RemainderSpread {
			parts = append(parts, "KV-deferral hold without the remainder spread: held alone it lowers every step "+
				"priced past the hold, so use the two together")
		}
	default:
		parts = append(parts, "KV-deferral hold not applied: no KV-deferral line fitted")
	}
	switch {
	case !m.RemainderSpread:
	case m.Parts != nil && m.Parts.Remainder.Shape != nil:
		r := m.Parts.Remainder
		parts = append(parts, fmt.Sprintf("router remainder spread as the router's measured request processing "+
			"(mean %.3g s, p95 %.3g of its mean)", r.ShapeMean, r.ShapeQuantile(0.95)))
	default:
		parts = append(parts, fmt.Sprintf("router remainder drawn as its line: fewer than %d fit steps hold the router's "+
			"request-processing histogram (not recorded, or not exported by the router)", MinRemainderSpreadSteps))
	}
	switch {
	case !m.RemainderServer:
	case m.Parts != nil && m.Parts.Remainder.Server != nil:
		s := m.Parts.Remainder.Server
		fe := "frontend not recorded (in the rest)"
		if s.HasFrontend {
			fe = fmt.Sprintf("frontend %.3g s + %.3g us x L", s.F0, s.F1*1e6)
		}
		parts = append(parts, fmt.Sprintf("router remainder from the pods' API servers: router work outside render %.3g s, "+
			"render %.3g s + %.3g us x L, %s; unmeasured rest (sidecar, network, the decode pod's API server, the API server's "+
			"output path) %.3g s + %.3g us x L, %.2g of the fit window's remainder",
			s.other(0), s.R0, s.R1*1e6, fe, s.N0, s.N1*1e6, s.RestShare))
		if s.RestShare > MaxRemainderRestShare {
			parts = append(parts, fmt.Sprintf("the unmeasured rest is over %.2g of the remainder: a TTFT priced past the fit "+
				"window's top rate (%.3g r/s) is flagged %s", MaxRemainderRestShare, s.TopRate, RemainderRestPastFit))
		}
	default:
		parts = append(parts, "router remainder server not applied ("+m.RemainderServerOff+"): the remainder is the spread")
	}
	if len(parts) == 0 {
		return ""
	}
	return "; " + strings.Join(parts, "; ")
}
