package config

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// A tier selects and weights analyzers by name; these pin that the selection
// actually reaches the engine, which was an open question — the design doc
// records per-tier analyzer type-selection as "engine-future", so it could
// plausibly have been accepted in config and dropped before use.
//
// It does reach it, by way of two mechanisms meeting: Merge replaces the whole
// Analyzers list when a tier declares one, and AnalyzerEnabled reads that
// resolved list. Registration stays global — an analyzer registered but not
// selected by a tier simply does not participate for that tier's models.
func TestATierSelectsAndWeightsAnalyzers(t *testing.T) {
	enabled, disabled := true, false
	cfgMap := map[string]ScalingPolicy{
		"default": {
			ScaleUpThreshold:  0.85,
			ScaleDownBoundary: 0.70,
			Analyzers: []AnalyzerScoreConfig{
				{Name: domain.SaturationAnalyzerName, Enabled: &enabled, Score: 1.0},
				{Name: "throughput", Enabled: &enabled, Score: 1.0},
			},
		},
		// This tier runs saturation only, and weights it higher.
		"latency-first": {
			Analyzers: []AnalyzerScoreConfig{
				{Name: domain.SaturationAnalyzerName, Enabled: &enabled, Score: 3.0},
				{Name: "throughput", Enabled: &disabled},
			},
		},
	}

	base := ResolveScalingPolicyForTier(cfgMap, "m", "ns", "")
	if !base.AnalyzerEnabled("throughput") {
		t.Error("throughput must participate under the default entry")
	}

	tier := ResolveScalingPolicyForTier(cfgMap, "m", "ns", "latency-first")
	if !tier.AnalyzerEnabled(domain.SaturationAnalyzerName) {
		t.Error("saturation must still participate under the tier")
	}
	if tier.AnalyzerEnabled("throughput") {
		t.Error("a tier disabling an analyzer must stop it participating — otherwise the " +
			"selection is accepted in config and silently dropped before use")
	}
	if got := tier.AnalyzerScore(domain.SaturationAnalyzerName); got != 3.0 {
		t.Errorf("score = %v, want the tier's 3.0; weights are how a tier expresses what it "+
			"optimizes for", got)
	}
}

// A tier that says nothing about analyzers inherits the default entry's set,
// rather than silently running none — the list is replaced only when declared.
func TestATierWithoutAnalyzersInheritsThem(t *testing.T) {
	enabled := true
	cfgMap := map[string]ScalingPolicy{
		"default": {
			ScaleUpThreshold:  0.85,
			ScaleDownBoundary: 0.70,
			Analyzers: []AnalyzerScoreConfig{
				{Name: domain.SaturationAnalyzerName, Enabled: &enabled, Score: 1.0},
			},
		},
		"thresholds-only": {ScaleUpThreshold: 0.95, ScaleDownBoundary: 0.80},
	}

	tier := ResolveScalingPolicyForTier(cfgMap, "m", "ns", "thresholds-only")
	if !tier.AnalyzerEnabled(domain.SaturationAnalyzerName) {
		t.Error("a tier that only tunes thresholds must keep the default entry's analyzers")
	}
	if tier.ScaleUpThreshold != 0.95 {
		t.Errorf("scaleUpThreshold = %v, want the tier's 0.95", tier.ScaleUpThreshold)
	}
}
