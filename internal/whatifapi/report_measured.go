package whatifapi

import (
	"fmt"
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// A report's OVER-PROVISIONED verdict says a role has a replica to spare. The
// model can say so of a fleet whose TTFT, as the router measured it at the
// load judged, was over its target: the fleet missed the target it would be
// cut on. WithholdOverOnMeasured makes such a verdict NOT JUDGED.

// measuredOverWords says why an over-provisioned verdict is withheld.
const measuredOverWords = "a fleet that missed its TTFT target has nothing to spare"

// MeasuredTTFTAtLoad is the TTFT the router measured at the statistic q (0
// the mean) over the cycles a load judged at stat was read from: every cycle
// at or above rate for the p95 and the peak, every one with traffic for the
// mean. Only the cycles that served with at least fleet's replicas of each
// role count: a smaller fleet's miss says nothing of the fleet judged, and a
// larger one's says it would miss too. The cycles' router histograms are
// pooled, so a busy cycle weighs by its requests. ok is false with no router
// histogram at those cycles, or too few requests in one cycle's window to
// read the statistic.
func MeasuredTTFTAtLoad(series []SeriesSample, latency []LatencySample, fleet FleetCounts, stat string, rate, q float64) (float64, bool) {
	if len(series) != len(latency) || !(rate > 0) {
		return 0, false
	}
	pooled := map[float64]float64{}
	var sum, count, window float64
	n := 0
	for i, x := range series {
		l := latency[i].Router
		if l == nil || !atLoadCycle(x, stat, rate) || x.Prefill < fleet.Prefill || x.Decode < fleet.Decode {
			continue
		}
		for _, b := range l.TTFTBuckets {
			pooled[b.UpperBound] += b.Rate
		}
		sum, count = sum+l.TTFTSumRate, count+l.TTFTCountRate
		window += measureWindowOf(latency[i], MeasuredRouter)
		n++
	}
	if n == 0 {
		return 0, false
	}
	if q == 0 {
		// The pooled rate over one cycle's window: windows overlap, so the
		// sum over cycles is not a count.
		if !(count > 0) || count/float64(n)*window/float64(n) < minRequestsFor(0) {
			return 0, false
		}
		return sum / count, true
	}
	h, ok := histOf(bucketsOf(pooled))
	if !ok || h.Total/float64(n)*window/float64(n) < minRequestsFor(q) {
		return 0, false
	}
	v := h.Quantile(q)
	if !whatif.Finite(v) {
		// Past the top bucket: at least the top bound.
		return h.Top(), h.Top() > 0
	}
	return v, true
}

// atLoadCycle says the cycle x is one a load judged at stat and rate was read
// from: one with traffic for the mean, one at or above rate for the p95 and
// the peak. MeasuredTTFTAtLoad and DecodeKVSpreadOf select by it alone.
func atLoadCycle(x SeriesSample, stat string, rate float64) bool {
	floor := rate
	if stat == LoadMean {
		floor = 0
	}
	return x.Rate > 0 && x.Rate >= floor
}

// MinKVSpreadFill is the mean decode KV fill, a fraction of each pod's KV
// cache, under which a cycle's busiest/mean ratio is not read
// (DecodeKVSpreadOf keeps a mean at or above it). On a nearly idle fleet a
// pod's fill is a request or two, so the ratio is the count's noise: two
// requests against one reads 2, at a fill no decode price feels. 0.05 is a
// twentieth of the cache, far under any KV band the planner prices, so it
// leaves out idle stretches and nothing a figure could rest on. It is a
// fraction of the cache, not of any model's size.
const MinKVSpreadFill = 0.05

// DecodeKVSpreadOf is the decode pods' KV fill spread over series
// (ReportKVSpread), and with an observed load at stat and rate (above 0) the
// median over the cycles that load was read from (atLoadCycle). A cycle is
// read with two decode pods or more, a finite busiest fill and a finite mean
// of MinKVSpreadFill or more; a recording made before the series kept the
// fill reads no pod at any cycle. Nil where no cycle is read.
func DecodeKVSpreadOf(series []SeriesSample, stat string, rate float64) *ReportKVSpread {
	var all, atLoad, pods []float64
	out := &ReportKVSpread{}
	for _, x := range series {
		if x.DecodeKVPods < 2 || !whatif.Finite(x.DecodeKVMean) || x.DecodeKVMean < MinKVSpreadFill || !whatif.Finite(x.DecodeKVBusiest) {
			continue
		}
		ratio := x.DecodeKVBusiest / x.DecodeKVMean
		all = append(all, ratio)
		pods = append(pods, float64(x.DecodeKVPods))
		out.BusiestFillPeak, out.MeanFillPeak = max(out.BusiestFillPeak, x.DecodeKVBusiest), max(out.MeanFillPeak, x.DecodeKVMean)
		if rate > 0 && atLoadCycle(x, stat, rate) {
			atLoad = append(atLoad, ratio)
		}
	}
	if len(all) == 0 {
		return nil
	}
	out.Cycles, out.Median, out.P95, out.Peak = len(all), median(all), nearestRank(all, 0.95), slices.Max(all)
	out.Pods = ReportKVSpreadPods{Median: median(pods), Min: int(slices.Min(pods)), Max: int(slices.Max(pods))}
	if len(atLoad) > 0 {
		m := median(atLoad)
		out.AtLoad, out.AtLoadCycles = &m, len(atLoad)
	}
	return out
}

// AttachKVSpread sets each shape's decode KV spread (ReportDecodeUse.KVSpread)
// from series, the cycles of the window the report was read from, at the
// load judged where it was observed. The spread is the window's, of the
// fleet that ran, whatever fleet a shape is priced at. It moves no figure.
func (r *ReportResponse) AttachKVSpread(series []SeriesSample) {
	var rate float64
	if r.Load.Seen != nil && r.Load.Source == "observed" {
		rate = *r.Load.Seen
	}
	spread := DecodeKVSpreadOf(series, r.Load.Statistic, rate)
	if spread == nil {
		return
	}
	for i := range r.Shapes {
		s := *spread
		r.Shapes[i].Utilization.Decode.KVSpread = &s
	}
}

// bucketsOf is a pooled histogram, bound to rate, as buckets.
func bucketsOf(pooled map[float64]float64) []domain.LatencyBucket {
	out := make([]domain.LatencyBucket, 0, len(pooled))
	for ub, rate := range pooled {
		out = append(out, domain.LatencyBucket{UpperBound: ub, Rate: rate})
	}
	return out
}

// WithholdOverOnMeasured makes NOT JUDGED each shape's OVER-PROVISIONED
// verdict at the load judged where the router measured the TTFT over its
// target there (MeasuredTTFTAtLoad, over the cycles that served with at
// least the fleet's replicas), and flags the shape
// FlagMeasuredTTFTOverTarget. A shape the question typed is a what-if of
// other traffic, and a growth line one at a rate no cycle ran at: both are
// left as they are.
func (r *ReportResponse) WithholdOverOnMeasured(series []SeriesSample, latency []LatencySample) {
	t := r.SLO.TTFT
	if t == nil || r.Load.Seen == nil || r.Load.Source != "observed" {
		return
	}
	q, err := parseStatistic("ttft.statistic", t.Statistic)
	if err != nil {
		return
	}
	fleet := FleetCounts{Prefill: r.Fleet.Prefill, Decode: r.Fleet.Decode}
	m, ok := MeasuredTTFTAtLoad(series, latency, fleet, r.Load.Statistic, *r.Load.Seen, q)
	if !ok || !(m > t.Target) {
		return
	}
	for i := range r.Shapes {
		s := &r.Shapes[i]
		v := &s.Verdict
		if v.Verdict != VerdictOver || s.Source == shapeSourceAsked {
			continue
		}
		v.Verdict = VerdictNotJudged
		why := fmt.Sprintf("the router measured TTFT %s %s against a %s target at the load judged", t.Statistic, secondsWords(m), secondsWords(t.Target))
		v.Reason += ". " + why + ": " + measuredOverWords
		v.Summary = fmt.Sprintf("%s: the model finds a replica to spare, but %s; %s.", v.Verdict, why, measuredOverWords)
		v.Shrink = []string{}
		s.Flags = appendNew(s.Flags, FlagMeasuredTTFTOverTarget)
		r.ExitCode = max(r.ExitCode, whatif.ExitRefused)
	}
}
