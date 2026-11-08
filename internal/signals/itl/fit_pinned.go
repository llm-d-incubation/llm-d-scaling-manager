package itl

import (
	"math"
)

// DefaultBaselineSec is the per-token latency of an unloaded replica: the
// intercept B of ITL(k) = A*k + B when there is not enough spread in k to fit
// one. It is a hardware floor -- the time a single sequence takes per token
// with the cache nearly empty -- so it varies far less between deployments
// than the contention slope A does, which is what makes pinning it and fitting
// only A a reasonable trade when the observations are poor.
//
// The throughput analyzer carries the same number as its own
// DefaultBaselineITLSec. It is stated here rather than imported because that
// package is an analyzer and this is a signal; the two collapse onto this one
// when the throughput analyzer's in-flight work lands.
const DefaultBaselineSec = 0.006

// DefaultBPrior is the floor of the upper prior on B the one-sided rule at
// the cap pins (Admission.BPrior; BPrior): 30 ms. It is the top of the grid
// the rule was measured on (issue #467: B of 10 to 30 ms on A of 0.01 and
// 0.05), so the grid's worst case is priced exactly. Measured intercepts on
// the clusters this was built for were 1.5 to 15 ms; a whole-node decode
// with a true B above 30 ms is under-priced by (B - 30 ms)(1 - k/kbar) until
// it learns its own, or another variant on its physics key does.
const DefaultBPrior = 0.030

// BPrior is the prior the one-sided rule pins, before the window's cap
// (Admission.BPrior): DefaultBPrior, raised to the largest of learned, the
// intercepts measured on the variant's own physics key (the accelerator and
// the engine shape it runs), when one is larger. A learned B only ever
// raises the prior. One some other engine of the same physics measured says
// B can be that high; a lower one says nothing about this variant's, and
// the fleet's intercepts (1.5 to 15 ms) would otherwise pull every prior
// under a whole-node decode's B. A non-finite or non-positive entry is
// skipped.
func BPrior(learned ...float64) float64 {
	b := DefaultBPrior
	for _, v := range learned {
		if v > 0 && !math.IsInf(v, 0) {
			b = max(b, v)
		}
	}
	return b
}

// FitPinnedB fits ITL(k) = A*k + B with B held at the value given: A is the
// mean of the readings' own slopes (ITL_i - B) / k_i, each weighted by k_i,
// which is sum(ITL_i - B) / sum(k_i) -- the line through B that the readings'
// total latency above the floor lies on. On readings that share one k it is
// least squares through B; across k it gives each reading its weight in load
// rather than its square.
//
// Nothing is trimmed by value. A value cannot tell a transient from a replica
// that is really slower: a replica's first readings after it starts serving
// sit far above the line (on a two-namespace run, about 0.46 of k at 435 to
// 496 ms, slopes of 0.9 to 1.06 against 0.09 to 0.10 around them), and so does
// a replica serving a heavier shape. Trimmed, the slow replica was priced as if
// it did not exist: the interquartile mean set aside one replica at 0.12 among
// four at 0.08 entirely and over-priced the fleet's capacity by 8.5%, and it
// still let the warm-ups through once they carried more than a quarter of the
// weight, which three joiners on a fleet of six did. The weighted median jumped
// between two groups of slopes as their weight crossed a half. The transients
// are known instead, by the replica's state: Tracker keeps the readings of a
// replica still inside its settle out of this fit (see pinnedReadings), and
// what reaches it is blended in whole.
//
// This is the fit for a fleet whose replicas sit at similar loads. A router
// that balances well gives every replica nearly the same k, and a full
// two-parameter fit over points that share an x is meaningless -- Window.Ready
// exists to refuse exactly that, requiring DefaultMinKSpread before Fit is
// trusted. Pinning B leaves one parameter, which a cluster of points at one k
// can still determine: it answers "how much slower than an idle replica is one
// at this load", which is what the caller needs, without pretending to know
// where the line crosses zero.
//
// ok is false when there are no usable observations or the result is not a
// model any reading can be taken from. A negative slope is rejected: more
// cache in use cannot make a replica faster, and a fit that says so is
// measurement noise, not a model.
func FitPinnedB(obs []Observation, b float64) (Model, bool) {
	a, ok := pinnedSlope(obs, b)
	if !ok || !(a >= 0) {
		return Model{}, false
	}
	m := Model{A: a, B: b}
	if !ValidModel(m.A, m.B) {
		return Model{}, false
	}
	return m, true
}

// pinnedSlope is the pinned fit's slope through b, sum(ITL_i - b) /
// sum(k_i), over the readings with a finite positive k and ITL: the kernel
// FitPinnedB and the above-band fallback (fitAboveBand) share, each with
// its own rule for which slopes are a line. ok is false when b is not
// positive or no reading is usable.
func pinnedSlope(obs []Observation, b float64) (a float64, ok bool) {
	if !(b > 0) {
		return 0, false
	}
	var above, load float64
	for _, o := range obs {
		if !(o.K > 0) || !(o.ITLSec > 0) || math.IsInf(o.K, 0) || math.IsInf(o.ITLSec, 0) {
			continue
		}
		above += o.ITLSec - b
		load += o.K
	}
	if !(load > 0) {
		return 0, false
	}
	return above / load, true
}
