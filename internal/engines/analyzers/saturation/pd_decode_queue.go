package saturation

import (
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
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
//   - Where vLLM also says which deferred requests already have their KV
//     (LandedQueueKnown, v0.32.0 and later: the "completed_waiting" stage
//     of num_requests_kv_fetch_by_stage), those are added: their pull is
//     done and they wait for this replica to schedule them, which the
//     scheduler does only inside its waiting loop -- a loop that stops at
//     max_num_seqs, whether or not that cap was stated, and when a block
//     cannot be allocated. So below a stated cap, at a default one, and on
//     a full decode alike, a landed pull is a backlog this replica drains.
//     Capped at the full count, for two reasons. The two are one-minute
//     peaks, which need not fall in the same instant. And a landed request
//     the scheduler has promoted but not yet scheduled may be counted in
//     both the capacity part and completed_waiting; every landed request is
//     one of the waiting ones, so min(full, ...) bounds that double count.
//     Without the series the queue is the capacity part alone, as before.
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
//     decode's backlog -- the one ev-shindin/llm-scaling-manager#287 measured at 256 running and 768
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
	full, fullBusiest := rm.QueueLength, rm.BusiestRankQueue()
	rm.MaxRankQueueLength = rm.BusiestRankCapacityQueue()
	rm.QueueLength = rm.CapacityQueueLength
	if rm.LandedQueueKnown {
		rm.QueueLength = min(full, rm.QueueLength+rm.LandedQueueLength)
		rm.MaxRankQueueLength = min(fullBusiest, rm.MaxRankQueueLength+rm.BusiestRankLandedQueue())
	}
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

// SwitchDecodeLandedNeedsABound is the replay switch for a landed request on
// a decode replica that has room (issue
// llm-d-incubation/llm-d-scaling-manager#201).
//
// decodeCapacityView adds the landed requests to the queue because the
// scheduler promotes a landed pull only inside its waiting loop, which stops
// at max_num_seqs and when a block cannot be allocated. A replica below its
// stated cap on every rank, with its busiest rank's KV under the threshold,
// stops at neither: a landed request waits for the next step, and the
// one-minute peak of that wait is a burst of arrivals, not a backlog. Read as
// one, five landed requests on a decode rank at 24 % KV, its pod running 9
// sequences against a cap of 64 per rank, met the queue threshold of 5,
// recorded a k2 at 24 % of the cache, and priced the variant there for the
// rest of the day.
//
// The loop also stops when the step's token budget is spent. A rank whose
// budget is known is clear of it only below the batch the budget holds in
// steady state (steadyBatch, the derived k2's own: the prompt it computes per
// request and the tokens a step takes with speculation, decodeStepBudget),
// read against its busiest rank's running count (busiestRankRunning).
// That, not one step's sum, is the test: with chunked prefill a landed
// request is admitted on any budget left after the running requests, and
// computes its prompt over the next steps. One step's sum of the landed
// prompts reads every landed request as bound where B is small against the
// prompt a decode computes: 256 tokens against 1,600 a request.
//
// On, such a replica's queue is the capacity part alone, as without the
// landed series. Where the cap is not stated, the running count is not read,
// the busiest rank's KV is at or over the threshold, or the busiest rank's
// running count is at the cap or at the budget's steady batch, the landed
// requests stay in, as before. Off, as before.
//
// Off from the constructor: it moves what decode records and charges, and
// the default changes only after review.
const SwitchDecodeLandedNeedsABound = "decode-landed-queue-needs-a-bound"

// landedWithoutBound returns view, decodeCapacityView's reading of raw, with
// the landed requests taken back out where raw cannot be at a bound
// (SwitchDecodeLandedNeedsABound): a decode of a P/D model whose stated cap
// maxNumSeqs, one rank's, is above its busiest rank's running count
// (busiestRankRunning), whose busiest rank's KV is under kvThreshold, and
// whose running count is under the batch its token budget holds where params
// state one (landedHasBudgetRoom). Any other row is view unchanged. The cap
// is one rank's, and the engine reports running summed over ranks: read
// against the sum, a pod of four ranks with a cap of 64 kept its landed
// requests from 16 a rank on.
func landedWithoutBound(view, raw domain.ReplicaMetrics, role string, maxNumSeqs int64, kvThreshold float64,
	params *capacity.EngineParams) domain.ReplicaMetrics {
	if domain.CanonicalRole(role) != domain.RoleDecode || !raw.CapacityQueueKnown || !raw.LandedQueueKnown ||
		maxNumSeqs <= 0 || !raw.RunningRequestsKnown || !(busiestRankRunning(raw) < float64(maxNumSeqs)) ||
		!(raw.BusiestRankKvUsage() < kvThreshold) || !landedHasBudgetRoom(raw, role, maxNumSeqs, params) {
		return view
	}
	view.QueueLength = raw.CapacityQueueLength
	view.MaxRankQueueLength = raw.BusiestRankCapacityQueue()
	return view
}

// landedHasBudgetRoom reports whether raw, a decode replica, runs fewer
// requests on its busiest rank (busiestRankRunning) than one rank holds at its
// token budget in steady state: steadyBatch at params' budget B, capped at
// maxNumSeqs, with the prompt the replica computes and the tokens a step
// takes (decodeStepBudget; the whole prompt and one token where the replica
// does not report them). Where params state no budget, B is unknown and the
// budget is not what stops the waiting loop: true. A figure that is not a
// number (an unread shape) is no room.
func landedHasBudgetRoom(raw domain.ReplicaMetrics, role string, maxNumSeqs int64, params *capacity.EngineParams) bool {
	if params == nil || params.EffectiveMaxBatchedTokens <= 0 {
		return true
	}
	steady := steadyBatch(float64(params.EffectiveMaxBatchedTokens), float64(maxNumSeqs),
		raw.AvgInputTokens, raw.AvgOutputTokens, decodeStepBudget(raw, role, params))
	return busiestRankRunning(raw) < steady
}

// busiestRankRunning is the requests raw's busiest rank runs. The engine
// reports the running count summed over ranks, so it is the mean a rank, scaled
// by the busiest rank's KV over the pod's mean KV (requests hold KV, so the
// busiest rank by KV runs about that share more), and never above the sum.
// Where the mean KV or the busiest rank's KV is not read it is the sum, which
// no rank can exceed. On a pod of one rank it is the count. It is an
// estimate: the rank with the most KV need not run the most requests, and a
// rank of short requests can reach its cap at less KV than its siblings. Measured on a day-long P/D recording: a pod
// of four ranks running 12 at a busiest-rank KV of 0.24 read as the sum was
// over a steady batch of 9.7 a rank, where its ranks ran about 3 each.
func busiestRankRunning(raw domain.ReplicaMetrics) float64 {
	sum := float64(raw.RunningRequests)
	ranks := raw.Ranks()
	if ranks <= 1 || !(raw.MaxRankKvCacheUsage > 0) {
		return sum
	}
	mean := raw.KvCacheUsage
	if !(mean > 0) || !(mean <= 1) {
		return sum
	}
	skew := max(1, raw.BusiestRankKvUsage()/mean)
	return min(sum, sum/float64(ranks)*skew)
}
