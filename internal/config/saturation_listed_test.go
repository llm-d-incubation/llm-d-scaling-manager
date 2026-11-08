package config

import "testing"

func TestSaturationListed(t *testing.T) {
	off := false
	for _, tc := range []struct {
		name      string
		analyzers []AnalyzerScoreConfig
		want      bool
	}{
		{"no analyzers section", nil, true},
		{"saturation listed", []AnalyzerScoreConfig{{Name: "saturation"}}, true},
		{"saturation with others", []AnalyzerScoreConfig{{Name: "saturation"}, {Name: "throughput"}}, true},
		{"by type", []AnalyzerScoreConfig{{Type: "saturation"}}, true},
		{"others only", []AnalyzerScoreConfig{{Name: "throughput"}}, false},
		{"saturation disabled", []AnalyzerScoreConfig{{Name: "saturation", Enabled: &off}, {Name: "throughput"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := (ScalingPolicy{Analyzers: tc.analyzers}).SaturationListed(); got != tc.want {
				t.Fatalf("SaturationListed() = %v, want %v", got, tc.want)
			}
		})
	}
}
