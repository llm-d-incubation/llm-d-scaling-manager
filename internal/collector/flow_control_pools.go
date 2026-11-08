package collector

import (
	"context"
	"sort"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/prometheus"
)

// poolLabel is the label the router's flow-control series name their
// InferencePool by.
const poolLabel = "inference_pool"

// CollectFlowControlPools collects the flow-control layer of each
// InferencePool that serves modelID alone in namespace: the saturation by
// stage and the outcome rates, over registration.FlowControlWindow. Recorded
// for the TTFT model and read by nothing yet, so it issues nothing and
// returns nil unless a learner recording is on (SetRecordTTFTInputs). The
// wait histogram carries the model, and is read per model with the router
// latency (RouterLatency.FlowControlWaitBuckets).
//
// effective is the stage the gate reads: the larger of prefill and decode
// (llm-d-inference-scheduler, flow_control_pool_saturation). A pool with no
// endpoint reads 1.0 on its effective stage, so a 1.0 is either a full pool
// or an empty one.
//
// A pool is joined to the model when the flow-control queue gauge names the
// pool with this model and no other (registration.QueryFlowControlPoolModels).
// A pool shared by several models is joined to none: its series carry no
// model, and a wait or saturation charged to the wrong model is worse than
// none. Returns nil when no pool is joined, which is also what flow control
// switched off, or a failed query, reads as: absent, never zeros.
//
// The result is in pool-name order, the names dropped: a recording holds
// numbers only.
func (c *ReplicaMetricsCollector) CollectFlowControlPools(
	ctx context.Context,
	modelID, namespace string,
) []domain.FlowControlPool {
	if !c.RecordsTTFTInputs() {
		return nil
	}
	logger := ctrl.LoggerFrom(ctx)
	queries := []string{
		registration.QueryFlowControlPoolModels,
		registration.QueryFlowControlPoolSaturation,
		registration.QueryFlowControlOutcomes,
	}
	recording := func(string) string { return constants.QueryTypeLearnerRecording }
	results, issued, err := c.refreshSharedIssued(ctx, queries, map[string]string{source.ParamNamespace: namespace})
	if err != nil {
		reason := countRefreshFailure(issued, err, recording)
		logger.V(logging.DEBUG).Info("Flow-control pool series unavailable",
			"modelID", modelID, "namespace", namespace, "reason", reason, "error", err)
		return nil
	}
	// Counted once, by the call that sent the query, not by every model
	// that reads it from the cycle's memo.
	countFailedIssued(issued, results, recording)
	for _, q := range queries {
		if r := results[q]; r != nil && r.HasError() {
			logger.V(logging.DEBUG).Info("Flow-control pool query failed",
				"modelID", modelID, "namespace", namespace, "query", q,
				"reason", prometheus.CategorizePrometheusError(r.Error))
		}
	}

	pools := poolsServing(results[registration.QueryFlowControlPoolModels], modelID)
	if len(pools) == 0 {
		return nil
	}
	byPool := make(map[string]*domain.FlowControlPool, len(pools))
	for _, p := range pools {
		byPool[p] = &domain.FlowControlPool{WindowSeconds: registration.FlowControlWindowSeconds}
	}

	for _, v := range finiteValues(results[registration.QueryFlowControlPoolSaturation]) {
		p := byPool[v.Labels[poolLabel]]
		if p == nil {
			continue
		}
		var value *float64
		var known *bool
		switch strings.ToLower(v.Labels["stage"]) {
		case "effective":
			value, known = &p.EffectiveSaturation, &p.EffectiveSaturationKnown
		case "prefill":
			value, known = &p.PrefillSaturation, &p.PrefillSaturationKnown
		case "decode":
			value, known = &p.DecodeSaturation, &p.DecodeSaturationKnown
		default:
			// A stage the router does not export today: not one of the
			// three, so not read as any of them.
			continue
		}
		if !*known || v.Value > *value {
			*value = v.Value
		}
		*known = true
	}

	for _, v := range finiteValues(results[registration.QueryFlowControlOutcomes]) {
		p := byPool[v.Labels[poolLabel]]
		if p == nil || v.Value < 0 {
			continue
		}
		outcome := strings.ToLower(v.Labels["outcome"])
		switch {
		case strings.HasPrefix(outcome, "dispatch"):
			p.DispatchedRate += v.Value
		case strings.HasPrefix(outcome, "reject"):
			p.RejectedRate += v.Value
		case strings.HasPrefix(outcome, "evict"):
			p.EvictedRate += v.Value
		default:
			p.OtherOutcomeRate += v.Value
		}
		p.OutcomesKnown = true
	}

	out := make([]domain.FlowControlPool, 0, len(pools))
	for _, p := range pools {
		out = append(out, *byPool[p])
	}
	return out
}

// poolsServing is the pools the queue gauge names with modelID and no other
// model, sorted. A series resolves to a model as the scheduler-queue ones do
// (eppSeriesModel); one that resolves to none names no model and joins
// nothing. A failed or missing result serves none.
func poolsServing(result *source.MetricResult, modelID string) []string {
	if result == nil || result.HasError() {
		return nil
	}
	models := map[string]map[string]bool{}
	for _, v := range result.Values {
		pool, model := v.Labels[poolLabel], eppSeriesModel(v.Labels)
		if pool == "" || model == "" {
			continue
		}
		if models[pool] == nil {
			models[pool] = map[string]bool{}
		}
		models[pool][model] = true
	}
	var out []string
	for pool, ms := range models {
		if len(ms) == 1 && ms[modelID] {
			out = append(out, pool)
		}
	}
	sort.Strings(out)
	return out
}

// finiteValues is a result's finite samples; none for a nil or failed one.
func finiteValues(result *source.MetricResult) []source.MetricValue {
	if result == nil || result.HasError() {
		return nil
	}
	out := make([]source.MetricValue, 0, len(result.Values))
	for _, v := range result.Values {
		if v.Finite() {
			out = append(out, v)
		}
	}
	return out
}
