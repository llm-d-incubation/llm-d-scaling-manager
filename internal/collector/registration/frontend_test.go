package registration

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
)

// The API server's queries: the HTTP series by handler, grouped by instance
// and pod only (they carry no model), the generation handlers apart from
// their /render, and vLLM's end-to-end mean per pod and model, all over the
// router latency's window so the frontend is a difference of means over the
// same requests.
func TestFrontendQueries(t *testing.T) {
	q := builtQueries(t, inferenceengine.EngineVLLM, QueryAvgHTTPGenerationTime, QueryAvgHTTPRenderTime, QueryHTTPRenderRate,
		QueryAvgE2ELatency)
	gen := `handler=~"/v1/(chat/completions|completions|messages|responses)"`
	ren := `handler=~"/v1/(chat/completions|completions|messages|responses)/render"`
	for name, want := range map[string]string{
		QueryAvgHTTPGenerationTime: `sum by (instance, pod) (rate(http_request_duration_seconds_sum{namespace="ns",` + gen + `}[5m])) / ` +
			`sum by (instance, pod) (rate(http_request_duration_seconds_count{namespace="ns",` + gen + `}[5m]))`,
		QueryAvgHTTPRenderTime: `sum by (instance, pod) (rate(http_request_duration_seconds_sum{namespace="ns",` + ren + `}[5m])) / ` +
			`sum by (instance, pod) (rate(http_request_duration_seconds_count{namespace="ns",` + ren + `}[5m]))`,
		QueryHTTPRenderRate: `sum by (instance, pod) (rate(http_request_duration_seconds_count{namespace="ns",` + ren + `}[5m]))`,
		QueryAvgE2ELatency: `sum by (model_name, instance, pod) (rate(vllm:e2e_request_latency_seconds_sum{namespace="ns"}[5m])) / ` +
			`sum by (model_name, instance, pod) (rate(vllm:e2e_request_latency_seconds_count{namespace="ns"}[5m]))`,
	} {
		if q[name] != want {
			t.Errorf("%s:\n got %s\nwant %s", name, q[name], want)
		}
		if IsEngineSpecific(name) {
			t.Errorf("%s registered per engine: vLLM's alone", name)
		}
	}
	if FrontendWindow != SLORouterWindow {
		t.Errorf("the frontend's window %s is not the router's %s", FrontendWindow, SLORouterWindow)
	}
}
