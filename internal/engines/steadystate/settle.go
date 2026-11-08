package steadystate

import (
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle/settlerow"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
)

// The settle tracker (internal/signals/settle, analyzer-learner.md 4.2): the
// engine owns it, one per controller, keyed by model, and writes it once per
// model per cycle at the top of runAnalyzers, before the load-balance
// diagnostic and the input builder read the cycle's rows. Readers take a
// read-only view; the load-balance tracker is the first.

// settleTracker is the engine's settle tracker, set to the configured
// optimize interval, which sets how long a replica may go without a row
// (settle.Tolerance). Built on first use, as the other per-model state here:
// optimize cycles run one at a time, and an Engine built as a literal in a
// test has none.
func (e *Engine) settleTracker() *settle.Tracker {
	if e.settle == nil {
		e.settle = settle.NewTracker()
	}
	e.settle.SetInterval(e.sloMissedAfter())
	return e.settle
}

// observeSettle observes one model's rows into the settle tracker at now,
// and records the observation when a learner recording is open: the model
// and now, the rows being the ones the model's Analyze events record.
func (e *Engine) observeSettle(namespace, modelID string, rows []domain.ReplicaMetrics,
	states []domain.VariantReplicaState, now time.Time) {
	e.learnRec.Record(settle.KindObserve, now, settle.ObserveEvent{Namespace: namespace, ModelID: modelID,
		Interval: e.sloMissedAfter()})
	e.settleTracker().Observe(utils.GetNamespacedKey(namespace, modelID),
		settlerow.Of(rows, variantRoles(states)), now)
}

// holdSettle records a cycle that read no rows of the model (its collection
// failed or returned nothing, or its namespace's scaling policy was not
// loaded yet) at now, and records the hold when a learner
// recording is open. The replicas seen last keep their sighting through it
// (settle.Tracker.Hold): a Prometheus outage is not an absence.
func (e *Engine) holdSettle(namespace, modelID string, now time.Time) {
	e.learnRec.Record(settle.KindHold, now, settle.ObserveEvent{Namespace: namespace, ModelID: modelID,
		Interval: e.sloMissedAfter()})
	e.settleTracker().Hold(utils.GetNamespacedKey(namespace, modelID), now)
}

// sightingsMaxAge is how old a document may be and still give back its
// sightings: the gap one process would tolerate without a row
// (settle.Tolerance), plus the time between two writes
// (learnedStateInterval), since the document was up to that old when the gap
// began. 2 min at the default interval, the restart section 4.4 of
// analyzer-learner.md reasons about. Past it a replica has been unread for
// longer than one process would let pass, so load may have come back in the
// gap, and every replica waits.
func (e *Engine) sightingsMaxAge() time.Duration {
	return settle.Tolerance(e.sloMissedAfter()) + learnedStateInterval
}

// restoreSettle hands the settle tracker the sightings of a restored
// document, read at now, records them when a learner recording is open, and
// returns how many the tracker took. A document older than sightingsMaxAge
// gives none back, and the log says why. Only the snapshot's restore calls
// it, so with the snapshot off nothing is restored (SwitchFirstSightRestored).
func (e *Engine) restoreSettle(logger logr.Logger, doc learnedStateDoc, now time.Time) int {
	sightings := doc.Settle
	if len(sightings) == 0 {
		return 0
	}
	if age, limit := now.Sub(doc.WrittenAt), e.sightingsMaxAge(); age > limit {
		logger.Info("Learned state: the snapshot is too old to keep its replica sightings; every replica waits its settle",
			"ageSeconds", age.Seconds(), "maxAgeSeconds", limit.Seconds())
		return 0
	}
	ev := settle.RestoreEvent{Interval: e.sloMissedAfter()}
	for _, model := range slices.Sorted(maps.Keys(sightings)) {
		// A namespace holds no slash; a model ID may.
		namespace, modelID, _ := strings.Cut(model, "/")
		ev.Models = append(ev.Models, settle.RestoredModel{Namespace: namespace, ModelID: modelID, Sightings: sightings[model]})
	}
	e.learnRec.Record(settle.KindRestore, now, ev)
	return e.settleTracker().Restore(sightings, now)
}

// modelCollectionFailed is what a cycle whose collection failed for the
// model does to the per-replica state the engine keeps: the settle tracker
// holds its sightings, and the load-balance series stand until collection
// has failed for balance.StaleAfter (loadBalanceCollectionFailed).
func (e *Engine) modelCollectionFailed(namespace, modelID string) {
	now := e.observeClock()
	e.holdSettle(namespace, modelID, now)
	e.loadBalanceCollectionFailed(namespace, modelID, now)
}

// retainSettle forgets the settle record of every model whose namespaced key
// is not in active; nil forgets them all. A model that is still reconciled
// keeps its record through a cycle with no rows or a failed collection
// (holdSettle), until it has gone settle.TraceHorizon without rows.
func (e *Engine) retainSettle(active map[string]bool) {
	if e.settle != nil {
		e.settle.Retain(active)
	}
}

// variantRoles maps each variant to its role as discovery recorded it.
func variantRoles(states []domain.VariantReplicaState) map[string]string {
	roles := make(map[string]string, len(states))
	for _, vs := range states {
		roles[vs.VariantName] = vs.Role
	}
	return roles
}
