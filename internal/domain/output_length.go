package domain

import (
	"math"
	"slices"
)

// The spread of output lengths behind the KV a resident request holds
// (issue ev-shindin/llm-scaling-manager#276): a lower bound on E[O^2] from a length histogram, which
// ReplicaMetrics.AvgOutputTokensSquared carries and shape.Dispersion reads.

// LengthBucket is one cumulative bucket of a length histogram: the requests a second
// whose length was at most UpperBound. The last bucket's UpperBound is +Inf.
type LengthBucket struct {
	UpperBound float64
	Rate       float64
}

// consistencyTolerance is how far, as a fraction of the mean, the mean may
// sit outside what the buckets allow before the pair is refused as not one
// reading. A sum and buckets scraped together agree exactly; rate()
// extrapolates each series on its own, which moves them apart a little.
const consistencyTolerance = 0.01

// SquaredMeanLowerBound returns the least mean squared length that the
// cumulative buckets and the exact mean allow, and whether they make a
// reading at all.
//
// The buckets say how many requests fell between two bounds, and not where:
// each bucket's mean can be anywhere between them, so E[O^2] is not known,
// only bounded. Its least value puts every bucket's requests at one length,
// the same length t in every bucket wherever its bounds allow, with t set so
// the lengths add up to the mean (a water level). The true E[O^2] is never
// below it: within a bucket the spread only adds, and no other placement that
// keeps the mean lowers the sum of squares. So a dispersion priced from it
// corrects the KV per request towards the truth and never past it, and a
// fleet whose requests are all one length reads no spread at all (D = 1).
//
// Not a reading: no +Inf bucket (the requests above the last bound are not
// counted), no requests, a mean that is not a positive finite number, or a
// mean the buckets cannot hold. A bucket rate that falls from one bound to the
// next (a counter reset inside the window) is read as no requests in that
// bucket.
func SquaredMeanLowerBound(buckets []LengthBucket, mean float64) (float64, bool) {
	if !(mean > 0) || math.IsInf(mean, 0) {
		return 0, false
	}
	bs := make([]LengthBucket, 0, len(buckets))
	for _, b := range buckets {
		// A length is never negative, so neither is a bound; -Inf or a
		// negative bound would put a span's top under its bottom.
		if !(b.UpperBound >= 0) || !(b.Rate >= 0) || math.IsInf(b.Rate, 0) {
			continue
		}
		bs = append(bs, b)
	}
	slices.SortFunc(bs, func(x, y LengthBucket) int {
		switch {
		case x.UpperBound < y.UpperBound:
			return -1
		case x.UpperBound > y.UpperBound:
			return 1
		}
		return 0
	})
	if len(bs) == 0 || !math.IsInf(bs[len(bs)-1].UpperBound, 1) {
		return 0, false
	}
	// Per-bucket counts between consecutive bounds; a length is never
	// negative, so the first bucket starts at 0.
	type span struct{ lo, hi, n float64 }
	spans := make([]span, 0, len(bs))
	lo, prev := 0.0, 0.0
	for _, b := range bs {
		n := max(b.Rate-prev, 0)
		prev = max(prev, b.Rate)
		if n > 0 {
			spans = append(spans, span{lo: max(lo, 0), hi: b.UpperBound, n: n})
		}
		lo = b.UpperBound
	}
	total := prev
	if !(total > 0) {
		return 0, false
	}
	// fill is the sum of lengths with every bucket at t, clamped into its
	// bounds: non-decreasing and piecewise linear in t.
	fill := func(t float64) float64 {
		var s float64
		for _, sp := range spans {
			s += sp.n * min(max(t, sp.lo), sp.hi)
		}
		return s
	}
	target := mean * total
	floor := fill(0)
	if target < floor {
		if target < floor*(1-consistencyTolerance) {
			return 0, false
		}
		target = floor
	}
	// The ceiling is finite only when nothing is above the last finite
	// bound.
	top := spans[len(spans)-1]
	if !math.IsInf(top.hi, 1) {
		if ceil := fill(top.hi); target > ceil {
			if target > ceil*(1+consistencyTolerance) {
				return 0, false
			}
			target = ceil
		}
	}
	// Bisect for the water level: between 0 and a level at which the
	// highest bucket alone carries the whole mean.
	hiT := top.lo + target/top.n
	if !math.IsInf(top.hi, 1) {
		hiT = top.hi
	}
	loT := 0.0
	for range 200 {
		mid := (loT + hiT) / 2
		if fill(mid) < target {
			loT = mid
		} else {
			hiT = mid
		}
	}
	t := (loT + hiT) / 2
	var sq float64
	for _, sp := range spans {
		v := min(max(t, sp.lo), sp.hi)
		sq += sp.n * v * v
	}
	// A mean clamped onto the buckets (within consistencyTolerance) moved the
	// bound with it. The caller divides by its own mean, so the bound is
	// rescaled to keep the spread the buckets and the clamped mean imply:
	// sq/total / clamped^2 = result / mean^2. Unscaled, a mean 1% under the
	// floor read a spread up to 2% above it.
	if clamped := target / total; clamped != mean {
		sq *= (mean / clamped) * (mean / clamped)
	}
	// Never below mean^2 (Jensen). In exact arithmetic the water level's sum
	// of squares already is, so this only guards rounding in the bisection,
	// which must not make a one-length fleet read a spread under 1; no test
	// can tell it from the bare sum.
	return max(sq/total, mean*mean), true
}
