package whatifapi

import (
	"math"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// The busiest hour as measured: the latency the router measured over the
// source's busiest UTC hour, so that the page leads with what users saw
// before it shows any prediction. It reads the router's histograms only, as
// a backtest scores against them, and prices nothing.

// MeasuredHour is the source's busiest UTC hour, by mean request rate over
// its cycles, and the TTFT and TPOT the router measured over it: its
// cycles' histograms pooled.
type MeasuredHour struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Rate is the hour's mean request rate over its cycles with traffic, r/s.
	Rate float64 `json:"rate"`
	// Requests is about how many requests the router measured in the hour.
	Requests float64        `json:"requests"`
	TTFT     []MeasuredStat `json:"ttft"`
	TPOT     []MeasuredStat `json:"tpot"`
}

// MeasuredStat is one statistic measured, seconds: Value where the
// histogram places it; Bucket, with no Value, where the bucket it falls in
// is too wide to place it (wider than MeasuredBucketWidth); Why where
// neither is given ("above the top bucket", "too few requests").
type MeasuredStat struct {
	Statistic string   `json:"statistic"`
	Value     *float64 `json:"value,omitempty"`
	Bucket    *Band    `json:"bucket,omitempty"`
	Why       string   `json:"why,omitempty"`
}

// minHourCycles is the fewest cycles with traffic an hour needs to be the
// busiest: fewer read one burst as the hour.
const minHourCycles = 10

// BusiestHourMeasured is the source's busiest UTC hour and what the router
// measured over it; nil with no router histogram in the source, latency not
// aligned with the series, or no hour of minHourCycles cycles with traffic.
func BusiestHourMeasured(series []SeriesSample, latency []LatencySample) *MeasuredHour {
	if len(series) == 0 || len(series) != len(latency) {
		return nil
	}
	type hour struct {
		sum float64
		idx []int
	}
	hours := map[time.Time]*hour{}
	for i, x := range series {
		if !(x.Rate > 0) || latency[i].Router == nil {
			continue
		}
		h := x.At.UTC().Truncate(time.Hour)
		if hours[h] == nil {
			hours[h] = &hour{}
		}
		hours[h].sum += x.Rate
		hours[h].idx = append(hours[h].idx, i)
	}
	var best time.Time
	bestRate := 0.0
	for h, v := range hours {
		if len(v.idx) < minHourCycles {
			continue
		}
		if r := v.sum / float64(len(v.idx)); r > bestRate || (r == bestRate && h.Before(best)) {
			best, bestRate = h, r
		}
	}
	if !(bestRate > 0) {
		return nil
	}
	idx := hours[best].idx
	ttft, tpot := map[float64]float64{}, map[float64]float64{}
	var ttftSum, ttftCount, tpotSum, tpotCount, window float64
	for _, i := range idx {
		r := latency[i].Router
		for _, b := range r.TTFTBuckets {
			ttft[b.UpperBound] += b.Rate
		}
		for _, b := range r.TPOTBuckets {
			tpot[b.UpperBound] += b.Rate
		}
		ttftSum, ttftCount = ttftSum+r.TTFTSumRate, ttftCount+r.TTFTCountRate
		tpotSum, tpotCount = tpotSum+r.TPOTSumRate, tpotCount+r.TPOTCountRate
		window += measureWindowOf(latency[i], MeasuredRouter)
	}
	n := float64(len(idx))
	// The pooled rates over one cycle's window: the windows overlap, so a
	// sum over cycles is not a count.
	perWindow := window / n / n
	out := &MeasuredHour{From: best, To: best.Add(time.Hour), Rate: bestRate, Requests: ttftCount / n * 3600}
	out.TTFT = measuredStats(ttft, ttftSum, ttftCount, perWindow)
	out.TPOT = measuredStats(tpot, tpotSum, tpotCount, perWindow)
	return out
}

// measuredStats is the mean, p90 and p95 of a pooled histogram, each
// placed, bracketed by its bucket, or said why not.
func measuredStats(pooled map[float64]float64, sum, count, perWindow float64) []MeasuredStat {
	out := make([]MeasuredStat, 0, 3)
	mean := MeasuredStat{Statistic: statisticMean}
	switch {
	case !(count > 0):
		mean.Why = "not measured"
	case count*perWindow < minRequestsFor(0):
		mean.Why = "too few requests"
	default:
		mean.Value = ptr(sum / count)
	}
	out = append(out, mean)
	buckets := make([]domain.LatencyBucket, 0, len(pooled))
	for ub, rate := range pooled {
		buckets = append(buckets, domain.LatencyBucket{UpperBound: ub, Rate: rate})
	}
	h, ok := histOf(buckets)
	for _, q := range []float64{0.9, 0.95} {
		s := MeasuredStat{Statistic: statisticName(q)}
		switch {
		case !ok:
			s.Why = "not measured"
		case h.Total*perWindow < minRequestsFor(q):
			s.Why = "too few requests"
		default:
			v := h.Quantile(q)
			switch {
			case math.IsInf(v, 1):
				s.Why = "above the top bucket"
			case h.coarseBucket(q) != nil:
				s.Bucket = h.coarseBucket(q)
			default:
				s.Value = ptr(v)
			}
		}
		out = append(out, s)
	}
	return out
}
