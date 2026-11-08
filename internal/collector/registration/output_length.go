package registration

import (
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
)

// QueryOutputTokensBuckets is the query name for the output-length histogram
// per instance (the model_name, instance and pod labels), one series per
// bucket bound (le), as requests a second over the [5m] window
// QueryAvgOutputTokens is taken over, so the two are one reading.
//
// The mean alone prices every request at the same length, and the KV a
// resident request holds depends on the spread too: a long request stays
// resident longer (issue #276). The collector reduces the buckets and the
// mean to a lower bound on E[O^2]
// (domain.ReplicaMetrics.AvgOutputTokensSquared).
//
// Engine-specific, like QueryAvgOutputTokens: vLLM's
// request_generation_tokens histogram, and SGLang's
// generation_tokens_histogram, which SGLang publishes behind
// --collect-tokens-histogram, the flag its QueryAvgOutputTokens needs too. An
// engine that publishes no buckets returns no series, and
// AvgOutputTokensSquaredKnown stays unset.
// Source: vllm:request_generation_tokens_bucket,
// sglang:generation_tokens_histogram_bucket (histograms)
const QueryOutputTokensBuckets = "output_tokens_buckets"

// registerOutputLengthQueries registers QueryOutputTokensBuckets for vLLM and
// SGLang.
func registerOutputLengthQueries(registry *source.QueryList) {
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryOutputTokensBuckets,
		Type:        source.QueryTypePromQL,
		Template:    `sum by (model_name, instance, pod, le) (rate(vllm:request_generation_tokens_bucket{namespace="{{.namespace}}"}[5m]))`,
		Params:      []string{source.ParamNamespace},
		Description: "Output-length histogram per pod (5m rate, one series per bucket), the spread of output lengths behind the KV a resident request holds",
	})
	// A max, as SGLang's QueryAvgOutputTokens takes it: a duplicate series
	// must not double a bucket.
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryOutputTokensBuckets,
		Type:        source.QueryTypePromQL,
		Template:    `max by (model_name, instance, pod, le) (rate(sglang:generation_tokens_histogram_bucket{namespace="{{.namespace}}"}[5m]))`,
		Params:      []string{source.ParamNamespace},
		Description: "Output-length histogram per pod (5m rate, one series per bucket) (SGLang)",
	})
}
