package slo

import (
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle/settlerow"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
)

// The SLO line on the settle tracker (analyzer-learner.md, section 7, PR
// 4a): the same six changes as the saturation analyzer's ITL line, each
// behind a switch of this package, since this analyzer fits its own line
// with its own itl.Tracker. The engine writes the tracker once per model per
// cycle, before any analyzer runs, and hands this analyzer a read-only view
// (WithSettleView). An analyzer given none (a test that drives Analyze
// alone) observes a tracker of its own at the top of each Analyze, as one
// that has watched the fleet: nothing restarted.

// SwitchITLReadsSettle is the replay switch for the move: on, a replica's
// settle starts where the settle tracker says (itl.Settle); off, as before,
// the line's own trace of each replica.
const SwitchITLReadsSettle = "slo-itl-reads-the-settle-tracker"

// SwitchITLLongGapFirstSeen is the replay switch for a long gap on the
// analyzer's own tracker (issue ev-shindin/llm-scaling-manager#459): on, a model last analyzed more than
// the state timeout ago is first-seen, whether or not EvictStale ran
// meanwhile, and wherever in the cycle it ran (observeOwnSettle); off, as
// before, only a model EvictStale evicted is, and one it did not reads the
// gap as an absence (settle.Rejoined). Read only with SwitchITLReadsSettle
// on, and only by an analyzer with no engine view.
const SwitchITLLongGapFirstSeen = "slo-itl-own-settle-reads-a-long-gap-as-first-seen"

// SwitchITLOwnSettleBackdates is the replay switch for the backdate on the
// analyzer's own tracker (issue ev-shindin/llm-scaling-manager#511): on, a model's first observation is
// one, at the cycle, of replicas Joined ownSettleBackdate earlier
// (settlerow.ObserveOwnSince), so the prefill share and decode term read
// them as settled on that cycle; off, as before, the rows are observed at
// the backdate and again at the cycle (settlerow.ObserveOwn), the gap
// between the two is past the tracker's absence tolerance, and every
// replica reads settle.Rejoined at the cycle. Read only by an analyzer with
// no engine view.
const SwitchITLOwnSettleBackdates = "slo-itl-own-settle-backdates-the-first-sighting"

// SwitchITLWaitsAfterRestart is the replay switch for the line's wait after
// a controller restart: on, a replica already serving on the line's first
// cycle waits its settle too, so the line does not refit for
// steadyGate.steadyFor after a restart; off, as before, it is taken as
// settled. An analyzer with its own tracker never waits.
const SwitchITLWaitsAfterRestart = "slo-itl-waits-after-controller-restart"

// SwitchITLKeepsSettleThroughMissingRow is the replay switch for the
// absence rule: on, a replica missing from a cycle keeps its settle up to
// the tracker's absence tolerance; off, as before, it starts over.
const SwitchITLKeepsSettleThroughMissingRow = "slo-itl-keeps-the-settle-through-a-missing-row"

// SwitchITLReadsFinitePodMean is the replay switch for what a reading's ITL
// is: on, a row is a reading only in the Finite state its presence bits
// give, so a Pod with an Absent rank is refused; off, as before, any finite
// AvgITL above zero is a reading.
const SwitchITLReadsFinitePodMean = "slo-itl-reads-the-finite-pod-mean"

// SwitchITLRestartUnderName is the replay switch for an engine restart under
// the same Pod name, on the tracker: on, a new Ready time under the same
// name starts a settle; off, as on the line's own trace, only a row that
// was not Ready shows the restart.
const SwitchITLRestartUnderName = "slo-itl-restart-under-its-name-settles"

// SwitchITLIdleOnEvidence is the replay switch for what an idle row is, on
// the tracker: on, idle only on evidence (a known KV of 0, and a known
// empty ITL window or a known generation rate of 0); off, as on the line's
// own trace, a row with neither an ITL nor a k is idle.
const SwitchITLIdleOnEvidence = "slo-itl-idle-needs-evidence"

// WithSettleView sets the settle tracker the line reads each replica's
// settle from: the engine's, which it observes at the top of every cycle.
// Call it before the analyzer is used. Without it the analyzer observes a
// tracker of its own.
func (a *Analyzer) WithSettleView(v settle.View) *Analyzer {
	a.settle = v
	return a
}

// WithOptimizationInterval sets the controller's optimize interval, which
// sets how long a replica may go without a row and keep its settle on the
// line's own trace (settle.Tolerance), as the saturation analyzer's. Call it
// before the analyzer is used. Unset, the least tolerance, settle.Period.
func (a *Analyzer) WithOptimizationInterval(d time.Duration) *Analyzer {
	a.settleInterval = d
	return a
}

// ownSettleBackdate is how far before its first Analyze of a model the
// analyzer's own tracker takes the model's replicas as seen: the line's
// settle, so a replica on the first cycle has settled, Joined that long ago
// (settlerow.ObserveOwnSince: one observation, since the backdate is longer
// than the tracker's absence tolerance, issue ev-shindin/llm-scaling-manager#511). The saturation analyzer
// backdates by k2's JoinedReplicaWindow instead, the wait its own tracker
// serves first; for the ITL line neither matters, since a line with no
// engine tracker never waits after a restart.
const ownSettleBackdate = itl.DefaultSteadyFor

// settleView is the tracker the line reads: the engine's when it was given
// one, else its own.
func (a *Analyzer) settleView() settle.View {
	if a.settle != nil {
		return a.settle
	}
	return a.ownSettle
}

// observeOwnSettle observes input's rows into the analyzer's own tracker at
// now, when the engine gave it none.
//
// A model the own tracker does not hold -- never seen, or evicted by
// evictOwnSettleLocked -- and one last observed more than the state timeout
// ago, not yet evicted, are both first-seen: what the tracker held of the
// model is forgotten, its rows are observed backdated, and every line key of
// the model forgets its replicas' settle traces (itl.Tracker.ForgetTraces,
// absent variants included), so nothing settles. The eviction runs only from
// EvictStale, so before this a gap past the timeout read as an absence
// (settle.Rejoined), and every replica settled, unless EvictStale had run
// first; and run between two models' Analyze calls, it settled the model
// analyzed before it and not the one after. Reading a stale model as an
// evicted one makes the settle reading after the gap depend only on the
// model's own last sight, as the throughput analyzer's (learner PR 4b-2).
// Only the settle reading: the line's windows, intercepts and held slope
// are still swept by EvictStale (a.lines.Sweep), so the fit after a long
// gap still depends on whether and when it ran. SwitchITLLongGapFirstSeen,
// read with SwitchITLReadsSettle: either off, as before, only a model the
// tracker does not hold is backdated, and the line's own traces are kept.
//
// Everything runs under a.mu (the trackers' own locks nest under it, as in
// evictStale), so two Analyze calls for one model cannot interleave the
// stamp, the forget and the observation.
func (a *Analyzer) observeOwnSettle(input domain.AnalyzerInput, now time.Time) {
	if a.settle != nil {
		return
	}
	roles := make(map[string]string, len(input.VariantStates))
	for _, vs := range input.VariantStates {
		roles[vs.VariantName] = vs.Role
	}
	model := utils.GetNamespacedKey(input.Namespace, input.ModelID)
	a.mu.Lock()
	defer a.mu.Unlock()
	last, seen := a.ownSettled[model]
	first := !seen
	if a.itlReadsSettle && a.itlLongGapFirstSeen && seen && now.Sub(last) > a.stateTimeout {
		first = true
		// The key shape of windowKey; the trailing separator keeps "m" from
		// matching "m2".
		prefix := input.Namespace + "|" + input.ModelID + "|"
		a.lines.ForgetTraces(func(key string) bool { return strings.HasPrefix(key, prefix) })
		a.ownSettle.Forget(model)
	}
	a.ownSettled[model] = now
	observe := settlerow.ObserveOwn
	if a.itlOwnBackdate {
		observe = settlerow.ObserveOwnSince
	}
	observe(a.ownSettle, model, settlerow.Of(input.ReplicaMetrics, roles), first, ownSettleBackdate, now)
}

// evictOwnSettleLocked forgets what the analyzer's own tracker holds of a
// model it has not analyzed for the state timeout. Callers hold a.mu.
func (a *Analyzer) evictOwnSettleLocked(now time.Time) {
	for model, at := range a.ownSettled {
		if now.Sub(at) > a.stateTimeout {
			delete(a.ownSettled, model)
			a.ownSettle.Forget(model)
		}
	}
}

// settleAdmission sets on adm how the line reads its replicas' settle for a
// model, by the six switches.
func (a *Analyzer) settleAdmission(adm *itl.Admission, namespace, modelID string) {
	a.mu.Lock()
	reads, waits, keeps, finite := a.itlReadsSettle, a.itlWaitsAfterRestart, a.itlKeepsSettle, a.itlFiniteOnly
	restartUnderName, idleOnEvidence := a.itlRestartUnderName, a.itlIdleOnEvidence
	interval := a.settleInterval
	a.mu.Unlock()
	if interval > 0 {
		adm.AbsenceTolerance = settle.Tolerance(interval)
	}
	if reads {
		adm.Settle = &itl.Settle{View: a.settleView(), Model: utils.GetNamespacedKey(namespace, modelID)}
	}
	adm.WaitsAfterRestart = waits && a.settle != nil
	adm.KeepsThroughMissingRow = keeps
	adm.FiniteOnly = finite
	adm.RestartUnderName, adm.IdleOnEvidence = restartUnderName, idleOnEvidence
}
