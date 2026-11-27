package whatif

import (
	"cmp"
	"math"
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// LatencyHist is a cumulative latency histogram of request rates, repaired
// to be monotone, ending in +Inf: the router's TTFT or TPOT histogram as the
// capacity planner and the slo analyzer's v2 models read it.
type LatencyHist struct {
	// Buckets is the repaired histogram, sorted by bound; Total its +Inf
	// bucket, the requests a second it holds.
	Buckets []domain.LatencyBucket
	Total   float64
}

// LatencyHistOf is the histogram in, repaired as Prometheus's
// histogram_quantile repairs one; ok false with no +Inf bucket or no request.
func LatencyHistOf(in []domain.LatencyBucket) (LatencyHist, bool) {
	bs := make([]domain.LatencyBucket, 0, len(in))
	for _, b := range in {
		if math.IsNaN(b.UpperBound) || !Finite(b.Rate) || b.Rate < 0 {
			continue
		}
		bs = append(bs, b)
	}
	slices.SortFunc(bs, func(x, y domain.LatencyBucket) int { return cmp.Compare(x.UpperBound, y.UpperBound) })
	if len(bs) == 0 || !math.IsInf(bs[len(bs)-1].UpperBound, 1) {
		return LatencyHist{}, false
	}
	for i := 1; i < len(bs); i++ {
		bs[i].Rate = max(bs[i].Rate, bs[i-1].Rate)
	}
	total := bs[len(bs)-1].Rate
	if !(total > 0) {
		return LatencyHist{}, false
	}
	return LatencyHist{Buckets: bs, Total: total}, true
}

// Top is the highest finite bound, 0 with none.
func (h LatencyHist) Top() float64 {
	if len(h.Buckets) < 2 {
		return 0
	}
	return h.Buckets[len(h.Buckets)-2].UpperBound
}

// Quantile is the q-quantile by linear interpolation inside its bucket, as
// histogram_quantile, but +Inf in the +Inf bucket: the top bound would read
// the tail low.
func (h LatencyHist) Quantile(q float64) float64 {
	rank := q * h.Total
	for i, b := range h.Buckets {
		if b.Rate < rank {
			continue
		}
		if math.IsInf(b.UpperBound, 1) {
			return math.Inf(1)
		}
		lower, below := 0.0, 0.0
		if i > 0 {
			lower, below = h.Buckets[i-1].UpperBound, h.Buckets[i-1].Rate
		}
		if b.Rate == below {
			return b.UpperBound
		}
		return lower + (b.UpperBound-lower)*(rank-below)/(b.Rate-below)
	}
	return math.Inf(1)
}
