package config

import "testing"

// A tier or a per-model override that sets the shape-change hold must carry it
// into the resolved policy. Merge copied neither field, so both were silently
// dropped everywhere except the default entry.
func TestMergeCarriesShapeChangeHold(t *testing.T) {
	set := ScalingPolicySet{
		GlobalDefaultsKey: {},
		"long-context":    {ShapeChangeHoldSeconds: 900},
		"no-hold":         {DisableShapeChangeHold: true},
		"one-model":       {ModelID: "m", Namespace: "ns", ShapeChangeHoldSeconds: 1200},
	}

	tier := ResolveScalingPolicyForTier(set, "other", "ns", "long-context")
	if tier.ShapeChangeHoldSeconds != 900 {
		t.Errorf("tier shapeChangeHoldSeconds = %d, want 900", tier.ShapeChangeHoldSeconds)
	}

	off := ResolveScalingPolicyForTier(set, "other", "ns", "no-hold")
	if !off.DisableShapeChangeHold {
		t.Error("tier disableShapeChangeHold was dropped by the merge")
	}

	model := ResolveScalingPolicyForTier(set, "m", "ns", "long-context")
	if model.ShapeChangeHoldSeconds != 1200 {
		t.Errorf("override shapeChangeHoldSeconds = %d, want 1200 (override beats tier)", model.ShapeChangeHoldSeconds)
	}
}

// A set the default disables stays disabled under a tier that does not mention
// the hold: Merge only overlays what an entry sets.
func TestMergeInheritsShapeChangeHoldFromDefault(t *testing.T) {
	set := ScalingPolicySet{
		GlobalDefaultsKey: {DisableShapeChangeHold: true, ShapeChangeHoldSeconds: 300},
		"batch":           {ScaleUpThreshold: 0.95},
	}
	got := ResolveScalingPolicyForTier(set, "m", "ns", "batch")
	if !got.DisableShapeChangeHold || got.ShapeChangeHoldSeconds != 300 {
		t.Errorf("tier lost the default's hold settings: disable=%t seconds=%d",
			got.DisableShapeChangeHold, got.ShapeChangeHoldSeconds)
	}
}
