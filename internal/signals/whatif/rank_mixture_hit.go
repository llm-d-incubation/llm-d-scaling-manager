package whatif

import (
	"math"
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// The rank mixture at another prefill count's prefix hit (issue
// llm-d-incubation/llm-d-scaling-manager#384): the model draws each
// request's computed tokens from the source's computed-token histogram, so
// where the hit priced at another prefill count moves the computed tokens a
// request (prefill.HitAtCountOf), the histogram moves with it
// (ComputedAtCount): at a lower hit as a miss mixture, some of the requests
// that hit at the fit now missing their whole prompt; at a higher one by
// scaling its bounds.

// CumulativeMean is the mean of a cumulative histogram (upper bound to
// cumulative count, +Inf the total) as the rank mixture draws from it:
// log-uniform inside a bucket, uniform in one starting at or under 1. ok is
// false where it holds nothing.
func CumulativeMean(b map[float64]float64) (float64, bool) {
	h, ok := newCumHist(b)
	if !ok {
		return 0, false
	}
	var mean, prev float64
	for i := range h.cum {
		mean += (h.cum[i] - prev) * h.bucketMean(i)
		prev = h.cum[i]
	}
	return mean, true
}

// CumulativeQuantile is the length at share q in (0, 1) of a cumulative
// histogram as the rank mixture draws from it (CumulativeMean). ok is false
// where it holds nothing or q is not in (0, 1).
func CumulativeQuantile(b map[float64]float64, q float64) (float64, bool) {
	h, ok := newCumHist(b)
	if !ok || !(q > 0 && q < 1) {
		return 0, false
	}
	return h.ppf(q), true
}

// ComputedAtScale is computed, a cumulative computed-token histogram, with
// every bound scaled by scale, and capped at the prompt draw: at each bound
// its cumulative share is at least prompt's, so no quantile of it is above
// the whole prompt's at the same share, and no request computes more tokens
// than its prompt holds. Its total is computed's. computed comes back as it
// is where scale is 1, not a positive finite number, or either histogram
// holds nothing.
func ComputedAtScale(computed, prompt map[float64]float64, scale float64) map[float64]float64 {
	if scale == 1 || !(scale > 0) || math.IsInf(scale, 0) {
		return computed
	}
	hc, okc := newCumHist(computed)
	hp, okp := newCumHist(prompt)
	if !okc || !okp {
		return computed
	}
	var total float64
	edges := make([]float64, 0, len(computed)+len(prompt))
	for le, c := range computed {
		if Finite(c) {
			total = max(total, c)
		}
		if Finite(le) {
			edges = append(edges, le*scale)
		}
	}
	for le := range prompt {
		if Finite(le) {
			edges = append(edges, le)
		}
	}
	slices.Sort(edges)
	edges = slices.Compact(edges)
	out := make(map[float64]float64, len(edges)+1)
	for _, x := range edges {
		out[x] = total * max(hc.cdf(x/scale), hp.cdf(x))
	}
	out[math.Inf(1)] = total
	return out
}

// ComputedMissMixture is computed, a cumulative computed-token histogram,
// at a lower prefix hit: (1 - w) computed + w prompt, a share w of the
// requests missing their whole prompt, with w set so that its mean is
// target. A lower hit is a prefix no longer resident: the requests that
// reused it compute their whole prompt, and the rest compute what they did.
// Scaling the bounds instead moves every request's computed share alike,
// and on the lab run read the count transfer's TTFT at 0.67 of the
// measured where the mixture read 1.1. Its total is computed's. computed
// comes back as it is where target is not above its mean, or either
// histogram holds nothing; a target at or above the prompt's mean is every
// request missing.
func ComputedMissMixture(computed, prompt map[float64]float64, target float64) map[float64]float64 {
	ec, okc := CumulativeMean(computed)
	ep, okp := CumulativeMean(prompt)
	if !okc || !okp || !(target > ec) || !Finite(target) {
		return computed
	}
	if !(ep > ec) {
		return computed
	}
	lo, hi := 0.0, 1.0
	if target < ep {
		for range 60 {
			w := (lo + hi) / 2
			if m, _ := CumulativeMean(missMixture(computed, prompt, w)); m < target {
				lo = w
			} else {
				hi = w
			}
		}
	} else {
		lo = 1
	}
	return missMixture(computed, prompt, (lo+hi)/2)
}

// missMixture is (1 - w) computed + w prompt on the union of their bounds,
// computed's total.
func missMixture(computed, prompt map[float64]float64, w float64) map[float64]float64 {
	hc, _ := newCumHist(computed)
	hp, _ := newCumHist(prompt)
	var total float64
	edges := make([]float64, 0, len(computed)+len(prompt))
	for le, c := range computed {
		if Finite(c) {
			total = max(total, c)
		}
		if Finite(le) {
			edges = append(edges, le)
		}
	}
	for le := range prompt {
		if Finite(le) {
			edges = append(edges, le)
		}
	}
	slices.Sort(edges)
	edges = slices.Compact(edges)
	out := make(map[float64]float64, len(edges)+1)
	for _, x := range edges {
		out[x] = total * ((1-w)*hc.cdf(x) + w*hp.cdf(x))
	}
	out[math.Inf(1)] = total
	return out
}

// ComputedAtCount prices the prefix hit at count from the fit's computed
// and prompt histograms read on fitCount pods (prefill.HitAtCountOf, with
// the reuse-gap law m where it was fitted), and the computed-token
// histogram that hit gives: a miss mixture where the hit falls
// (ComputedMissMixture), the bounds scaled where it rises (ComputedAtScale).
// The fit's hit is 1 - E[computed] / E[prompt]. ok is false where nothing
// is priced; computed then comes back as it is. The planner's E[I_eff]
// (prefill.HitAtCountOf on the same hit and law), the main view's model
// and a backtest's count cells all price through it.
func ComputedAtCount(computed, prompt map[float64]float64, fitCount, count int, m *prefill.HitModel) (
	prefill.HitAtCount, map[float64]float64, bool) {
	ec, okc := CumulativeMean(computed)
	el, okl := CumulativeMean(prompt)
	if !okc || !okl || !(el > 0) {
		return prefill.HitAtCount{}, computed, false
	}
	r, ok := prefill.HitAtCountOf(max(1-ec/el, 0), ec, fitCount, count, m)
	switch {
	case !ok || !r.Moved():
		return r, computed, ok
	case r.Hit < r.FitHit:
		return r, ComputedMissMixture(computed, prompt, r.Computed), true
	}
	return r, ComputedAtScale(computed, prompt, r.ComputedScale), true
}

// ReuseHistogramsOf is the KV residency histograms read from pooled
// cumulative ones, upper bound to rate (PromptWindow.HitModel): nil where
// either holds nothing. A bound at or under 0 is left out: its count is in
// the next one's.
func ReuseHistogramsOf(gap, idle map[float64]float64) *prefill.ReuseHistograms {
	g, i := reuseBuckets(gap), reuseBuckets(idle)
	if len(g) == 0 || len(i) == 0 {
		return nil
	}
	return &prefill.ReuseHistograms{Gap: g, Idle: i}
}

// reuseBuckets is a cumulative histogram's buckets, bounds ascending.
func reuseBuckets(m map[float64]float64) []prefill.ReuseBucket {
	edges := make([]float64, 0, len(m))
	for le, c := range m {
		if le > 0 && Finite(c) {
			edges = append(edges, le)
		}
	}
	slices.Sort(edges)
	out := make([]prefill.ReuseBucket, 0, len(edges))
	for _, le := range edges {
		out = append(out, prefill.ReuseBucket{UpperEdge: le, Count: m[le]})
	}
	return out
}

// HitModel is the reuse-gap law (prefill.FitHitModel) fitted on the window
// read on fitCount prefill pods: its hit over every tier (ComputedHit), the
// GPU prefix cache's share of it from the cached tokens by tier where they
// were read (TierLoad), else all of it, and its KV residency histograms
// summed over its cycles. The tiers' sizes are not recorded: a tier that
// served hits keeps the total at its read. The duplication K is read from
// the affinity the router gave up over the window on fitCount pods
// (AffinityLoss, prefill.DuplicationOfAffinityLoss); with no router prefix
// series it is taken as none (k = 1), which prices fewer hits at one pod
// than a router that duplicates prefixes gives. a = 1, gaps that move with
// the rate: the planner prices the hit at the traffic read. nil where the
// window holds no histograms or they do not close the fit.
func (w *PromptWindow) HitModel(fitCount int) *prefill.HitModel {
	if w == nil {
		return nil
	}
	gap, idle := map[float64]float64{}, map[float64]float64{}
	for _, s := range w.samples {
		for le, r := range s.reuseGap {
			gap[le] += r
		}
		for le, r := range s.idle {
			idle[le] += r
		}
	}
	reuse := ReuseHistogramsOf(gap, idle)
	total, ok := w.ComputedHit()
	if reuse == nil || !ok {
		return nil
	}
	gpu := total
	if tl, ok := w.TierLoad(); ok {
		gpu = min(tl.Shares[SourceDevice], total)
	}
	k := 1.0
	if loss, ok := w.AffinityLoss(); ok {
		k = prefill.DuplicationOfAffinityLoss(loss, fitCount)
	}
	m, ok := prefill.FitHitModel(prefill.HitModelInputs{TotalHit: total, GPUHit: gpu, FitCount: fitCount, Reuse: reuse,
		K: k, A: 1})
	if !ok {
		return nil
	}
	return &m
}

// bucketMean is bucket i's mean as ppf draws it.
func (h cumHist) bucketMean(i int) float64 {
	lo, hi := h.lo[i], h.hi[i]
	switch {
	case hi <= lo:
		return lo
	case lo <= 1:
		return (lo + hi) / 2
	}
	return (hi - lo) / (math.Log(hi) - math.Log(lo))
}

// cdf is the cumulative share at or under x, ppf's inverse.
func (h cumHist) cdf(x float64) float64 {
	start := 0.0
	for i := range h.cum {
		lo, hi := h.lo[i], h.hi[i]
		switch {
		case x >= hi:
			start = h.cum[i]
			continue
		case x <= lo:
			return start
		case lo <= 1:
			return start + (h.cum[i]-start)*(x-lo)/(hi-lo)
		}
		return start + (h.cum[i]-start)*(math.Log(x)-math.Log(lo))/(math.Log(hi)-math.Log(lo))
	}
	return 1
}

// HitAtCountFlag is the flag a figure priced at r's prefill count carries:
// PrefixHitAtCount where the priced hit moved from the read one by more
// than MaterialPrefixHitMove/2, PrefixHitHeld where a hit above that is held
// at more pods, else none.
func HitAtCountFlag(r prefill.HitAtCount) Flag {
	material := MaterialPrefixHitMove / 2
	switch {
	case r.Moved() && math.Abs(r.FitHit-r.Hit) > material:
		return PrefixHitAtCount
	case r.Rule == prefill.HitHeld && r.FitHit > material:
		return PrefixHitHeld
	}
	return ""
}
