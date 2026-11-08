package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestScaleFromZeroEnvelopeYAML round-trips the operator-facing key, so the
// documented spelling stays wired to the field the engine reads.
func TestScaleFromZeroEnvelopeYAML(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want bool
	}{
		{
			name: "requirePrefill true",
			yaml: "model_id: m\nscaleFromZero:\n  requirePrefill: true\n",
			want: true,
		},
		{
			name: "requirePrefill false",
			yaml: "model_id: m\nscaleFromZero:\n  requirePrefill: false\n",
			want: false,
		},
		{
			name: "envelope present but field absent inherits the default",
			yaml: "model_id: m\nscaleFromZero: {}\n",
			want: false,
		},
		{
			name: "no envelope at all",
			yaml: "model_id: m\n",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg ScalingPolicy
			if err := yaml.Unmarshal([]byte(tt.yaml), &cfg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := cfg.RequirePrefillOnScaleFromZero(); got != tt.want {
				t.Fatalf("RequirePrefillOnScaleFromZero() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestMergeCarriesScaleFromZero: Merge is the single overlay every per-entry
// setting resolves through, so a field it forgets is silently unconfigurable
// per model however correct the struct looks.
func TestMergeCarriesScaleFromZero(t *testing.T) {
	tests := []struct {
		name     string
		base     ScalingPolicy
		override ScalingPolicy
		want     bool
	}{
		{
			name:     "override turns it on over a default that leaves it off",
			base:     ScalingPolicy{},
			override: ScalingPolicy{ScaleFromZero: &ScaleFromZeroEnvelope{RequirePrefill: boolPtr(true)}},
			want:     true,
		},
		{
			name:     "override turns it off over a default that has it on",
			base:     ScalingPolicy{ScaleFromZero: &ScaleFromZeroEnvelope{RequirePrefill: boolPtr(true)}},
			override: ScalingPolicy{ScaleFromZero: &ScaleFromZeroEnvelope{RequirePrefill: boolPtr(false)}},
			want:     false,
		},
		{
			// An absent envelope means "inherit", matching ScaleToZeroEnvelope:
			// an override that says nothing about scale-from-zero must not wipe
			// the default's setting.
			name:     "an override without the envelope inherits the default",
			base:     ScalingPolicy{ScaleFromZero: &ScaleFromZeroEnvelope{RequirePrefill: boolPtr(true)}},
			override: ScalingPolicy{},
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			merged := tt.base
			merged.Merge(tt.override)
			if got := merged.RequirePrefillOnScaleFromZero(); got != tt.want {
				t.Fatalf("after Merge, RequirePrefillOnScaleFromZero() = %v, want %v", got, tt.want)
			}
		})
	}
}
