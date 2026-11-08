package collector

import (
	"context"
	"fmt"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/prometheus"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// refreshShared executes queries, reusing any result already fetched in this
// cycle for the same (query, params) pair.
//
// Only the queries missing from the memo are sent to the source, so the first
// model in a namespace pays for the namespace-scoped queries and the rest read
// them, while a mixed-engine namespace still fetches each engine's variant
// exactly once.
//
// A result carrying a query error is memoized like any other: the query did
// fail for this namespace this cycle, and re-running it once per model would
// multiply a Prometheus outage by the number of models rather than surface it
// once.
//
// params must contain exactly the parameters the queries take. Passing extra
// ones (a modelID a namespace-scoped query ignores, say) does not change the
// PromQL, but it does change the memo key and would silently defeat sharing.
func (c *ReplicaMetricsCollector) refreshShared(
	ctx context.Context,
	queries []string,
	params map[string]string,
) (map[string]*source.MetricResult, error) {
	results, _, err := c.refreshSharedIssued(ctx, queries, params)
	return results, err
}

// refreshSharedIssued is refreshShared, also returning the queries this call
// sent to the source: those not in the memo. A caller that counts a failed
// query counts it for these only, so a failure is counted once, by the
// model whose collection issued it, and not again by every model in the
// namespace that reads it from the memo.
func (c *ReplicaMetricsCollector) refreshSharedIssued(
	ctx context.Context,
	queries []string,
	params map[string]string,
) (map[string]*source.MetricResult, []string, error) {
	results := make(map[string]*source.MetricResult, len(queries))
	missing := queries

	c.cycleMu.Lock()
	if c.cycleResults != nil {
		missing = make([]string, 0, len(queries))
		for _, name := range queries {
			// A memoized nil records a query the source returned nothing for;
			// that is an answer, so it is not asked again this cycle.
			if cached, ok := c.cycleResults[source.BuildCacheKey(name, params)]; ok {
				if cached != nil {
					results[name] = cached
				}
				continue
			}
			missing = append(missing, name)
		}
	}
	c.cycleMu.Unlock()

	if len(missing) == 0 {
		return results, nil, nil
	}

	fetched, err := c.source.Refresh(ctx, source.RefreshSpec{Queries: missing, Params: params})
	if err != nil && fetched == nil {
		return nil, missing, err
	}

	c.cycleMu.Lock()
	for _, name := range missing {
		result := fetched[name]
		if result != nil {
			results[name] = result
		}
		if c.cycleResults != nil {
			c.cycleResults[source.BuildCacheKey(name, params)] = result
		}
	}
	c.cycleMu.Unlock()

	// Memoized above even when every query failed, so the next model in this
	// namespace reads the failed results instead of asking a down Prometheus
	// again; the error is returned once, to the model that asked.
	if err != nil {
		return nil, missing, err
	}
	return results, missing, nil
}

// countRefreshFailure counts a refresh that failed as a whole once for each
// query of issued, the queries the failed call sent, under the query type
// typeOf names for it, and returns the failure's reason for the caller's log.
func countRefreshFailure(issued []string, err error, typeOf func(string) string) string {
	reason := prometheus.CategorizePrometheusError(err)
	for _, q := range issued {
		metrics.IncMetricsCollectionErrors(typeOf(q), reason)
	}
	return reason
}

// countFailedIssued counts each query of issued whose result carries an
// error, under the query type typeOf names for it: once per query this
// collection sent, never for a result read from the memo. A query typeOf
// names no type for ("") is not counted here.
func countFailedIssued(issued []string, results map[string]*source.MetricResult, typeOf func(string) string) {
	for _, q := range issued {
		queryType := typeOf(q)
		if r := results[q]; queryType != "" && r != nil && r.HasError() {
			metrics.IncMetricsCollectionErrors(queryType, prometheus.CategorizePrometheusError(r.Error))
		}
	}
}

// queryReplicaSeries fetches every per-replica series for one model: the
// engine-specific queries for each engine the model's variants run, merged
// back under their logical names and cut down to this model's series. The
// engines are returned with the results because the SGLang cache-config
// series is read through its physical key downstream (extractPodMetrics).
func (c *ReplicaMetricsCollector) queryReplicaSeries(
	ctx context.Context,
	modelID string,
	namespace string,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
) (map[string]*source.MetricResult, []inferenceengine.Engine, error) {
	logger := ctrl.LoggerFrom(ctx)

	// Every replica query is namespace-scoped: the model is selected from the
	// returned series, not by a PromQL matcher, so the params carry no modelID.
	params := map[string]string{
		source.ParamNamespace: namespace,
	}

	// Determine which inference engines this model's variants run. Engine-specific
	// queries are refreshed once per present engine (vLLM pods emit vllm:* series,
	// SGLang pods emit sglang:* series — the per-pod results are disjoint and are
	// merged back under the logical query name below). For vLLM-only models this
	// is identical to the previous fixed query list.
	engines := inferenceengine.Present(scaleTargets)

	// Log the engine detected for each scale target, plus the resolved engine set.
	// inferenceengine.Detect defaults to vLLM when a leader pod template is nil or
	// unresolvable, or when an SGLang image/command isn't matched — so a misdetected
	// SGLang variant silently gets vllm:* queries and emits nothing. Logging the
	// per-target engine lets operators tell "wrong engine detected" apart from
	// "engine correct, but no metrics".
	if debug := logger.V(logging.DEBUG); debug.Enabled() {
		for key, st := range scaleTargets {
			debug.Info("Detected inference engine for scale target",
				"scaleTarget", key, "engine", inferenceengine.Detect(st).String())
		}
		engineNames := make([]string, len(engines))
		for i, e := range engines {
			engineNames[i] = e.String()
		}
		debug.Info("Resolved inference engines for model",
			"modelID", modelID, "namespace", namespace, "engines", engineNames)
	}

	// Refresh all Prometheus-sourced queries (engine_queries.go lists them):
	// - Saturation: KV cache, queue length, cache config, prefix cache hit rate
	// - Shared: avg input tokens, avg output tokens
	// - Per-request timings: avg ITL, avg service time
	// - Throughput analyzer: generation token rate, instantaneous KV usage (k*), request rate
	// - With a TTFT target in the namespace, what the TTFT pricing reads
	//   (ttftReadReplicaQueries)
	// - With a learner recording on, every TTFT input (recordOnlyReplicaQueries too)
	recording := c.RecordsTTFTInputs()
	ttftRead := c.readsTTFTInputs(namespace)
	engineSpecific, agnostic := replicaQueryLists(recording, ttftRead)
	queries := buildEngineQueryList(engines, engineSpecific, agnostic)

	// Execute the query with timing. refreshShared skips whatever another model
	// in this namespace already fetched this cycle, so the timing observed here
	// is the cost of the queries this collection actually issued.
	startTime := time.Now()
	results, issued, err := c.refreshSharedIssued(ctx, queries, params)
	duration := time.Since(startTime).Seconds()
	metrics.ObserveMetricsCollectionDuration(duration, constants.QueryTypeKVCache)
	metrics.ObserveMetricsCollectionDuration(duration, constants.QueryTypeQueueLength)
	metrics.ObserveMetricsCollectionDuration(duration, constants.QueryTypeCacheConfig)

	if err != nil {
		reason := prometheus.CategorizePrometheusError(err)
		metrics.IncMetricsCollectionErrors(constants.QueryTypeKVCache, reason)
		metrics.IncMetricsCollectionErrors(constants.QueryTypeQueueLength, reason)
		metrics.IncMetricsCollectionErrors(constants.QueryTypeCacheConfig, reason)
		return nil, nil, fmt.Errorf("failed to refresh replica metrics: %w", err)
	}
	// A record-only query or a TTFT reader's that failed is counted under its
	// own type, so an operator can tell its failures from the scaling
	// queries'. The scaling queries' own failures are counted where they are
	// read, as before.
	if recording || ttftRead {
		typeOf := make(map[string]string)
		for _, q := range buildEngineQueryList(engines, ttftReadEngineSpecificQueries, ttftReadAgnosticQueries) {
			typeOf[q] = constants.QueryTypeTTFTInputs
		}
		for _, q := range buildEngineQueryList(engines, recordOnlyEngineSpecificQueries, recordOnlyAgnosticQueries) {
			typeOf[q] = constants.QueryTypeLearnerRecording
		}
		countFailedIssued(issued, results, func(q string) string { return typeOf[q] })
	}

	// Re-key engine-specific results under their logical query names so the per-pod
	// processing (extractPodMetrics) is engine-agnostic. For SGLang-only models this renames the
	// "sglang/<query>" results to "<query>"; for mixed-engine models it concatenates
	// the per-engine series. The structural cache-config difference is handled by a
	// dedicated SGLang pass after the vLLM cache-config block, in extractPodMetrics.
	mergeEngineResults(results, engines, engineSpecificReplicaQueries)

	// Take this model's slice of the namespace-wide series. Everything from here
	// on operates on model-scoped results, as it did when the model was a PromQL
	// matcher.
	filterResultsToModel(results, engineSpecificReplicaQueries, modelID)
	filterResultsToModel(results, capacityQueueQueries, modelID)
	filterResultsToModel(results, ttftAgnosticQueries, modelID)

	return results, engines, nil
}

// CollectSchedulerQueueMetrics collects model-level queue metrics from the
// llm-d inference scheduler flow control layer. These metrics are not per-pod
// but per-model, representing requests queued upstream before reaching the engine.
// Returns nil (not an error) when flow control metrics are unavailable.
//
// The two queries take no parameters: one cluster-wide execution of each
// covers every model the controller manages, and this picks its own series out
// of the result by model AND namespace (eppSeriesInNamespace). The namespace is
// the one Prometheus stamps on the EPP's series at scrape time, the same label
// the arrival-rate query matches on. Before it was kept, two namespaces serving
// the same model name read the SUM of both routers' queues: a lightly loaded
// model was charged its saturated twin's queue as its own demand.
func (c *ReplicaMetricsCollector) CollectSchedulerQueueMetrics(
	ctx context.Context,
	modelID, namespace string,
) *domain.SchedulerQueueMetrics {
	logger := ctrl.LoggerFrom(ctx)

	queries := []string{
		registration.QuerySchedulerQueueSize,
		registration.QuerySchedulerQueueBytes,
	}

	results, err := c.refreshShared(ctx, queries, nil)
	if err != nil {
		logger.V(logging.DEBUG).Info("Scheduler queue metrics unavailable",
			"modelID", modelID, "namespace", namespace, "error", err)
		return nil
	}

	var queueSize, queueBytes int64
	hasData := false
	// Series of this model that belong to another namespace's router. Counted
	// only to say so: an EPP deployed outside its model's namespace lands here.
	elsewhere := 0

	sum := func(name string, into *int64) {
		result := results[name]
		if result == nil || result.HasError() {
			return
		}
		for _, value := range result.Values {
			if eppSeriesModel(value.Labels) != modelID {
				continue
			}
			if !eppSeriesInNamespace(value.Labels, namespace) {
				elsewhere++
				continue
			}
			// Presence does not depend on finiteness. A non-nil result is
			// what AnalyzerInput.FlowControlSeries reads as "the router
			// publishes its flow-control queue series", which is how the
			// slo analyzer infers the gate is on; a NaN series is still a
			// published series. Only the sum skips it, by Finite rather
			// than the raw value, since the Prometheus source stores a NaN
			// as 0 with a flag set.
			hasData = true
			if value.Finite() {
				*into += int64(value.Value)
			}
		}
	}
	sum(registration.QuerySchedulerQueueSize, &queueSize)
	sum(registration.QuerySchedulerQueueBytes, &queueBytes)

	if !hasData {
		if elsewhere > 0 {
			logger.V(logging.DEBUG).Info("Scheduler queue series for this model exist only in other namespaces; reading none",
				"modelID", modelID, "namespace", namespace, "otherNamespaceSeries", elsewhere)
		}
		return nil
	}

	logger.V(logging.DEBUG).Info("Collected scheduler queue metrics",
		"modelID", modelID,
		"namespace", namespace,
		"queueSize", queueSize,
		"queueBytes", queueBytes)

	return &domain.SchedulerQueueMetrics{
		QueueSize:  queueSize,
		QueueBytes: queueBytes,
	}
}

// CollectModelArrivalRate collects the model-level request arrival rate (req/s)
// from the llm-d inference scheduler. It sums the source metric across the whole
// model with no pod_name/port labels to reconcile against vLLM's per-instance
// metrics — which is exactly why the per-instance form of this query no longer
// exists. Returns 0 (not an error) when the metric is unavailable.
func (c *ReplicaMetricsCollector) CollectModelArrivalRate(
	ctx context.Context,
	modelID, namespace string,
) float64 {
	logger := ctrl.LoggerFrom(ctx)

	params := map[string]string{
		source.ParamNamespace: namespace,
	}

	arrivalType := func(string) string { return constants.QueryTypeArrivalRate }
	results, issued, err := c.refreshSharedIssued(ctx, []string{registration.QueryModelArrivalRate}, params)
	if err != nil {
		// Categorize rather than swallow: a broken or misconfigured arrival query and
		// genuine zero traffic both surface here as a zero rate, but only the former is a
		// fault. Record the categorized reason as a collection error so an operator can
		// tell the two apart (a nonzero arrival_rate error counter means a query problem,
		// not idle traffic). Demand still falls back to 0 — zero only ever permits
		// scale-down, gated by the multi-analyzer live-consensus veto.
		reason := countRefreshFailure(issued, err, arrivalType)
		logger.V(logging.DEBUG).Info("Model arrival rate unavailable",
			"modelID", modelID, "namespace", namespace, "reason", reason, "error", err)
		return 0
	}

	// A failed result is counted by the call that sent the query, not again
	// by every model in the namespace that reads it from the cycle's memo:
	// counted per model, one failure read as N.
	countFailedIssued(issued, results, arrivalType)
	result := results[registration.QueryModelArrivalRate]
	if result == nil {
		return 0
	}
	if result.HasError() {
		logger.V(logging.DEBUG).Info("Model arrival rate result carried an error",
			"modelID", modelID, "namespace", namespace,
			"reason", prometheus.CategorizePrometheusError(result.Error))
		return 0
	}

	// Strictly target_model_name, with no model_name fallback — see the note on
	// QueryModelArrivalRate. The series for other models in this namespace are
	// skipped here rather than in PromQL. A NaN or infinite series is skipped
	// by Finite: the Prometheus source stores it as 0 with a flag set, so a
	// check of the raw value read it as a series of rate 0.
	var arrivalRate float64
	series := 0
	for _, value := range result.Values {
		if value.Labels[seriesTargetModelLabel] != modelID {
			continue
		}
		if value.Finite() && value.Value >= 0 {
			arrivalRate += value.Value
			series++
		}
	}

	logger.V(logging.DEBUG).Info("Collected model arrival rate",
		"modelID", modelID, "namespace", namespace, "arrivalRate", arrivalRate, "series", series)

	return arrivalRate
}
