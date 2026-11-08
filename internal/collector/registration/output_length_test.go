package registration

import (
	"context"
	"strings"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source/prometheus"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
)

// The output-length histogram is read per bucket bound, over the window the
// mean output length is read over, so the two are one reading (issue ev-shindin/llm-scaling-manager#276).
func TestOutputLengthQueries(t *testing.T) {
	registry := source.NewSourceRegistry()
	src := prometheus.NewPrometheusSource(context.Background(), &mockPrometheusAPI{}, prometheus.DefaultPrometheusSourceConfig())
	if err := registry.Register("prometheus", src); err != nil {
		t.Fatalf("Register: %v", err)
	}
	RegisterSaturationQueries(registry)
	queries := registry.Get("prometheus").QueryList()
	q := queries.Get(QueryOutputTokensBuckets)
	if q == nil {
		t.Fatalf("query %q is not registered", QueryOutputTokensBuckets)
	}
	mean := queries.Get(QueryAvgOutputTokens)
	if mean == nil {
		t.Fatalf("query %q is not registered", QueryAvgOutputTokens)
	}
	for _, want := range []string{"vllm:request_generation_tokens_bucket", "le)", "[5m]"} {
		if !strings.Contains(q.Template, want) {
			t.Errorf("%s = %q, want %s in it", QueryOutputTokensBuckets, q.Template, want)
		}
	}
	if !strings.Contains(mean.Template, "[5m]") {
		t.Errorf("%s = %q: the mean is no longer over [5m]; read the histogram over its window", QueryAvgOutputTokens, mean.Template)
	}

	// SGLang's histogram, behind the --collect-tokens-histogram its mean needs
	// too, read the same way.
	if !IsEngineSpecific(QueryOutputTokensBuckets) {
		t.Errorf("%s is not engine-specific: SGLang's variant would never be queried", QueryOutputTokensBuckets)
	}
	sg := queries.Get(EngineQuery(inferenceengine.EngineSGLang, QueryOutputTokensBuckets))
	if sg == nil {
		t.Fatalf("the SGLang variant of %q is not registered", QueryOutputTokensBuckets)
	}
	for _, want := range []string{"sglang:generation_tokens_histogram_bucket", "le)", "[5m]"} {
		if !strings.Contains(sg.Template, want) {
			t.Errorf("SGLang %s = %q, want %s in it", QueryOutputTokensBuckets, sg.Template, want)
		}
	}
	sgMean := queries.Get(EngineQuery(inferenceengine.EngineSGLang, QueryAvgOutputTokens))
	if sgMean == nil || !strings.Contains(sgMean.Template, "[5m]") {
		t.Errorf("SGLang's mean output length is not over [5m]; read the histogram over its window")
	}
}
