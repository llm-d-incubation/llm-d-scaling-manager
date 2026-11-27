package registration

import (
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
)

// The per-pod series that say how long a request spends in the engine's API
// server outside the engine: the HTTP handler's time against the engine's
// own end-to-end time, and the /render calls the router makes to a prefill
// pod before it picks one. They are the capacity planner's router remainder
// by pod (whatif.RankMixtureOptions.RemainderServer). Recorded only, issued
// only while recording, vLLM only; a pod that does not publish one returns
// no series, and the row reads it as unknown.
//
// The HTTP series (http_request_duration_seconds, the API server's
// instrumentator) carry no model: they are read per instance and attached to
// the instances the model's own queries found, as cache_config_info is.
// vLLM's end-to-end latency carries model_name and is cut down to the model.
// All four are over the router latency's [5m], so a pod's frontend is the
// difference of two means over the same requests the router's remainder is
// read over.
const (
	// QueryAvgHTTPGenerationTime is the mean HTTP handler time of the
	// generation endpoints per instance (seconds, [5m]).
	QueryAvgHTTPGenerationTime = "avg_http_generation_time"
	// QueryAvgHTTPRenderTime is the mean HTTP handler time of the render
	// endpoints (the generation endpoints' /render) per instance (seconds,
	// [5m]), and QueryHTTPRenderRate their requests a second.
	QueryAvgHTTPRenderTime = "avg_http_render_time"
	QueryHTTPRenderRate    = "http_render_rate"
	// QueryAvgE2ELatency is vLLM's mean end-to-end request latency per
	// instance (seconds, [5m]): the engine's time from the request's
	// arrival in the engine to its last token.
	QueryAvgE2ELatency = "avg_e2e_latency"

	// FrontendWindow is the window of the queries above.
	FrontendWindow = SLORouterWindow
)

// The handler label values of vLLM's OpenAI-compatible generation
// endpoints, and of their render endpoints (each one's path with /render).
// A PromQL regex is anchored at both ends.
const (
	generationHandlers = `/v1/(chat/completions|completions|messages|responses)`
	renderHandlers     = generationHandlers + `/render`
)

// perInstanceHTTP is the per-instance rate of an HTTP handler series for
// the handlers the regex matches, summed over method and status.
func perInstanceHTTP(metric, handlers string) string {
	return `sum by (instance, pod) (rate(` + metric + `{namespace="{{.namespace}}",handler=~"` + handlers + `"}[` +
		FrontendWindow + `]))`
}

// registerFrontendQueries registers the queries above.
func registerFrontendQueries(registry *source.QueryList) {
	for _, q := range []struct{ name, template, what string }{
		{QueryAvgHTTPGenerationTime, perInstanceHTTP("http_request_duration_seconds_sum", generationHandlers) + ` / ` +
			perInstanceHTTP("http_request_duration_seconds_count", generationHandlers),
			"Mean HTTP handler time of the generation endpoints per instance (s, 5m); the series carries no model"},
		{QueryAvgHTTPRenderTime, perInstanceHTTP("http_request_duration_seconds_sum", renderHandlers) + ` / ` +
			perInstanceHTTP("http_request_duration_seconds_count", renderHandlers),
			"Mean HTTP handler time of the render endpoints per instance (s, 5m); the series carries no model"},
		{QueryHTTPRenderRate, perInstanceHTTP("http_request_duration_seconds_count", renderHandlers),
			"Render endpoint requests per instance (req/s, 5m); the series carries no model"},
		{QueryAvgE2ELatency, perPodMean("vllm:e2e_request_latency_seconds_sum", "vllm:e2e_request_latency_seconds_count", FrontendWindow),
			"Mean end-to-end request latency per instance (s, 5m)"},
	} {
		registry.MustRegister(source.QueryTemplate{
			Name:        q.name,
			Type:        source.QueryTypePromQL,
			Template:    q.template,
			Params:      []string{source.ParamNamespace},
			Description: q.what + " (vLLM only)",
		})
	}
}
