package config

import "testing"

// A struct copy of a ScalingPolicy shares its Analyzers slice with the stored
// entry. Defaulting the copy must not write through to the original: the stored
// entry is read by many goroutines at once, and would also stop inheriting the
// fields it does not set.
func TestApplyEntryDefaultsLeavesTheOriginalAnalyzersAlone(t *testing.T) {
	stored := ScalingPolicy{Analyzers: []AnalyzerScoreConfig{{Name: "saturation"}, {Name: "slo", Score: 2}}}
	working := stored
	working.ApplyEntryDefaults()

	if working.Analyzers[0].Score != 1.0 || working.Analyzers[0].Enabled == nil {
		t.Fatalf("setup: the copy was not defaulted: %+v", working.Analyzers[0])
	}
	for i, a := range stored.Analyzers {
		if a.Enabled != nil {
			t.Fatalf("stored Analyzers[%d].Enabled = %v, want nil (written through the copy)", i, *a.Enabled)
		}
	}
	if stored.Analyzers[0].Score != 0 {
		t.Fatalf("stored Analyzers[0].Score = %v, want 0 (written through the copy)", stored.Analyzers[0].Score)
	}
}
