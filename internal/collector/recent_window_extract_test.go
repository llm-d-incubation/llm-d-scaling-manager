package collector

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// The short-window shape fields and the prefill computed-token rate, from the
// query result to domain.ReplicaMetrics.
//
// None of these three fields was populated or asserted by any test: a dropped
// extraction block leaves them at zero, which is indistinguishable from "the
// engine published nothing", and every consumer then silently falls back. The
// NaN cases matter for the same reason -- rate() over a window with no
// completions divides zero by zero, so NaN is the ordinary case here, not a
// pathological one.
func TestCollectReplicaMetrics_ShortWindowShapeAndPrefillRate(t *testing.T) {
	podLabels := map[string]string{
		seriesModelLabel: "test-model",
		"pod":            "pod-abc",
		"instance":       "10.0.0.1:8000",
	}

	collect := func(t *testing.T, vals map[string]float64) []struct {
		out, in, prefill float64
	} {
		t.Helper()
		reg := prometheus.NewRegistry()
		if err := metrics.InitMetrics(reg); err != nil {
			t.Fatalf("InitMetrics: %v", err)
		}
		scheme := runtime.NewScheme()
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		ts := time.Now()

		results := map[string]*source.MetricResult{
			// Makes the pod discoverable; without it there is no row to assert on.
			"kv_cache_usage": {Values: []source.MetricValue{{Labels: podLabels, Value: 0.5, Timestamp: ts}}},
		}
		for name, v := range vals {
			results[name] = &source.MetricResult{
				Values: []source.MetricValue{{Labels: podLabels, Value: v, Timestamp: ts}},
			}
		}

		mockSource := &mockMetricsSource{
			refreshFunc: func(_ context.Context, _ source.RefreshSpec) (map[string]*source.MetricResult, error) {
				return results, nil
			},
		}
		c := NewReplicaMetricsCollector(mockSource, k8sClient, nil, nil,
			scalerLocator(map[string]string{"pod-abc": "va-1"}))
		got, err := c.CollectReplicaMetrics(context.Background(), "test-model", "test-ns",
			make(map[string]scaletarget.ScaleTargetAccessor),
			make(map[string]*llmdvariant.Variant), nil)
		if err != nil {
			t.Fatalf("CollectReplicaMetrics: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("expected 1 replica row, got %d", len(got))
		}
		return []struct{ out, in, prefill float64 }{{
			out:     got[0].AvgOutputTokensRecent,
			in:      got[0].AvgInputTokensRecent,
			prefill: got[0].PrefillComputedTokenRate,
		}}
	}

	t.Run("a reading of each lands on its own field", func(t *testing.T) {
		// Deliberately all different, and none equal to the [5m] values below,
		// so a block that assigns to the wrong field cannot pass.
		r := collect(t, map[string]float64{
			registration.QueryAvgOutputTokensRecent:    4000,
			registration.QueryAvgInputTokensRecent:     1000,
			registration.QueryPrefillComputedTokenRate: 170000,
		})[0]
		if r.out != 4000 {
			t.Errorf("AvgOutputTokensRecent = %v, want 4000", r.out)
		}
		if r.in != 1000 {
			t.Errorf("AvgInputTokensRecent = %v, want 1000", r.in)
		}
		if r.prefill != 170000 {
			t.Errorf("PrefillComputedTokenRate = %v, want 170000", r.prefill)
		}
	})

	t.Run("the short-window fields do not borrow the long-window ones", func(t *testing.T) {
		// Only the [5m] queries answer. The recent fields must stay zero rather
		// than inherit them: the caller's whole fallback decision is "is there a
		// short-window reading or not".
		r := collect(t, map[string]float64{
			registration.QueryAvgOutputTokens: 1000,
			registration.QueryAvgInputTokens:  6000,
		})[0]
		if r.out != 0 || r.in != 0 {
			t.Errorf("recent fields = (out %v, in %v), want (0, 0) when only the [5m] queries answered", r.out, r.in)
		}
	})

	t.Run("NaN is not a reading", func(t *testing.T) {
		// rate() over a window in which nothing completed is 0/0. Left
		// unguarded this reaches the fleet average and turns it, and every
		// figure priced from it, into NaN.
		r := collect(t, map[string]float64{
			registration.QueryAvgOutputTokensRecent:    math.NaN(),
			registration.QueryAvgInputTokensRecent:     math.NaN(),
			registration.QueryPrefillComputedTokenRate: math.NaN(),
		})[0]
		if !(r.out == 0) || !(r.in == 0) || !(r.prefill == 0) {
			t.Errorf("NaN survived extraction: out %v, in %v, prefill %v", r.out, r.in, r.prefill)
		}
	})

	t.Run("an infinity is not a reading either", func(t *testing.T) {
		r := collect(t, map[string]float64{
			registration.QueryAvgOutputTokensRecent:    math.Inf(1),
			registration.QueryAvgInputTokensRecent:     math.Inf(-1),
			registration.QueryPrefillComputedTokenRate: math.Inf(1),
		})[0]
		if r.out != 0 || r.in != 0 || r.prefill != 0 {
			t.Errorf("an infinity survived extraction: out %v, in %v, prefill %v", r.out, r.in, r.prefill)
		}
	})
}
