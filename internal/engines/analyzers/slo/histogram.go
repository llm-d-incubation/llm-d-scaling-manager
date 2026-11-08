package slo

import (
	"cmp"
	"math"
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// histogram is a cumulative latency histogram in ascending bucket order,
// repaired to be monotone, ending in the +Inf bucket.
type histogram struct {
	buckets []domain.LatencyBucket
	// total is the +Inf bucket's rate: every request.
	total float64
}

// newHistogram orders and repairs router bucket rates. ok is false when there
// is no +Inf bucket or no request at all.
//
// The repair is the one Prometheus's histogram_quantile makes: bucket rates
// summed across series and taken over a window can come out non-monotone, and
// a cumulative histogram cannot hold fewer requests at a larger bound.
func newHistogram(in []domain.LatencyBucket) (histogram, bool) {
	bs := make([]domain.LatencyBucket, 0, len(in))
	for _, b := range in {
		if math.IsNaN(b.UpperBound) || !whatif.Finite(b.Rate) || b.Rate < 0 {
			continue
		}
		bs = append(bs, b)
	}
	slices.SortFunc(bs, func(x, y domain.LatencyBucket) int { return cmp.Compare(x.UpperBound, y.UpperBound) })
	if len(bs) == 0 || !math.IsInf(bs[len(bs)-1].UpperBound, 1) {
		return histogram{}, false
	}
	for i := 1; i < len(bs); i++ {
		bs[i].Rate = max(bs[i].Rate, bs[i-1].Rate)
	}
	total := bs[len(bs)-1].Rate
	if !(total > 0) {
		return histogram{}, false
	}
	return histogram{buckets: bs, total: total}, true
}

// quantile returns the q-quantile, 0 < q < 1, by linear interpolation inside
// the bucket it falls in, as histogram_quantile does -- with one deliberate
// difference: a quantile that falls in the +Inf bucket is +Inf, not the
// highest finite bound. Reporting the top bound there would under-read exactly
// the tail an SLO is about.
func (h histogram) quantile(q float64) float64 {
	rank := q * h.total
	for i, b := range h.buckets {
		if b.Rate < rank {
			continue
		}
		if math.IsInf(b.UpperBound, 1) {
			return math.Inf(1)
		}
		lower, below := 0.0, 0.0
		if i > 0 {
			lower, below = h.buckets[i-1].UpperBound, h.buckets[i-1].Rate
		}
		if b.Rate == below {
			return b.UpperBound
		}
		return lower + (b.UpperBound-lower)*(rank-below)/(b.Rate-below)
	}
	return math.Inf(1)
}

// fractionWithin returns the share of requests at or under target, and whether
// target is a bucket bound. On a bound it is exact; between two bounds it is
// interpolated, which assumes requests are spread evenly inside the bucket.
func (h histogram) fractionWithin(target float64) (float64, bool) {
	lower, below := 0.0, 0.0
	for _, b := range h.buckets {
		if b.UpperBound == target {
			return b.Rate / h.total, true
		}
		if b.UpperBound > target {
			if math.IsInf(b.UpperBound, 1) {
				return below / h.total, false
			}
			within := below + (b.Rate-below)*(target-lower)/(b.UpperBound-lower)
			return within / h.total, false
		}
		lower, below = b.UpperBound, b.Rate
	}
	return 1, false
}

// minSamples is the fewest requests a window must hold before a percentile p
// (in percent) or an attainment at it is reported: max(50, 10/(1-p)). That is
// 100 at p90 and 1000 at p99 -- ten requests beyond the percentile, so the tail
// the figure describes is more than a handful of requests. A mean needs 50.
// Computed as 1000/(100-p), exact for a whole p: 10/(1-0.9) is
// 100.00000000000003 in floating point, and a window of exactly the documented
// 100 was not judged.
func minSamples(percentile int) float64 {
	if percentile <= 0 || percentile >= 100 {
		return 50
	}
	return max(50, 1000/float64(100-percentile))
}
