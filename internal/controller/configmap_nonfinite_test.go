package controller

import (
	"slices"
	"testing"

	"github.com/go-logr/logr"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
)

// YAML .inf and .nan in priority and in an analyzer's score, typed or under
// parameters, are cleared and reported, and the default entry keeps its
// limiters (ev-shindin/llm-scaling-manager#171).
func TestNonFinitePriorityAndScoreAreClearedAtLoad(t *testing.T) {
	data := map[string]string{
		"default": "priority: .inf\n" +
			"analyzers:\n" +
			"  - name: saturation\n    score: .nan\n" +
			"  - name: throughput\n    parameters:\n      score: .inf\n" +
			"limiters:\n  - type: quota\n    name: team-quota\n    scope: cluster\n    quotas: { H100: 8 }\n",
		"team-b": "model_id: m\nnamespace: team-b\npriority: .nan\n",
	}
	configs, count, rejected, ignored := parseScalingPolicyConfigRejects(data, logr.Discard())
	if count != 2 || len(rejected) != 0 {
		t.Fatalf("parsed %d entries, rejected %v; want both kept", count, rejected)
	}
	fields := make([]string, 0, len(ignored[config.GlobalDefaultsKey]))
	for _, f := range ignored[config.GlobalDefaultsKey] {
		fields = append(fields, f.Field)
	}
	if want := []string{"priority", "analyzers[0].score", "analyzers[1].score"}; !slices.Equal(fields, want) {
		t.Errorf("default entry ignored %v, want %v", fields, want)
	}
	if len(ignored["team-b"]) != 1 || ignored["team-b"][0].Field != "priority" {
		t.Errorf("team-b ignored %+v, want priority", ignored["team-b"])
	}
	def := configs[config.GlobalDefaultsKey]
	if len(def.Limiters) != 1 {
		t.Errorf("default entry limiters = %+v, want the team-quota limiter", def.Limiters)
	}
	got := config.ResolveScalingPolicyForTier(configs, "m", "team-b", "")
	if got.Priority != config.DefaultPriority || got.AnalyzerScore("saturation") != 1 || got.AnalyzerScore("throughput") != 1 {
		t.Errorf("resolved priority %v, scores %v/%v; want the defaults",
			got.Priority, got.AnalyzerScore("saturation"), got.AnalyzerScore("throughput"))
	}
}

// A cleared value inherits the layer below: a tier or per-model entry that
// writes .inf or .nan gets the default entry's value, not the built-in one.
// An analyzers list replaces the default's whole, so a cleared score there
// is the built-in 1.
func TestClearedNonFiniteValueInheritsTheDefaultEntry(t *testing.T) {
	data := map[string]string{
		"default":  "priority: 7\nkvCacheThreshold: 0.8\nanalyzers:\n  - name: saturation\n    score: 3\n",
		"batch":    "priority: .inf\nkvCacheThreshold: .nan\n",
		"team-b":   "model_id: m\nnamespace: team-b\npriority: .nan\n",
		"scored":   "analyzers:\n  - name: saturation\n    score: .inf\n",
		"negative": "analyzers:\n  - name: saturation\n    parameters:\n      score: -2\n",
	}
	configs, _, rejected, ignored := parseScalingPolicyConfigRejects(data, logr.Discard())
	if len(rejected) != 0 {
		t.Fatalf("rejected %v, want every entry kept", rejected)
	}
	tier := config.ResolveScalingPolicyForTier(configs, "other", "ns", "batch")
	if tier.Priority != 7 || tier.KvCacheThreshold != 0.8 || tier.AnalyzerScore("saturation") != 3 {
		t.Errorf("tier resolved priority %v, kvCacheThreshold %v, score %v; want 7, 0.8, 3",
			tier.Priority, tier.KvCacheThreshold, tier.AnalyzerScore("saturation"))
	}
	if got := config.ResolveScalingPolicyForTier(configs, "m", "team-b", ""); got.Priority != 7 {
		t.Errorf("per-model entry resolved priority %v, want the default entry's 7", got.Priority)
	}
	if got := config.ResolveScalingPolicyForTier(configs, "other", "ns", "scored"); got.AnalyzerScore("saturation") != 1 {
		t.Errorf("tier analyzers list resolved score %v, want the built-in 1", got.AnalyzerScore("saturation"))
	}
	if n := ignored["negative"]; len(n) != 1 || n[0].Field != "analyzers[0].score" || n[0].Value != "-2" {
		t.Errorf("negative parameters.score ignored %+v, want analyzers[0].score=-2", n)
	}
}
