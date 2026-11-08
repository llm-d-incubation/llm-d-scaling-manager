package learnreplay

import (
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
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
// mean agree. A row with no finite ITL, or edges another row does not share,
// is left out. ok is false with no row left.
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
