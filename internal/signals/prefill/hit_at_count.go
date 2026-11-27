package prefill

import (
	"math"
	"slices"
)

// The prefix hit at another prefill count (issue
// llm-d-incubation/llm-d-scaling-manager#384).
//
// A cache-aware router keeps each shared prefix on one prefill pod, so N pods
// split the prefixes N ways and each pod's cache holds a larger share of its
// own. Where the prefixes do not fit one pod's cache, the hit then rises with
// N: on a lab run of 64 shared documents against one pod's GPU cache, the
// hit was 0.345 on 1 pod, 0.526 on 2 and 0.792 on 4. A hit read at one count
// and held at another under-prices the prompt tokens a pod computes at fewer
// pods (7,580 a request priced at 1 pod, 10,470 measured) and over-prices
// them at more.
//
// HitAtCount gives the hit at the priced count N' from the hit h read at the
// fit's count N, and the computed tokens a request with it:
//
//   - N' = N: h, nothing moves.
//   - N' != N, with the engines' KV residency histograms (ReuseHistograms):
//     the reuse-gap rule, Che's characteristic time over a reuse-gap law
//     fitted at N (HitModel, hit_model.go), at fewer pods and at more. The
//     histograms close Che's occupancy equation at the fit; at N' a pod
//     takes N/N' times the block accesses with the same reuse gaps
//     (affinity keeps a prefix on one pod), so the cache horizon T' solves
//     the occupancy at N', and the hits priced are those whose gap is under
//     T'. It is held to the bracket below; with no tier past the GPU the
//     law's ceiling is assumed, and at more pods the miss is held to fall
//     no faster than the pods rise, 1 - h' >= (1 - h) N/N'.
//   - N' < N without them: the floor h x N'/N. The hit is concave in each
//     pod's cache over its working set, so it is at least that; on every
//     measured and simulated pair of the lab run it read at or under the
//     truth, at the cost of 13% to 28% more computed tokens than measured.
//   - N' > N: h, held. Under affinity the hit rises with N, so the computed
//     tokens read high; where the working set fits one pod it stays near
//     flat, less the cold copies a prefix sent to a second pod makes. The
//     bracket's upper edge, min(1, h x N'/N), is how high the hit may rise.
//
// The computed tokens a request scale by (1 - h')/(1 - h): the whole prompt
// does not move with the count, only the share of it the cache serves.

// HitRule names how HitAtCount priced the hit.
type HitRule string

// The rules.
const (
	// HitSameCount is the fit's own count: the hit as read.
	HitSameCount HitRule = "same"
	// HitFloor is the concavity floor h x N'/N, at fewer pods.
	HitFloor HitRule = "floor"
	// HitReuseGap is the reuse-gap rule (HitModel), at fewer or more
	// pods.
	HitReuseGap HitRule = "gap"
	// HitHeld is the fit's hit held, at more pods.
	HitHeld HitRule = "held"
)

// ReuseBucket is one cumulative bucket of a Prometheus histogram: Count
// observations at or under UpperEdge, seconds. The last edge may be +Inf.
type ReuseBucket struct {
	UpperEdge, Count float64
}

// ReuseHistograms are a prefill variant's KV residency histograms over the
// fit's window (vLLM with --kv-cache-metrics), summed over its pods: Gap is
// vllm:kv_block_reuse_gap_seconds, one observation per block hit, the time
// since the block's last use; Idle is
// vllm:kv_block_idle_before_evict_seconds, one observation per block
// evicted, the time it sat unused. Both cumulative, edges ascending.
type ReuseHistograms struct {
	Gap  []ReuseBucket
	Idle []ReuseBucket
}

// HitAtCount is the hit priced at a prefill count other than the fit's.
type HitAtCount struct {
	// FitHit is h, the fit's hit (1 - computed / whole prompt, every tier);
	// Hit is h', the hit priced at Count.
	FitHit, Hit float64
	// FitCount is N, the prefill pods the hit was read on; Count is N'.
	FitCount, Count int
	// Rule is how Hit was priced.
	Rule HitRule
	// Lo and Hi bracket the hit at Count: the floor and the fit's hit at
	// fewer pods, the fit's hit and min(1, h x N'/N) at more.
	Lo, Hi float64
	// Computed is the computed tokens a request at Count, ComputedScale
	// that over the fit's: (1 - h')/(1 - h).
	Computed, ComputedScale float64
	// GPUScale is the GPU prefix-cache hit at Count over the fit's, and
	// TierScale the share of the tiers past the GPU at Count over the fit's:
	// the reuse-gap rule's, 1 under any other.
	GPUScale, TierScale float64
}

// Moved says the hit priced differs from the fit's.
func (a HitAtCount) Moved() bool { return a.Hit != a.FitHit }

// HitAtCountOf prices the hit and the computed tokens a request at count,
// from the fit's hit h and computed tokens a request on fitCount pods, with
// the reuse-gap rule where m is non-nil (FitHitModel), else the floor at
// fewer pods and the fit's hit held at more. ok is false where nothing can
// be priced: a count or the fit's count under 1, h outside [0, 1) or not a
// number, or computed not a positive finite number; the caller then keeps
// the fit's figures. At h = 0 the hit stays 0 at every count.
func HitAtCountOf(h, computed float64, fitCount, count int, m *HitModel) (HitAtCount, bool) {
	if fitCount < 1 || count < 1 || !(h >= 0 && h < 1) || !(computed > 0) || math.IsInf(computed, 0) {
		return HitAtCount{}, false
	}
	x := float64(count) / float64(fitCount)
	out := HitAtCount{FitHit: h, Hit: h, FitCount: fitCount, Count: count, Lo: h, Hi: h, Computed: computed, ComputedScale: 1,
		GPUScale: 1, TierScale: 1}
	switch {
	case count == fitCount:
		out.Rule = HitSameCount
		return out, true
	case count > fitCount:
		out.Rule, out.Hi = HitHeld, min(1, h*x)
	default:
		out.Rule, out.Hit, out.Lo = HitFloor, h*x, h*x
	}
	if r, ok := m.moveOf(fitCount, count); ok && h > 0 {
		if m.CeilingAssumed && count > fitCount {
			// No tier read the share ever reused, so the law's ceiling is
			// 1 and at many more pods it prices every prompt token cached.
			// The miss is held to fall no faster than the pods rise.
			// Stated, not derived: the miss falls slower than that
			// while each pod's cache holds a small part of the
			// prefixes it serves, and the law is not trusted past it.
			// On the lab run it priced 0.764 at 4 pods from 2 (0.792
			// measured; the law alone 0.816).
			out.Hi = min(out.Hi, 1-(1-h)/x)
		}
		out.Rule = HitReuseGap
		out.Hit = min(max(r.move(h), out.Lo), out.Hi)
		out.GPUScale, out.TierScale = r.gpu, r.tier
	}
	out.ComputedScale = (1 - out.Hit) / (1 - h)
	out.Computed = computed * out.ComputedScale
	return out, true
}

// hitMove is what the reuse-gap law moves from the fit's count to another
// (HitModel.moveOf): the total hit, the GPU hit and the share of the tiers
// past it, each as a ratio of the fit's.
type hitMove struct {
	t0, t1, gpu, tier float64
}

// move is h, a hit read at the fit, at the other count. Where h is the
// law's own total, t0, it is t1. A caller whose h is read otherwise (a
// histogram's mean) moves by the law's hit ratio, t1/t0, at or under t0,
// and by its computed share's ratio, (1 - t1)/(1 - t0), above it: each
// stays in [0, 1], where the other can leave it, and the first does not
// amplify a low reading's error. On the lab run's 4-pod fit priced at 2
// pods, a histogram reading 0.74 for the law's 0.81 moved by the computed
// share's ratio to 0.38, the floor; by the hit ratio to 0.50 (0.48 measured
// the same way).
func (r hitMove) move(h float64) float64 {
	if h <= r.t0 {
		return h * r.t1 / r.t0
	}
	return 1 - (1-h)*(1-r.t1)/(1-r.t0)
}

// moveOf is the law's move from fitCount to count at the fit's rate, false
// for a nil model or one that does not price both counts.
func (m *HitModel) moveOf(fitCount, count int) (hitMove, bool) {
	if m == nil {
		return hitMove{}, false
	}
	g0, t0, ok0 := m.HitAt(fitCount, 1)
	g1, t1, ok1 := m.HitAt(count, 1)
	if !ok0 || !ok1 || !(g0 > 0) || !(t0 < 1) {
		return hitMove{}, false
	}
	r := hitMove{t0: t0, t1: t1, gpu: g1 / g0, tier: 1}
	if t0-g0 > 0 {
		r.tier = (t1 - g1) / (t0 - g0)
	}
	return r, true
}

// reuseHist is a cumulative histogram read with every observation spread
// evenly across its bucket, the first from 0; an observation past the last
// finite edge sits on that edge.
type reuseHist struct {
	edges, counts []float64
}

// newReuseHist reads b, false with no observation, edges not ascending or
// not above 0, or counts falling or not finite.
func newReuseHist(b []ReuseBucket) (reuseHist, bool) {
	var h reuseHist
	prevEdge, prevCount := 0.0, 0.0
	for _, x := range b {
		if !(x.UpperEdge > prevEdge) || !(x.Count >= prevCount) || math.IsInf(x.Count, 0) {
			return reuseHist{}, false
		}
		h.edges = append(h.edges, x.UpperEdge)
		h.counts = append(h.counts, x.Count)
		prevEdge, prevCount = x.UpperEdge, x.Count
	}
	if len(h.edges) == 0 || !(prevCount > 0) {
		return reuseHist{}, false
	}
	if math.IsInf(h.edges[len(h.edges)-1], 1) && len(h.edges) == 1 {
		return reuseHist{}, false
	}
	return h, true
}

// total is the observations counted.
func (h reuseHist) total() float64 { return h.counts[len(h.counts)-1] }

// bucket is bucket i's lower and upper edge, an infinite upper edge read as
// its lower one.
func (h reuseHist) bucket(i int) (lo, hi float64) {
	if i > 0 {
		lo = h.edges[i-1]
	}
	hi = h.edges[i]
	if math.IsInf(hi, 1) {
		hi = lo
	}
	return lo, hi
}

// quantile is the q quantile, interpolated in its bucket; the last finite
// edge where it falls past it.
func (h reuseHist) quantile(q float64) float64 {
	target := q * h.total()
	i := max(slices.IndexFunc(h.counts, func(c float64) bool { return c >= target }), 0)
	lo, hi := h.bucket(i)
	below := 0.0
	if i > 0 {
		below = h.counts[i-1]
	}
	c := h.counts[i] - below
	if !(c > 0) || hi == lo {
		return hi
	}
	return lo + (hi-lo)*(target-below)/c
}
