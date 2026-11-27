package saturation

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// Prefill capacity, observed (docs/proposals/slo-capacity-planner.md, PR P1;
// the model is docs/proposals/slo-capacity-planner-ttft.md). Each prefill
// variant's rows go to internal/signals/prefill, which learns V_P, a and f,
// and the result is logged at perReplicaLogLevel as prefill-capacity-observed.
// Nothing sizes from it. Each variant's latest estimate is stashed, stamped
// with the cycle, for the slo analyzer's TTFT pricing to read
// (PrefillEstimate).
//
// It is wired here, not in the slo analyzer, because two of its inputs are
// this analyzer's: whether decode is back-pressuring prefill this cycle
// (decodeSaturated, the test computeK2 applies), and the prefill lower-bound
// window (prefill_bound.go), V_P's third source. This analyzer also runs for
// every model, where the slo analyzer runs only when a policy lists it.

// SwitchPrefillCapacityObserve is the replay switch for the observation. On,
// each prefill variant's rows are observed and logged every cycle. Off,
// nothing is observed or logged. It moves no golden output in S1a: the
// observation writes no state any output reads there. S1b's TTFT pricing
// reads the stashed estimate (PrefillEstimate).
const SwitchPrefillCapacityObserve = "prefill-capacity-observe"

// SwitchPrefillBusyMinutes is the replay switch for V_P's first source,
// busy minutes. On, each prefill row's queue minimum over the minute
// (domain.ReplicaMetrics.QueueMin, where QueueMinKnown) goes to the
// observation with the pods whose KV is full (prefillKVFull), and a minute
// whose queue never emptied, on a pod whose KV was not full, is a busy
// minute. Off, neither goes, and V_P comes only from the two lower-bound
// sources, as P1 shipped. It moves no golden output: the observation writes
// no state any output reads, and no committed recording carries a queue
// minimum. The collector reads the queue minimum in a namespace with a TTFT
// target (collector ReadTTFTInputsIn) and while a learner recording is on;
// elsewhere no row carries one and no new busy minute is read, though those
// already read stay V_P's source for prefill.BusyWindow.
const SwitchPrefillBusyMinutes = "prefill-busy-minutes"

// SwitchPrefillAQuadratic is the replay switch for the curved fit of the
// fixed latency a (prefill/fixed.go). On, a is the intercept of the convex
// curve s0 + b*I + c*I*I through the low-load minutes' prompt-length
// clusters where at least prefill.MinQuadraticClusters of them span
// prefill.MinQuadraticSpread, and of the straight line otherwise, and an
// intercept extrapolated from prompts all longer than
// prefill.MaxAShortestPrompt is refused (NoShortPrompts), as is a curve's
// intercept whose standard error is above prefill.MaxAInterceptRelSE of it
// (AUncertain); the slope bound on V_P is the curve's at V_P's prompt
// length. The short-prompt rule holds the line too: on, a deployment whose
// prompts are all long (its second shortest low-load minute over 2500
// tokens) has a refused, where P1 shipped published the line's intercept.
// Off, a and the bound are always the line's, as P1 shipped. It moves no
// golden output: the observation writes no state any output reads.
const SwitchPrefillAQuadratic = "prefill-a-quadratic"

// SwitchPrefillALowLoadPerRank is the replay switch for the low-load test of
// a read per engine rank (prefill.Cycle.ALowLoadPerRank). On, a row's
// occupancy is its request rate times its service time over its engine
// ranks, so a row of an LWS group of four engines, each at low load, is a
// low-load minute. Off, the row is read whole, as P1 shipped. Planner-only:
// off from the constructor, so the controller never reads it;
// learnreplay.StateAt turns it on (learnreplay.PlannerSwitches).
const SwitchPrefillALowLoadPerRank = "prefill-a-low-load-per-rank"

// SwitchPrefillAEngineTiming is the replay switch for a fitted from the
// engine's own prefill timing where the low-load fit has too few minutes
// (prefill.Cycle.AFromTiming, prefill/timing.go). On, a busy fleet's a is
// the intercept of its prefill time against its computed tokens over the
// quieter half of its minutes, plus the median overhead outside the prefill
// and the queue, flagged by its source (prefill.ASourceEngineTiming). Off,
// a is refused TooFewLowLoadMinutes there, as P1 shipped. Planner-only, as
// SwitchPrefillALowLoadPerRank is.
const SwitchPrefillAEngineTiming = "prefill-a-engine-timing"

// SwitchPrefillQueueByCapacity is the replay switch for what V_P counts as a
// queue (prefill.Cycle.QueueByCapacity). On, a prefill row is queued, for
// its busy minutes and its peak under a queue, only by the part of its
// waiting count that waits for scheduling capacity, where vLLM publishes it.
// The rest is deferred, such as a cached prefix still loading, and on a pod
// that loads long prefixes it waits at almost any load: counted, V_P reads
// the load the pods served, and the knee priced from it follows the traffic.
// Off, the whole count, as before this switch. Planner-only, as
// SwitchPrefillALowLoadPerRank is.
const SwitchPrefillQueueByCapacity = "prefill-queue-by-capacity"

// SwitchPrefillFBusyMinutes is the replay switch for which minutes f is the
// median of (prefill.Cycle.FBusyMinutes). On, the busiest: those at or
// above half the busiest minute's computed tokens per pod. The shares are
// uneven at light load and even out near capacity, so a median over every
// minute reads a
// fleet that spent much of its window light as more uneven than it is at
// the knee f prices (issue llm-d-incubation/llm-d-scaling-manager#266).
// Off, every minute, as before this switch. Planner-only, as
// SwitchPrefillALowLoadPerRank is.
const SwitchPrefillFBusyMinutes = "prefill-f-busy-minutes"

// SwitchPrefillFPooledMinutes is the replay switch for f on a fleet whose
// minutes carry fewer than prefill.MinRequestsPerPodMinute requests a pod
// (prefill.Cycle.FPooledMinutes). On, consecutive minutes on the same pods
// are pooled until they carry that many, at most prefill.MaxPooledMinutes,
// and the pool gives one f, de-biased at its pooled count. A fleet of long
// prompts serves under the threshold at its knee: the Qwen3-32B lab fleet
// served about 78 requests a pod a minute, learned no f, and was priced at
// whatif.ImbalanceFloor at every count above one. Off, such minutes give no
// f, as before this switch. Planner-only, as SwitchPrefillALowLoadPerRank
// is.
const SwitchPrefillFPooledMinutes = "prefill-f-pooled-minutes"

// SwitchPrefillVPFloorServed is the replay switch for the floor under a
// lower-bound V_P (prefill.Cycle.VPFloorServed). On, a V_P that is a lower
// bound (the peak under a queue, the back-pressure bound, busy minutes of a
// data-parallel pod) is raised to the highest computed-token rate a pod was
// seen to sustain, queued or not: a pod never computes faster than V_P, so
// that rate is a lower bound too. On a fleet whose pods never queue for
// capacity, the peak under a queue comes from a few light queued minutes,
// and the knee priced from it sat far under the load the fleet served
// (issue llm-d-incubation/llm-d-scaling-manager#280). Off, the source's own
// figure, as before this switch. Planner-only, as
// SwitchPrefillALowLoadPerRank is.
const SwitchPrefillVPFloorServed = "prefill-vp-floor-served"

// SwitchPrefillVPServedUnqueued is the replay switch for V_P on a fleet that
// never queued (prefill.Cycle.VPServedUnqueued). On, where no source has a
// reading (no busy minutes, no PeakRun queued minutes in a row, no
// back-pressure bound), V_P is the highest computed-token rate a pod
// sustained, a lower bound (prefill.NeverQueued), not NoPrefillCapacity: a
// pod never computes faster than V_P. The planner then prices the prefill
// knee as at least the rate that V_P gives and refuses every TTFT priced from
// it (whatif.Prefill.VPServedOnly; issue
// llm-d-incubation/llm-d-scaling-manager#369). Off, NoPrefillCapacity, as
// before this switch. Planner-only, as SwitchPrefillALowLoadPerRank is.
const SwitchPrefillVPServedUnqueued = "prefill-vp-served-unqueued"

// SwitchPrefillVPWindowBest is the replay switch for the peak under a queue
// read over the whole window (prefill.Cycle.VPWindowBest). On, V_P's peak
// under a queue is the best the key read since its first cycle, kept past
// prefill.Retention, so a recording whose prefill pods queued only at its
// busiest hours keeps that lower bound after them, flagged with its age; the
// served peak floors it as any lower bound (SwitchPrefillVPFloorServed). Off,
// the samples kept, as the controller reads them. Planner-only, as
// SwitchPrefillALowLoadPerRank is: the controller prices the fleet it runs
// now, so a reading hours old should age out there.
const SwitchPrefillVPWindowBest = "prefill-vp-window-best"

// SwitchPrefillVPAtServedWork is the replay switch for the V_P a TTFT is
// priced from where the prefill knee rests on a lower bound of it. On, where
// the knee that lower bound gives is under the rate the prefill pods'
// sustained computed-token work serves (the planner's servedKneeFloor), V_P
// is raised by the same ratio for the TTFT too, so the TTFT and the knee are
// read off one V_P: the pods computed that work, so they compute at least
// that fast. Off, as #280 and the served floor left it: only the knee is
// raised, and a TTFT priced from the lower V_P reads the prefill pods past
// their capacity at rates they served, so the TTFT just under the floored
// knee reads many times what such a fleet measures. Planner-only, as
// SwitchPrefillALowLoadPerRank is.
const SwitchPrefillVPAtServedWork = "prefill-vp-at-served-work"

// SwitchPrefillVPOccupancyPlateau is the replay switch for V_P read without
// a queue (prefill.Cycle.VPOccupancyPlateau, prefill/plateau.go). On, a V_P
// refused for want of a queue, or a lower bound under it, is read from the
// plateau of a pod's served computed tokens against its requests in prefill
// per engine (Little's law): V_P where the curve flattened in the window, a
// lower bound where it was still rising. A V_P from busy minutes of a
// single-rank pod, and a lower bound at or above the plateau, stand. Off,
// the sources that need a queue only, as before this switch (issue
// llm-d-incubation/llm-d-scaling-manager#368). Planner-only, as
// SwitchPrefillALowLoadPerRank is.
const SwitchPrefillVPOccupancyPlateau = "prefill-vp-occupancy-plateau"

// SwitchPrefillVPPromptCoverage is the replay switch for the prompts a V_P
// speaks for (prefill.Cycle.VPPromptCoverage). On, V_P carries the range of
// effective prompts its minutes read (prefill.Estimate.VPPromptLow,
// VPPromptHigh), and a price at a mean prompt more than whatif.MaxVPShapeRatio
// outside it is flagged whatif.VPPromptNotCovered, out of the region V_P
// was learned in, and prices no TTFT. Off, V_P is priced at any prompt, as
// before this switch (issue llm-d-incubation/llm-d-scaling-manager#405).
// Planner-only, as SwitchPrefillALowLoadPerRank is.
const SwitchPrefillVPPromptCoverage = "prefill-vp-prompt-coverage"

// SwitchPrefillVPLoneRate is the replay switch for V_P read from light
// load (prefill.Cycle.VPLoneRate, prefill/lonerate.go). On, V_P refused for
// want of a queue, or a lower bound, is read from the lone rate: one
// request's computed tokens over its prefill time in minutes at under
// prefill.LoneMaxOccupancy requests in prefill per engine, at the prompt
// priced, from prefill.MinLoneMinutes such minutes; V_P only where the
// served peak is at most prefill.LoneServedTolerance over it, else it is
// recorded as a lower bound and not taken. Off, as before this switch
// (issue llm-d-incubation/llm-d-scaling-manager#406). Planner-only, as
// SwitchPrefillALowLoadPerRank is.
const SwitchPrefillVPLoneRate = "prefill-vp-lone-rate"

// SwitchPrefillVPServiceLine is the replay switch for V_P read from the
// prefill service line (prefill.Cycle.VPServiceLine,
// prefill/serviceline.go). On, V_P refused for want of a queue, or the
// served peak of a fleet that never queued under the line's, is
// ranks * E[x] / (s0 + E[x]/V): the line
// S(x) = s0 + x/V fitted to the engines' prefill time, inverted for
// processor sharing, against their computed tokens a request, s0 kept in
// the capacity, at the minutes' mean computed prompt E[x]. Not a bound. A
// V_P read under a queue, a lower bound or not, stands. Off, as before this
// switch (issues llm-d-incubation/llm-d-scaling-manager#368, #416).
// Planner-only, as SwitchPrefillALowLoadPerRank is.
const SwitchPrefillVPServiceLine = "prefill-vp-service-line"

// SwitchPrefillFLoadCurve is the replay switch for f against the load per
// pod (prefill.Cycle.FLoadCurve, prefill.FLoadCurve). On, each pod count's
// minutes give f(u) = f_inf + delta/u, u the computed tokens a second a pod,
// fitted by least squares on 1/u (prefill.Estimate.FCurveByPods); the
// planner prices f at a rate as the higher of its f and the curve's
// (whatif.Prefill.ImbalanceCurveFloor). Off, no curve is fitted, as before
// this switch (issue llm-d-incubation/llm-d-scaling-manager#482).
// Planner-only and not in learnreplay.PlannerSwitches: the capacity
// planner's --prefill-f-load-curve turns it on.
const SwitchPrefillFLoadCurve = "prefill-f-load-curve"

// SwitchPrefillHitAtCount is the replay switch for the prefix hit at a
// prefill count other than the one the hit was read on
// (prefill.HitAtCountOf; issue llm-d-incubation/llm-d-scaling-manager#384).
// On, a planner question priced at another prefill count than the fit's
// prices the hit there by the reuse-gap rule where the recording holds the
// KV residency histograms (prefill.FitHitModel), flagged PrefixHitAtCount;
// without them, at fewer pods the floor hit x N'/N, and the computed prompt
// tokens a request with it, flagged PrefixHitAtCount, and at more pods the
// fit's hit held, flagged PrefixHitHeld. Off, as before: the fit's hit at every count. The saturation
// analyzer reads nothing with it; it holds it for the name. Planner-only, as
// SwitchPrefillALowLoadPerRank is.
const SwitchPrefillHitAtCount = "prefill-hit-at-count"

// SwitchPrefillALowLoadByRate is the replay switch for which minutes the fit
// of a reads as low load (prefill.Cycle.ALowLoadByRate). On, a quiet minute
// (no queue) is low load when its request rate times its prompt cluster's
// service time is at most prefill.LowLoadOccupancy. Off, its rate times its
// own service time, as P1 shipped: at a given rate a minute whose prefill
// ran slow is cut, so the fit selects on the response it reads and its
// intercept reads low (issue llm-d-incubation/llm-d-scaling-manager#306).
// Planner-only, as SwitchPrefillALowLoadPerRank is.
const SwitchPrefillALowLoadByRate = "prefill-a-low-load-by-rate"

// SwitchPrefillAMedianOverhead is the replay switch for the overhead a adds
// to the intercept (prefill.Cycle.AMedianOverhead). On, the median of the
// low-load minutes' AvgTTFT - AvgServiceTime. Off, the least, as P1
// shipped: the least of ten to thirty noisy minutes sits about two standard
// deviations under the typical one, and a smaller a prices TTFT lower
// (issue llm-d-incubation/llm-d-scaling-manager#306). Planner-only, as
// SwitchPrefillALowLoadPerRank is.
const SwitchPrefillAMedianOverhead = "prefill-a-median-overhead"

// prefillKVFull is the pods of rows whose KV cache is full (kvFull at
// kvCacheThreshold), nil when none is. A full prefill pod can hold a queue
// it cannot schedule, so its minute is not a busy one.
func prefillKVFull(rows []domain.ReplicaMetrics, kvCacheThreshold float64) map[string]bool {
	var out map[string]bool
	for _, rm := range rows {
		if !kvFull(rm, kvCacheThreshold) {
			continue
		}
		if out == nil {
			out = map[string]bool{}
		}
		out[rm.PodName] = true
	}
	return out
}

// prefillObserveKey names a prefill variant's observed state: the variant
// key's fields (itlWindowKey), and the engine arguments it runs (windowArgs)
// behind stashKey's separator when they are known. V_P is a max over the
// samples a key keeps, for prefill.Retention, so a key that outlived a
// redeploy with a lower --max-num-batched-tokens kept the old engine's peak:
// with the arguments in the key, the new engine starts its own state and the
// old one ages out. Only the arguments: like every window the arguments
// bound, an image alone does not start a new key; an operator resets one
// with llm-d.ai/forget-learned-state (forgetVariant).
//
// The accelerator is read through stableAcceleratorFor, which touches
// nothing: observing must not keep the accelerator memo alive.
func (a *SaturationAnalyzer) prefillObserveKey(namespace, modelID, variant, accelerator string, gpuCount int,
	params *capacity.EngineParams) string {
	key := fmt.Sprintf("%s|%s|%s|%s|%d", namespace, modelID, variant,
		a.stableAcceleratorFor(namespace, variant, accelerator), gpuCount)
	if args := windowArgs(params); args != "" {
		key = stashKey(key, args)
	}
	return key
}

// prefillBoundStashKey names a variant's back-pressure bound for the cycle
// in progress (notePrefillBound writes it, observePrefill reads it).
func prefillBoundStashKey(namespace, modelID, variant string) string {
	return namespace + "|" + modelID + "|" + variant
}

// stashedBound is a back-pressure bound stashed for the cycle in progress,
// with when it was stashed (for the eviction sweep).
type stashedBound struct {
	bound prefill.Bound
	at    time.Time
}

// stashPrefillBound keeps the bound notePrefillBound read for the variant,
// in computed tokens/s, for observePrefill. It is written once per replica
// priced from the bound, and the replicas of one variant can read different
// windows (one per input bucket): the highest is kept, as the bound's own
// window keeps its highest reading, each being a floor on the true rate.
// Called with a.mu held.
func (a *SaturationAnalyzer) stashPrefillBound(rm domain.ReplicaMetrics, tokens float64, samples int) {
	if !a.prefillObserve {
		return
	}
	k := prefillBoundStashKey(rm.Namespace, rm.ModelID, rm.VariantName)
	now := a.now()
	if prev, ok := a.prefillBounds[k]; ok && prev.at.Equal(now) && prev.bound.Tokens >= tokens {
		return
	}
	a.prefillBounds[k] = stashedBound{bound: prefill.Bound{Tokens: tokens, Samples: samples}, at: now}
}

// prefillBoundsStaleAfter is how old a stashed bound is when the eviction
// sweep drops it. A bound is stashed and taken within one Analyze; one left
// longer belongs to a cycle that never took it.
const prefillBoundsStaleAfter = time.Minute

// evictPrefillObservationLocked sweeps the observation's keys with no cycle
// for prefill.Retention, the stashed bounds no cycle took, and the stashed
// estimates older than variantTimeout, and returns how many of each.
// Callers hold a.mu; the observer's lock is its own.
//
// An estimate is swept on the variant timeout, not on
// prefillBoundsStaleAfter: the sweep runs before each cycle's Analyze, so at
// an interval over a minute every variant's estimate from the cycle before
// would be evicted, and logged, every cycle. A stale one is never read
// anyway (the slo analyzer refuses an estimate not stamped with its own
// cycle); the sweep only keeps the map from outliving its variants.
func (a *SaturationAnalyzer) evictPrefillObservationLocked(now time.Time, variantTimeout time.Duration) (keys, bounds, estimates int) {
	keys = a.prefillObserver.Sweep(now)
	for k, sb := range a.prefillBounds {
		if now.Sub(sb.at) > prefillBoundsStaleAfter {
			delete(a.prefillBounds, k)
			bounds++
		}
	}
	for k, se := range a.prefillEstimates {
		if now.Sub(se.at) > variantTimeout {
			delete(a.prefillEstimates, k)
			estimates++
		}
	}
	return keys, bounds, estimates
}

// stashedEstimate is a prefill variant's latest estimate (prefill.Estimate),
// with the analyzer's clock at the write: the slo analyzer reads it only in
// the cycle that wrote it (docs/proposals/slo-tiers.md, "Where P1's figures
// come from").
type stashedEstimate struct {
	est prefill.Estimate
	at  time.Time
}

// stashPrefillEstimate keeps est as the variant's latest estimate, stamped
// with now. It takes a.mu for the write only: the observer's lock is not
// held here, so the order a.mu then the observer's lock is never reversed.
func (a *SaturationAnalyzer) stashPrefillEstimate(namespace, modelID, variant string, est prefill.Estimate, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.prefillEstimates[prefillBoundStashKey(namespace, modelID, variant)] = stashedEstimate{est: est, at: now}
}

// PrefillEstimate is a prefill variant's latest stashed estimate, when it was
// stashed on the analyzer's clock, and whether there is one: the handle the
// slo analyzer reads P1's figures through (slo.PrefillEstimates), so it
// never rebuilds the observer's key. The estimate's maps are copies. It
// takes a.mu and nothing else; it reads, and stamps nothing.
func (a *SaturationAnalyzer) PrefillEstimate(namespace, modelID, variant string) (prefill.Estimate, time.Time, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	se, ok := a.prefillEstimates[prefillBoundStashKey(namespace, modelID, variant)]
	if !ok {
		return prefill.Estimate{}, time.Time{}, false
	}
	est := se.est
	est.Refused = maps.Clone(est.Refused)
	est.FByPods = maps.Clone(est.FByPods)
	est.FSamplesByPods = maps.Clone(est.FSamplesByPods)
	est.FLoadByPods = maps.Clone(est.FLoadByPods)
	est.FCurveByPods = maps.Clone(est.FCurveByPods)
	return est, se.at, true
}

// takePrefillBounds returns and clears the bounds stashed for the cycle's
// model, and whether the observation is on. Every cycle clears them, the
// ones whose pricing failed too (Analyze), so a bound is only ever read by
// the cycle that stashed it; and it clears the model's entries for variants
// the cycle does not have, which no cycle would take.
func (c *cycle) takePrefillBounds() (map[string]prefill.Bound, bool) {
	a := c.a
	a.mu.Lock()
	defer a.mu.Unlock()
	bounds := map[string]prefill.Bound{}
	prefix := prefillBoundStashKey(c.input.Namespace, c.input.ModelID, "")
	for k, sb := range a.prefillBounds {
		variant, ok := strings.CutPrefix(k, prefix)
		if !ok || prefillBoundStashKey(c.input.Namespace, c.input.ModelID, variant) != k {
			continue
		}
		if _, ours := c.rolesByVariant[variant]; ours {
			bounds[variant] = sb.bound
		}
		delete(a.prefillBounds, k)
	}
	return bounds, a.prefillObserve
}

// observePrefill observes each prefill variant's rows of the cycle and logs
// what its key has learned. After priceReplicas, so the bound each variant's
// replicas read this cycle is stashed.
func (c *cycle) observePrefill() {
	a := c.a
	bounds, on := c.takePrefillBounds()
	if !on {
		return
	}
	rows := map[string][]domain.ReplicaMetrics{}
	for _, rm := range c.input.ReplicaMetrics {
		if domain.CanonicalRole(c.rolesByVariant[rm.VariantName]) == domain.RolePrefill {
			rows[rm.VariantName] = append(rows[rm.VariantName], rm)
		}
	}
	view := &prefill.Settle{View: a.settleView(), Model: settleModel(c.input.Namespace, c.input.ModelID)}
	for _, variant := range slices.Sorted(maps.Keys(rows)) {
		var engine inferenceengine.Engine
		params := engineParamsFor(a, c.input.Namespace, c.input.ModelID, variant)
		if params != nil {
			engine = params.Engine
		}
		key := a.prefillObserveKey(c.input.Namespace, c.input.ModelID, variant, c.accelByVariant[variant],
			c.gpusByVariant[variant], params)
		// queueMinPods counts the pods that read a queue minimum whether or
		// not the switch feeds them: with busyMinutesOn, a zero says the
		// collector read none (no TTFT target here and no recording on).
		queueMin := prefill.QueueMins(rows[variant])
		queueMinPods := len(queueMin)
		var kvFullPods map[string]bool
		if a.prefillBusyMinutes {
			kvFullPods = prefillKVFull(rows[variant], c.cfg.KvCacheThreshold)
		} else {
			queueMin = nil
		}
		now := a.now()
		est := a.prefillObserver.Observe(prefill.Cycle{
			Key: key, Engine: engine, Disaggregated: true, Rows: rows[variant],
			QueueMin: queueMin, KVFull: kvFullPods, DownstreamSaturated: c.decodeSaturated,
			Bound: bounds[variant], Settle: view, ALineOnly: !a.prefillAQuadratic,
			ALowLoadPerRank: a.prefillALowLoadPerRank, AFromTiming: a.prefillAEngineTiming,
			QueueByCapacity: a.prefillQueueByCapacity, FBusyMinutes: a.prefillFBusyMinutes, FLoadCurve: a.prefillFLoadCurve,
			FPooledMinutes: a.prefillFPooledMinutes, VPFloorServed: a.prefillVPFloorServed, VPServedUnqueued: a.prefillVPServedUnqueued, VPWindowBest: a.prefillVPWindowBest,
			VPOccupancyPlateau: a.prefillVPOccupancyPlateau, VPPromptCoverage: a.prefillVPPromptCoverage, VPLoneRate: a.prefillVPLoneRate,
			VPServiceLine: a.prefillVPServiceLine, ALowLoadByRate: a.prefillALowLoadByRate, AMedianOverhead: a.prefillAMedianOverhead, Now: now,
		})
		a.stashPrefillEstimate(c.input.Namespace, c.input.ModelID, variant, est, now)
		logger := c.logger
		if a.prefillVPLoneRate {
			// Only with the switch on, so the line is unchanged off.
			logger = logger.WithValues("vpLone", est.VPLone, "vpLoneMinutes", est.VPLoneMinutes, "vpLoneIsVP", est.VPLoneIsVP)
		}
		if a.prefillVPServiceLine {
			// Only with the switch on, so the line is unchanged off.
			logger = logger.WithValues("vpLine", est.VPLine, "vpLineS0", est.VPLineS0, "vpLineSlope", est.VPLineSlope,
				"vpLineMinutes", est.VPLineMinutes, "vpLineRefused", string(est.VPLineRefused))
		}
		logger.V(perReplicaLogLevel).Info("prefill-capacity-observed",
			"modelID", c.input.ModelID, "namespace", c.input.Namespace, "variant", variant, "key", key,
			"engine", string(engine), "pods", len(rows[variant]),
			"vp", est.VP, "vpSource", string(est.VPSource), "vpLowerBound", est.VPLowerBound,
			"vpLowerBoundReason", string(est.VPLowerBoundReason),
			"vpRefused", string(est.VPRefused), "vpSamples", est.VPSamples,
			"vpServedPeak", est.VPServedPeak, "vpFlooredAtServed", est.VPFlooredAtServed,
			"vpPlateau", est.VPPlateau, "vpPlateauFlat", est.VPPlateauFlat,
			"vpPlateauMinutes", est.VPPlateauMinutes, "vpPlateauSpan", est.VPPlateauSpan,
			"vpPromptLow", est.VPPromptLow, "vpPromptHigh", est.VPPromptHigh,
			"slopeBound", est.SlopeBound, "vpAboveSlopeBound", est.VPAboveSlopeBound,
			"a", est.A, "aIntercept", est.AIntercept, "aOverhead", est.AOverhead,
			"aOverheadKnown", est.AOverheadKnown, "aRefused", string(est.ARefused), "aSamples", est.ASamples,
			"aPromptMin", est.APromptMin, "aPromptMax", est.APromptMax,
			"aModel", string(est.AModel), "aLineReason", string(est.ALineReason),
			"aSlope", est.ASlope, "aCurve", est.ACurve, "aClusters", est.AClusters,
			"aInterceptSE", est.AInterceptSE, "aShortestPrompt", est.AShortestPrompt,
			"aExtrapolated", est.AExtrapolated, "vpPromptTokens", est.VPPromptTokens,
			"queuedMinutes", est.VPQueuedMinutes, "longestQueuedRun", est.VPLongestQueuedRun,
			"busyMinutes", est.VPBusyMinutes, "busyMinutesOn", a.prefillBusyMinutes,
			"queueMinPods", queueMinPods, "kvFullPods", len(kvFullPods),
			"f", est.F, "fRefused", string(est.FRefused), "fSamples", est.FSamples,
			"fPods", est.FPodsLast, "fAtPods", est.FByPods[est.FPodsLast],
			"fSamplesAtPods", est.FSamplesByPods[est.FPodsLast],
			"downstreamSaturated", c.decodeSaturated,
			"admitted", est.Admitted, "refused", est.Refused)
	}
}
