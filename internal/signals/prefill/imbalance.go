package prefill

import (
	"cmp"
	"math"
	"slices"
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

// NormalMaxMean is the expected max of n standard normals: the integral over
// x > 0 of 1 - Phi(x)^n less the integral over x < 0 of Phi(x)^n, folded
// onto x > 0 as 1 - Phi(x)^n - Phi(-x)^n, which is smooth, and summed by
// Simpson's rule over [0, 10] at a step of 0.01. Beyond 10 the integrand is
// below float64's resolution for any n a variant has.
func NormalMaxMean(n int) float64 {
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
	// pod is the replica's pod name: a pool of minutes (Cycle.FPooledMinutes)
	// sums each pod's work across them.
	pod        string
	rate       float64
	rateOK     bool
	requests   float64
	requestsOK bool
	// multiRank is a row of more than one engine rank (domain.ReplicaMetrics.Ranks).
	multiRank bool
}

// minuteImbalance is the de-biased imbalance of one cycle's shares, and
// whether the cycle gives one, with the cycle's computed tokens per second
// per pod.
func minuteImbalance(shares []share) (f, load float64, ok bool) {
	n := len(shares)
	if n < 2 {
		return 0, 0, false
	}
	var total, busiest, requests float64
	for _, s := range shares {
		if !s.rateOK || !s.requestsOK {
			return 0, 0, false
		}
		total += s.rate
		requests += s.requests
		busiest = math.Max(busiest, s.rate)
	}
	perPod := requests * 60 / float64(n)
	if !(total > 0) || !(perPod >= MinRequestsPerPodMinute) || !isFinite(perPod) {
		return 0, 0, false
	}
	raw := float64(n) * busiest / total
	f = math.Max(1, raw-poissonMaxExcess(n, perPod))
	return f, total / float64(n), isFinite(f)
}

// noteImbalance folds the cycle's imbalance into the key's minutes, when
// every serving replica was admitted.
// With pooled (Cycle.FPooledMinutes), minutes under MinRequestsPerPodMinute
// are pooled with the ones after them (notePooledImbalance).
func (ks *keyState) noteImbalance(shares []share, allAdmitted, pooled bool, now time.Time) {
	if pooled {
		ks.notePooledImbalance(shares, allAdmitted, now)
		return
	}
	if !allAdmitted {
		return
	}
	f, load, ok := minuteImbalance(shares)
	if !ok {
		return
	}
	ks.putF(fSample{at: now, f: f, pods: len(shares), load: load, multiRank: multiRank(shares)}, now)
}

// putF keeps s as the key's latest minute: it rewrites the last one when
// that is under SampleSpacing old as of now, keeping its time, and is added
// after it otherwise.
func (ks *keyState) putF(s fSample, now time.Time) {
	if n := len(ks.f); n > 0 && now.Sub(ks.f[n-1].at) < SampleSpacing {
		ks.f[n-1].f, ks.f[n-1].pods, ks.f[n-1].load, ks.f[n-1].multiRank = s.f, s.pods, s.load, s.multiRank
		return
	}
	ks.f = append(ks.f, s)
	if len(ks.f) > maxSamples {
		ks.f = ks.f[len(ks.f)-maxSamples:]
	}
}

// imbalance sets f on est from the key's minutes, and names the refusal by
// the cycle's own shares when there are too few: serving is how many of
// the variant's replicas served this cycle, admitted or not. It sets the
// per-count figures too (imbalanceByPods), whether or not F is refused.
// With busy (Cycle.FBusyMinutes), each median is of the busiest minutes
// only (busiestMinutes). FSamples and FSamplesByPods still count every
// minute kept, so what is refused, and when, does not change.
func (est *Estimate) imbalance(ks *keyState, shares []share, serving int, busy bool) {
	est.FPodsLast = serving
	est.FSamples = len(ks.f)
	est.FByPods, est.FSamplesByPods, est.FLoadByPods = imbalanceByPods(ks.f, busy)
	est.FMultiRank = slices.ContainsFunc(ks.f, func(s fSample) bool { return s.multiRank })
	if len(ks.f) >= MinImbalanceMinutes {
		est.F, est.FLoad = median(minuteFs(ks.f, busy)), median(minuteLoads(ks.f, busy))
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
// that count only, for the counts with at least MinImbalanceMinutes, how
// many minutes each count kept, and the median load of the minutes each f
// is the median of. All nil when no minute was kept. With busy, each count's
// medians are of its own busiest minutes (busiestMinutes).
func imbalanceByPods(fs []fSample, busy bool) (map[int]float64, map[int]int, map[int]float64) {
	if len(fs) == 0 {
		return nil, nil, nil
	}
	byPods := map[int][]fSample{}
	for _, s := range fs {
		byPods[s.pods] = append(byPods[s.pods], s)
	}
	f, samples, load := map[int]float64{}, make(map[int]int, len(byPods)), map[int]float64{}
	for pods, xs := range byPods {
		samples[pods] = len(xs)
		if len(xs) >= MinImbalanceMinutes {
			f[pods], load[pods] = median(minuteFs(xs, busy)), median(minuteLoads(xs, busy))
		}
	}
	return f, samples, load
}

// MinFLoadCurveMinutes is the fewest minutes at a pod count f against the
// load is fitted from (fLoadCurves).
const MinFLoadCurveMinutes = 8

// FLoadCurve is f against the load at one pod count (Cycle.FLoadCurve):
//
//	f(u) = Floor + Excess/u,
//
// u the computed tokens a second a pod. The busiest pod carries a roughly
// fixed amount of prefill work over the mean (a prefix group or a long
// prompt the router keeps on one pod) and the router spreads the rest, so
// the excess share falls as the inverse of the load and f falls to a floor.
// It is fitted by least squares of each minute's de-biased f on 1/u, over
// every minute kept at the count (not only the busiest): the light minutes
// are what the curve is for. Nothing else is fitted, and no minute is
// weighted or dropped.
//
// Measured on the Qwen3-32B lab fleet of long prompts (2 prefill pods,
// minutes pooled, issue llm-d-incubation/llm-d-scaling-manager#482): Floor
// 0.96 and Excess 591 tok/s a pod, from 23 pools at 850 to 13,200 tok/s a
// pod that read f 1.5 to 1.7 under 1,000 and about 1.0 above 9,000. On a
// later run at the same count, held out, it read the pools' f with a mean
// absolute error of 0.029, against 0.048 for one f at every load. That run
// had no pool under 3,000 tok/s a pod: below it the curve rests on the
// fit window alone, whose light minutes were its first twelve (a cold
// prefix cache is not ruled out for them).
type FLoadCurve struct {
	// Floor is f_inf, f at an unbounded load; Excess is delta, in computed
	// tokens a second a pod.
	Floor, Excess float64
	// Minutes is how many minutes the fit read.
	Minutes int
}

// Identified reports whether the curve may be priced: at least
// MinFLoadCurveMinutes minutes, an excess above 0 (f falls with the load),
// and a floor above 0, so the busiest pod's utilization, f(u) times the
// mean, rises with the load.
func (c FLoadCurve) Identified() bool {
	return c.Minutes >= MinFLoadCurveMinutes && c.Excess > 0 && c.Floor > 0 && isFinite(c.Excess) && isFinite(c.Floor)
}

// At is f at u computed tokens a second a pod: Floor + Excess/u, at least
// 1; +Inf at no load.
func (c FLoadCurve) At(u float64) float64 {
	if !(u > 0) {
		return math.Inf(1)
	}
	return math.Max(1, c.Floor+c.Excess/u)
}

// fLoadCurves is the f-against-load fit (FLoadCurve) at each pod count of
// fs; nil when fs is empty.
func fLoadCurves(fs []fSample) map[int]FLoadCurve {
	if len(fs) == 0 {
		return nil
	}
	inv, f := map[int][]float64{}, map[int][]float64{}
	for _, s := range fs {
		if !(s.load > 0) || !isFinite(s.load) {
			continue
		}
		inv[s.pods] = append(inv[s.pods], 1/s.load)
		f[s.pods] = append(f[s.pods], s.f)
	}
	out := make(map[int]FLoadCurve, len(inv))
	for pods, xs := range inv {
		c := FLoadCurve{Minutes: len(xs)}
		if len(xs) >= MinFLoadCurveMinutes {
			c.Floor, c.Excess = ols(xs, f[pods])
		}
		out[pods] = c
	}
	return out
}

// minuteLoads is the load of each minute minuteFs reads.
func minuteLoads(fs []fSample, busy bool) []float64 {
	if busy {
		fs = busiestMinutes(fs)
	}
	out := make([]float64, len(fs))
	for i, s := range fs {
		out[i] = s.load
	}
	return out
}

// BusyMinuteShare is the share of the busiest minute's computed tokens per
// pod a minute must carry to be one of the busiest (busiestMinutes).
const BusyMinuteShare = 0.5

// minuteFs is the f of each minute of fs, or with busy of its busiest
// minutes only (busiestMinutes).
func minuteFs(fs []fSample, busy bool) []float64 {
	if busy {
		fs = busiestMinutes(fs)
	}
	out := make([]float64, len(fs))
	for i, s := range fs {
		out[i] = s.f
	}
	return out
}

// busiestMinutes is the minutes of fs at or above BusyMinuteShare of the
// busiest one's computed tokens per pod; short of MinImbalanceMinutes of
// them, the MinImbalanceMinutes busiest, so a window that gives f still
// gives one. Busy is prefill work, not requests: on a window of short and
// long prompts, the minutes with the most requests are the short prompts',
// and they need not be the ones near the knee the long prompts are priced
// at.
//
// f is read for the knee, where the shares are most even: the router spreads
// a light load unevenly and a heavy one nearly evenly. On the public
// staircase the busiest pod's raw share fell from about 1.5 at 10 requests/s
// to 1.01-1.06 near capacity, and a median over every minute of a window
// that was light for much of it read 1.23, which put the prefill knee about
// 20% low (issue llm-d-incubation/llm-d-scaling-manager#266).
//
// What it gives up: f also prices the worst pod's queue below the knee, where
// the shares are less even than the busiest minutes say. The queue is short
// there, so the price moves little, but it moves down.
func busiestMinutes(fs []fSample) []fSample {
	peak := 0.0
	for _, s := range fs {
		peak = math.Max(peak, s.load)
	}
	busy := make([]fSample, 0, len(fs))
	for _, s := range fs {
		if s.load >= BusyMinuteShare*peak {
			busy = append(busy, s)
		}
	}
	if len(busy) >= MinImbalanceMinutes {
		return busy
	}
	busy = slices.Clone(fs)
	slices.SortStableFunc(busy, func(a, b fSample) int { return cmp.Compare(b.load, a.load) })
	return busy[:min(MinImbalanceMinutes, len(busy))]
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
		return NormalMaxMean(n) / math.Sqrt(m)
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

// multiRank reports whether any share is a row of more than one engine rank.
func multiRank(shares []share) bool {
	return slices.ContainsFunc(shares, func(s share) bool { return s.multiRank })
}
