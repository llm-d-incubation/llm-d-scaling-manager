package prefill

import (
	"math"
	"time"
)

// The imbalance f (slo-capacity-planner-ttft.md, "The engine queue").
//
// The router sends each request to one pod, so the worst pod sees f times
// the mean share of prefill work. f is measured as the busiest pod's share of
// the computed-token rate over the mean share, over minutes with at least
// MinRequestsPerPodMinute requests per pod on average, minus what Poisson
// arrivals alone would show: the expected max of N_p Poisson counts over
// their mean, less one. It is never below 1.
//
// A minute counts only when every serving replica of the variant was
// admitted (settled) and read a computed-token rate and a request rate:
// shares over a subset of the pods are not shares of the variant's work.
// f is the median of the minutes, over at least MinImbalanceMinutes.
//
// The de-bias is exact only for equal pods, and it errs both ways
// (slo-capacity-planner-ttft.md, "Where it is most wrong"):
//
//   - Low on a real imbalance. The excess subtracted is the equal pods'
//     chance excess, but a pod clearly busier than the rest is the busiest
//     whatever the draw, so little of that excess is in its share. Four pods
//     at 2:1:1:1 (f 1.6) read about 1.50 at 100 requests a pod, about 6% low,
//     and about 1.57 at 1500. A low f over-promises TTFT.
//   - High on equal pods with variable prompts. The share is of computed
//     tokens, whose count per pod varies by about sqrt(1+Cs^2) times the
//     request count's, Cs the prompt length's coefficient of variation; the
//     de-bias takes off only the request count's part. Four equal pods with
//     exponential prompts (Cs^2 = 1) at 100 requests a pod read about 1.03.

// MinRequestsPerPodMinute is the fewest requests per pod per minute, on
// average, a minute must carry for its shares to be read.
const MinRequestsPerPodMinute = 100

// MinImbalanceMinutes is the fewest minutes f is taken from.
const MinImbalanceMinutes = 5

// GaussianMaxMean is the mean count per pod above which poissonMaxExcess
// uses the normal approximation instead of the exact sum.
const GaussianMaxMean = 1e4

// normalMaxMean is the expected max of n standard normals: the integral over
// x > 0 of 1 - Phi(x)^n less the integral over x < 0 of Phi(x)^n, folded
// onto x > 0 as 1 - Phi(x)^n - Phi(-x)^n, which is smooth, and summed by
// Simpson's rule over [0, 10] at a step of 0.01. Beyond 10 the integrand is
// below float64's resolution for any n a variant has.
func normalMaxMean(n int) float64 {
	const lim, steps = 10.0, 1000
	h := lim / steps
	f := func(x float64) float64 {
		// Phi(-x), read directly so the tail keeps its precision; 1 -
		// Phi(x)^n as -expm1(n*log1p(-Phi(-x))) for the same reason.
		down := 0.5 * math.Erfc(x/math.Sqrt2)
		return -math.Expm1(float64(n)*math.Log1p(-down)) - math.Pow(down, float64(n))
	}
	sum := f(0) + f(lim)
	for i := 1; i < steps; i++ {
		w := 2.0
		if i%2 == 1 {
			w = 4
		}
		sum += w * f(float64(i)*h)
	}
	return sum * h / 3
}

// share is one replica's work this cycle.
type share struct {
	rate       float64
	rateOK     bool
	requests   float64
	requestsOK bool
}

// minuteImbalance is the de-biased imbalance of one cycle's shares, and
// whether the cycle gives one.
func minuteImbalance(shares []share) (float64, bool) {
	n := len(shares)
	if n < 2 {
		return 0, false
	}
	var total, busiest, requests float64
	for _, s := range shares {
		if !s.rateOK || !s.requestsOK {
			return 0, false
		}
		total += s.rate
		requests += s.requests
		busiest = math.Max(busiest, s.rate)
	}
	perPod := requests * 60 / float64(n)
	if !(total > 0) || !(perPod >= MinRequestsPerPodMinute) || !isFinite(perPod) {
		return 0, false
	}
	raw := float64(n) * busiest / total
	f := math.Max(1, raw-poissonMaxExcess(n, perPod))
	return f, isFinite(f)
}

// noteImbalance folds the cycle's imbalance into the key's minutes, when
// every serving replica was admitted.
func (ks *keyState) noteImbalance(shares []share, allAdmitted bool, now time.Time) {
	if !allAdmitted {
		return
	}
	f, ok := minuteImbalance(shares)
	if !ok {
		return
	}
	if n := len(ks.f); n > 0 && now.Sub(ks.f[n-1].at) < SampleSpacing {
		ks.f[n-1].f, ks.f[n-1].pods = f, len(shares)
		return
	}
	ks.f = append(ks.f, fSample{at: now, f: f, pods: len(shares)})
	if len(ks.f) > maxSamples {
		ks.f = ks.f[len(ks.f)-maxSamples:]
	}
}

// imbalance sets f on est from the key's minutes, and names the refusal by
// the cycle's own shares when there are too few: serving is how many of
// the variant's replicas served this cycle, admitted or not. It sets the
// per-count figures too (imbalanceByPods), whether or not F is refused.
func (est *Estimate) imbalance(ks *keyState, shares []share, serving int) {
	est.FPodsLast = serving
	est.FSamples = len(ks.f)
	est.FByPods, est.FSamplesByPods = imbalanceByPods(ks.f)
	if len(ks.f) >= MinImbalanceMinutes {
		fs := make([]float64, len(ks.f))
		for i, s := range ks.f {
			fs[i] = s.f
		}
		est.F = median(fs)
		return
	}
	switch {
	case serving < 2:
		est.FRefused = OnePod
	case !anyRate(shares):
		est.FRefused = NoComputedTokens
	default:
		est.FRefused = TooFewImbalanceMinutes
	}
}

// imbalanceByPods is the median f at each pod count, of the minutes taken at
// that count only, for the counts with at least MinImbalanceMinutes, and how
// many minutes each count kept. Both nil when no minute was kept.
func imbalanceByPods(fs []fSample) (map[int]float64, map[int]int) {
	if len(fs) == 0 {
		return nil, nil
	}
	byPods := map[int][]float64{}
	for _, s := range fs {
		byPods[s.pods] = append(byPods[s.pods], s.f)
	}
	f, samples := map[int]float64{}, make(map[int]int, len(byPods))
	for pods, xs := range byPods {
		samples[pods] = len(xs)
		if len(xs) >= MinImbalanceMinutes {
			f[pods] = median(xs)
		}
	}
	return f, samples
}

// anyRate reports whether any share read a computed-token rate.
func anyRate(shares []share) bool {
	for _, s := range shares {
		if s.rateOK {
			return true
		}
	}
	return false
}

// poissonMaxExcess is E[max of n Poisson(m) counts]/m - 1: how far above the
// mean the busiest of n equally loaded pods reads by chance alone. Summed
// over k from 12 standard deviations either side of m, outside which the
// terms are below float64's resolution.
//
// The sum runs about 24·sqrt(m) terms, under the observer's lock, so above
// GaussianMaxMean it is replaced by the normal approximation: E[max] is m
// plus sqrt(m) times the expected max of n standard normals, a cost that does
// not grow with m. TestPoissonMaxExcess pins how far the two differ there.
func poissonMaxExcess(n int, m float64) float64 {
	if n < 2 || !(m > 0) || !isFinite(m) {
		return 0
	}
	if m > GaussianMaxMean {
		return normalMaxMean(n) / math.Sqrt(m)
	}
	sd := math.Sqrt(m)
	lo := math.Max(0, math.Floor(m-12*sd))
	hi := math.Ceil(m + 12*sd)
	logM := math.Log(m)
	// E[max] = sum over k >= 0 of P(max > k) = sum of 1 - F(k)^n. Below lo,
	// F(k) is 0 to float64 resolution and each term is 1.
	expMax := lo
	cdf := 0.0
	for k := lo; k <= hi; k++ {
		lg, _ := math.Lgamma(k + 1)
		cdf += math.Exp(-m + k*logM - lg)
		if cdf > 1 {
			cdf = 1
		}
		expMax += 1 - math.Pow(cdf, float64(n))
	}
	return expMax/m - 1
}
