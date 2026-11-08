package collector

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// TestCollectReplicaMetrics_OutageIsCountedOnceAndNotRequeried: when every
// query fails the Prometheus source returns the failed results AND an error.
// The first model in the namespace must see that error, and the collection
// error counter must move; the second model must read the memoized failure
// rather than send the same queries to a Prometheus that is down.
func TestCollectReplicaMetrics_OutageIsCountedOnceAndNotRequeried(t *testing.T) {
	registry := prometheus.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))
	scheme := runtime.NewScheme()
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	outage := errors.New("dial tcp: connection refused")
	refreshes := 0
	mockSource := &mockMetricsSource{
		refreshFunc: func(_ context.Context, spec source.RefreshSpec) (map[string]*source.MetricResult, error) {
			refreshes++
			out := make(map[string]*source.MetricResult, len(spec.Queries))
			for _, q := range spec.Queries {
				out[q] = &source.MetricResult{QueryName: q, Error: outage}
			}
			return out, outage
		},
	}
	collector := NewReplicaMetricsCollector(mockSource, k8sClient, nil, nil, scalerLocator(nil))
	collect := func(modelID string) error {
		_, err := collector.CollectReplicaMetrics(
			context.Background(), modelID, "test-ns",
			make(map[string]scaletarget.ScaleTargetAccessor),
			make(map[string]*llmdvariant.Variant),
			nil,
		)
		return err
	}

	collector.BeginCycle()
	defer collector.EndCycle()

	require.Error(t, collect("model-a"), "the model that hit the outage must get an error")
	_ = collect("model-b")

	assert.Equal(t, 1, refreshes, "a second model must not re-query a Prometheus that just failed every query")
	assert.Positive(t, collectionErrors(t, registry), "the outage must reach %s", constants.LLMSMMetricsCollectionErrorsTotal)
}

func collectionErrors(t *testing.T, registry *prometheus.Registry) float64 {
	t.Helper()
	families, err := registry.Gather()
	require.NoError(t, err)
	total := 0.0
	for _, f := range families {
		if f.GetName() != constants.LLMSMMetricsCollectionErrorsTotal {
			continue
		}
		for _, m := range f.GetMetric() {
			total += m.GetCounter().GetValue()
		}
	}
	return total
}
