package scalefromzero

import (
	"context"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// wakeSignalTTL is how long a pool's "does EPP export a flow-control queue"
// verdict is trusted before it is checked again.
//
// The answer changes only when EPP restarts or its config changes, so it does not
// belong on the 100ms wake loop — and a pool with nothing parked would otherwise
// be scraped ten times a second to answer a question about its configuration.
// The alert on this reason waits 10 minutes, so 30-second granularity costs
// nothing an operator can perceive.
const wakeSignalTTL = 30 * time.Second

// wakeSignalRetry is when to try again after a sweep that could determine
// nothing at all — every pool unreadable, or none resolvable yet.
//
// Distinct from the TTL because the two mean different things: the TTL is how
// long a real answer stays good, and this is how long to wait after failing to
// get one. Reusing the TTL would make a cluster that is still coming up take 30
// seconds per attempt to notice it has finished.
const wakeSignalRetry = 5 * time.Second

// poolWakeVerdict is one pool's answer and when it was obtained.
type poolWakeVerdict struct {
	exported bool
	at       time.Time
}

// wakeModelRef is a model, the EPP pool whose queue would wake it, and how many
// replicas are ready to serve it.
type wakeModelRef struct {
	namespace string
	modelID   string
	pool      string
	// readyReplicas is summed across every variant of the model, parked or not.
	readyReplicas int
}

// wakeSignalState caches per-pool verdicts and remembers which models were last
// reported on, so a model that leaves the fleet has its reason cleared.
type wakeSignalState struct {
	mu       sync.Mutex
	verdicts map[string]poolWakeVerdict
	reported map[string]wakeModelRef
	// nextAt is when the sweep may run again. Held as a deadline rather than as
	// "when it last ran" so a sweep that determined nothing can ask to be retried
	// sooner without pretending it never ran.
	nextAt time.Time
}

// recordPoolWakeVerdict caches what a scrape has already proved.
//
// processInactiveModel scrapes the pool anyway on every tick it considers a
// parked model, so for any pool with something parked the verdict is free and
// current; the periodic sweep then only pays for pools nothing is parked on.
func (e *Engine) recordPoolWakeVerdict(pool string, exported bool, now time.Time) {
	if pool == "" {
		return
	}
	e.wakeSignal.mu.Lock()
	defer e.wakeSignal.mu.Unlock()
	if e.wakeSignal.verdicts == nil {
		e.wakeSignal.verdicts = make(map[string]poolWakeVerdict)
	}
	e.wakeSignal.verdicts[pool] = poolWakeVerdict{exported: exported, at: now}
}

// cachedPoolWakeVerdict returns a pool's verdict when it is still fresh.
func (e *Engine) cachedPoolWakeVerdict(pool string, now time.Time) (exported, fresh bool) {
	e.wakeSignal.mu.Lock()
	defer e.wakeSignal.mu.Unlock()
	v, ok := e.wakeSignal.verdicts[pool]
	if !ok || now.Sub(v.at) > wakeSignalTTL {
		return false, false
	}
	return v.exported, true
}

// queueFamilyExported reports whether any flow-control queue sample is present,
// for any model. This is a property of the EPP, not of a model: an idle queue
// reports 0, and a queue that does not exist reports nothing at all.
func queueFamilyExported(values []source.MetricValue) bool {
	for _, value := range values {
		name := value.Labels[metricNameLabel]
		for _, want := range targetEPPMetricNames {
			if name == want {
				return true
			}
		}
	}
	return false
}

// reportWakeSignal publishes the no-wake-signal reason for every model the scaling manager knows
// about — parked or serving — and clears it for models that have gone away.
//
// It covers SERVING models deliberately. A model that is up right now but sits
// behind an EPP with no flow control is the dangerous case, not a safe one: the
// enforcer will park it on the vLLM request counter, which has nothing to do with
// flow control, and only then will it be unwakeable. Reporting at the moment it
// parks is reporting after the trap has closed.
//
// It also does not live in processInactiveModel, where it started. That function
// returns early on half a dozen bootstrap conditions — no pool in the datastore,
// no resolvable candidates, no EPP metrics source, a scrape failure with no
// fallback — and an emission below those returns silently stops updating, leaving
// whatever it last said standing. A sweep that runs off its own clock cannot be
// skipped by any of them.
func (e *Engine) reportWakeSignal(
	ctx context.Context,
	inactiveVAs []llmdvariant.Variant,
	inactiveTargets map[string]scaletarget.ScaleTargetAccessor,
	activeVAs []llmdvariant.Variant,
	activeTargets map[string]scaletarget.ScaleTargetAccessor,
	now time.Time,
) {
	logger := log.FromContext(ctx)

	e.wakeSignal.mu.Lock()
	due := e.wakeSignal.nextAt.IsZero() || !now.Before(e.wakeSignal.nextAt)
	e.wakeSignal.mu.Unlock()

	// refs is FLEET MEMBERSHIP, and carries an empty pool for a model whose pool
	// cannot be resolved yet. That distinction is the point: an unresolvable pool
	// means "cannot tell", and a model scaling manager cannot currently tell about is not the
	// same as a model that has gone away. Dropping it from refs would have the
	// prune below clear its reason, which is an answer, from a sweep that has none.
	//
	// Pool resolution is skipped unless a wake check is due: it scans the datastore
	// for every model, and this runs on the 100ms loop. Replica counts come off the
	// scale targets already in hand and cost nothing, so they are NOT throttled.
	refs := make(map[string]wakeModelRef)
	e.collectWakeRefs(ctx, refs, inactiveVAs, inactiveTargets, due)
	e.collectWakeRefs(ctx, refs, activeVAs, activeTargets, due)

	// The model-level replica count, published from HERE because this is the only
	// place that sees the whole fleet. The steady-state engine cannot publish it:
	// it lists variants with isActive, meaning spec replicas > 0, so a PARKED model
	// never enters that engine at all — and a parked model is the entire subject of
	// llmsm_model_replicas == 0. Emitted there, the series would only ever have held
	// counts of 1 or more, and the symptom alert would have been unreachable while
	// looking perfectly well-formed.
	//
	// Every cycle, not on the wake clock: this is the current state of the fleet,
	// and it is what the symptom alert reads. Holding it behind the 30s throttle
	// would have made "is anything serving this model" up to 30 seconds stale for
	// no saving — nothing here is scraped.
	for _, ref := range refs {
		metrics.SetModelReplicas(ref.namespace, ref.modelID, ref.readyReplicas)
	}

	// Departures are handled every cycle too, so a deleted workload stops
	// reporting promptly rather than at the next wake check.
	e.pruneWakeReports(refs)

	if !due {
		return
	}

	var attempted, determined int
	for _, ref := range refs {
		if ref.pool == "" {
			// Bootstrap: no pool yet. Hold whatever was last said.
			continue
		}
		attempted++

		exported, fresh := e.cachedPoolWakeVerdict(ref.pool, now)
		if !fresh {
			var ok bool
			exported, ok = e.scrapePoolWakeVerdict(ctx, ref.pool, now)
			if !ok {
				// Could not look. Say nothing rather than assert an absence we have
				// not observed — a false no-wake-signal sends an operator to restart
				// a healthy EPP. Same treatment as an unresolvable pool above: both
				// are "cannot tell", and they must not differ in what they leave
				// behind.
				logger.V(logging.DEBUG).Info("Wake-signal check skipped: pool not readable",
					"pool", ref.pool, "modelID", ref.modelID)
				continue
			}
		}
		determined++

		var reasons []string
		if !exported {
			reasons = []string{constants.ScalingBlockedNoWakeSignal}
		}
		metrics.SetModelScalingBlockedReasons(ref.namespace, ref.modelID,
			constants.ScalingBlockedReasonsWake, reasons)
	}

	// A sweep that answered nothing at all asks to be retried soon rather than in
	// a full TTL: that is a cluster still coming up, not a settled answer.
	next := now.Add(wakeSignalTTL)
	if attempted > 0 && determined == 0 {
		next = now.Add(wakeSignalRetry)
	}
	e.wakeSignal.mu.Lock()
	e.wakeSignal.nextAt = next
	e.wakeSignal.mu.Unlock()
}

// collectWakeRefs adds one entry per model, resolving the pool from the first
// variant whose workload resolves one. Variants of a model serve the same model
// behind the same EPP, so they share a queue.
//
// A model whose pool does not resolve is still recorded, with an empty pool: the
// entry means "the scaling manager has this model", and the pool means "and here is where its
// queue would be". Conflating the two would make a bootstrapping model look
// departed.
func (e *Engine) collectWakeRefs(
	ctx context.Context,
	into map[string]wakeModelRef,
	vas []llmdvariant.Variant,
	targets map[string]scaletarget.ScaleTargetAccessor,
	resolvePool bool,
) {
	for i := range vas {
		va := vas[i]
		key := utils.GetNamespacedKey(va.Namespace, va.Spec.ModelID)

		ref, seen := into[key]
		if !seen {
			ref = wakeModelRef{namespace: va.Namespace, modelID: va.Spec.ModelID}
		}

		scaleTarget, err := e.resolveScaleTarget(ctx, targets, va)
		if err == nil && scaleTarget != nil {
			// READY, not spec: the question is whether anything is actually serving
			// this model, and a workload scaled to 3 whose pods are all pending
			// serves nothing. Accumulated across BOTH calls — a model's variants are
			// split between the inactive and active lists, so neither alone is the
			// model's total.
			ref.readyReplicas += int(scaleTarget.GetStatusReadyReplicas())
			if resolvePool && ref.pool == "" {
				ref.pool = e.poolNameForScaleTarget(va.Namespace, scaleTarget)
			}
		}
		into[key] = ref
	}
}

// poolNameForScaleTarget resolves the namespaced EPP pool name behind a
// workload, or "" when it cannot be resolved — the normal bootstrap state, and
// not something to report a missing wake signal for.
func (e *Engine) poolNameForScaleTarget(
	namespace string,
	scaleTarget scaletarget.ScaleTargetAccessor,
) string {
	if scaleTarget == nil {
		return ""
	}
	podTemplateSpec := scaleTarget.GetLeaderPodTemplateSpec()
	if podTemplateSpec == nil || podTemplateSpec.Labels == nil {
		return ""
	}
	pool, err := e.Datastore.PoolGetFromLabels(namespace, podTemplateSpec.Labels)
	if err != nil || pool == nil {
		return ""
	}
	return pool.Namespace + "/" + pool.Name
}

// scrapePoolWakeVerdict reads a pool's EPP directly. Reports ok=false when the
// pool cannot be read at all, which is not the same as the family being absent.
func (e *Engine) scrapePoolWakeVerdict(ctx context.Context, pool string, now time.Time) (exported, ok bool) {
	eppSource := e.Datastore.PoolGetMetricsSource(pool)
	if eppSource == nil {
		return false, false
	}
	results, err := eppSource.Refresh(ctx, source.RefreshSpec{})
	if err != nil {
		return false, false
	}
	result := results["all_metrics"]
	if result == nil {
		// No result for the query at all is a failure to look, not an observation
		// that the family is absent.
		return false, false
	}
	exported = queueFamilyExported(result.Values)
	e.recordPoolWakeVerdict(pool, exported, now)
	return exported, true
}

// pruneWakeReports clears the wake reason for models no longer in the fleet.
//
// Only this producer's reason is cleared: the enforcer owns the policy reasons
// and prunes its own. A model that has gone away entirely is cleared wholesale by
// the enforcer's prune, which crosses ownership on purpose because by then no
// producer runs for it at all.
func (e *Engine) pruneWakeReports(current map[string]wakeModelRef) {
	e.wakeSignal.mu.Lock()
	previous := e.wakeSignal.reported
	e.wakeSignal.reported = make(map[string]wakeModelRef, len(current))
	for key, ref := range current {
		e.wakeSignal.reported[key] = ref
	}
	e.wakeSignal.mu.Unlock()

	for key, ref := range previous {
		if _, still := current[key]; still {
			continue
		}
		metrics.SetModelScalingBlockedReasons(ref.namespace, ref.modelID,
			constants.ScalingBlockedReasonsWake, nil)
		// Deleted, not set to zero: zero asserts the model exists and is parked,
		// which would keep the symptom alert firing for a workload that is gone.
		metrics.ClearModelReplicas(ref.namespace, ref.modelID)
	}
}
