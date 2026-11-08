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
}
