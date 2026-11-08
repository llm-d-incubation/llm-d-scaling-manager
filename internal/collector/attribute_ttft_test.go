package collector

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/locator"
)

// The two guards on TTFT, neither of which had a test.
//
// TTFT feeds the prefill model's fit, so a bad reading does not look like a bad
// reading -- it becomes a point on a line, and the line prices capacity. Both
// guards exist because the same two impossibilities were already found on
// service time: a reading taken while the pod was still loading, and a latency
// longer than the pod has been alive.
//
// Every fail-open path is asserted too. A bound that cannot be established must
// not delete a value it is unable to judge: on an RBAC error the listing fails
// for the whole namespace, and a guard that treated that as "every pod is brand
// new" would discard every TTFT at once.
func TestAttributeInstance_TTFTGuards(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	now := time.Now()

	pod := func(ready bool, startedAgo time.Duration) *corev1.Pod {
		cond := corev1.ConditionFalse
		if ready {
			cond = corev1.ConditionTrue
		}
		started := metav1.NewTime(now.Add(-startedAgo))
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "pod-abc", Namespace: "ns"},
			Status: corev1.PodStatus{
				Phase:      corev1.PodRunning,
				StartTime:  &started,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: cond}},
			},
		}
	}

	newCollector := func(objs ...client.Object) *ReplicaMetricsCollector {
		loc := &mockLocator{
			locateFunc: func(context.Context, string, string) (*locator.ManagedScaler, error) {
				return &locator.ManagedScaler{Namespace: "ns", Name: "decode"}, nil
			},
			getPodLabelsFunc: func(context.Context, string, string) map[string]string {
				return map[string]string{}
			},
		}
		var reader client.Reader
		if objs != nil {
			reader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
		}
		return NewReplicaMetricsCollector(nil, nil, reader, nil, loc)
	}

	data := func(ttft float64) *podMetricData {
		return &podMetricData{
			podName: "pod-abc", vaName: "decode",
			hasKv: true, kvUsage: 0.5,
			avgTTFT: ttft,
		}
	}
	freshness := map[string]map[string]int{}

	ttftOf := func(t *testing.T, c *ReplicaMetricsCollector, ttft float64) float64 {
		t.Helper()
		m, _, ok := c.attributeInstance(ctx, "m", "ns", "pod-abc:8000", data(ttft), now, nil, freshness)
		if !ok {
			t.Fatalf("expected a row for pod-abc")
		}
		return m.AvgTTFT
	}

	t.Run("a not-Ready pod's TTFT is discarded", func(t *testing.T) {
		// A pod still loading weights reports a first-token latency for the
		// handful of requests the router sent it, and it is not the latency the
		// fleet will serve at.
		c := newCollector(pod(false, time.Hour))
		if got := ttftOf(t, c, 0.4); got != 0 {
			t.Errorf("AvgTTFT = %v, want 0 from a not-Ready pod", got)
		}
	})

	t.Run("a TTFT longer than the pod has existed is discarded", func(t *testing.T) {
		// 300s of first-token latency on a pod alive for 120s cannot have been
		// measured by that pod.
		c := newCollector(pod(true, 2*time.Minute))
		if got := ttftOf(t, c, 300); got != 0 {
			t.Errorf("AvgTTFT = %v, want 0 when it exceeds the pod's uptime", got)
		}
	})

	t.Run("a plausible TTFT on a Ready, long-running pod is kept", func(t *testing.T) {
		// The negative control for both guards above: if either fired here,
		// every real reading would be thrown away and the prefill model would
		// never fit.
		c := newCollector(pod(true, time.Hour))
		if got := ttftOf(t, c, 0.4); got != 0.4 {
			t.Errorf("AvgTTFT = %v, want 0.4 kept", got)
		}
	})

	t.Run("the uptime bound does not apply inside the first minute", func(t *testing.T) {
		// minUptimeForServiceTimeBound: a pod seconds old has no uptime worth
		// comparing against, and bounding by it would delete every reading
		// during a scale-up -- exactly when the fit is most needed.
		c := newCollector(pod(true, 10*time.Second))
		if got := ttftOf(t, c, 30); got != 30 {
			t.Errorf("AvgTTFT = %v, want 30 kept: the bound must not apply below minUptimeForServiceTimeBound", got)
		}
	})

	t.Run("an unreadable listing keeps the reading", func(t *testing.T) {
		// No API reader at all. Both guards fail open, or an RBAC error would
		// silently empty the prefill model's input for the whole namespace.
		c := newCollector()
		if got := ttftOf(t, c, 300); got != 300 {
			t.Errorf("AvgTTFT = %v, want 300: an unjudgeable reading must survive", got)
		}
	})
}
