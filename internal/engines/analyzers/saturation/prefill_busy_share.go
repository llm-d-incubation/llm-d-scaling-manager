package saturation

import (
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// A lower bound on prefill's service rate below the knee (issue
// llm-d-incubation/llm-d-scaling-manager#97).
//
// Below the knee a P/D prefill learns no mu and no k2: both are written only
// from an engine queue at or over the queue threshold, and prefill admits
// every prompt it holds, so its own queue reads empty. The back-pressure
// bound (prefill_bound.go) needs that queue too. With no mu the throughput
// floor has no term for prefill, which is then priced on its resident KV
// against k1 alone: 0.04-0.30 replicas on a 2 prefill + 2 decode TTFT
// staircase whose fleet missed a 200 ms p90 TTFT with two prefill replicas.
//
// The row already says what a replica does while it works. A prefill replica
// completes RequestRate requests a second, and each occupies it for
// AvgServiceTime seconds, queue wait excluded (on a P/D prefill the sidecar
// asks for one token, so that is the prefill). By Little's law
// RequestRate x AvgServiceTime is the requests in service on average, and the
// replica's busy share is at most that, and at most one. Its rate while busy,
// RequestRate over the busy share, is then at least
//
//	max(RequestRate, ranks / AvgServiceTime)
//
// and that is the reading (prefillBusyShareRate). It is a lower bound on what
// the replica sustains, not a reading of it:
//
//   - RequestRate is what the replica served, and it cannot serve more than
//     it can;
//   - 1 / AvgServiceTime is one prompt at a time on each engine rank. A
//     prompt that shares a step with others takes longer than one alone, and
//     a batch computes at least as many prompts a second as one prompt alone
//     does, so the batch's rate is at least 1 / AvgServiceTime however many
//     prompts share the step.
//
// Sharing is what makes one reading loose. Under load more prompts share a
// step, AvgServiceTime grows, and the reading falls toward what the replica
// serves: on the 30,000-token three-pod recording a replica read what it
// served, 2.6-4.0 req/s, where one prompt alone took about 0.21 s (4.8
// req/s). Priced cycle by cycle, the floor asked for 4 prefill replicas
// where 3 ran with their queues near empty. So the readings
// are kept as the back-pressure bound keeps its own, in a window of computed
// tokens per second (the reading times the prompt length the key was built
// with) under the throughput key with its role field set to prefillBusyRole,
// one sample a minute at the mean across pods, and the bound is the larger of
// the window's mean and the held lower confidence bound
// (prefill_bound_held.go). Each reading is a lower bound, so the held figure
// is one too; it keeps the highest reading a quieter period gave, which is
// the closest to one prompt alone, and it never falls while the window
// lives. It is working state: a restart starts it again from the window. Kept
// so, the three-pod recording's bound held at 4.6 req/s and the floor asked
// for 3; the 8000-token staircases held 27-33 req/s, one prompt in 0.031 s.
//
// The error goes one way: a bound under the true rate over-orders prefill by
// their ratio, the cost side. It is bounded as the back-pressure bound is
// (floor.Estimate): a replica's own window always wins, the bound orders
// only with floor.MinThroughputSamplesToOrder samples and a steady shape, and
// its floor is capped at floor.LowerBoundMaxGrowth times the role's READY
// supply. A prefill priced on a lower bound also has its share of the router
// queue capped there (applyThroughputFloor): kept whole beside the floor, it
// priced the router's queue as KV held at once.
//
// A row that is not serving (not Ready, a warm-pool bridge, mid-rollout,
// idle) records nothing, nor does one with no request rate or service time
// read.

// SwitchPrefillBusyShareBound is the replay switch for the bound. On, a
// prefill replica with no window of its own is priced at the larger of its
// back-pressure bound and its busy-share bound, as a lower bound
// (capacity.ReplicaCapacity.SaturatedThroughputLowerBound), so the throughput
// floor sizes prefill from the arrival rate below the knee; and a prefill the
// floor prices on a lower bound keeps its share of the router queue only up
// to floor.LowerBoundGrowthCap, as one with no mu does. Off, nothing is
// recorded or read, and prefill is priced as before: on its back-pressure
// bound where it has one (enablePrefillThroughputBound), and otherwise not at
// all. On errs toward more prefill replicas (the cost side); off, a P/D
// prefill below the knee had no floor, and under-provisioned TTFT.
const SwitchPrefillBusyShareBound = "saturation-prefill-busy-share-bound"

// prefillBusyRole is the role field of the busy-share bound's window key. A
// distinct role field keeps its window from being read as mu's or as the
// back-pressure bound's, which the prefill capacity observation reads
// (prefill_observe.go) and enablePrefillThroughputBound governs.
const prefillBusyRole = "prefill-busy"

// busyShareBucket is the bucket label a replica's throughput carries in the
// replica-capacity-decision line when the busy-share bound priced it.
const busyShareBucket = "busy-share"

// prefillBusyKey is the busy-share bound's window key for a prefill
// throughput key; empty for a key of any other shape or role.
func prefillBusyKey(throughputKey string) string {
	return lowerBoundKeyAs(throughputKey, prefillBusyRole)
}

// prefillBusyShareRate is one prefill row's busy-share reading, in
// requests/s (file header): 0 for a row of another role, a row that is not
// serving, or one with no request rate or service time read.
func prefillBusyShareRate(rm domain.ReplicaMetrics, role string, adm admission) float64 {
	if domain.CanonicalRole(role) != domain.RolePrefill || !adm.serving() {
		return 0
	}
	if !finitePositive(rm.RequestRate) || !finitePositive(rm.AvgServiceTime) {
		return 0
	}
	// One prompt at a time per engine rank: a pod of several data-parallel
	// ranks serves that many at once.
	return max(rm.RequestRate, float64(rm.Ranks())/rm.AvgServiceTime)
}

// notePrefillBusyShare records a prefill row's busy-share reading on the
// bound's window and returns the bound on record for its key, in requests/s
// at ileff, the effective prompt length the throughput key was built with
// this cycle. Nothing when the switch is off, the role is not prefill, or the
// key has no bound shape.
func (a *SaturationAnalyzer) notePrefillBusyShare(rm domain.ReplicaMetrics, role, throughputKey, args string,
	ileff float64, adm admission) prefillBoundReading {
	if !a.prefillBusyShare || domain.CanonicalRole(role) != domain.RolePrefill {
		return prefillBoundReading{}
	}
	key := prefillBusyKey(throughputKey)
	if key == "" || !(ileff > 0) {
		return prefillBoundReading{}
	}
	a.mu.Lock()
	a.throughputWindowUnderLocked(key, args)
	a.mu.Unlock()
	if rate := prefillBusyShareRate(rm, role, adm); rate > 0 {
		a.notePrefillBoundSample(key, rm.PodName, rate*ileff, args, evidenceOf(rm))
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	ra, ok := a.saturatedThroughput[key]
	if !ok || ra.Len() == 0 {
		return prefillBoundReading{}
	}
	// Read counts as use, as it does for the back-pressure bound.
	ra.TouchAt(a.now())
	tokens := max(ra.Average(), a.prefillBoundHeld[key].tokens)
	return prefillBoundReading{rate: tokens / ileff, samples: ra.Len()}
}
