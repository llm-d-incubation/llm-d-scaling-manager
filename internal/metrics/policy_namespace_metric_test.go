package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

// TestSetPolicyNamespaceUnverified reads the gauge back after each write: 1
// while the controller runs on an unverified policy namespace, 0 once it is
// verified, and one series throughout.
func TestSetPolicyNamespaceUnverified(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}
	read := func() (float64, int) {
		t.Helper()
		families, err := registry.Gather()
		if err != nil {
			t.Fatalf("Gather: %v", err)
		}
		for _, mf := range families {
			if mf.GetName() != constants.LLMSMPolicyNamespaceUnverified {
				continue
			}
			series := mf.GetMetric()
			if len(series) == 0 {
				return 0, 0
			}
			return series[0].GetGauge().GetValue(), len(series)
		}
		t.Fatalf("metric %s not gathered", constants.LLMSMPolicyNamespaceUnverified)
		return 0, 0
	}

	for _, step := range []struct {
		unverified bool
		want       float64
	}{
		{true, 1},
		{false, 0},
		{true, 1},
	} {
		SetPolicyNamespaceUnverified(step.unverified)
		got, n := read()
		if n != 1 {
			t.Fatalf("after Set(%v): %d series, want 1", step.unverified, n)
		}
		if got != step.want {
			t.Errorf("after Set(%v): gauge = %v, want %v", step.unverified, got, step.want)
		}
	}
}
