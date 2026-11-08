package external

import (
	"context"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
)

// An external analyzer counts the SAME Pods a built-in one does.
//
// It runs the operator's PromQL and sums the series itself, so it never reaches
// the collector's builder -- which is where deleted and terminating Pods are
// dropped for every other analyzer. Without the gate, a per-Pod query body goes
// on counting a replica for the five minutes Prometheus keeps its series after
// the Pod is gone, and the operator has no way to see it.
func TestTheGateDropsSeriesFromPodsThatAreGone(t *testing.T) {
	values := []source.MetricValue{
		{Labels: map[string]string{"pod": "alive"}, Value: 10},
		{Labels: map[string]string{"pod": "deleted"}, Value: 7},
		{Labels: map[string]string{"pod_name": "also-deleted"}, Value: 5},
	}

	gone := map[string]bool{"deleted": true, "also-deleted": true}
	gate := func(_ context.Context, _ string, labels map[string]string) bool {
		pod := labels["pod"]
		if pod == "" {
			pod = labels["pod_name"]
		}
		return gone[pod]
	}

	var withGate, without float64
	for _, v := range values {
		without += v.Value
		if !gate(context.Background(), "tenant", v.Labels) {
			withGate += v.Value
		}
	}

	if without != 22 {
		t.Fatalf("fixture: ungated sum = %v, want 22", without)
	}
	if withGate != 10 {
		t.Errorf("gated sum = %v, want 10 -- only the live Pod's demand counts", withGate)
	}
}

// An ALREADY-AGGREGATED body is summed untouched.
//
// Prometheus did the reduction, so there is no per-Pod identity left to judge.
// Dropping such a series would silently zero the demand of every analyzer whose
// query aggregates, which is most of the useful ones.
func TestAnAggregatedSeriesIsNotDropped(t *testing.T) {
	gate := func(_ context.Context, _ string, labels map[string]string) bool {
		return labels["pod"] != "" || labels["pod_name"] != ""
	}
	if gate(context.Background(), "tenant", map[string]string{}) {
		t.Error("a series with no Pod label has no Pod to be gone; it must be kept")
	}
}
