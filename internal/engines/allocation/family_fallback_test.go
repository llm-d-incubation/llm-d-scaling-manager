package allocation

import "testing"

// Review A2(a): an allowlist that lists a family key EXPLICITLY admits every
// product of the family it does not list, up to the family's bound; a listed
// product is a sub-cap. The merged map alone cannot tell an explicit family key
// from one computed from products, so FindBudgetKey denied every unlisted
// product beside a listed sibling: a quota of {H100: 8, HBM3: 4} denied SXM5.
func TestExplicitFamilyKeyAdmitsAnUnlistedProduct(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T) []*ResourceConstraints
	}{
		{"cluster-scope quota", func(t *testing.T) []*ResourceConstraints {
			return []*ResourceConstraints{famClusterQuota(t, map[string]int{"H100": 8, famHBM3: 4})}
		}},
		{"namespace-scope quota", func(t *testing.T) []*ResourceConstraints {
			return []*ResourceConstraints{famNamespaceQuota(t, map[string]map[string]int{"x": {"H100": 8, famHBM3: 4}})}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := famTargets(c.build(t), famWanting("x", "sxm", famSXM5, 20, 1)); got["sxm-v"] != 8 {
				t.Errorf("SXM5 alone: %d, want 8 (the family's bound; SXM5 is not listed, H100 is)", got["sxm-v"])
			}
			if got := famTargets(c.build(t), famWanting("x", "hbm", famHBM3, 20, 1)); got["hbm-v"] != 4 {
				t.Errorf("HBM3 alone: %d, want 4 (its own sub-cap)", got["hbm-v"])
			}
			got := famTargets(c.build(t), famWanting("x", "sxm", famSXM5, 20, 1), famWanting("x", "hbm", famHBM3, 20, 1))
			if got["sxm-v"]+got["hbm-v"] > 8 || got["hbm-v"] > 4 {
				t.Errorf("together: SXM5 %d + HBM3 %d, want at most 8 in all and HBM3 at most 4", got["sxm-v"], got["hbm-v"])
			}
		})
	}
}

// The other half of A2(a), kept: a family key the merge COMPUTED from an
// allowlist of products is the bound on their sum, not a grant of the family.
func TestComputedFamilyKeyKeepsTheDeny(t *testing.T) {
	constraints := []*ResourceConstraints{
		famPhysical(t, map[string]int{famHBM3: 8, famPCIe: 8}),
		famClusterQuota(t, map[string]int{famHBM3: 4, famSXM5: 4}),
	}
	if got := famTargets(constraints, famWanting("x", "pcie", famPCIe, 4, 1)); got["pcie-v"] != 0 {
		t.Errorf("PCIe under a quota naming HBM3 and SXM5 only: %d, want 0", got["pcie-v"])
	}
}

// Review A2(b): a workload that names the bare family, under an allowance of
// several of its products, is bounded by their SUM. It got slices.Min of the
// product keys -- one product's figure -- and debited that product alone.
// Whole 8-GPU units: an allowance of 8 HBM3 + 8 SXM5 holds two. (The physical
// inventory keys every product by its family already, so the case is a quota
// that names products.)
func TestFamilyNamedWorkloadGetsTheFamilySum(t *testing.T) {
	for _, c := range []struct {
		name  string
		build func() []*ResourceConstraints
	}{
		{"cluster-scope quota", func() []*ResourceConstraints {
			return []*ResourceConstraints{famClusterQuota(t, map[string]int{famHBM3: 8, famSXM5: 8})}
		}},
		{"namespace-scope quota", func() []*ResourceConstraints {
			return []*ResourceConstraints{famNamespaceQuota(t, map[string]map[string]int{
				"x": {famHBM3: 8, famSXM5: 8}, "y": {famHBM3: 8, famSXM5: 8}})}
		}},
		{"beside a physical limiter", func() []*ResourceConstraints {
			return []*ResourceConstraints{famPhysical(t, map[string]int{famHBM3: 8, famSXM5: 8}),
				famClusterQuota(t, map[string]int{famHBM3: 8, famSXM5: 8})}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := famTargets(c.build(), famWanting("x", "fam", "H100", 4, 8)); got["fam-v"] != 2 {
				t.Errorf("H100-named workload under 8 HBM3 + 8 SXM5: %d replicas of 8, want 2", got["fam-v"])
			}
			// It debits through the family: what it takes is gone for a
			// product workload in the same pass.
			got := famTargets(c.build(), famWanting("x", "fam", "H100", 4, 8), famWanting("x", "hbm", famHBM3, 4, 8))
			if total := (got["fam-v"] + got["hbm-v"]) * 8; total > 16 {
				t.Errorf("family %d + HBM3 %d replicas of 8 = %d GPUs, the allowance is 16", got["fam-v"], got["hbm-v"], total)
			}
		})
	}
}
