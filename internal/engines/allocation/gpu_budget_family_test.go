package allocation

import "testing"

// Review A1: the wake check fitted each accelerator key alone. A decode on
// HBM3 and a prefill on PCIe both draw on an H100 family residual: 8 + 8 does
// not fit 12, though each product alone does.
func TestFitsGPUBudgetSumsDemandPerFamilyKey(t *testing.T) {
	cluster := []*ResourceConstraints{
		famPhysical(t, map[string]int{famHBM3: 8, famPCIe: 8}),
		famClusterQuota(t, map[string]int{"H100": 12}),
	}
	if FitsGPUBudget(cluster, "x", map[string]int{famHBM3: 8, famPCIe: 8}) {
		t.Error("cluster: HBM3 8 + PCIe 8 under a family residual of 12 fits, want refused")
	}
	if !FitsGPUBudget(cluster, "x", map[string]int{famHBM3: 8, famPCIe: 4}) {
		t.Error("cluster: HBM3 8 + PCIe 4 under 12 refused, want fits")
	}

	namespaced := []*ResourceConstraints{
		famPhysical(t, map[string]int{famHBM3: 8, famPCIe: 8}),
		famNamespaceQuota(t, map[string]map[string]int{"x": {"H100": 12}}),
	}
	if FitsGPUBudget(namespaced, "x", map[string]int{famHBM3: 8, famPCIe: 8}) {
		t.Error("namespace: HBM3 8 + PCIe 8 under an allowance of 12 fits, want refused")
	}
	if !FitsGPUBudget(namespaced, "x", map[string]int{famHBM3: 8, famPCIe: 4}) {
		t.Error("namespace: HBM3 8 + PCIe 4 under 12 refused, want fits")
	}
}
