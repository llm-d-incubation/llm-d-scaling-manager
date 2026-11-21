package learnreplay

import (
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// engineTPOTWindow is the decode engines' per-request TPOT histogram of one
// cycle, pooled over the rows, as the what-if library reads it: the fall-back
// where the router's histogram is empty or coarse
// (docs/proposals/slo-capacity-planner.md, "Sources"). Read, not fed back, so
// it needs no replay switch.
//
// The collector reads the histogram's buckets and not its _sum, so the
// window's mean is each row's own ITL, weighted by its requests: the assumption
// a mean TPOT target already makes, that a request-weighted mean and a step-time
// mean agree. That sum is not the histogram's, so SumMeasured stays false and
// a coarse bucket is read at its upper edge: the mean bound needs the sum of
// the very requests the buckets count. A row with no finite ITL, or edges
// another row does not share, is left out. ok is false with no row left.
func engineTPOTWindow(rows []domain.ReplicaMetrics, roles map[string]string) (whatif.TPOTWindow, bool) {
	var w whatif.TPOTWindow
	for _, rm := range rows {
		if len(rm.TPOTBuckets) == 0 || rm.ITLState() != domain.ITLFinite ||
			domain.CanonicalRole(roles[rm.VariantName]) == domain.RolePrefill {
			continue
		}
		if w.Buckets == nil {
			w.Buckets = make([]whatif.HistogramBucket, len(rm.TPOTBuckets))
			for i, b := range rm.TPOTBuckets {
				w.Buckets[i].UpperEdge = b.UpperBound
			}
		}
		if !slices.EqualFunc(w.Buckets, rm.TPOTBuckets, func(h whatif.HistogramBucket, b domain.LatencyBucket) bool {
			return h.UpperEdge == b.UpperBound
		}) {
			continue
		}
		for i, b := range rm.TPOTBuckets {
			w.Buckets[i].Count += b.Rate
		}
		count := rm.TPOTBuckets[len(rm.TPOTBuckets)-1].Rate
		w.Count += count
		w.Sum += count * rm.AvgITL
	}
	return w, w.Buckets != nil
}

// decodeFill is the decode replicas' mean KV fill in one cycle, the fill a
// TPOT window of the cycle is filed under for R_q by fill
// (whatif.TPOTWindow.Fill). Each serving decode row reads the k its ITL pairs
// with (domain.ReplicaMetrics.KvUsageForITL, or at the busiest rank for a
// variant busiest names: fillForITL); a row with no fill read, a prefill row,
// and a row that serves nothing yet (prefill.Serving) are left out. ok is
// false with no row left.
func decodeFill(rows []domain.ReplicaMetrics, roles map[string]string, busiest func(variant string) bool) (float64, bool) {
	sum, n := 0.0, 0
	for _, rm := range rows {
		if !rm.KvUsageMeanKnown && !rm.KvUsageInstantKnown || !prefill.Serving(rm) ||
			domain.CanonicalRole(roles[rm.VariantName]) == domain.RolePrefill {
			continue
		}
		k := fillForITL(rm, busiest)
		if !whatif.Finite(k) || k < 0 {
			continue
		}
		sum += k
		n++
	}
	if n == 0 {
		return 0, false
	}
	return sum / float64(n), true
}

// withFill is w filed under the cycle's decode fill, when it has one.
func withFill(w whatif.TPOTWindow, fill float64, ok bool) whatif.TPOTWindow {
	w.Fill, w.FillKnown = fill, ok
	return w
}
