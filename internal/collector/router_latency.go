package collector

import (
	"context"

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
// every model every cycle, and it adds two more, the router's TPOT histogram
// and the flow-control wait histogram, which only the recording reads.
func (c *ReplicaMetricsCollector) CollectRouterLatency(
	ctx context.Context,
	modelID, namespace string,
) *domain.RouterLatency {
	logger := ctrl.LoggerFrom(ctx)
	queries := []string{
		registration.QuerySLORouterTTFTBuckets,
		registration.QuerySLORouterTPOTSum,
		registration.QuerySLORouterTPOTCount,
	}
	if c.RecordsTTFTInputs() || c.readsTTFTInputs(namespace) {
		queries = append(queries, registration.QuerySLORouterTTFTSum, registration.QuerySLORouterTTFTCount)
	}
	if c.RecordsTTFTInputs() {
		queries = append(queries, registration.QuerySLORouterTPOTBuckets, registration.QueryFlowControlWaitBuckets)
	}
	results, issued, err := c.refreshSharedIssued(ctx, queries, map[string]string{source.ParamNamespace: namespace})
	if err != nil {
		// Counted, not only logged: an absent latency series reads the same
		// whether the router is unscraped or Prometheus is down, and the
		// collection-error counter is what tells the two apart.
		reason := countRefreshFailure(issued, err, routerQueryType)
		logger.V(logging.DEBUG).Info("Router latency unavailable",
			"modelID", modelID, "namespace", namespace, "reason", reason, "error", err)
		return nil
	}

	// The Prometheus source reports a failed query on its result, not as an
	// error from the refresh, so each result is checked as the arrival-rate
	// path checks its own: counted by the call that sent it, and not again
	// by every model that reads it from the cycle's memo.
	countFailedIssued(issued, results, routerQueryType)
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
	// The two histograms ride along for a recording, read by nothing yet.
	// Either one keeps the struct, so the recording holds it; the slo
	// analyzer judges only what RouterLatency.Judgeable says is a reading,
	// so it sees the same nil it always did.
	out.TPOTBuckets = routerBuckets(results[registration.QuerySLORouterTPOTBuckets], modelID)
	out.FlowControlWaitBuckets = routerBuckets(results[registration.QueryFlowControlWaitBuckets], modelID)
	if !out.Judgeable() && out.TPOTBuckets == nil && out.FlowControlWaitBuckets == nil {
		return nil
	}
	return out
}

// routerQueryType is the query_type a failed router query is counted under:
// the slo analyzer's three under router_latency, as before they were
// recorded, the TTFT pricing's two under ttft_inputs with its other inputs, and
// the two only a recording reads under learner_recording.
func routerQueryType(query string) string {
	switch query {
	case registration.QuerySLORouterTPOTBuckets, registration.QueryFlowControlWaitBuckets:
		return constants.QueryTypeLearnerRecording
	case registration.QuerySLORouterTTFTSum, registration.QuerySLORouterTTFTCount:
		return constants.QueryTypeTTFTInputs
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
	if result == nil || result.HasError() {
		return nil
	}
	var out []source.MetricValue
	for _, v := range result.Values {
		if eppSeriesModel(v.Labels) != modelID {
			continue
		}
		if !v.Finite() || v.Value < 0 {
			continue
		}
		out = append(out, v)
	}
	return out
}
