package controller

import (
	"testing"

	"github.com/go-logr/logr"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
)

// A tier or a per-model override that does not mention kvCacheThreshold,
// queueLengthThreshold or priority must inherit them from the default entry.
// Defaulting every parsed entry to 0.80 / 5 / 1.0 made each one "set", so the
// merge always took the entry's built-in default over the operator's tuning.
func TestParsedEntriesInheritCapacityThresholdsAndPriority(t *testing.T) {
	data := map[string]string{
		"default": "kvCacheThreshold: 0.9\nqueueLengthThreshold: 8\npriority: 3\n" +
			"analyzers:\n  - name: saturation\n",
		"batch":     "scaleUpThreshold: 0.95\n",
		"one-model": "model_id: m\nnamespace: ns\nscaleDownBoundary: 0.5\n",
	}
	configs, count := parseScalingPolicyConfig(data, logr.Discard())
	if count != 3 {
		t.Fatalf("parsed %d entries, want 3", count)
	}

	for _, tc := range []struct {
		name         string
		model, tier  string
		wantUp       float64
		wantDownBand float64
	}{
		{name: "tier", model: "other", tier: "batch", wantUp: 0.95, wantDownBand: 0.70},
		{name: "override", model: "m", tier: "", wantUp: 0.85, wantDownBand: 0.5},
	} {
		got := config.ResolveScalingPolicyForTier(configs, tc.model, "ns", tc.tier)
		if got.KvCacheThreshold != 0.9 {
			t.Errorf("%s: kvCacheThreshold = %v, want the default's 0.9", tc.name, got.KvCacheThreshold)
		}
		if got.QueueLengthThreshold != 8 {
			t.Errorf("%s: queueLengthThreshold = %v, want the default's 8", tc.name, got.QueueLengthThreshold)
		}
		if got.Priority != 3 {
			t.Errorf("%s: priority = %v, want the default's 3", tc.name, got.Priority)
		}
		if got.ScaleUpThreshold != tc.wantUp || got.ScaleDownBoundary != tc.wantDownBand {
			t.Errorf("%s: band = (%v, %v), want (%v, %v)", tc.name,
				got.ScaleUpThreshold, got.ScaleDownBoundary, tc.wantUp, tc.wantDownBand)
		}
	}

	// With nothing set anywhere, the resolved policy still gets the built-in values.
	bare, _ := parseScalingPolicyConfig(map[string]string{"default": "scaleUpThreshold: 0.8\n"}, logr.Discard())
	got := config.ResolveScalingPolicyForTier(bare, "m", "ns", "")
	if got.KvCacheThreshold != config.DefaultKvCacheThreshold ||
		got.QueueLengthThreshold != config.DefaultQueueLengthThreshold ||
		got.Priority != config.DefaultPriority {
		t.Errorf("unset fields resolved to (%v, %v, %v), want the built-in defaults",
			got.KvCacheThreshold, got.QueueLengthThreshold, got.Priority)
	}
}

// A tier or per-model override that says only `analyzerName: saturation` must
// not replace the default entry's band or analyzer list. Defaulting each
// stored V2-shaped entry gave it the built-in band (0.85/0.70) and a
// saturation-only analyzer list, which the merge then copied over the
// default's tuned 0.9 and its throughput analyzer.
func TestParsedEntriesInheritBandAndAnalyzers(t *testing.T) {
	data := map[string]string{
		"default": "scaleUpThreshold: 0.9\nscaleDownBoundary: 0.6\n" +
			"analyzers:\n  - name: saturation\n  - name: throughput\n",
		"batch":     "analyzerName: saturation\nkvCacheThreshold: 0.7\n",
		"one-model": "model_id: m\nnamespace: ns\nanalyzerName: saturation\nkvCacheThreshold: 0.7\n",
	}
	configs, count := parseScalingPolicyConfig(data, logr.Discard())
	if count != 3 {
		t.Fatalf("parsed %d entries, want 3", count)
	}
	for _, tc := range []struct{ name, model, tier string }{
		{name: "tier", model: "other", tier: "batch"},
		{name: "override", model: "m", tier: ""},
	} {
		got := config.ResolveScalingPolicyForTier(configs, tc.model, "ns", tc.tier)
		if got.KvCacheThreshold != 0.7 {
			t.Errorf("%s: kvCacheThreshold = %v, want the entry's 0.7", tc.name, got.KvCacheThreshold)
		}
		if got.ScaleUpThreshold != 0.9 || got.ScaleDownBoundary != 0.6 {
			t.Errorf("%s: band = (%v, %v), want the default's (0.9, 0.6)", tc.name,
				got.ScaleUpThreshold, got.ScaleDownBoundary)
		}
		if !got.AnalyzerEnabled("saturation") || !got.AnalyzerEnabled("throughput") || len(got.Analyzers) != 2 {
			t.Errorf("%s: analyzers = %+v, want the default's saturation and throughput", tc.name, got.Analyzers)
		}
	}

	// The stored entries are left as written; only the resolved policy is defaulted.
	if b := configs["batch"]; b.ScaleUpThreshold != 0 || len(b.Analyzers) != 0 {
		t.Errorf("stored tier was defaulted: band %v, analyzers %+v", b.ScaleUpThreshold, b.Analyzers)
	}
	// A V2-shaped tier on its own still resolves to a full V2 policy.
	alone, _ := parseScalingPolicyConfig(map[string]string{"batch": "analyzerName: saturation\n"}, logr.Discard())
	got := config.ResolveScalingPolicyForTier(alone, "m", "ns", "batch")
	if got.ScaleUpThreshold != config.DefaultScaleUpThreshold || len(got.Analyzers) != 1 ||
		got.Analyzers[0].Score != 1 || got.Analyzers[0].Enabled == nil || !*got.Analyzers[0].Enabled {
		t.Errorf("lone V2 tier resolved to band %v, analyzers %+v; want the built-in defaults",
			got.ScaleUpThreshold, got.Analyzers)
	}
}

// A bad scaleToZero.retentionPeriod in the default entry must not drop the
// entry. Dropping it drops its limiters, which turns off GPU quota enforcement
// over a typo in an unrelated field.
func TestDefaultEntryWithBadRetentionKeepsLimiters(t *testing.T) {
	data := map[string]string{
		"default": "scaleToZero:\n  retentionPeriod: 5 minutes\n" +
			"shapeChangeHoldSeconds: -10\n" +
			"limiters:\n  - type: quota\n    name: team-quota\n    scope: cluster\n    quotas: { H100: 8 }\n",
	}
	configs, count, rejected, _ := parseScalingPolicyConfigRejects(data, logr.Discard())
	if count != 1 || len(rejected) != 0 {
		t.Fatalf("parsed %d entries, rejected %v; want the default entry kept", count, rejected)
	}
	def := configs[config.GlobalDefaultsKey]
	if len(def.Limiters) != 1 || def.Limiters[0].Name != "team-quota" {
		t.Errorf("default entry limiters = %+v, want the team-quota limiter", def.Limiters)
	}
	if def.ScaleToZero == nil || def.ScaleToZero.RetentionPeriod != "" {
		t.Errorf("bad retentionPeriod was kept: %+v", def.ScaleToZero)
	}
	if def.ShapeChangeHoldSeconds != 0 {
		t.Errorf("negative shapeChangeHoldSeconds was kept: %d", def.ShapeChangeHoldSeconds)
	}
	if got := config.ResolveScaleToZeroRetention(&def); got != config.DefaultScaleToZeroRetentionPeriod {
		t.Errorf("retention = %v, want the built-in default %v", got, config.DefaultScaleToZeroRetentionPeriod)
	}
}
