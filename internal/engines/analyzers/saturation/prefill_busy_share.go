package saturation

import (
	"math"
	"strings"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
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
// the replica sustains at the prompt length it is being fed, not a reading
// of it:
//
//   - RequestRate is what the replica served, and it cannot serve more than
//     it can;
//   - 1 / AvgServiceTime is one prompt at a time on each engine rank. A
//     prompt that shares a step with others takes longer than one alone, and
//     a batch computes at least as many prompts a second as one prompt alone
//     does, so the batch's rate is at least 1 / AvgServiceTime however many
//     prompts share the step.
//
// The second term needs a service time that is the prompt's own. One read
// near zero would price the replica at any rate, the side that orders too
// few: SGLang's service time is rebuilt as e2e minus queue time, clamped at
// zero, and a mean taken over a different subset of requests than the e2e
// mean can come out at almost nothing. So the term is used only when the
// service time is at least busyShareMinServiceShare of the replica's time
// to first token less its queue time (AvgTTFT, less AvgQueueTime where that
// was read). On a prefill pod that difference is the prefill plus a fixed
// overhead (tokenization, scheduling), so a service time under a quarter of
// it is three quarters unexplained, and not the prefill. Such a row gives no
// reading at all, as one with no service time read always has: RequestRate
// alone is a lower bound too, but priced on what each replica serves the
// floor asks for the fleet it has over the scale-up threshold, every cycle,
// a ratchet; only the 1 / AvgServiceTime term rises as a grown fleet's
// replicas share fewer steps. Where AvgTTFT is not read there is no guard.
// On SGLang a service time clamped to zero gives no reading, and one under a
// quarter of TTFT less queue time gives none either. The guard errs toward
// fewer replicas than a reading would order (no floor, as before the bound),
// never toward a bound read high. On the staircase recordings the service
// time was 0.65 of TTFT at low load.
//
// Sharing is what makes one reading loose. Under load more prompts share a
// step, AvgServiceTime grows, and the reading falls toward what the replica
// serves: on the 30,000-token three-pod recording a replica read what it
// served, 2.6-4.0 req/s, where one prompt alone took about 0.21 s (4.8
// req/s). Priced cycle by cycle, the floor asked for 4 prefill replicas
// where 3 ran with their queues near empty. So the readings are kept as the
// back-pressure bound keeps its own, in a window of computed tokens per
// second (the reading times the prompt length the key was built with) under
// the throughput key with its role field set to prefillBusyRole, one sample
// a minute at the mean across pods, and the bound is the larger of the
// window's mean and the held lower confidence bound (prefill_bound_held.go).
// Each reading is a lower bound, so the held figure is one too; it keeps the
// highest figure a quieter period gave, which is the closest to one prompt
// alone. Kept so, the three-pod recording's bound held at 4.6 req/s and the
// floor asked for 3; the 8000-token staircases held 27-33 req/s, one prompt
// in 0.031 s.
//
// The held figure is a lower bound at the prompt length it was taken at, and
// only there. It is kept in computed tokens per second and read back at the
// prompt length the key has now, and a prompt's cost per token rises with
// its length (attention), so at longer prompts the same figure over-states
// the rate, the side that orders too few. The key's input bucket spans a
// factor of two, and the top one has no upper end. So the held figure keeps
// the prompt length it was first taken at, and is dropped when the length
// moves past shape.DefaultChangeTolerance of it, the tolerance a fleet-shape
// change is declared at; the window's mean then prices until the samples
// taken at the new length raise a new one. It never falls otherwise while the
// window lives.
//
// The held figure is in the learned-state snapshot with its window
// (ThroughputSnapshot.Held), so a restart under load does not fall back to
// the window's mean of loaded readings, which over-orders by up to the growth
// cap until a full window of fresh samples is tested. A snapshot written
// before the field restores the window alone, as before.
//
// The error goes one way: a bound under the true rate over-orders prefill by
// their ratio, the cost side. It is bounded as the back-pressure bound is
// (floor.Estimate): a replica's own window always wins, the bound orders
// only with floor.MinThroughputSamplesToOrder samples and a steady shape, and
// its floor is capped at floor.LowerBoundMaxGrowth times the role's READY
// supply.
//
// A row that is not serving (not Ready, a warm-pool bridge, mid-rollout,
// idle) records nothing, nor does one with no request rate read.

// SwitchPrefillBusyShareBound is the replay switch for the bound. On, a
// prefill replica with no window of its own is priced at the larger of its
// back-pressure bound and its busy-share bound, as a lower bound
// (capacity.ReplicaCapacity.SaturatedThroughputLowerBound), so the throughput
// floor sizes prefill from the arrival rate below the knee. Off, nothing is
// recorded or read, and prefill is priced as before: on its back-pressure
// bound where it has one (enablePrefillThroughputBound), and otherwise not at
// all. On errs toward more prefill replicas (the cost side); off, a P/D
// prefill below the knee had no floor, and under-provisioned TTFT. It is
// always on in a running controller: there is no policy key for it.
const SwitchPrefillBusyShareBound = "saturation-prefill-busy-share-bound"

// SwitchLowerBoundCapsRouterShare is the replay switch for the router-queue
// share of a prefill the floor prices on a lower bound, either one: the
// busy-share bound or the back-pressure bound. On, with
// SwitchPrefillKeepsRouterPrompts on, the share is kept only up to
// floor.LowerBoundGrowthCap of prefill's ready supply, as for a prefill with
// no mu (applyThroughputFloor). Kept whole beside the floor's own term, it
// priced the router's queue as KV held at once: up to 20 prefill replicas
// from 2 in the runaway of the TTFT staircase recorded with flow control on.
// Off, or with SwitchPrefillKeepsRouterPrompts off, a prefill on a lower
// bound keeps the share whole, as any prefill the floor prices does. On errs
// toward fewer prefill replicas (the SLO side), and only where the uncapped
// share was over one doubling of the ready fleet.
const SwitchLowerBoundCapsRouterShare = "saturation-lower-bound-caps-router-share"

// prefillBusyRole is the role field of the busy-share bound's window key. A
// distinct role field keeps its window from being read as mu's or as the
// back-pressure bound's, which the prefill capacity observation reads
// (prefill_observe.go) and enablePrefillThroughputBound governs.
const prefillBusyRole = "prefill-busy"

// busyShareBucket is the bucket label a replica's throughput carries in the
// replica-capacity-decision line when the busy-share bound priced it.
const busyShareBucket = "busy-share"

// busyShareMinServiceShare is the least share of a prefill replica's time to
// first token, less its queue time, its service time must be for
// 1 / AvgServiceTime to price it (file header).
const busyShareMinServiceShare = 0.25

// prefillBusyKey is the busy-share bound's window key for a prefill
// throughput key; empty for a key of any other shape or role.
func prefillBusyKey(throughputKey string) string {
	return lowerBoundKeyAs(throughputKey, prefillBusyRole)
}

// isPrefillBusyKey reports whether key is the busy-share bound's window key.
func isPrefillBusyKey(key string) bool {
	f := strings.Split(key, "|")
	return len(f) == 9 && f[5] == prefillBusyRole
}

// prefillBusyShareRate is one prefill row's busy-share reading, in
// requests/s (file header): 0 for a row of another role, a row that is not
// serving, one with no request rate read, or one whose service time is not
// plausibly the prompt's own (serviceTimePlausible).
func prefillBusyShareRate(rm domain.ReplicaMetrics, role string, adm admission) float64 {
	if domain.CanonicalRole(role) != domain.RolePrefill || !adm.serving() {
		return 0
	}
	if !finitePositive(rm.RequestRate) || !serviceTimePlausible(rm) {
		return 0
	}
	// One prompt at a time per engine rank: a pod of several data-parallel
	// ranks serves that many at once.
	return max(rm.RequestRate, float64(rm.Ranks())/rm.AvgServiceTime)
}

// serviceTimePlausible reports whether rm's AvgServiceTime is a finite
// positive figure at least busyShareMinServiceShare of its time to first
// token less its queue time, where the time to first token was read (file
// header).
func serviceTimePlausible(rm domain.ReplicaMetrics) bool {
	if !finitePositive(rm.AvgServiceTime) {
		return false
	}
	if !finitePositive(rm.AvgTTFT) {
		return true
	}
	inEngine := rm.AvgTTFT
	if rm.AvgQueueTimeKnown && rm.AvgQueueTime >= 0 && !math.IsInf(rm.AvgQueueTime, 0) {
		inEngine = max(rm.AvgTTFT-rm.AvgQueueTime, 0)
	}
	return rm.AvgServiceTime >= busyShareMinServiceShare*inEngine
}

// notePrefillBusyShare records a prefill row's busy-share reading on the
// bound's window and returns the bound on record for its key, in requests/s
// at ileff, the effective prompt length the throughput key was built with
// this cycle. Nothing when the switch is off, the role is not prefill, the
// key has no bound shape, or ileff is not a finite positive length.
func (a *SaturationAnalyzer) notePrefillBusyShare(rm domain.ReplicaMetrics, role, throughputKey, args string,
	ileff float64, adm admission) prefillBoundReading {
	if !a.prefillBusyShare || domain.CanonicalRole(role) != domain.RolePrefill {
		return prefillBoundReading{}
	}
	key := prefillBusyKey(throughputKey)
	if key == "" || !finitePositive(ileff) {
		return prefillBoundReading{}
	}
	a.mu.Lock()
	a.throughputWindowUnderLocked(key, args)
	// A held figure taken at another prompt length is not a bound at this
	// one (file header).
	if h, ok := a.prefillBoundHeld[key]; ok && h.prompt > 0 &&
		math.Abs(ileff-h.prompt) > shape.DefaultChangeTolerance*h.prompt {
		delete(a.prefillBoundHeld, key)
	}
	a.mu.Unlock()
	if rate := prefillBusyShareRate(rm, role, adm); rate > 0 {
		a.notePrefillBoundSample(key, rm.PodName, rate*ileff, args, evidenceOf(rm))
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if h, ok := a.prefillBoundHeld[key]; ok && h.tokens > 0 && h.prompt == 0 {
		h.prompt = ileff
		a.prefillBoundHeld[key] = h
	}
	ra, ok := a.saturatedThroughput[key]
	if !ok || ra.Len() == 0 {
		return prefillBoundReading{}
	}
	// Read counts as use, as it does for the back-pressure bound.
	ra.TouchAt(a.now())
	tokens := max(ra.Average(), a.prefillBoundHeld[key].tokens)
	return prefillBoundReading{rate: tokens / ileff, samples: ra.Len()}
}

// HeldBoundSnapshot is the busy-share bound's held figure as exported with
// its window (ThroughputSnapshot.Held): the max of the lower confidence
// bounds taken, in computed tokens/s, how many were taken, and the prompt
// length it was first taken at.
type HeldBoundSnapshot struct {
	Tokens float64 `json:"tokens"`
	Tests  int     `json:"tests"`
	Prompt float64 `json:"prompt"`
}

// busyHeldSnapshotLocked is key's held figure for the snapshot: nil for a
// key that is not the busy-share bound's, or with no figure held. Callers
// hold a.mu.
func (a *SaturationAnalyzer) busyHeldSnapshotLocked(key string) *HeldBoundSnapshot {
	if !isPrefillBusyKey(key) {
		return nil
	}
	h, ok := a.prefillBoundHeld[key]
	if !ok || !finitePositive(h.tokens) {
		return nil
	}
	return &HeldBoundSnapshot{Tokens: h.tokens, Tests: h.tests, Prompt: h.prompt}
}

// restoreBusyHeldLocked installs a restored window's held figure, for the
// busy-share bound's key only, and only a finite positive one. Its count of
// fresh samples starts at zero, so the next full window it tests is of
// samples taken since the restart. Callers hold a.mu.
func (a *SaturationAnalyzer) restoreBusyHeldLocked(key string, held *HeldBoundSnapshot) {
	if held == nil || !isPrefillBusyKey(key) || !finitePositive(held.Tokens) {
		return
	}
	prompt := held.Prompt
	if !finitePositive(prompt) {
		prompt = 0
	}
	a.prefillBoundHeld[key] = prefillHeldBound{tokens: held.Tokens, tests: max(held.Tests, 1), prompt: prompt}
}
