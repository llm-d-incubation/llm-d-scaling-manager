package collector

import (
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// The slo analyzer's v2 models' inputs
// (docs/proposals/predicted-latency-in-controller.md, "Signals"). The
// experimental latency models read per-pod series that a controller with no
// learner recording and no TTFT target never asks for. With
// slo.SwitchV2Inputs on, a cycle names every namespace it collects
// (ReadPredictionInputsIn) and the collection issues those queries too,
// through the same memo, once per namespace.
//
// What they return goes to the v2 models alone. CollectReplicaMetrics keeps
// the full rows for it (PredictionRows) and hands the analyzers the rows as a
// collection without the switch would have built them: every field only a
// prediction query filled is cleared again (clearPredictionOnly), so no
// analyzer reads a value it would not have read, and no scaling decision
// moves. With a recording on nothing is cleared, as every one of them is
// issued for the recording already.

// predictionReadReplicaQueries are the replica queries the slo analyzer's v2
// models add, each with what clears the fields it fills on a row: the
// rank-mixture TTFT model's prefill timings and histograms. Each is a
// record-only query or a TTFT reader's, never a scaling query.
var predictionReadReplicaQueries = map[string]func(rm *domain.ReplicaMetrics){
	registration.QueryAvgPrefillTime: func(rm *domain.ReplicaMetrics) {
		rm.AvgPrefillTime, rm.AvgPrefillTimeKnown = 0, false
		for i := range rm.GroupEngines {
			rm.GroupEngines[i].AvgPrefillTime, rm.GroupEngines[i].AvgPrefillTimeKnown = 0, false
		}
	},
	registration.QueryAvgQueueTime: func(rm *domain.ReplicaMetrics) {
		rm.AvgQueueTime, rm.AvgQueueTimeKnown = 0, false
		for i := range rm.GroupEngines {
			rm.GroupEngines[i].AvgQueueTime, rm.GroupEngines[i].AvgQueueTimeKnown = 0, false
		}
	},
	registration.QueryQueueTimeBuckets:      func(rm *domain.ReplicaMetrics) { rm.QueueTimeBuckets = nil },
	registration.QueryPromptTokensBuckets:   func(rm *domain.ReplicaMetrics) { rm.PromptTokensBuckets = nil },
	registration.QueryComputedTokensBuckets: func(rm *domain.ReplicaMetrics) { rm.ComputedTokensBuckets = nil },
	// The tokens per iteration: whether the step budget binds, the engine
	// class the v2 models' queue prior and aggregated model will key on
	// (capacity.EngineParams.StepBudgetBinding over a capacity.StepTokens of
	// the variant's PredictionRows).
	registration.QueryIterationTokensBuckets: func(rm *domain.ReplicaMetrics) { rm.IterationTokensBuckets = nil },
}

// isPredictionQuery is a query of predictionReadReplicaQueries.
func isPredictionQuery(name string) bool {
	_, ok := predictionReadReplicaQueries[name]
	return ok
}

// replicaQueryListsFor is replicaQueryLists with the slo analyzer's v2
// models' queries added where predict is set: the scaling queries, the
// TTFT readers' where ttftRead is, and the prediction queries, each once.
// A recording issues every one already.
func replicaQueryListsFor(recording, ttftRead, predict bool) (engineSpecific, agnostic []string) {
	if recording || !predict {
		return replicaQueryLists(recording, ttftRead)
	}
	keep := func(name string) bool {
		return isScalingQuery(name) || (ttftRead && isTTFTReadQuery(name)) || isPredictionQuery(name)
	}
	return filterQueries(engineSpecificReplicaQueries, keep), filterQueries(agnosticReplicaQueries, keep)
}

// clearPredictionOnly clears, on each of rows, what only a prediction query
// filled: the fields of predictionReadReplicaQueries but for a TTFT reader's
// where ttftRead (the namespace issues it anyway). A row's group engines are
// copied before they are cleared, so the rows PredictionRows keeps are not
// touched.
func clearPredictionOnly(rows []domain.ReplicaMetrics, ttftRead bool) {
	for i := range rows {
		rows[i].GroupEngines = slices.Clone(rows[i].GroupEngines)
		for q, clear := range predictionReadReplicaQueries {
			if ttftRead && isTTFTReadQuery(q) {
				continue
			}
			clear(&rows[i])
		}
	}
}

// ReadPredictionInputsIn names the namespaces whose replica collections this
// cycle issue the slo analyzer's v2 models' queries. Outside a cycle it
// does nothing, and EndCycle clears it, with the rows it kept.
func (c *ReplicaMetricsCollector) ReadPredictionInputsIn(namespaces map[string]bool) {
	c.cycleMu.Lock()
	defer c.cycleMu.Unlock()
	if c.cycleResults == nil {
		return
	}
	c.cyclePredictNamespaces = namespaces
}

// readsPredictionInputs reports whether this cycle reads the
// slo analyzer's v2 models' inputs for namespace.
func (c *ReplicaMetricsCollector) readsPredictionInputs(namespace string) bool {
	c.cycleMu.Lock()
	defer c.cycleMu.Unlock()
	return c.cyclePredictNamespaces[namespace]
}

// PredictionRows is the rows this cycle's collection of the model read, with
// the slo analyzer's v2 models' inputs on them; ok is false where the
// cycle did not read them for its namespace or did not collect the model.
// The rows are the v2 models': the analyzers have their own.
func (c *ReplicaMetricsCollector) PredictionRows(namespace, modelID string) ([]domain.ReplicaMetrics, bool) {
	c.cycleMu.Lock()
	defer c.cycleMu.Unlock()
	rows, ok := c.cyclePredictRows[namespace+"/"+modelID]
	return rows, ok
}

// splitPredictionRows keeps rows for the v2 models when this cycle reads its
// inputs for namespace, and returns the rows the analyzers read: rows itself
// otherwise.
func (c *ReplicaMetricsCollector) splitPredictionRows(namespace, modelID string, rows []domain.ReplicaMetrics) []domain.ReplicaMetrics {
	if !c.readsPredictionInputs(namespace) {
		return rows
	}
	kept := slices.Clone(rows)
	c.cycleMu.Lock()
	if c.cyclePredictRows == nil {
		c.cyclePredictRows = map[string][]domain.ReplicaMetrics{}
	}
	c.cyclePredictRows[namespace+"/"+modelID] = kept
	recording := c.cycleRecord
	ttftRead := c.cycleTTFTNamespaces[namespace]
	c.cycleMu.Unlock()
	if recording {
		return rows
	}
	out := slices.Clone(rows)
	clearPredictionOnly(out, ttftRead)
	return out
}
