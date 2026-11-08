package config

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("analyzer matching by EffectiveType", func() {
	It("matches a type-only analyzer entry (no name) for score/threshold/enabled", func() {
		up := 0.95
		disabled := false
		cfg := ScalingPolicy{
			ScaleUpThreshold:  0.90,
			ScaleDownBoundary: 0.60,
			Analyzers: []AnalyzerScoreConfig{
				{Type: "saturation", Score: 2.0, ScaleUpThreshold: &up},
				{Type: "throughput", Enabled: &disabled},
			},
		}

		Expect(cfg.AnalyzerScore("saturation")).To(Equal(2.0))
		gotUp, _ := cfg.AnalyzerThresholds("saturation")
		Expect(gotUp).To(Equal(0.95), "per-analyzer override resolved via EffectiveType")
		Expect(cfg.AnalyzerEnabled("saturation")).To(BeTrue())
		Expect(cfg.AnalyzerEnabled("throughput")).To(BeFalse(), "type-only disabled entry is honored")
	})
})
