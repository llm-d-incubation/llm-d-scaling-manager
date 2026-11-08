package steadystate

import (
	"testing"

	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// The scaling manager scales a MODEL: the optimizer distributes its replicas across its variants
// against one set of thresholds. Variants naming different tiers therefore have no
// correct merge, so the choice must at least be deterministic — an allocation that
// flips tier as map order changes is worse than either tier.
func TestModelPolicyIsDeterministicWhenVariantsDisagree(t *testing.T) {
	vas := []llmdvariant.Variant{
		{Spec: llmdvariant.Spec{ScalingPolicy: "interactive"}},
		{Spec: llmdvariant.Spec{ScalingPolicy: "batch"}},
	}
	chosen, conflicting := modelPolicy(vas)
	if chosen != "batch" {
		t.Errorf("chosen = %q, want the lexicographically smallest (%q) for determinism", chosen, "batch")
	}
	if len(conflicting) != 2 {
		t.Errorf("conflicting = %v, want both tiers reported", conflicting)
	}

	// Reversing the input must not change the outcome.
	reversed, _ := modelPolicy([]llmdvariant.Variant{vas[1], vas[0]})
	if reversed != chosen {
		t.Errorf("input order changed the resolved policy: %q vs %q", reversed, chosen)
	}
}

func TestModelPolicyAgreementIsNotAConflict(t *testing.T) {
	vas := []llmdvariant.Variant{
		{Spec: llmdvariant.Spec{ScalingPolicy: "interactive"}},
		{Spec: llmdvariant.Spec{ScalingPolicy: "interactive"}},
		{Spec: llmdvariant.Spec{}}, // names none: inherits, not a disagreement
	}
	chosen, conflicting := modelPolicy(vas)
	if chosen != "interactive" {
		t.Errorf("chosen = %q, want interactive", chosen)
	}
	if len(conflicting) > 1 {
		t.Errorf("conflicting = %v, want no conflict when the named tiers agree", conflicting)
	}
}
