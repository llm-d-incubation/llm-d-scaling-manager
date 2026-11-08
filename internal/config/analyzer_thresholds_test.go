package config

import (
	"slices"
	"testing"
)

// InertAnalyzerThresholds names, by entry name, each enabled analyzer other than saturation whose
// entry sets a band, typed or folded from parameters, and never saturation,
// whose band decides.
func TestInertAnalyzerThresholds(t *testing.T) {
	up, down := 0.5, 0.4
	disabled := false
	policy := ScalingPolicy{Analyzers: []AnalyzerScoreConfig{
		{Name: "saturation", ScaleUpThreshold: &up, ScaleDownBoundary: &down},
		{Name: "throughput", ScaleUpThreshold: &up},
		{Name: "slo"},
		{Name: "queue", Type: "external", Parameters: map[string]any{"scaleDownBoundary": 0.3}},
		{Name: "ttft", Type: "external", ScaleUpThreshold: &up},
		{Name: "off", Type: "external", ScaleUpThreshold: &up, Enabled: &disabled},
	}}
	for i := range policy.Analyzers {
		if err := policy.Analyzers[i].Normalize(); err != nil {
			t.Fatalf("Normalize: %v", err)
		}
	}
	got := policy.InertAnalyzerThresholds()
	if want := []string{"throughput", "queue", "ttft"}; !slices.Equal(got, want) {
		t.Errorf("InertAnalyzerThresholds() = %v, want %v", got, want)
	}
	if got := (ScalingPolicy{}).InertAnalyzerThresholds(); got != nil {
		t.Errorf("no analyzers: got %v, want none", got)
	}
}
