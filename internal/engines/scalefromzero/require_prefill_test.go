package scalefromzero

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// A per-model requirePrefill lives in an entry under an ARBITRARY key and is
// identified by the model_id and namespace in its body -- a ConfigMap key cannot
// contain "#" or "/". It used to be looked up under "{modelID}#{namespace}", so
// only the default entry ever applied.
func TestRequirePrefillResolvesPerModelAndTierEntries(t *testing.T) {
	const (
		modelID   = "org/model"
		namespace = "tenant"
	)
	on, off := true, false
	group := func(tiers ...string) modelGroup {
		g := modelGroup{namespace: namespace, modelID: modelID}
		for _, tier := range tiers {
			va := llmdvariant.Variant{}
			va.Spec.ScalingPolicy = tier
			g.variants = append(g.variants, va)
		}
		return g
	}

	cases := []struct {
		name    string
		entries map[string]config.ScalingPolicy
		group   modelGroup
		want    bool
	}{
		{
			name: "override under an arbitrary key turns it on",
			entries: map[string]config.ScalingPolicy{
				"default": {},
				"my-model-override": {
					ModelID: modelID, Namespace: namespace,
					ScaleFromZero: &config.ScaleFromZeroEnvelope{RequirePrefill: &on},
				},
			},
			group: group(""),
			want:  true,
		},
		{
			name: "override under an arbitrary key turns the default off",
			entries: map[string]config.ScalingPolicy{
				"default": {ScaleFromZero: &config.ScaleFromZeroEnvelope{RequirePrefill: &on}},
				"pd-model": {
					ModelID: modelID, Namespace: namespace,
					ScaleFromZero: &config.ScaleFromZeroEnvelope{RequirePrefill: &off},
				},
			},
			group: group(""),
			want:  false,
		},
		{
			name: "the selected tier turns it on",
			entries: map[string]config.ScalingPolicy{
				"default":     {},
				"interactive": {ScaleFromZero: &config.ScaleFromZeroEnvelope{RequirePrefill: &on}},
			},
			group: group("interactive"),
			want:  true,
		},
		{
			// Any one tier requiring prefill is enough, wherever it sorts.
			name: "the second of two tiers turns it on",
			entries: map[string]config.ScalingPolicy{
				"default": {},
				"a":       {},
				"b":       {ScaleFromZero: &config.ScaleFromZeroEnvelope{RequirePrefill: &on}},
			},
			group: group("a", "b"),
			want:  true,
		},
		{
			name: "the second of two tiers turns it on, variants in reverse order",
			entries: map[string]config.ScalingPolicy{
				"default": {},
				"a":       {},
				"b":       {ScaleFromZero: &config.ScaleFromZeroEnvelope{RequirePrefill: &on}},
			},
			group: group("b", "a"),
			want:  true,
		},
		{
			name: "no tier requires it",
			entries: map[string]config.ScalingPolicy{
				"default": {},
				"a":       {},
				"b":       {ScaleFromZero: &config.ScaleFromZeroEnvelope{RequirePrefill: &off}},
			},
			group: group("a", "b"),
			want:  false,
		},
		{
			// An untagged variant follows the tagged one, as the thresholds do:
			// the default entry is not folded in beside a named tier.
			name: "an untagged variant follows the tagged tier, not the default",
			entries: map[string]config.ScalingPolicy{
				"default": {ScaleFromZero: &config.ScaleFromZeroEnvelope{RequirePrefill: &on}},
				"pd-gpu":  {ScaleFromZero: &config.ScaleFromZeroEnvelope{RequirePrefill: &off}},
			},
			group: group("pd-gpu", ""),
			want:  false,
		},
		{
			name: "another model's override does not apply",
			entries: map[string]config.ScalingPolicy{
				"default": {},
				"other": {
					ModelID: "org/other", Namespace: namespace,
					ScaleFromZero: &config.ScaleFromZeroEnvelope{RequirePrefill: &on},
				},
			},
			group: group(""),
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.NewTestConfig()
			cfg.UpdateScalingPolicyConfigForNamespace(namespace, tc.entries)
			e := &Engine{config: cfg}
			if got := e.requirePrefill(tc.group); got != tc.want {
				t.Fatalf("requirePrefill = %v, want %v", got, tc.want)
			}
		})
	}
}
