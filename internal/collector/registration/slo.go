package registration

import (
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
)

// The router-side latency the slo analyzer judges an SLO on. See
// docs/proposals/slo-tiers.md, "Which latency, measured where".
const (
	// QuerySLORouterTTFTBuckets is the router's TTFT histogram, as per-second
	// bucket rates, per model.
	QuerySLORouterTTFTBuckets = "slo_router_ttft_buckets"
	// QuerySLORouterTTFTSum and QuerySLORouterTTFTCount are the rates of that
	// histogram's _sum and _count, per model: the router's mean TTFT, which
	// the TTFT model's hop term is read from (prefill.DecodeTermWindow).
	QuerySLORouterTTFTSum   = "slo_router_ttft_sum"
	QuerySLORouterTTFTCount = "slo_router_ttft_count"
	// QuerySLORouterTPOTSum and QuerySLORouterTPOTCount are the rates of the
	// router's streaming time-per-output-token histogram's _sum and _count.
	QuerySLORouterTPOTSum   = "slo_router_tpot_sum"
	QuerySLORouterTPOTCount = "slo_router_tpot_count"
	// QuerySLORouterTPOTBuckets is that histogram's per-second bucket rates,
	// per model: the distribution of per-request TPOT behind the mean.
	// Recorded for the TTFT and TPOT models and read by nothing yet
	// (docs/proposals/analyzer-learner.md, 2.9), so issued only while a
	// learner recording is on.
	QuerySLORouterTPOTBuckets = "slo_router_tpot_buckets"
	// QuerySLORouterProcessingBuckets is the router's request-processing
	// histogram, as per-second bucket rates, and QuerySLORouterProcessingSum
	// and QuerySLORouterProcessingCount the rates of its _sum and _count:
	// the router's own time inside its TTFT. The series carries no model
	// label, so these are per namespace (the collector joins them to a
	// model). Recorded for the capacity planner's experimental TTFT model,
	// so issued only while a learner recording is on.
	QuerySLORouterProcessingBuckets = "slo_router_processing_buckets"
	QuerySLORouterProcessingSum     = "slo_router_processing_sum"
	QuerySLORouterProcessingCount   = "slo_router_processing_count"
	// QuerySLORouterRenderSum and QuerySLORouterRenderCount are the rates
	// of the router's token-producer render histogram's _sum and _count:
	// the router's call to a prefill pod's /render handler, inside its
	// request processing. No model label, so per namespace, joined to a
	// model as the processing series are. Recorded for the capacity
	// planner's remainder server mode, so issued only while a learner
	// recording is on.
	QuerySLORouterRenderSum   = "slo_router_render_sum"
	QuerySLORouterRenderCount = "slo_router_render_count"
	// QuerySLORouterPrefixPredicted and QuerySLORouterPrefixBestAvailable
	// are the rates of the router prefix producer's cached tokens predicted
	// on the endpoint it chose and available on the best endpoint, per
	// producer (plugin_name, endpoint_role): the affinity the routing gave up,
	// which the capacity planner reads the prefix duplication across prefill
	// pods from (prefill.DuplicationOfAffinityLoss). The series carry no
	// model, so these are per namespace, joined to a model as the processing
	// ones are. Issued only while a learner recording is on.
	QuerySLORouterPrefixPredicted     = "slo_router_prefix_predicted"
	QuerySLORouterPrefixBestAvailable = "slo_router_prefix_best_available"
	// QuerySLORouterPrefixBestPredicted, QuerySLORouterPrefixPrompt and
	// QuerySLORouterPrefixRequests are the rest of the prefix producer's
	// counts, per router Pod and producer: the cached tokens on the best
	// endpoint the filters kept, the prompt tokens, and the requests (the
	// predicted histogram's _count). With the two above they are
	// domain.RouterLatency.PrefixProducers. Issued only while a learner
	// recording is on.
	QuerySLORouterPrefixBestPredicted = "slo_router_prefix_best_predicted"
	QuerySLORouterPrefixPrompt        = "slo_router_prefix_prompt"
	QuerySLORouterPrefixRequests      = "slo_router_prefix_requests"

	// QuerySLORouterEndpointPlacements is the requests a second each router
	// Pod placed on each model-server Pod, per model; the serving router
	// Pods are read from it. QuerySLORouterEndpointQueue is each endpoint's
	// waiting queue as each router Pod read it, and
	// QuerySLORouterEndpointInFlightRequests and
	// QuerySLORouterEndpointInFlightTokens what each router Pod holds in
	// flight on each endpoint, averaged over the window. Together they are
	// domain.RouterLatency.Endpoints. Issued only while a learner recording
	// is on.
	QuerySLORouterEndpointPlacements       = "slo_router_endpoint_placements"
	QuerySLORouterEndpointQueue            = "slo_router_endpoint_queue"
	QuerySLORouterEndpointInFlightRequests = "slo_router_endpoint_inflight_requests"
	QuerySLORouterEndpointInFlightTokens   = "slo_router_endpoint_inflight_tokens"

	// SLORouterWindow is the rate window of the queries above. Five
	// minutes, not the one minute the engine queries use: a percentile and an
	// attainment need enough requests to mean anything, and the analyzer
	// refuses to report either below a sample minimum.
	SLORouterWindow        = "5m"
	SLORouterWindowSeconds = 300.0
)

// Grouped by model_name and target_model_name, and resolved to one model in Go
// the way the scheduler-queue queries are (target_model_name, else
// model_name). The EPP also exports per-flow series that carry neither; they
// group under an empty model and are dropped there.
//
// Only the current llm_d_epp_* names. The router's predecessor published
// inference_objective_request_ttft_seconds with no `streaming` label, and for a
// non-streaming response the router's TTFT is the whole request duration, so
// reading it would mix two different quantities. A router too old to publish
// the current name leaves the SLO unjudged, which the analyzer reports.
//
// Rate inside the sum, never the other way round, so a counter reset on one
// EPP replica cannot read as a burst on the model.
const (
	sloRouterTTFTBuckets = `sum by (model_name, target_model_name, le) (` +
		`rate(llm_d_epp_request_ttft_seconds_bucket{namespace="{{.namespace}}",streaming="true"}[` + SLORouterWindow + `]))`
	sloRouterTTFTSum = `sum by (model_name, target_model_name) (` +
		`rate(llm_d_epp_request_ttft_seconds_sum{namespace="{{.namespace}}",streaming="true"}[` + SLORouterWindow + `]))`
	sloRouterTTFTCount = `sum by (model_name, target_model_name) (` +
		`rate(llm_d_epp_request_ttft_seconds_count{namespace="{{.namespace}}",streaming="true"}[` + SLORouterWindow + `]))`
	sloRouterTPOTSum = `sum by (model_name, target_model_name) (` +
		`rate(llm_d_epp_request_streaming_tpot_seconds_sum{namespace="{{.namespace}}"}[` + SLORouterWindow + `]))`
	sloRouterTPOTCount = `sum by (model_name, target_model_name) (` +
		`rate(llm_d_epp_request_streaming_tpot_seconds_count{namespace="{{.namespace}}"}[` + SLORouterWindow + `]))`
	sloRouterTPOTBuckets = `sum by (model_name, target_model_name, le) (` +
		`rate(llm_d_epp_request_streaming_tpot_seconds_bucket{namespace="{{.namespace}}"}[` + SLORouterWindow + `]))`
	sloRouterProcessingBuckets = `sum by (le) (` +
		`rate(llm_d_epp_request_processing_duration_seconds_bucket{namespace="{{.namespace}}"}[` + SLORouterWindow + `]))`
	sloRouterProcessingSum = `sum(` +
		`rate(llm_d_epp_request_processing_duration_seconds_sum{namespace="{{.namespace}}"}[` + SLORouterWindow + `]))`
	sloRouterProcessingCount = `sum(` +
		`rate(llm_d_epp_request_processing_duration_seconds_count{namespace="{{.namespace}}"}[` + SLORouterWindow + `]))`
	sloRouterRenderSum = `sum(` +
		`rate(llm_d_epp_token_producer_render_duration_seconds_sum{namespace="{{.namespace}}"}[` + SLORouterWindow + `]))`
	sloRouterRenderCount = `sum(` +
		`rate(llm_d_epp_token_producer_render_duration_seconds_count{namespace="{{.namespace}}"}[` + SLORouterWindow + `]))`
	// A P/D router runs one prefix producer per scheduling profile, and each
	// counts every prompt: the producers are kept apart, never summed. Kept
	// apart per router Pod (the scrape's pod label) too, so the series of
	// the router Pods that serve a model can be told from another
	// replica's.
	sloRouterPrefixPredicted = `sum by (pod, plugin_name, endpoint_role) (` +
		`rate(llm_d_epp_prefix_predicted_cached_tokens_sum{namespace="{{.namespace}}"}[` + SLORouterWindow + `]))`
	sloRouterPrefixBestAvailable = `sum by (pod, plugin_name, endpoint_role) (` +
		`rate(llm_d_epp_prefix_best_available_cached_tokens_sum{namespace="{{.namespace}}"}[` + SLORouterWindow + `]))`
	sloRouterPrefixBestPredicted = `sum by (pod, plugin_name, endpoint_role) (` +
		`rate(llm_d_epp_prefix_best_predicted_cached_tokens_sum{namespace="{{.namespace}}"}[` + SLORouterWindow + `]))`
	sloRouterPrefixPrompt = `sum by (pod, plugin_name, endpoint_role) (` +
		`rate(llm_d_epp_prefix_prompt_tokens_sum{namespace="{{.namespace}}"}[` + SLORouterWindow + `]))`
	sloRouterPrefixRequests = `sum by (pod, plugin_name, endpoint_role) (` +
		`rate(llm_d_epp_prefix_predicted_cached_tokens_count{namespace="{{.namespace}}"}[` + SLORouterWindow + `]))`

	// The router's per-endpoint series (llm-d router v1.0.0). The placement
	// counter names the model-server Pod in endpoint_name and the model in
	// target_model_name; a placement with no endpoint (endpoint_name "") is
	// not one. The two gauges name an endpoint, Pod and rank, as
	// <pod>-rank-<n>: the queue in model_server_endpoint, the in-flight load
	// in endpoint_name. The in-flight gauges are summed over fairness_id and
	// priority within a producer, and two producers, which track the same
	// requests, are not summed. Each router Pod is kept apart.
	sloRouterEndpointPlacements = `sum by (pod, target_model_name, endpoint_name) (` +
		`rate(llm_d_epp_scheduler_attempts_total{namespace="{{.namespace}}",status="success",endpoint_name!=""}[` + SLORouterWindow + `]))`
	sloRouterEndpointQueue = `sum by (pod, model_server_endpoint) (` +
		`avg_over_time(llm_d_epp_per_endpoint_queue_size{namespace="{{.namespace}}"}[` + SLORouterWindow + `]))`
	sloRouterEndpointInFlightRequests = `max by (pod, endpoint_name) (sum by (pod, endpoint_name, producer_name) (` +
		`avg_over_time(llm_d_epp_inflight_requests{namespace="{{.namespace}}"}[` + SLORouterWindow + `])))`
	sloRouterEndpointInFlightTokens = `max by (pod, endpoint_name) (sum by (pod, endpoint_name, producer_name) (` +
		`avg_over_time(llm_d_epp_inflight_tokens{namespace="{{.namespace}}"}[` + SLORouterWindow + `])))`
)

// RegisterSLOQueries registers the router latency queries the slo analyzer
// reads. They run for a model whose policy lists that analyzer; while a
// learner recording is on, the engine also collects them every cycle for
// every model, into AnalyzerInput.RouterLatency, and the slo analyzer reads
// that cycle's results from the collector's memo.
func RegisterSLOQueries(sourceRegistry *source.SourceRegistry) {
	metricsSource := sourceRegistry.Get("prometheus")
	if metricsSource == nil {
		ctrl.Log.V(logging.DEBUG).Info("Prometheus source not registered, skipping SLO query registration")
		return
	}
	registry := metricsSource.QueryList()
	registry.MustRegister(source.QueryTemplate{
		Name:        QuerySLORouterTTFTBuckets,
		Type:        source.QueryTypePromQL,
		Template:    sloRouterTTFTBuckets,
		Params:      []string{source.ParamNamespace},
		Description: "Router (EPP) TTFT histogram bucket rates per model, streaming requests only; includes the flow-control queue",
	})
	registry.MustRegister(source.QueryTemplate{
		Name:        QuerySLORouterTTFTSum,
		Type:        source.QueryTypePromQL,
		Template:    sloRouterTTFTSum,
		Params:      []string{source.ParamNamespace},
		Description: "Router (EPP) TTFT histogram _sum rate per model, streaming requests only",
	})
	registry.MustRegister(source.QueryTemplate{
		Name:        QuerySLORouterTTFTCount,
		Type:        source.QueryTypePromQL,
		Template:    sloRouterTTFTCount,
		Params:      []string{source.ParamNamespace},
		Description: "Router (EPP) TTFT histogram _count rate per model, streaming requests only",
	})
	registry.MustRegister(source.QueryTemplate{
		Name:        QuerySLORouterTPOTSum,
		Type:        source.QueryTypePromQL,
		Template:    sloRouterTPOTSum,
		Params:      []string{source.ParamNamespace},
		Description: "Router (EPP) streaming TPOT histogram _sum rate per model",
	})
	registry.MustRegister(source.QueryTemplate{
		Name:        QuerySLORouterTPOTCount,
		Type:        source.QueryTypePromQL,
		Template:    sloRouterTPOTCount,
		Params:      []string{source.ParamNamespace},
		Description: "Router (EPP) streaming TPOT histogram _count rate per model",
	})
	registry.MustRegister(source.QueryTemplate{
		Name:        QuerySLORouterTPOTBuckets,
		Type:        source.QueryTypePromQL,
		Template:    sloRouterTPOTBuckets,
		Params:      []string{source.ParamNamespace},
		Description: "Router (EPP) streaming TPOT histogram bucket rates per model",
	})
	registry.MustRegister(source.QueryTemplate{
		Name:        QuerySLORouterProcessingBuckets,
		Type:        source.QueryTypePromQL,
		Template:    sloRouterProcessingBuckets,
		Params:      []string{source.ParamNamespace},
		Description: "Router (EPP) request-processing histogram bucket rates per namespace; the series carries no model",
	})
	registry.MustRegister(source.QueryTemplate{
		Name:        QuerySLORouterProcessingSum,
		Type:        source.QueryTypePromQL,
		Template:    sloRouterProcessingSum,
		Params:      []string{source.ParamNamespace},
		Description: "Router (EPP) request-processing histogram _sum rate per namespace",
	})
	registry.MustRegister(source.QueryTemplate{
		Name:        QuerySLORouterProcessingCount,
		Type:        source.QueryTypePromQL,
		Template:    sloRouterProcessingCount,
		Params:      []string{source.ParamNamespace},
		Description: "Router (EPP) request-processing histogram _count rate per namespace",
	})
	registry.MustRegister(source.QueryTemplate{
		Name:        QuerySLORouterRenderSum,
		Type:        source.QueryTypePromQL,
		Template:    sloRouterRenderSum,
		Params:      []string{source.ParamNamespace},
		Description: "Router (EPP) token-producer render histogram _sum rate per namespace; the series carries no model",
	})
	registry.MustRegister(source.QueryTemplate{
		Name:        QuerySLORouterRenderCount,
		Type:        source.QueryTypePromQL,
		Template:    sloRouterRenderCount,
		Params:      []string{source.ParamNamespace},
		Description: "Router (EPP) token-producer render histogram _count rate per namespace",
	})
	registry.MustRegister(source.QueryTemplate{
		Name:        QuerySLORouterPrefixPredicted,
		Type:        source.QueryTypePromQL,
		Template:    sloRouterPrefixPredicted,
		Params:      []string{source.ParamNamespace},
		Description: "Router (EPP) prefix producer's cached tokens predicted on the chosen endpoint, rate per producer and namespace",
	})
	registry.MustRegister(source.QueryTemplate{
		Name:        QuerySLORouterPrefixBestAvailable,
		Type:        source.QueryTypePromQL,
		Template:    sloRouterPrefixBestAvailable,
		Params:      []string{source.ParamNamespace},
		Description: "Router (EPP) prefix producer's cached tokens available on the best endpoint, rate per producer and namespace",
	})
	for _, q := range []source.QueryTemplate{
		{Name: QuerySLORouterPrefixBestPredicted, Template: sloRouterPrefixBestPredicted,
			Description: "Router (EPP) prefix producer's cached tokens on the best endpoint the filters kept, rate per router Pod and producer"},
		{Name: QuerySLORouterPrefixPrompt, Template: sloRouterPrefixPrompt,
			Description: "Router (EPP) prefix producer's prompt tokens, rate per router Pod and producer"},
		{Name: QuerySLORouterPrefixRequests, Template: sloRouterPrefixRequests,
			Description: "Router (EPP) prefix producer's requests, rate per router Pod and producer"},
		{Name: QuerySLORouterEndpointPlacements, Template: sloRouterEndpointPlacements,
			Description: "Router (EPP) requests placed per model-server Pod, rate per router Pod and model"},
		{Name: QuerySLORouterEndpointQueue, Template: sloRouterEndpointQueue,
			Description: "Router (EPP) view of each endpoint's waiting queue, mean over the window, per router Pod"},
		{Name: QuerySLORouterEndpointInFlightRequests, Template: sloRouterEndpointInFlightRequests,
			Description: "Router (EPP) in-flight requests per endpoint, mean over the window, per router Pod"},
		{Name: QuerySLORouterEndpointInFlightTokens, Template: sloRouterEndpointInFlightTokens,
			Description: "Router (EPP) in-flight uncached prompt tokens per endpoint, mean over the window, per router Pod"},
	} {
		q.Type = source.QueryTypePromQL
		q.Params = []string{source.ParamNamespace}
		registry.MustRegister(q)
	}
}
