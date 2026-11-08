package registration

import (
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
)

// The per-pod series the TTFT model learns from
// (docs/proposals/analyzer-learner.md, 2.9). They are collected and recorded
// (domain.ReplicaMetrics, "Recorded for the TTFT model"). Three are read:
// the saturation analyzer's prefill observation takes QueryQueueMin's busy
// minutes, and the slo analyzer's TTFT pricing and the what-if CLI take
// QueryComputedTokensBuckets and QueryAvgKVTransferTime. Those three are
// issued every cycle in a namespace with a TTFT target (collector
// ReadTTFTInputsIn); the rest only while recording.
//
// Each is namespace-scoped like every other replica query, grouped by
// (model_name, instance, pod) and cut down to the model in Go. A histogram is
// one series per bucket bound (le), as requests a second at or under it. A
// mean is perPodMean: the ranks of a pod summed on both sides of the division.
const (
	// QueryPromptTokensBuckets is the prompt-length histogram per instance,
	// over the [5m] QueryAvgInputTokens is taken over. vLLM
	// request_prompt_tokens; SGLang prompt_tokens_histogram, behind
	// --collect-tokens-histogram like its QueryAvgInputTokens.
	QueryPromptTokensBuckets = "prompt_tokens_buckets"

	// QueryComputedTokensBuckets is the per-request histogram of the prefill
	// tokens computed, the prompt net of prefix hits, per instance, over the
	// [1m] of QueryPrefillComputedTokenRate: the mean the spread is read
	// against is taken over the same requests. vLLM only: SGLang publishes no
	// per-stage figure, so it is registered once, under its bare name, and an
	// SGLang pod returns no series.
	QueryComputedTokensBuckets = "computed_tokens_buckets"

	// QueryTTFTBuckets is the engine's time-to-first-token histogram per
	// instance, over the [1m] QueryAvgTTFT is taken over. The same name on
	// vLLM and SGLang.
	QueryTTFTBuckets = "ttft_buckets"

	// QueryAvgQueueTime is the mean time a request waited in the engine's
	// queue before it was scheduled, per instance (seconds, [1m]), and
	// QueryQueueTimeBuckets its histogram. vLLM request_queue_time_seconds;
	// SGLang queue_time_seconds.
	QueryAvgQueueTime     = "avg_queue_time"
	QueryQueueTimeBuckets = "queue_time_buckets"

	// QueryAvgPrefillTime is the mean time a request spent in prefill, queue
	// wait excluded, per instance (seconds, [1m]). vLLM only: SGLang
	// publishes no per-stage figure (sgl-project/sglang issue #14303), so it
	// is registered once, under its bare name, like
	// QueryPrefillComputedTokenRate. Source:
	// constants.VLLMRequestPrefillTimeSeconds.
	QueryAvgPrefillTime = "avg_prefill_time"

	// QueryAvgKVTransferTime is the mean time of one KV transfer through the
	// NIXL connector, per instance (seconds, [1m]): the P/D transfer term of
	// TTFT. vLLM only, exported by a pod running the NIXL connector; every
	// other pod returns no series.
	QueryAvgKVTransferTime = "avg_kv_transfer_time"

	// QueryTPOTBuckets is the engine's per-request time-per-output-token
	// histogram per instance, over [1m] like QueryAvgITL: each request's own
	// mean TPOT, the fallback source for the TPOT ratio where the router's
	// histogram is missing. vLLM only.
	QueryTPOTBuckets = "tpot_buckets"

	// QueryQueueMin is the least the engine's waiting-request gauge read
	// over the last minute, per instance: a minute in which it never fell
	// to zero is a minute the engine always had work waiting. vLLM
	// num_requests_waiting, each rank's minimum summed over the instance's
	// ranks as QueryQueueLength sums their peaks; SGLang num_queue_reqs, a
	// max over duplicate series as its QueryQueueLength takes it.
	QueryQueueMin = "queue_min"

	// TTFTInputWindow is the window of the [1m] queries above.
	TTFTInputWindow = "1m"

	// TTFTLengthWindow is the window of the prompt-length histogram: the
	// [5m] of QueryAvgInputTokens, so the histogram and the mean cover the
	// same requests.
	TTFTLengthWindow = "5m"
)

// perPodBuckets is the per-pod bucket rates of a vLLM histogram over window:
// rate(_bucket) summed down to (model_name, instance, pod, le), so a pod's
// ranks add, as the output-length histogram is taken
// (QueryOutputTokensBuckets).
func perPodBuckets(bucketMetric, window string) string {
	return `sum by (model_name, instance, pod, le) (rate(` + bucketMetric + `{namespace="{{.namespace}}"}[` + window + `]))`
}

// perPodBucketsOnce is perPodBuckets with each rank read once however many
// times its target is scraped, as QueryPrefillComputedTokenRate is. A pod
// scraped twice would otherwise weigh double in the variant's pooled spread.
func perPodBucketsOnce(bucketMetric, window string) string {
	return `sum by (model_name, instance, pod, le) (max by (model_name, instance, pod, engine, le) (rate(` +
		bucketMetric + `{namespace="{{.namespace}}"}[` + window + `])))`
}

// sglangPerPodBuckets is perPodBuckets for SGLang: a max, as SGLang's
// QueryOutputTokensBuckets takes it, so a duplicate series does not double a
// bucket.
func sglangPerPodBuckets(bucketMetric, window string) string {
	return `max by (model_name, instance, pod, le) (rate(` + bucketMetric + `{namespace="{{.namespace}}"}[` + window + `]))`
}

// registerTTFTInputQueries registers the queries above, both engines'
// variants where SGLang publishes the series.
func registerTTFTInputQueries(registry *source.QueryList) {
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryPromptTokensBuckets,
		Type:        source.QueryTypePromQL,
		Template:    perPodBuckets("vllm:request_prompt_tokens_bucket", TTFTLengthWindow),
		Params:      []string{source.ParamNamespace},
		Description: "Prompt-length histogram per pod (5m rate, one series per bucket)",
	})
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryPromptTokensBuckets,
		Type:        source.QueryTypePromQL,
		Template:    sglangPerPodBuckets("sglang:prompt_tokens_histogram_bucket", TTFTLengthWindow),
		Params:      []string{source.ParamNamespace},
		Description: "Prompt-length histogram per pod (5m rate, one series per bucket) (SGLang)",
	})

	registry.MustRegister(source.QueryTemplate{
		Name:        QueryComputedTokensBuckets,
		Type:        source.QueryTypePromQL,
		Template:    perPodBucketsOnce("vllm:request_prefill_kv_computed_tokens_bucket", TTFTInputWindow),
		Params:      []string{source.ParamNamespace},
		Description: "Computed prefill tokens per request, histogram per pod (1m rate, one series per bucket) (vLLM only)",
	})

	registry.MustRegister(source.QueryTemplate{
		Name:        QueryTTFTBuckets,
		Type:        source.QueryTypePromQL,
		Template:    perPodBuckets("vllm:time_to_first_token_seconds_bucket", TTFTInputWindow),
		Params:      []string{source.ParamNamespace},
		Description: "Engine time-to-first-token histogram per pod (1m rate, one series per bucket)",
	})
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryTTFTBuckets,
		Type:        source.QueryTypePromQL,
		Template:    sglangPerPodBuckets("sglang:time_to_first_token_seconds_bucket", TTFTInputWindow),
		Params:      []string{source.ParamNamespace},
		Description: "Engine time-to-first-token histogram per pod (1m rate, one series per bucket) (SGLang)",
	})

	registry.MustRegister(source.QueryTemplate{
		Name:        QueryAvgQueueTime,
		Type:        source.QueryTypePromQL,
		Template:    perPodMean("vllm:request_queue_time_seconds_sum", "vllm:request_queue_time_seconds_count", TTFTInputWindow),
		Params:      []string{source.ParamNamespace},
		Description: "Average engine queue wait per pod (seconds, 1m)",
	})
	// A max of the ratio, as SGLang's other means take it (QueryAvgTTFT).
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name: QueryAvgQueueTime,
		Type: source.QueryTypePromQL,
		Template: `max by (model_name, instance, pod) (rate(sglang:queue_time_seconds_sum{namespace="{{.namespace}}"}[` + TTFTInputWindow + `])` +
			` / rate(sglang:queue_time_seconds_count{namespace="{{.namespace}}"}[` + TTFTInputWindow + `]))`,
		Params:      []string{source.ParamNamespace},
		Description: "Average engine queue wait per pod (seconds, 1m) (SGLang)",
	})

	registry.MustRegister(source.QueryTemplate{
		Name:        QueryQueueTimeBuckets,
		Type:        source.QueryTypePromQL,
		Template:    perPodBuckets("vllm:request_queue_time_seconds_bucket", TTFTInputWindow),
		Params:      []string{source.ParamNamespace},
		Description: "Engine queue wait histogram per pod (1m rate, one series per bucket)",
	})
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryQueueTimeBuckets,
		Type:        source.QueryTypePromQL,
		Template:    sglangPerPodBuckets("sglang:queue_time_seconds_bucket", TTFTInputWindow),
		Params:      []string{source.ParamNamespace},
		Description: "Engine queue wait histogram per pod (1m rate, one series per bucket) (SGLang)",
	})

	registry.MustRegister(source.QueryTemplate{
		Name: QueryQueueMin,
		Type: source.QueryTypePromQL,
		Template: `sum by (model_name, instance, pod) (max by (model_name, instance, pod, engine) (` +
			`min_over_time(vllm:num_requests_waiting{namespace="{{.namespace}}"}[` + TTFTInputWindow + `])))`,
		Params:      []string{source.ParamNamespace},
		Description: "Least queue length per pod over the last minute",
	})
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name: QueryQueueMin,
		Type: source.QueryTypePromQL,
		Template: `max by (model_name, instance, pod) (` +
			`min_over_time(sglang:num_queue_reqs{namespace="{{.namespace}}"}[` + TTFTInputWindow + `]))`,
		Params:      []string{source.ParamNamespace},
		Description: "Least queue length per pod over the last minute (SGLang)",
	})

	registry.MustRegister(source.QueryTemplate{
		Name:        QueryAvgPrefillTime,
		Type:        source.QueryTypePromQL,
		Template:    perPodMean("vllm:request_prefill_time_seconds_sum", "vllm:request_prefill_time_seconds_count", TTFTInputWindow),
		Params:      []string{source.ParamNamespace},
		Description: "Average prefill time per pod, queue wait excluded (seconds, 1m) (vLLM only)",
	})
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryAvgKVTransferTime,
		Type:        source.QueryTypePromQL,
		Template:    perPodMean("vllm:nixl_xfer_time_seconds_sum", "vllm:nixl_xfer_time_seconds_count", TTFTInputWindow),
		Params:      []string{source.ParamNamespace},
		Description: "Average NIXL KV transfer time per pod (seconds, 1m) (vLLM only)",
	})
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryTPOTBuckets,
		Type:        source.QueryTypePromQL,
		Template:    perPodBuckets("vllm:request_time_per_output_token_seconds_bucket", TTFTInputWindow),
		Params:      []string{source.ParamNamespace},
		Description: "Engine per-request TPOT histogram per pod (1m rate, one series per bucket) (vLLM only)",
	})
}
