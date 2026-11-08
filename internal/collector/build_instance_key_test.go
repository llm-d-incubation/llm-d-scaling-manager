/*
Copyright 2025 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package collector

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/locator"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// scalerLocator returns a PodLocator that resolves the given pods to the given
// scaler names, standing in for the real ownerReference walk (pod → ReplicaSet →
// Deployment/LWS → ScaledObject). A pod absent from the map is unmanaged, which
// is how the real locator reports a pod with no scaler above it.
func scalerLocator(scalerByPod map[string]string) *mockLocator {
	return &mockLocator{
		locateFunc: func(_ context.Context, _, podName string) (*locator.ManagedScaler, error) {
			name, ok := scalerByPod[podName]
			if !ok {
				return nil, nil
			}
			return &locator.ManagedScaler{
				Name: name,
			}, nil
		},
	}
}

// allPodsLocator returns a PodLocator that resolves every pod to one scaler, for
// tests where a single scale target owns all the pods in play.
func allPodsLocator(scalerName string) *mockLocator {
	return &mockLocator{
		locateFunc: func(_ context.Context, _, _ string) (*locator.ManagedScaler, error) {
			return &locator.ManagedScaler{
				Name: scalerName,
			}, nil
		},
	}
}

// buildInstanceKeyTestCase drives a single call through CollectReplicaMetrics with
// a source that returns exactly one KV-cache sample, then checks that the resulting
// ReplicaMetrics carries the vaName the locator resolved (or none when it resolved
// nothing).
type buildInstanceKeyTestCase struct {
	name        string
	labels      map[string]string
	located     map[string]string // pod → scaler name the locator resolves
	wantVAName  string
	wantSkipped bool // true when buildInstanceKey returns ("","","") → no entry produced
}

var buildInstanceKeyTestCases = []buildInstanceKeyTestCase{
	{
		name: "pod label present – locator resolves the scaler",
		labels: map[string]string{
			seriesModelLabel: "test-model",
			"pod":            "pod-abc",
			"instance":       "10.0.0.1:8000",
		},
		located:    map[string]string{"pod-abc": "my-va"},
		wantVAName: "my-va",
	},
	{
		name: "pod_name fallback – locator resolves the scaler",
		labels: map[string]string{
			seriesModelLabel: "test-model",
			"pod_name":       "pod-xyz",
			"instance":       "10.0.0.2:8000",
		},
		located:    map[string]string{"pod-xyz": "other-va"},
		wantVAName: "other-va",
	},
	{
		// Pods the locator cannot attribute are skipped ("Skipping pod that
		// doesn't match any scale target"), so no ReplicaMetrics is produced.
		name: "pod has no managed scaler above it – pod skipped, no result",
		labels: map[string]string{
			seriesModelLabel: "test-model",
			"pod":            "pod-unmanaged",
			"instance":       "10.0.0.3:8000",
		},
		wantSkipped: true,
	},
	{
		// Regression guard: llm_d_ai_variant no longer short-circuits the walk.
		// The label is not emitted by any engine and is no longer carried in the
		// query groupings, so a series that still has one must not be attributed
		// by it — the locator is the only authority.
		name: "llm_d_ai_variant present but pod unmanaged – label must not attribute it",
		labels: map[string]string{
			seriesModelLabel: "test-model",
			"pod":            "pod-labelled",
			"instance":       "10.0.0.4:8000",
			// Spelled out rather than taken from a constant. This is a series
			// arriving from OUTSIDE — a ServiceMonitor relabeling that predates
			// llm-d/llm-d-workload-variant-autoscaler#1263, or a shadow-pod layout — so the test should say the literal
			// string the cluster sends, not import our name for it. The constant
			// itself is gone: nothing in the controller reads this label.
			"llm_d_ai_variant": "stale-va",
		},
		wantSkipped: true,
	},
	{
		name: "no pod identity labels – entry skipped entirely",
		labels: map[string]string{
			seriesModelLabel: "test-model",
			"instance":       "10.0.0.5:8000",
		},
		wantSkipped: true,
	},
}

func TestBuildInstanceKey_VANameExtraction(t *testing.T) {
	for _, tc := range buildInstanceKeyTestCases {
		t.Run(tc.name, func(t *testing.T) {
			registry := prometheus.NewRegistry()
			if err := metrics.InitMetrics(registry); err != nil {
				t.Fatalf("InitMetrics: %v", err)
			}

			scheme := runtime.NewScheme()
			k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

			mockSource := &mockMetricsSource{
				refreshFunc: func(_ context.Context, _ source.RefreshSpec) (map[string]*source.MetricResult, error) {
					return map[string]*source.MetricResult{
						"kv_cache_usage": {
							Values: []source.MetricValue{
								{
									Labels:    tc.labels,
									Value:     0.5,
									Timestamp: time.Now(),
								},
							},
						},
					}, nil
				},
			}

			collector := NewReplicaMetricsCollector(mockSource, k8sClient, nil, nil, scalerLocator(tc.located))
			results, err := collector.CollectReplicaMetrics(
				context.Background(),
				"test-model",
				"test-ns",
				make(map[string]scaletarget.ScaleTargetAccessor),
				make(map[string]*llmdvariant.Variant),
				nil,
			)
			if err != nil {
				t.Fatalf("CollectReplicaMetrics: %v", err)
			}

			if tc.wantSkipped {
				if len(results) != 0 {
					t.Errorf("expected no results for skipped entry, got %d", len(results))
				}
				return
			}

			if len(results) == 0 {
				t.Fatalf("expected at least one ReplicaMetrics result")
			}

			got := results[0].VariantName
			if got != tc.wantVAName {
				t.Errorf("VariantName: got %q, want %q", got, tc.wantVAName)
			}
		})
	}
}

// mockLocator implements locator.PodLocator for testing.
type mockLocator struct {
	locateFunc       func(ctx context.Context, namespace, podName string) (*locator.ManagedScaler, error)
	getPodLabelsFunc func(ctx context.Context, namespace, podName string) map[string]string
}

func (m *mockLocator) Locate(ctx context.Context, namespace, podName string) (*locator.ManagedScaler, error) {
	if m == nil || m.locateFunc == nil {
		return nil, nil
	}
	return m.locateFunc(ctx, namespace, podName)
}

// TODO(va-removal): remove ResolveScaleTarget from the mock when the CRD-based
// dual-mode fallback (and the interface method) are removed.
func (m *mockLocator) ResolveScaleTarget(_ context.Context, _, _ string) (autoscalingv2.CrossVersionObjectReference, bool, error) {
	return autoscalingv2.CrossVersionObjectReference{}, false, nil
}

func (m *mockLocator) GetPodLabels(ctx context.Context, namespace, podName string) map[string]string {
	if m == nil || m.getPodLabelsFunc == nil {
		return nil
	}
	return m.getPodLabelsFunc(ctx, namespace, podName)
}

// TODO(va-removal): three VA-CRD-based tests were removed here because the
// old CRD and its indexer (VAScaleTargetKey, VAScaleTargetIndexFunc)
// have been deleted. The tests were:
//   - TestBuildInstanceKey_VACRDNameDiffersFromHPAName
//   - TestBuildInstanceKey_UnmanagedHPAFallsBackToVALookup
//   - TestBuildInstanceKey_UnmanagedHPANoMatchingVA
