package registration

import (
	"strings"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
)

// TestSLORouterQueries asserts on the queries the REGISTRY BUILDS, as the
// arrival-rate guard does, for the same reason: a broken router query reads as
// "the router exports nothing", which leaves the SLO silently unjudged rather
// than failing.
func TestSLORouterQueries(t *testing.T) {
	reg := source.NewSourceRegistry()
	if err := reg.Register("prometheus", source.NewNoOpSource()); err != nil {
		t.Fatal(err)
	}
	RegisterSLOQueries(reg)
	ql := reg.Get("prometheus").QueryList()
	build := func(name string) string {
		q, err := ql.Build(name, map[string]string{source.ParamNamespace: "ns"})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return strings.Join(strings.Fields(q), " ")
	}

	ttft := build(QuerySLORouterTTFTBuckets)
	for _, want := range []string{
		"llm_d_epp_request_ttft_seconds_bucket",
		`streaming="true"`, // a non-streaming TTFT is the whole request
		`namespace="ns"`,
		"sum by (model_name, target_model_name, le)",
		"rate(", "[" + SLORouterWindow + "]",
	} {
		if !strings.Contains(ttft, want) {
			t.Errorf("TTFT query lacks %q: %s", want, ttft)
		}
	}
	if strings.Index(ttft, "sum by") > strings.Index(ttft, "rate(") {
		t.Errorf("TTFT query must rate inside the sum: %s", ttft)
	}
	for name, metric := range map[string]string{
		QuerySLORouterTTFTSum:   "llm_d_epp_request_ttft_seconds_sum",
		QuerySLORouterTTFTCount: "llm_d_epp_request_ttft_seconds_count",
	} {
		q := build(name)
		// As the buckets: a non-streaming TTFT is the whole request.
		for _, want := range []string{metric, `streaming="true"`, `namespace="ns"`, "sum by (model_name, target_model_name)", "[" + SLORouterWindow + "]"} {
			if !strings.Contains(q, want) {
				t.Errorf("%s lacks %q: %s", name, want, q)
			}
		}
	}
	for name, metric := range map[string]string{
		QuerySLORouterTPOTSum:   "llm_d_epp_request_streaming_tpot_seconds_sum",
		QuerySLORouterTPOTCount: "llm_d_epp_request_streaming_tpot_seconds_count",
	} {
		q := build(name)
		// The namespace too: without it the mean blends in a same-named
		// model's streams from every other namespace.
		if !strings.Contains(q, metric) || !strings.Contains(q, "sum by (model_name, target_model_name)") ||
			!strings.Contains(q, "["+SLORouterWindow+"]") || !strings.Contains(q, `namespace="ns"`) {
			t.Errorf("%s: %s", name, q)
		}
		if strings.Contains(q, "streaming=") {
			t.Errorf("%s selects on a streaming label the TPOT histogram does not carry: %s", name, q)
		}
	}
	for name, metric := range map[string]string{
		QuerySLORouterProcessingBuckets: "llm_d_epp_request_processing_duration_seconds_bucket",
		QuerySLORouterProcessingSum:     "llm_d_epp_request_processing_duration_seconds_sum",
		QuerySLORouterProcessingCount:   "llm_d_epp_request_processing_duration_seconds_count",
		QuerySLORouterRenderSum:         "llm_d_epp_token_producer_render_duration_seconds_sum",
		QuerySLORouterRenderCount:       "llm_d_epp_token_producer_render_duration_seconds_count",
	} {
		q := build(name)
		// The series carries no model: summed over the namespace, rate
		// inside the sum, over the router latency's window.
		for _, want := range []string{metric, `namespace="ns"`, "rate(", "[" + SLORouterWindow + "]"} {
			if !strings.Contains(q, want) {
				t.Errorf("%s lacks %q: %s", name, want, q)
			}
		}
		if strings.Contains(q, "model_name") || strings.Contains(q, "streaming=") {
			t.Errorf("%s groups or selects on a label the series does not carry: %s", name, q)
		}
		if strings.Index(q, "sum") > strings.Index(q, "rate(") {
			t.Errorf("%s must rate inside the sum: %s", name, q)
		}
	}
	if !strings.Contains(build(QuerySLORouterProcessingBuckets), "sum by (le)") {
		t.Errorf("the processing buckets are not grouped by bound: %s", build(QuerySLORouterProcessingBuckets))
	}
	if SLORouterWindowSeconds != 300 || SLORouterWindow != "5m" {
		t.Errorf("window %q / %v disagree", SLORouterWindow, SLORouterWindowSeconds)
	}
}

// The router's per-endpoint and prefix-producer queries (#475) keep each
// router Pod apart, so the collector can read the Pods that serve a model
// and leave another replica's series out; a counter is rated inside the
// sum, a gauge averaged over the router's window.
func TestSLORouterEndpointQueries(t *testing.T) {
	reg := source.NewSourceRegistry()
	if err := reg.Register("prometheus", source.NewNoOpSource()); err != nil {
		t.Fatal(err)
	}
	RegisterSLOQueries(reg)
	ql := reg.Get("prometheus").QueryList()
	for name, want := range map[string][]string{
		QuerySLORouterEndpointPlacements: {"sum by (pod, target_model_name, endpoint_name) (rate(llm_d_epp_scheduler_attempts_total{",
			`status="success"`, `endpoint_name!=""`},
		QuerySLORouterEndpointQueue: {"sum by (pod, model_server_endpoint) (avg_over_time(llm_d_epp_per_endpoint_queue_size{"},
		QuerySLORouterEndpointInFlightRequests: {"max by (pod, endpoint_name) (sum by (pod, endpoint_name, producer_name) (" +
			"avg_over_time(llm_d_epp_inflight_requests{"},
		QuerySLORouterEndpointInFlightTokens: {"max by (pod, endpoint_name) (sum by (pod, endpoint_name, producer_name) (" +
			"avg_over_time(llm_d_epp_inflight_tokens{"},
		QuerySLORouterPrefixPredicted:     {"sum by (pod, plugin_name, endpoint_role) (rate(llm_d_epp_prefix_predicted_cached_tokens_sum{"},
		QuerySLORouterPrefixBestAvailable: {"sum by (pod, plugin_name, endpoint_role) (rate(llm_d_epp_prefix_best_available_cached_tokens_sum{"},
		QuerySLORouterPrefixBestPredicted: {"sum by (pod, plugin_name, endpoint_role) (rate(llm_d_epp_prefix_best_predicted_cached_tokens_sum{"},
		QuerySLORouterPrefixPrompt:        {"sum by (pod, plugin_name, endpoint_role) (rate(llm_d_epp_prefix_prompt_tokens_sum{"},
		QuerySLORouterPrefixRequests:      {"sum by (pod, plugin_name, endpoint_role) (rate(llm_d_epp_prefix_predicted_cached_tokens_count{"},
	} {
		q, err := ql.Build(name, map[string]string{source.ParamNamespace: "ns"})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		q = strings.Join(strings.Fields(q), " ")
		for _, w := range append(want, `namespace="ns"`, "["+SLORouterWindow+"]") {
			if !strings.Contains(q, w) {
				t.Errorf("%s lacks %q: %s", name, w, q)
			}
		}
	}
}
