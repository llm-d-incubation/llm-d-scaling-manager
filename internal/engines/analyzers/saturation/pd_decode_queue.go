package saturation

import (
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// decodeCapacityView returns rm, a replica of a disaggregated model, with its
// waiting queue as decode capacity reads it if it is a decode replica; any
// other role's row is returned as it is. decodeCapacityViews decides whether
// the model is disaggregated.
//
// vLLM counts a decode request whose KV is still in flight from prefill in
// num_requests_waiting. That request has its blocks and is waiting on the
// network, not on this replica, so it is not a backlog this replica's
// capacity drains: read as one, a decode at KV 0.5 with 768 such requests
// tripped the saturation test, recorded a k2 at half its cache, and was
// charged the 768 as queue demand and floor backlog.
//
//   - Where vLLM says why each request waits (CapacityQueueKnown, v0.20.0
//     and later), the queue is the part waiting for scheduling capacity
//     (reason="capacity"). A request that cannot get blocks for its pull
//     waits there, so a full decode still reads queued.
//   - Except at the sequence cap (maxNumSeqs, the engine's max_num_seqs
//     when its flag or the llm-d.ai/max-num-seqs annotation states it,
//     never a default or a learned cap: atCapBatchCap, and
//     domain.ReplicaMetrics.AtBatchCap): there the queue is the full
//     count. vLLM keeps a request deferred from the moment its pull starts
//     until it is scheduled, and the scheduler promotes a landed pull only
//     when it looks at it, which it stops doing once the batch is at
//     max_num_seqs. So at the cap every request whose pull began
//     below it reads deferred, landed or not, and each of them waits for a
//     batch slot on this replica. Read as the capacity part alone, a capped
//     decode's backlog -- the one #287 measured at 256 running and 768
//     waiting -- would record no k2 and charge no queue demand.
//   - Where it does not, the queue is the full count, as before. Which part
//     of it is in flight cannot be told, and reading too little of it is the
//     direction that breaks SLOs: a decode capped by --max-num-seqs below a
//     full cache queues for real, and dropping that queue would record no k2
//     and leave its capacity at k1.
//
// Which readers take the view, and which keep the raw row, follows the same
// asymmetry: each takes the reading that errs toward stricter learning and
// toward keeping capacity.
//
//   - The view prices the replica's own capacity and demand -- the saturation
//     test and k2 record (computeK2), the k2 settle gate, the throughput
//     admission's unqueued test (servedBounds), the local queue demand
//     (waitingQueueDemand), the floor's backlog (ReplicaCapacity.QueueLength)
//     and the idle admission (admissionOf), where a row reading idle records
//     nothing.
//   - The raw row is kept where a deferred request counted as waiting only
//     holds more: decode's saturation as prefill sees it (roleSaturated,
//     which gates prefill's learning and holds its demand), the busiest-queue
//     load that keeps a replica from being given away
//     (busiestQueueLoadByVariant), and the idle tests (replicaIdle,
//     kvFromInstant), since a request in flight to a replica is load on it.
//     roleSaturated also requires decode's KV full (kvFull), and on a full
//     decode a landed pull that cannot be scheduled reads deferred too (it
//     is promoted only when it is scheduled), so the view there would clear a
//     decode that is genuinely full and queued and let prefill learn decode's
//     back-pressure as its own k2.
//
// Aggregated replicas, prefill, and decode of a model with no prefill variant
// serve whole requests, and their queue stays the full count.
func decodeCapacityView(rm domain.ReplicaMetrics, role string, maxNumSeqs int64) domain.ReplicaMetrics {
	if domain.CanonicalRole(role) != domain.RoleDecode || !rm.CapacityQueueKnown ||
		rm.AtBatchCap(maxNumSeqs) {
		return rm
	}
	rm.MaxRankQueueLength = rm.BusiestRankCapacityQueue()
	rm.QueueLength = rm.CapacityQueueLength
	return rm
}

// decodeCapacityViews is decodeCapacityView over rows when the model is
// disaggregated (domain.HasPrefill), and rows as they are when it is not.
// maxNumSeqs is each variant's stated max_num_seqs (atCapBatchCap: zero, or
// absent, when neither its flag nor its annotation states one).
func decodeCapacityViews(rows []domain.ReplicaMetrics, rolesByVariant map[string]string,
	maxNumSeqs map[string]int64) []domain.ReplicaMetrics {
	if !domain.HasPrefill(rolesByVariant) {
		return rows
	}
	out := make([]domain.ReplicaMetrics, len(rows))
	for i, rm := range rows {
		out[i] = decodeCapacityView(rm, rolesByVariant[rm.VariantName], maxNumSeqs[rm.VariantName])
	}
	return out
}
