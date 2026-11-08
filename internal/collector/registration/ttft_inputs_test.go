package registration

import (
	"context"
	"strings"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source/prometheus"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
)

// builtQueries registers every query the engine registers and builds names
// in namespace ns, each under its physical name for engine.
func builtQueries(t *testing.T, engine inferenceengine.Engine, names ...string) map[string]string {
	t.Helper()
	reg := source.NewSourceRegistry()
	if err := reg.Register("prometheus", prometheus.NewPrometheusSource(
		context.Background(), &mockPrometheusAPI{}, prometheus.DefaultPrometheusSourceConfig())); err != nil {
		t.Fatalf("registering the prometheus source: %v", err)
	}
	RegisterSaturationQueries(reg)
	RegisterQueueingModelQueries(reg)
	RegisterAlwaysOnQueries(reg)
	RegisterSLOQueries(reg)
	RegisterFlowControlPoolQueries(reg)
	out := make(map[string]string, len(names))
	for _, name := range names {
		q, err := reg.Get("prometheus").QueryList().Build(EngineQuery(engine, name), map[string]string{source.ParamNamespace: "ns"})
		if err != nil {
			t.Fatalf("building %s for %s: %v", name, engine, err)
		}
		out[name] = strings.Join(strings.Fields(q), " ")
	}
	return out
}

// The computed-token histogram is over the window of the computed-token rate
// the mean is read from, each rank once however often its target is scraped,
// and vLLM's alone: SGLang publishes no per-stage figure.
func TestComputedTokensBucketsQuery(t *testing.T) {
	q := builtQueries(t, inferenceengine.EngineVLLM, QueryComputedTokensBuckets, QueryPrefillComputedTokenRate)
	got := q[QueryComputedTokensBuckets]
	want := `sum by (model_name, instance, pod, le) (max by (model_name, instance, pod, engine, le) (` +
		`rate(vllm:request_prefill_kv_computed_tokens_bucket{namespace="ns"}[1m])))`
	if got != want {
		t.Errorf("query:\n got %s\nwant %s", got, want)
	}
	if !strings.Contains(q[QueryPrefillComputedTokenRate], "[1m]") {
		t.Errorf("the mean's window moved: %s", q[QueryPrefillComputedTokenRate])
	}
	if IsEngineSpecific(QueryComputedTokensBuckets) {
		t.Error("registered per engine: SGLang has no such series")
	}
}

// Each TTFT input query reads the series 2.9 names, under the name the engine
// exposes, over the window its mean partner uses, grouped so the collector
// can key it to a pod and a model.
func TestTTFTInputQueries(t *testing.T) {
	type want struct {
		metrics []string
		prefix  string
		window  string
	}
	const bucketsBy = "sum by (model_name, instance, pod, le) (rate("
	const meanBy = "sum by (model_name, instance, pod) (rate("
	vllm := map[string]want{
		QueryPromptTokensBuckets: {[]string{"vllm:request_prompt_tokens_bucket"}, bucketsBy, "[5m]"},
		QueryTTFTBuckets:         {[]string{"vllm:time_to_first_token_seconds_bucket"}, bucketsBy, "[1m]"},
		QueryQueueTimeBuckets:    {[]string{"vllm:request_queue_time_seconds_bucket"}, bucketsBy, "[1m]"},
		QueryTPOTBuckets:         {[]string{"vllm:request_time_per_output_token_seconds_bucket"}, bucketsBy, "[1m]"},
		QueryAvgQueueTime: {[]string{"vllm:request_queue_time_seconds_sum", "vllm:request_queue_time_seconds_count"},
			meanBy, "[1m]"},
		QueryAvgPrefillTime: {[]string{"vllm:request_prefill_time_seconds_sum", "vllm:request_prefill_time_seconds_count"},
			meanBy, "[1m]"},
		QueryAvgKVTransferTime: {[]string{"vllm:nixl_xfer_time_seconds_sum", "vllm:nixl_xfer_time_seconds_count"},
			meanBy, "[1m]"},
	}
	names := make([]string, 0, len(vllm))
	for name := range vllm {
		names = append(names, name)
	}
	built := builtQueries(t, inferenceengine.EngineVLLM, names...)
	for name, w := range vllm {
		q := built[name]
		if !strings.HasPrefix(q, w.prefix) {
			t.Errorf("%s must start %q: %s", name, w.prefix, q)
		}
		for _, m := range w.metrics {
			if !strings.Contains(q, m+`{namespace="ns"}`+w.window) {
				t.Errorf("%s does not read %s over %s in the namespace: %s", name, m, w.window, q)
			}
		}
		if strings.Contains(q, "sglang:") {
			t.Errorf("%s (vLLM) reads an SGLang series: %s", name, q)
		}
	}
	// A mean is summed per pod on both sides, so each rank weighs by its own
	// traffic (TestPerPodQueriesFoldDataParallelRanks' rule).
	for _, name := range []string{QueryAvgQueueTime, QueryAvgPrefillTime, QueryAvgKVTransferTime} {
		if strings.Count(built[name], meanBy) != 2 || !strings.Contains(built[name], ") / sum by") {
			t.Errorf("%s must be sum(rate(_sum)) / sum(rate(_count)) per pod: %s", name, built[name])
		}
	}

	// SGLang publishes the histograms and the queue time, under its own names.
	sglang := map[string]string{
		QueryPromptTokensBuckets: "sglang:prompt_tokens_histogram_bucket",
		QueryTTFTBuckets:         "sglang:time_to_first_token_seconds_bucket",
		QueryQueueTimeBuckets:    "sglang:queue_time_seconds_bucket",
		QueryAvgQueueTime:        "sglang:queue_time_seconds_sum",
	}
	names = names[:0]
	for name := range sglang {
		names = append(names, name)
	}
	builtSGLang := builtQueries(t, inferenceengine.EngineSGLang, names...)
	for name, metric := range sglang {
		q := builtSGLang[name]
		if !strings.Contains(q, metric+`{namespace="ns"}`) || strings.Contains(q, "vllm:") {
			t.Errorf("SGLang %s must read %s and no vllm: series: %s", name, metric, q)
		}
		if !strings.Contains(q, "by (model_name, instance, pod") {
			t.Errorf("SGLang %s must group by pod and model: %s", name, q)
		}
	}

	// vLLM alone publishes these: one registration, under the bare name, so
	// an SGLang fleet asks the vLLM query and gets no series.
	for _, name := range []string{QueryAvgPrefillTime, QueryAvgKVTransferTime, QueryTPOTBuckets} {
		if IsEngineSpecific(name) {
			t.Errorf("%s is vLLM-only and must not be engine-specific", name)
		}
	}
	for _, name := range []string{QueryPromptTokensBuckets, QueryTTFTBuckets, QueryAvgQueueTime, QueryQueueTimeBuckets,
		QueryQueueMin} {
		if !IsEngineSpecific(name) {
			t.Errorf("%s has an SGLang variant and must be engine-specific", name)
		}
	}
}

// QueueMin is each engine's waiting gauge at its least over the minute, per
// pod: vLLM's per rank (engine) summed over the pod's ranks, as the queue
// length sums their peaks; SGLang's a max over duplicate series.
func TestQueueMinQuery(t *testing.T) {
	v := builtQueries(t, inferenceengine.EngineVLLM, QueryQueueMin)[QueryQueueMin]
	for _, want := range []string{
		"sum by (model_name, instance, pod) (max by (model_name, instance, pod, engine) (min_over_time(",
		`vllm:num_requests_waiting{namespace="ns"}[1m]`,
	} {
		if !strings.Contains(v, want) {
			t.Errorf("vLLM QueueMin lacks %q: %s", want, v)
		}
	}
	s := builtQueries(t, inferenceengine.EngineSGLang, QueryQueueMin)[QueryQueueMin]
	for _, want := range []string{
		"max by (model_name, instance, pod) (min_over_time(",
		`sglang:num_queue_reqs{namespace="ns"}[1m]`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("SGLang QueueMin lacks %q: %s", want, s)
		}
	}
	for _, q := range []string{v, s} {
		if strings.Contains(q, "max_over_time") || strings.Contains(q, "by_reason") {
			t.Errorf("QueueMin must read the whole gauge at its least: %s", q)
		}
	}
}

// The router's per-request TPOT histogram rides beside the TTFT one, over the
// same window and grouping.
func TestSLORouterTPOTBucketsQuery(t *testing.T) {
	q := builtQueries(t, inferenceengine.EngineVLLM, QuerySLORouterTPOTBuckets)[QuerySLORouterTPOTBuckets]
	for _, want := range []string{
		"sum by (model_name, target_model_name, le) (rate(",
		`llm_d_epp_request_streaming_tpot_seconds_bucket{namespace="ns"}[` + SLORouterWindow + `]`,
	} {
		if !strings.Contains(q, want) {
			t.Errorf("TPOT buckets query lacks %q: %s", want, q)
		}
	}
	if strings.Contains(q, "streaming=") {
		t.Errorf("selects on a streaming label the TPOT histogram does not carry: %s", q)
	}
}

// The flow-control queries read the current llm_d_epp_* names, in the
// namespace. The pool queries are keyed by pool, and the join query names
// each pool's models; the wait histogram is keyed by model, of dispatched
// requests only, the only waits a TTFT contains.
func TestFlowControlPoolQueries(t *testing.T) {
	built := builtQueries(t, inferenceengine.EngineVLLM, QueryFlowControlPoolModels, QueryFlowControlWaitBuckets,
		QueryFlowControlPoolSaturation, QueryFlowControlOutcomes)
	for name, wants := range map[string][]string{
		QueryFlowControlPoolModels: {"by (inference_pool, model_name, target_model_name)",
			`llm_d_epp_flow_control_queue_size{namespace="ns",inference_pool!=""}`},
		QueryFlowControlWaitBuckets: {"sum by (model_name, target_model_name, le) (rate(",
			`llm_d_epp_flow_control_request_queue_duration_seconds_bucket{namespace="ns",outcome="Dispatched"}[` + FlowControlWindow + `]`},
		QueryFlowControlPoolSaturation: {"max by (inference_pool, stage) (avg_over_time(",
			`llm_d_epp_flow_control_pool_saturation{namespace="ns"}[` + FlowControlWindow + `]`},
		QueryFlowControlOutcomes: {"sum by (inference_pool, outcome) (rate(",
			`llm_d_epp_flow_control_requests_total{namespace="ns"}[` + FlowControlWindow + `]`},
	} {
		for _, want := range wants {
			if !strings.Contains(built[name], want) {
				t.Errorf("%s lacks %q: %s", name, want, built[name])
			}
		}
		if strings.Contains(built[name], "inference_extension_") {
			t.Errorf("%s reads the deprecated alias, which carries no pool: %s", name, built[name])
		}
	}
	if FlowControlWindowSeconds != SLORouterWindowSeconds || FlowControlWindow != SLORouterWindow {
		t.Errorf("the flow-control window %q must be the router latency's %q", FlowControlWindow, SLORouterWindow)
	}
}
