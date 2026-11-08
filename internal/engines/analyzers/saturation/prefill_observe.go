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
			Bound: bounds[variant], Settle: view, ALineOnly: !a.prefillAQuadratic, Now: now,
		})
		a.stashPrefillEstimate(c.input.Namespace, c.input.ModelID, variant, est, now)
		c.logger.V(perReplicaLogLevel).Info("prefill-capacity-observed",
			"modelID", c.input.ModelID, "namespace", c.input.Namespace, "variant", variant, "key", key,
			"engine", string(engine), "pods", len(rows[variant]),
			"vp", est.VP, "vpSource", string(est.VPSource), "vpLowerBound", est.VPLowerBound,
			"vpLowerBoundReason", string(est.VPLowerBoundReason),
			"vpRefused", string(est.VPRefused), "vpSamples", est.VPSamples,
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
