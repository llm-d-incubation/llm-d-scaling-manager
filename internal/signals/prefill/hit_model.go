package prefill

import (
	"math"
)

// The prefix hit at another prefill count from the engines' KV residency
// histograms: Che's characteristic time over a reuse-gap law (issue
// llm-d-incubation/llm-d-scaling-manager#384, HitReuseGap).
//
// Every access to a cached block has a reuse gap g, the time since that
// block's last use; F(g) is the share of block accesses whose gap is at
// most g, and h_inf = F(inf) the share ever reused. An LRU cache of B_eff
// blocks under R block accesses a second holds the blocks used within a
// horizon T, with T set by the occupancy
//
//	R * integral_0^T (1 - F(g)) dg = B_eff
//
// and the hit is F(T): the accesses whose gap is under the horizon.
//
// F is read as h_inf * Phi((ln g - mu) / sigma). The fit closes it at the
// fit's count from what a recording holds:
//
//   - T_C, the horizon, is the median idle time before an eviction
//     (vllm:kv_block_idle_before_evict_seconds);
//   - F(T_C) is the GPU prefix-cache hit;
//   - the hit gaps' median (vllm:kv_block_reuse_gap_seconds) is the median
//     of F under T_C;
//   - h_inf is the hit over every tier where a tier past the GPU served
//     some (a floor on the share ever reused), else 1: the GPU tier alone
//     shows no gap past its horizon, so the law's ceiling is not read, and
//     the hit at more pods is then an upper estimate.
//
// R is calibrated at the fit, so B_eff cancels. At N' pods and a fleet rate
// rate', under a cache-aware router each pod takes N/N' times the accesses
// with the same per-block gaps, and gaps that shrink with load as
// rate^-a, so
//
//	R' = R * N/N' * k(N')/k(N) * (rate'/rate)^(1-a)
//
// where k(n) is the duplication of a prefix across pods (1 on one pod, K on
// more): a prefix on k pods reaches each with 1/k of its accesses. a = 1 is
// gaps that scale with the rate (the hit does not move with load); a = 0
// is gaps fixed in seconds (more load, shorter horizon, lower hit).
//
// A tier past the GPU is a second, inclusive LRU: a per-pod tier of Ratio
// times the pod's B_eff holds the accesses under the horizon of 1 + Ratio
// times the GPU's capacity at R'; a shared tier, of Ratio times one pod's
// B_eff, holds them under the horizon of the fleet's accesses to one copy
// of each prefix, N' R' / k(N'), which does not move with N' (a prefix's
// accesses on k pods are one stream there, its gaps the per-pod ones over
// k). The total is the larger of the two stacks' hits, a floor on their
// union. With no tier size known, a tier that served hits at the fit keeps the total at
// h_inf, and only the GPU's share of it moves.

// HitTier is a cache tier past the GPU prefix cache (HitModel.Tiers): an
// inclusive LRU behind it, of Ratio times one pod's GPU B_eff, one per pod
// or, Shared, one for the fleet.
type HitTier struct {
	Ratio  float64 `json:"ratio"`
	Shared bool    `json:"shared,omitempty"`
}

// HitModelInputs is what FitHitModel fits from: the fit's total hit (1 -
// computed / whole prompt, every tier) and its GPU prefix-cache share, on
// FitCount prefill pods, the KV residency histograms over the same window,
// the tiers past the GPU where their sizes are known, the duplication K (0
// reads as 1) and the elasticity A.
type HitModelInputs struct {
	TotalHit, GPUHit float64
	FitCount         int
	Reuse            *ReuseHistograms
	Tiers            []HitTier
	K, A             float64
}

// HitModel is the reuse-gap law fitted at one prefill count (FitHitModel).
type HitModel struct {
	// HInf, Mu and Sigma are the law, F(g) = HInf * Phi((ln g - Mu)/Sigma),
	// g in seconds. CeilingAssumed says HInf is 1 because no tier past the
	// GPU served a hit at the fit.
	HInf           float64 `json:"hInf"`
	Mu             float64 `json:"mu"`
	Sigma          float64 `json:"sigma"`
	CeilingAssumed bool    `json:"ceilingAssumed,omitempty"`
	// HorizonSeconds is T_C, the median idle time before an eviction, and
	// GapMedianSeconds the hit gaps' median, both read at the fit.
	HorizonSeconds   float64 `json:"horizonSeconds"`
	GapMedianSeconds float64 `json:"gapMedianSeconds"`
	// FitCount is the prefill pods the law was read on, TotalHit and GPUHit
	// the hits read there.
	FitCount int     `json:"fitCount"`
	TotalHit float64 `json:"totalHit"`
	GPUHit   float64 `json:"gpuHit"`
	// TierHit says a tier past the GPU served at least MinTierHitLift of
	// the prompt at the fit; Tiers are the tiers whose sizes are known.
	TierHit bool      `json:"tierHit,omitempty"`
	Tiers   []HitTier `json:"tiers,omitempty"`
	// K is the duplication of a prefix across pods, A the elasticity of
	// the reuse gaps with the rate.
	K float64 `json:"k"`
	A float64 `json:"a"`
}

// MinTierHitLift is the least share of the prompt a tier past the GPU must
// serve at the fit for the total hit to be read as the law's ceiling, h_inf.
// Below it the GPU tier alone is fitted and the ceiling is 1. Stated, not
// measured: one point of the prompt, as whatif.MinTierShare.
const MinTierHitLift = 0.01

// FitHitModel fits the law at the fit's count, false where the inputs do
// not close it: no residency histograms, or ones newReuseHist refuses; a
// GPU hit not in (0, 1) or above the total; a hit-gap median not under the
// horizon; or a count under 1.
func FitHitModel(in HitModelInputs) (HitModel, bool) {
	if in.FitCount < 1 || in.Reuse == nil || !(in.GPUHit > 0 && in.GPUHit < 1) ||
		!(in.TotalHit >= in.GPUHit && in.TotalHit < 1) {
		return HitModel{}, false
	}
	gap, okG := newReuseHist(in.Reuse.Gap)
	idle, okI := newReuseHist(in.Reuse.Idle)
	if !okG || !okI {
		return HitModel{}, false
	}
	tc, m50 := idle.quantile(0.5), gap.quantile(0.5)
	if !(m50 > 0) || !(tc > m50) || math.IsInf(tc, 0) {
		return HitModel{}, false
	}
	m := HitModel{HorizonSeconds: tc, GapMedianSeconds: m50, FitCount: in.FitCount, TotalHit: in.TotalHit, GPUHit: in.GPUHit,
		Tiers: in.Tiers, K: max(in.K, 1), A: in.A, HInf: 1, CeilingAssumed: true}
	if in.TotalHit >= in.GPUHit+MinTierHitLift {
		m.HInf, m.CeilingAssumed, m.TierHit = in.TotalHit, false, true
	}
	// F(T_C) = GPUHit and F(m50) = GPUHit/2: two points of the normal in
	// ln g.
	zc, z50 := normalQuantile(in.GPUHit/m.HInf), normalQuantile(in.GPUHit/m.HInf/2)
	if !(zc > z50) || math.IsInf(zc, 0) || math.IsInf(z50, 0) {
		return HitModel{}, false
	}
	m.Sigma = (math.Log(tc) - math.Log(m50)) / (zc - z50)
	m.Mu = math.Log(tc) - m.Sigma*zc
	return m, true
}

// F is the share of block accesses whose reuse gap is at most g seconds.
func (m HitModel) F(g float64) float64 {
	if !(g > 0) {
		return 0
	}
	if math.IsInf(g, 1) {
		return m.HInf
	}
	return m.HInf * normalCDF((math.Log(g)-m.Mu)/m.Sigma)
}

// occupancy is integral_0^T (1 - F(g)) dg: the cache a unit rate of block
// accesses fills at horizon T. With a lognormal law,
// integral_0^T Phi((ln g - mu)/sigma) dg = T Phi(z) - e^(mu + sigma^2/2) Phi(z - sigma),
// z = (ln T - mu)/sigma. It is summed as T (1 - h_inf Phi(z)) plus the
// rest, with 1 - Phi(z) read from its own tail: far past the law's median
// T and T Phi(z) agree to every digit, and their difference is noise.
func (m HitModel) occupancy(t float64) float64 {
	if !(t > 0) {
		return 0
	}
	z := (math.Log(t) - m.Mu) / m.Sigma
	above := 0.5 * math.Erfc(z/math.Sqrt2)
	return t*((1-m.HInf)+m.HInf*above) + m.HInf*math.Exp(m.Mu+m.Sigma*m.Sigma/2)*normalCDF(z-m.Sigma)
}

// horizonHit is F at the horizon whose occupancy is target, HInf where no
// horizon fills it (with HInf at 1 the occupancy is bounded: every block
// reused fits).
func (m HitModel) horizonHit(target float64) float64 {
	if !(target > 0) {
		return 0
	}
	lo, hi := math.Log(m.HorizonSeconds)-60, math.Log(m.HorizonSeconds)+60
	if m.occupancy(math.Exp(hi)) < target {
		return m.HInf
	}
	for range 100 {
		mid := (lo + hi) / 2
		if m.occupancy(math.Exp(mid)) < target {
			lo = mid
		} else {
			hi = mid
		}
	}
	return m.F(math.Exp((lo + hi) / 2))
}

// DuplicationOfAffinityLoss is K, the duplication of a prefix across count
// prefill pods, read from the affinity the router gave up there: loss is 1
// - predicted / best available, the router prefix producer's share of the
// cached tokens it could have hit on the best endpoint and sent elsewhere
// (domain.RouterLatency.PrefixPredictedTokenRate). Each such request writes
// the prefix again on the pod it went to, so a prefix's accesses split,
// 1 - loss on the pod holding it and loss over the other count - 1. K is
// the number of pods the accesses read as, evenly: the inverse of the sum
// of the squared shares (the inverse Simpson concentration),
//
//	K = 1 / ((1 - loss)^2 + loss^2 / (count - 1)),
//
// 1 at no loss and count at an even spread. A loss the router counts is a
// floor on the true one: once a second pod holds the prefix, a request sent
// there loses nothing the router sees. 1 on one pod, and with a loss not in
// [0, 1).
func DuplicationOfAffinityLoss(loss float64, count int) float64 {
	if count <= 1 || !(loss > 0 && loss < 1) {
		return 1
	}
	return 1 / ((1-loss)*(1-loss) + loss*loss/float64(count-1))
}

// duplication is k(n): 1 on one pod, K on more.
func (m HitModel) duplication(n int) float64 {
	if n <= 1 {
		return 1
	}
	return max(m.K, 1)
}

// HitAt is the GPU and the total hit on count prefill pods at rateRatio
// times the fit's fleet rate. At the fit's count and rate the GPU hit is
// the fit's. false with a count under 1 or a ratio not a positive finite
// number.
func (m HitModel) HitAt(count int, rateRatio float64) (gpu, total float64, ok bool) {
	if count < 1 || m.FitCount < 1 || !(rateRatio > 0) || math.IsInf(rateRatio, 0) {
		return 0, 0, false
	}
	// R'/R: the accesses one pod takes at count, over the fit's.
	ratio := float64(m.FitCount) / float64(count) * m.duplication(count) / m.duplication(m.FitCount) *
		math.Pow(rateRatio, 1-m.A)
	base := m.occupancy(m.HorizonSeconds) / ratio
	gpu = m.horizonHit(base)
	total = gpu
	switch {
	case len(m.Tiers) > 0:
		perPod := 1.0
		for _, t := range m.Tiers {
			if !(t.Ratio > 0) {
				continue
			}
			if t.Shared {
				// The fleet's accesses, one copy of a prefix: its gaps
				// are the fit's per-pod gaps over the fit's duplication,
				// whatever the duplication at count, so base's k(N')/k(N)
				// is taken back out.
				total = max(total, m.horizonHit(base*t.Ratio*m.duplication(count)/float64(count)))
				continue
			}
			perPod += t.Ratio
		}
		total = max(total, m.horizonHit(base*perPod))
	case m.TierHit:
		total = max(gpu, m.HInf)
	}
	return gpu, total, true
}

// normalCDF is the standard normal's cumulative distribution.
func normalCDF(z float64) float64 { return 0.5 * math.Erfc(-z/math.Sqrt2) }

// normalQuantile is normalCDF's inverse.
func normalQuantile(p float64) float64 { return math.Sqrt2 * math.Erfinv(2*p-1) }
