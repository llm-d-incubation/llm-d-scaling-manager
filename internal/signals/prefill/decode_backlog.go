package prefill

import (
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// A decode backlog: the requests a decode pod holds waiting on its own
// capacity. A decode engine's TTFT read while one stands is the queue's wait,
// not what the engine takes at low load: on the staircase recordings it read
// 6 to 21 s after an overload, against 0.15 to 0.23 s at low load. The capacity
// planner refuses such a figure (whatif.DecodeTTFTUnderLoad) and the SLO
// analyzer, behind its switch, prices a calm one instead
// (slo.SwitchTTFTLowLoadDecodeTTFT). Both read the backlog here.

// DecodeTTFTLag is how long a decode backlog taints the decode engine's TTFT
// after the last cycle that read it. The TTFT is a rate over one minute
// (registration.QueryAvgTTFT), and a request that queued has its first token
// only once it leaves the queue, so for up to a minute the mean holds the
// queue's wait with the queue empty. The capacity series is held over the
// same minute (max_over_time[1m], registration.QueryCapacityQueueLength), so
// while that hold lasts the two drain together. It does not always last: on
// ttft-staircase-c-on a pod that was going away read 143 waiting, then 0 on
// the next cycle, and one pod read 6.8 s with its own capacity at 0. So the
// lag is the query's minute, again, and 30 s for the scrape, the fetch cache
// and the cycle. Neither interval is in the recording, so it is a constant.
const DecodeTTFTLag = 90 * time.Second

// DecodeBacklog is the requests a serving decode row holds waiting on its
// own capacity, and whether the row says so. Where vLLM says why requests
// wait (CapacityQueueKnown) it is the part waiting for scheduling capacity.
// The rest of a P/D decode queue is KV pulls in flight, which a request pays
// at any load: on the staircase goldens a calm decode read 1 to 26 of them,
// with a decode TTFT under 0.3 s. A row without the series (an older vLLM,
// SGLang) cannot tell the two apart, so it reads no backlog, unknown.
//
// It is DecodeBacklogBy at the zero DecodeBacklogRule: the capacity queue
// alone.
func DecodeBacklog(rm domain.ReplicaMetrics) (int, bool) {
	n, _, ok := DecodeBacklogBy(rm, DecodeBacklogRule{})
	if !ok {
		return 0, false
	}
	return n, true
}

// DecodeBacklogRule is which part of a P/D decode row's waiting queue
// DecodeBacklogBy reads as its backlog. The zero value is the capacity queue
// alone, the rule the capacity planner and the slo analyzer read a decode
// TTFT's backlog by (DecodeBacklog). The saturation analyzer's capacity view
// of a decode row sets both fields.
type DecodeBacklogRule struct {
	// Landed adds the requests whose KV has landed (LandedQueueLength,
	// vLLM 0.32+), up to the full queue: the scheduler promotes a landed
	// pull only inside its waiting loop, which stops at max_num_seqs and
	// where a block cannot be allocated, so a landed request can wait on
	// capacity too.
	Landed bool
	// MaxNumSeqs is the row's stated max_num_seqs per rank; a row running
	// at it on every rank (domain.ReplicaMetrics.AtBatchCap) is read whole:
	// a decode capped below a full cache queues for real. Zero states no
	// cap.
	MaxNumSeqs int64
}

// DecodeBacklogBy is the backlog of a P/D decode row under rule, summed
// over its ranks and on its busiest rank, and whether the row is read by
// the rule. Not read (false) is a row that does not say why its requests
// wait (CapacityQueueKnown), or one at its stated cap: its queue is then
// the full count, the caller's to take or to leave.
func DecodeBacklogBy(rm domain.ReplicaMetrics, rule DecodeBacklogRule) (waiting, busiest int, ok bool) {
	if !rm.CapacityQueueKnown || rm.AtBatchCap(rule.MaxNumSeqs) {
		return 0, 0, false
	}
	waiting, busiest = rm.CapacityQueueLength, rm.BusiestRankCapacityQueue()
	if rule.Landed && rm.LandedQueueKnown {
		waiting = min(rm.QueueLength, waiting+rm.LandedQueueLength)
		busiest = min(rm.BusiestRankQueue(), busiest+rm.BusiestRankLandedQueue())
	}
	return waiting, busiest, true
}

// DecodeBacklogOf is the largest DecodeBacklog of rows' serving rows, and
// whether one of them did not say why its requests wait.
func DecodeBacklogOf(rows []domain.ReplicaMetrics) (waiting int, unread bool) {
	for _, rm := range rows {
		if !Serving(rm) {
			continue
		}
		n, known := DecodeBacklog(rm)
		waiting, unread = max(waiting, n), unread || !known
	}
	return waiting, unread
}

// DecodeBacklogCarry is one decode variant's latest backlog and when it was
// read, which Carry holds for DecodeTTFTLag after the queue drains. The zero
// value carries nothing.
type DecodeBacklogCarry struct {
	last   int
	lastAt time.Time
	seen   bool
}

// Carry folds in a cycle's backlog, read at at, and returns the backlog the
// cycle's decode TTFT carries: the cycle's own, or the latest one read within
// DecodeTTFTLag before it; 0 with none.
func (c *DecodeBacklogCarry) Carry(waiting int, at time.Time) int {
	switch {
	case waiting > 0:
		c.last, c.lastAt, c.seen = waiting, at, true
		return waiting
	case c.seen && at.Sub(c.lastAt) <= DecodeTTFTLag:
		return c.last
	}
	return 0
}
