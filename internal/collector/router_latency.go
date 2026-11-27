package collector

import (
	"context"
	"slices"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/prometheus"
)

// CollectRouterLatency collects one model's request latency as the router
// measured it: the TTFT histogram and the streaming TPOT mean, over
// registration.SLORouterWindow. Returns nil (not an error) when the router
// exports nothing for the model or the queries fail; the caller reports that
// as an unjudged SLO rather than a met or a missed one.
//
// The slo analyzer calls it for each model whose policy lists it, and reads
// three queries: the TTFT buckets and the TPOT _sum and _count. In a namespace
// the TTFT pricing reads (ReadTTFTInputsIn), and while recording, it adds the
// TTFT _sum and _count, the decode term's hop, and nothing else reads them. While a
// learner recording is on (SetRecordTTFTInputs) the engine also calls it for
// every model every cycle, and it adds the router's TPOT histogram, the
// flow-control wait histogram, the router's request-processing histogram
// with its _sum and _count, its render histogram's _sum and _count, and the
// router's per-endpoint and per-producer series (routerEndpointQueries),
// which only the recording reads.
func (c *ReplicaMetricsCollector) CollectRouterLatency(
	ctx context.Context,
	modelID, namespace string,
) *domain.RouterLatency {
	queries := []string{
		registration.QuerySLORouterTTFTBuckets,
		registration.QuerySLORouterTPOTSum,
		registration.QuerySLORouterTPOTCount,
	}
	if c.RecordsTTFTInputs() || c.readsTTFTInputs(namespace) {
		queries = append(queries, registration.QuerySLORouterTTFTSum, registration.QuerySLORouterTTFTCount)
	}
	if c.RecordsTTFTInputs() {
		queries = append(queries, registration.QuerySLORouterTPOTBuckets, registration.QueryFlowControlWaitBuckets,
			registration.QuerySLORouterProcessingBuckets, registration.QuerySLORouterProcessingSum,
			registration.QuerySLORouterProcessingCount, registration.QuerySLORouterRenderSum,
			registration.QuerySLORouterRenderCount, registration.QuerySLORouterPrefixPredicted,
			registration.QuerySLORouterPrefixBestAvailable)
		queries = append(queries, routerEndpointQueries...)
	}
	return c.collectRouterLatency(ctx, modelID, namespace, queries, routerQueryType)
}

// CollectPredictionLatency is the router's latency as the slo analyzer's v2
// models fit on it (docs/proposals/predicted-latency-in-controller.md): the
// slo analyzer's three queries, the TTFT _sum and _count, and the TPOT
// histogram, through the cycle's memo, so a model the slo analyzer or a
// recording reads too costs nothing more. It is its own reading, never
// handed to an analyzer: CollectRouterLatency answers them as it did before.
// A failed query only it sent is counted under slo_model_inputs. Nil as
// CollectRouterLatency is nil.
func (c *ReplicaMetricsCollector) CollectPredictionLatency(
	ctx context.Context,
	modelID, namespace string,
) *domain.RouterLatency {
	queries := []string{
		registration.QuerySLORouterTTFTBuckets,
		registration.QuerySLORouterTPOTSum,
		registration.QuerySLORouterTPOTCount,
		registration.QuerySLORouterTTFTSum,
		registration.QuerySLORouterTTFTCount,
		registration.QuerySLORouterTPOTBuckets,
	}
	typeOf := func(q string) string {
		if q == registration.QuerySLORouterTPOTBuckets && !c.RecordsTTFTInputs() {
			return constants.QueryTypeSLOModelInputs
		}
		return routerQueryType(q)
	}
	return c.collectRouterLatency(ctx, modelID, namespace, queries, typeOf)
}

// collectRouterLatency issues queries for one model through the cycle's
// memo and reads the router's latency from what came back, a failure counted
// under typeOf's query_type.
func (c *ReplicaMetricsCollector) collectRouterLatency(
	ctx context.Context,
	modelID, namespace string,
	queries []string,
	typeOf func(string) string,
) *domain.RouterLatency {
	logger := ctrl.LoggerFrom(ctx)
	results, issued, err := c.refreshSharedIssued(ctx, queries, map[string]string{source.ParamNamespace: namespace})
	if err != nil {
		// Counted, not only logged: an absent latency series reads the same
		// whether the router is unscraped or Prometheus is down, and the
		// collection-error counter is what tells the two apart.
		reason := countRefreshFailure(issued, err, typeOf)
		logger.V(logging.DEBUG).Info("Router latency unavailable",
			"modelID", modelID, "namespace", namespace, "reason", reason, "error", err)
		return nil
	}

	// The Prometheus source reports a failed query on its result, not as an
	// error from the refresh, so each result is checked as the arrival-rate
	// path checks its own: counted by the call that sent it, and not again
	// by every model that reads it from the cycle's memo.
	countFailedIssued(issued, results, typeOf)
	for _, q := range queries {
		if r := results[q]; r != nil && r.HasError() {
			logger.V(logging.DEBUG).Info("Router latency query failed",
				"modelID", modelID, "namespace", namespace, "query", q,
				"reason", prometheus.CategorizePrometheusError(r.Error))
		}
	}

	out := &domain.RouterLatency{WindowSeconds: registration.SLORouterWindowSeconds}
	// One series per (model_name, target_model_name, le); a model reached
	// under both labels contributes to the same bucket. Sorted by bound: the
	// input is recorded every cycle, and a map's order would make two
	// identical cycles record different bytes.
	out.TTFTBuckets = routerBuckets(results[registration.QuerySLORouterTTFTBuckets], modelID)
	out.TTFTSumRate, out.TTFTCountRate = routerMeanRates(results[registration.QuerySLORouterTTFTSum],
		results[registration.QuerySLORouterTTFTCount], modelID)
	// The mean is sum over count, so the pair is one reading: with either
	// query failed it is none. Summing the half that came back published a
	// mean of 0 -- a met TPOT -- whenever the _sum query alone failed.
	out.TPOTSumRate, out.TPOTCountRate = routerMeanRates(results[registration.QuerySLORouterTPOTSum],
		results[registration.QuerySLORouterTPOTCount], modelID)
	// The recorded histograms ride along for a recording; only the capacity
	// planner reads the processing one. Any one keeps the struct, so the recording holds it; the slo
	// analyzer judges only what RouterLatency.Judgeable says is a reading,
	// so it sees the same nil it always did.
	out.TPOTBuckets = routerBuckets(results[registration.QuerySLORouterTPOTBuckets], modelID)
	out.FlowControlWaitBuckets = routerBuckets(results[registration.QueryFlowControlWaitBuckets], modelID)
	out.ProcessingBuckets, out.ProcessingSumRate, out.ProcessingCountRate = routerProcessing(results, modelID)
	out.RenderSumRate, out.RenderCountRate = routerRender(results, modelID)
	out.PrefixPredictedTokenRate, out.PrefixBestAvailableTokenRate = routerPrefixAffinity(results, modelID)
	out.Endpoints, out.PrefixProducers = routerEndpoints(results, modelID)
	if !out.Judgeable() && out.TPOTBuckets == nil && out.FlowControlWaitBuckets == nil && out.ProcessingBuckets == nil &&
		out.ProcessingCountRate == 0 && out.RenderCountRate == 0 && out.PrefixBestAvailableTokenRate == 0 &&
		out.Endpoints == nil && out.PrefixProducers == nil {
		return nil
	}
	return out
}

// routerRender is the router's render histogram's _sum and _count rates for
// modelID, joined as routerProcessing joins the processing series: the
// namespace's, given to modelID only where the router's TTFT names it alone.
// Zeros otherwise, or with either query failed.
func routerRender(results map[string]*source.MetricResult, modelID string) (sumRate, countRate float64) {
	if !onlyModel(results[registration.QuerySLORouterTTFTBuckets], modelID) {
		return 0, 0
	}
	return namespaceMeanRates(results[registration.QuerySLORouterRenderSum], results[registration.QuerySLORouterRenderCount])
}

// namespaceMeanRates is a router histogram's _sum and _count rates over
// every series, for a histogram that carries no model: one reading, both
// zero with either query failed or no time at all.
func namespaceMeanRates(sum, count *source.MetricResult) (sumRate, countRate float64) {
	if sum == nil || sum.HasError() || count == nil || count.HasError() {
		return 0, 0
	}
	for _, v := range readableValues(sum) {
		sumRate += v.Value
	}
	for _, v := range readableValues(count) {
		countRate += v.Value
	}
	if !(sumRate > 0) {
		return 0, 0
	}
	return sumRate, countRate
}

// routerProcessing is the router's request-processing histogram and its
// _sum and _count rates, for modelID. The series carry no model, so they are
// the namespace's, and they are modelID's only where the router's TTFT
// histogram names modelID and no other model in the namespace: with two
// models behind the namespace's routers, neither is given the other's time.
// Nil and zeros otherwise, or with the queries failed; the pair of rates as
// routerMeanRates reads it.
func routerProcessing(results map[string]*source.MetricResult, modelID string) (b []domain.LatencyBucket, sumRate, countRate float64) {
	if !onlyModel(results[registration.QuerySLORouterTTFTBuckets], modelID) {
		return nil, 0, 0
	}
	byBound := map[float64]float64{}
	for _, v := range readableValues(results[registration.QuerySLORouterProcessingBuckets]) {
		if le, ok := bucketBound(v); ok {
			byBound[le] += v.Value
		}
	}
	b = sortedBuckets[domain.LatencyBucket](byBound)
	sumRate, countRate = namespaceMeanRates(results[registration.QuerySLORouterProcessingSum],
		results[registration.QuerySLORouterProcessingCount])
	return b, sumRate, countRate
}

// routerPrefixAffinity is the router prefix producer's predicted and
// best-available cached token rates, for modelID, joined as
// routerProcessing joins its series. A P/D router runs one producer per
// profile and each counts every prompt, so the producers are not summed:
// the pair is the producer (plugin_name, endpoint_role) that predicted the
// most, the one routing pods whose caches hit. Zeros with either query
// failed, no producer that predicted any, or a best-available rate under the
// predicted one (the two would not be one reading).
func routerPrefixAffinity(results map[string]*source.MetricResult, modelID string) (predicted, best float64) {
	if !onlyModel(results[registration.QuerySLORouterTTFTBuckets], modelID) {
		return 0, 0
	}
	pr, ba := results[registration.QuerySLORouterPrefixPredicted], results[registration.QuerySLORouterPrefixBestAvailable]
	if pr == nil || pr.HasError() || ba == nil || ba.HasError() {
		return 0, 0
	}
	producer := func(l map[string]string) string { return l["plugin_name"] + "/" + l["endpoint_role"] }
	bestBy := map[string]float64{}
	for _, v := range readableValues(ba) {
		bestBy[producer(v.Labels)] += v.Value
	}
	predBy := map[string]float64{}
	for _, v := range readableValues(pr) {
		predBy[producer(v.Labels)] += v.Value
	}
	var pick string
	for p, r := range predBy {
		if r > predicted || (r == predicted && r > 0 && p < pick) {
			pick, predicted = p, r
		}
	}
	best = bestBy[pick]
	if !(predicted > 0) || !(best >= predicted) {
		return 0, 0
	}
	return predicted, best
}

// onlyModel says result, a router series grouped by model, names modelID
// and no other model.
func onlyModel(result *source.MetricResult, modelID string) bool {
	seen := false
	for _, v := range finiteValues(result) {
		switch eppSeriesModel(v.Labels) {
		case modelID:
			seen = true
		case "":
		default:
			return false
		}
	}
	return seen
}

// routerQueryType is the query_type a failed router query is counted under:
// the slo analyzer's three under router_latency, as before they were
// recorded, the TTFT pricing's two under ttft_inputs with its other inputs, and
// the two only a recording reads under learner_recording.
func routerQueryType(query string) string {
	switch query {
	case registration.QuerySLORouterTPOTBuckets, registration.QueryFlowControlWaitBuckets,
		registration.QuerySLORouterProcessingBuckets, registration.QuerySLORouterProcessingSum,
		registration.QuerySLORouterProcessingCount, registration.QuerySLORouterRenderSum,
		registration.QuerySLORouterRenderCount, registration.QuerySLORouterPrefixPredicted,
		registration.QuerySLORouterPrefixBestAvailable:
		return constants.QueryTypeLearnerRecording
	case registration.QuerySLORouterTTFTSum, registration.QuerySLORouterTTFTCount:
		return constants.QueryTypeTTFTInputs
	}
	if slices.Contains(routerEndpointQueries, query) {
		return constants.QueryTypeLearnerRecording
	}
	return constants.QueryTypeRouterLatency
}

// routerMeanRates is one model's _sum and _count rates of a router histogram,
// a model reached under both labels summed. Both zero with either query failed
// or no time at all, which is not a reading of requests.
func routerMeanRates(sum, count *source.MetricResult, modelID string) (sumRate, countRate float64) {
	if sum == nil || sum.HasError() || count == nil || count.HasError() {
		return 0, 0
	}
	for _, v := range modelValues(sum, modelID) {
		sumRate += v.Value
	}
	for _, v := range modelValues(count, modelID) {
		countRate += v.Value
	}
	if !(sumRate > 0) {
		return 0, 0
	}
	return sumRate, countRate
}

// routerBuckets is one model's bucket rates of a router histogram, a model
// reached under both labels summed into one bucket, sorted by bound. Nil when
// the query failed or no bucket of the model was readable.
func routerBuckets(result *source.MetricResult, modelID string) []domain.LatencyBucket {
	byBound := map[float64]float64{}
	for _, v := range modelValues(result, modelID) {
		if le, ok := bucketBound(v); ok {
			byBound[le] += v.Value
		}
	}
	return sortedBuckets[domain.LatencyBucket](byBound)
}

// modelValues returns the finite, non-negative values of result that belong to
// modelID, resolving each series' model the way the scheduler-queue metrics are
// resolved (eppSeriesModel). A nil or failed result has none.
//
// Finite is read from the sample's flags as well as its value: the
// Prometheus source stores a NaN or an infinity as 0 and flags it
// (source.MetricValue.Finite), so a check on the value alone read a NaN
// bucket as a bucket of rate 0.
func modelValues(result *source.MetricResult, modelID string) []source.MetricValue {
	var out []source.MetricValue
	for _, v := range readableValues(result) {
		if eppSeriesModel(v.Labels) == modelID {
			out = append(out, v)
		}
	}
	return out
}

// readableValues is the finite, non-negative values of result, as
// modelValues reads them, of every model; a nil or failed result has none.
func readableValues(result *source.MetricResult) []source.MetricValue {
	var out []source.MetricValue
	for _, v := range finiteValues(result) {
		if v.Value >= 0 {
			out = append(out, v)
		}
	}
	return out
}
