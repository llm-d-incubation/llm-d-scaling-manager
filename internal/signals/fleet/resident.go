package fleet

import (
	"math"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// ResidentRead is the KV in use on rm and the requests running on it, both
// instant readings of the same scrape (KvUsageInstant, RunningRequests), so
// they describe the same requests, and whether rm is a resident reading at
// all. It is one when rm is Ready and not lent by the warm pool, read both
// with at least one request running, has a live KV capacity (not unread, and
// not the collector's overflow sentinel), and fills its cache to between
// minK, the ITL band's lower edge, and 1. Below the band a few requests read
// a prefix they share high, each carrying the whole of it.
//
// The saturation analyzer's KV per request (fleetResidentKVPerRequest)
// and the capacity planner's (learnreplay's noteResident) read each row by
// it, so a change here moves both: the analyzer sums its readings over the
// fleet for one cycle, the planner over a variant for each of its cycles.
func ResidentRead(rm domain.ReplicaMetrics, minK float64) (tokens float64, running int, ok bool) {
	if !rm.Ready || rm.FromWarmPool || !rm.RunningRequestsKnown || !rm.KvUsageInstantKnown || rm.RunningRequests <= 0 {
		return 0, 0, false
	}
	if rm.TotalKvCapacityTokens <= 0 || rm.TotalKvCapacityTokens == math.MaxInt64 {
		return 0, 0, false
	}
	if !(rm.KvUsageInstant >= minK && rm.KvUsageInstant <= 1) {
		return 0, 0, false
	}
	return rm.KvUsageInstant * float64(rm.TotalKvCapacityTokens), rm.RunningRequests, true
}
