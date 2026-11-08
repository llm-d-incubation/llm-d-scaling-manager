package allocation

import (
	"context"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/gpunodes"
)

// These tests pin what happens to GPU usage reported against an UNRESOLVED
// accelerator, which is a real state: gpuUsageByType keys usage by
// VariantMetadata.AcceleratorName, and accelerator.GetAcceleratorNameFromScaleTarget
// falls back to constants.DefaultAcceleratorName ("unknown") when a workload
// carries neither a nodeSelector/nodeAffinity GPU key nor the
// inference.optimization/acceleratorName label.
//
// The behaviour is a KNOWN LIMITATION rather than something these tests assert
// as desirable: usage that cannot be attributed to a type cannot be charged to a
// pool, and the durable fix is to resolve the accelerator (see the
// accelerator-silent-failure and synthetic-VA notes). They exist so the gap is
// documented and cannot change silently, since both the GPU-aware optimizer and
// the scale-from-zero placement check read these pools.
//
// The limitation is now scoped to HETEROGENEOUS clusters. Where exactly one
// accelerator type was discovered the usage is attributable by deduction — there
// is nowhere else it could be — and TypeInventory folds it in. Remaining
// permissive elsewhere is deliberate: denying on unattributable usage would let
// one mislabelled variant block scaling for every other workload. A quota
// limiter can bound it explicitly with an "unknown" entry, and the amount is
// always reported as llmsm_unattributed_gpus.

// TestUnresolvedUsageNeverReachesAnyPool: GetResourcePools iterates the
// DISCOVERED types, so a usage entry under a placeholder key has nowhere to land
// and simply disappears. The per-type budgets the optimizer consumes therefore
// over-state free capacity by however many GPUs unresolved variants hold.
func TestUnresolvedUsageNeverReachesAnyPool(t *testing.T) {
	// HETEROGENEOUS on purpose. With a single discovered type the unresolved GPUs
	// are attributable by deduction and are folded in — see
	// "TypeInventory unresolved-accelerator attribution". The gap this test pins
	// is the one that remains when there is more than one type to choose between.
	disc := &mockDiscovery{inventory: map[string]map[string]gpunodes.AcceleratorModelInfo{
		"node-a": {"H100": {Count: 8}},
		"node-b": {"A100": {Count: 4}},
	}}
	inv := NewTypeInventory("test", disc)
	if err := inv.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	// 2 GPUs on a resolved type, 4 held by variants whose accelerator is unknown.
	inv.SetUsed(map[string]int{"H100": 2, constants.DefaultAcceleratorName: 4})

	pools := inv.GetResourcePools()
	if _, ok := pools[constants.DefaultAcceleratorName]; ok {
		t.Fatalf("pools unexpectedly contain a %q entry: %v", constants.DefaultAcceleratorName, pools)
	}

	h100, ok := pools["H100"]
	if !ok {
		t.Fatal("pools should contain the discovered H100 type")
	}
	if h100.Used != 2 {
		t.Fatalf("H100 Used = %d, want 2", h100.Used)
	}
	// 6 of 8 GPUs are actually occupied, but the budget the optimizer reads says 6
	// are free. This is the limitation.
	if got := h100.Available(); got != 6 {
		t.Fatalf("H100 Available = %d, want 6 (the over-stated budget this test documents)", got)
	}
}

// TestUnattributableUsageIsExcludedConsistently: usage that matches no
// discovered type is dropped rather than stored, so the per-type pools and the
// TotalUsed aggregate agree.
//
// Previously SetUsed summed EVERY key into totalUsed while GetResourcePools
// iterated the discovered types only, so the two views of one inventory
// disagreed in opposite directions — pools under-counting, totals over-counting.
// Nothing read the totals, so it was latent; it is now consistent by
// construction.
func TestUnattributableUsageIsExcludedConsistently(t *testing.T) {
	// Heterogeneous, for the same reason as above.
	disc := &mockDiscovery{inventory: map[string]map[string]gpunodes.AcceleratorModelInfo{
		"node-a": {"H100": {Count: 8}},
		"node-b": {"A100": {Count: 4}},
	}}
	inv := NewTypeInventory("test", disc)
	if err := inv.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	inv.SetUsed(map[string]int{"H100": 2, constants.DefaultAcceleratorName: 4})

	poolUsed := 0
	for _, p := range inv.GetResourcePools() {
		poolUsed += p.Used
	}
	if poolUsed != inv.TotalUsed() {
		t.Fatalf("per-type pools account for %d used but TotalUsed is %d; the two views of one "+
			"inventory must agree", poolUsed, inv.TotalUsed())
	}
	if inv.TotalUsed() != 2 {
		t.Fatalf("TotalUsed = %d, want 2 (the unattributable 4 is not counted)", inv.TotalUsed())
	}
}

// TestUsageIsReconciledOntoPoolKeys is the fix for the defect that made the GPU
// budgets inert: usage arrives keyed the way a WORKLOAD declares its accelerator
// (the raw product label, for the common nodeSelector deployment) while limits
// are keyed by normalized short names. Unreconciled, Used stayed 0 and every
// pool claimed to be entirely free however much was running.
func TestUsageIsReconciledOntoPoolKeys(t *testing.T) {
	disc := &mockDiscovery{inventory: map[string]map[string]gpunodes.AcceleratorModelInfo{
		"node-a": {"NVIDIA-A100-PCIE-80GB": {Count: 8}}, // pool key becomes "A100"
		"node-b": {"Intel-Gaudi-2-96GB": {Count: 4}},    // pool key becomes "Gaudi-2"
	}}
	inv := NewTypeInventory("test", disc)
	if err := inv.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	inv.SetUsed(map[string]int{
		"NVIDIA-A100-PCIE-80GB": 5, // declared as a product label
		"Gaudi-2":               3, // declared as an already-short name with a hyphen
	})

	pools := inv.GetResourcePools()
	if got := pools["A100"].Used; got != 5 {
		t.Fatalf("A100 Used = %d, want 5 — a product-label key must land on its normalized pool", got)
	}
	if got := pools["A100"].Available(); got != 3 {
		t.Fatalf("A100 Available = %d, want 3", got)
	}
	// "Gaudi-2" must match directly: NormalizeAcceleratorName("Gaudi-2") is "2",
	// so normalizing unconditionally would file it under a type that does not exist.
	if got := pools["Gaudi-2"].Used; got != 3 {
		t.Fatalf("Gaudi-2 Used = %d, want 3 — a hyphenated short name must match directly", got)
	}
}

// TestPoolKeysAreShortNames is the inventory half of the candidate/pool key
// agreement. Pools are keyed by NORMALIZED short names, so anything compared
// against them must be normalized too — when the scale-from-zero candidate side
// used raw nodeSelector values, a variant selecting GPUs by product label
// matched no pool and was denied a wake.
//
// The expected literals here are the same ones
// TestCandidateAcceleratorIsInPoolKeySpace asserts from the candidate side: if
// either side stops normalizing, one of the two fails.
func TestPoolKeysAreShortNames(t *testing.T) {
	disc := &mockDiscovery{inventory: map[string]map[string]gpunodes.AcceleratorModelInfo{
		"node-a": {"NVIDIA-A100-PCIE-80GB": {Count: 8}},
		"node-b": {"AMD-MI300X-192G": {Count: 4}},
	}}
	inv := NewTypeInventory("test", disc)
	if err := inv.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	pools := inv.GetResourcePools()
	for _, want := range []string{"A100", "MI300X"} {
		if _, ok := pools[want]; !ok {
			t.Fatalf("pools = %v, want a %q key (pool keys are normalized short names)", pools, want)
		}
	}
	if _, raw := pools["NVIDIA-A100-PCIE-80GB"]; raw {
		t.Fatal("pools are keyed by the raw product name; candidates normalize, so they would never match")
	}
}

// TestConstraintProvidersFrom pins the unwrap both engines now share.
func TestConstraintProvidersFrom(t *testing.T) {
	direct := &DefaultLimiter{name: "gpu-limiter"}

	t.Run("nil limiter", func(t *testing.T) {
		if got := ConstraintProvidersFrom(nil); got != nil {
			t.Fatalf("got %v, want nil", got)
		}
	})

	t.Run("a limiter that is itself a provider", func(t *testing.T) {
		got := ConstraintProvidersFrom(direct)
		if len(got) != 1 || got[0].Name() != "gpu-limiter" {
			t.Fatalf("got %v, want the limiter itself", got)
		}
	})

	t.Run("a composite yields each constituent that provides constraints", func(t *testing.T) {
		// A composite is NOT itself a ConstraintProvider, so it must be unwrapped
		// rather than returned whole — otherwise a multi-entry quota config would
		// be consulted as one opaque provider.
		got := ConstraintProvidersFrom(NewCompositeLimiter("composite",
			[]Limiter{direct, &NoOpLimiter{}}))
		if len(got) != 1 || got[0].Name() != "gpu-limiter" {
			t.Fatalf("got %v, want only the constraint-providing constituent", got)
		}
	})
}

// TestQuotaUsageIsReconciledOntoQuotaKeys is the quota half of the key
// reconciliation. A quota whose Used stays 0 reports its full allowance free
// however much is running, so the cap never binds — the one thing a quota exists
// to do. Quota keys are whatever the operator typed; usage arrives keyed by the
// workload's declaration.
func TestQuotaUsageIsReconciledOntoQuotaKeys(t *testing.T) {
	inv := NewQuotaInventory(config.QuotaLimiterConfig{
		Name:          "cluster-quota",
		Scope:         config.QuotaScopeCluster,
		ClusterQuotas: map[string]int{"A100": 8},
	})

	// The workload declares its accelerator by product label.
	inv.SetUsed(map[string]int{"NVIDIA-A100-PCIE-80GB": 8})

	pools := inv.GetResourcePools()
	if got := pools["A100"].Used; got != 8 {
		t.Fatalf("A100 Used = %d, want 8 — a product-label usage key must land on the quota key", got)
	}
	if got := pools["A100"].Available(); got != 0 {
		t.Fatalf("A100 Available = %d, want 0; the quota is fully consumed and must bind", got)
	}
	if got := inv.TotalUsed(); got != 8 {
		t.Fatalf("TotalUsed = %d, want 8", got)
	}
}

// TestNamespaceQuotaUsageIsReconciled: same for the namespace scope, per the
// namespace's own configured keys.
func TestNamespaceQuotaUsageIsReconciled(t *testing.T) {
	inv := NewQuotaInventory(config.QuotaLimiterConfig{
		Name:            "ns-quota",
		Scope:           config.QuotaScopeNamespace,
		NamespaceQuotas: map[string]map[string]int{"team-a": {"A100": 4}},
	})

	inv.SetUsedByNamespace(map[string]map[string]int{
		"team-a": {"NVIDIA-A100-PCIE-80GB": 4},
	})

	pools := inv.NamespaceResourcePools([]string{"team-a"})
	if got := pools["team-a"]["A100"].Used; got != 4 {
		t.Fatalf("team-a A100 Used = %d, want 4", got)
	}
	if got := pools["team-a"]["A100"].Available(); got != 0 {
		t.Fatalf("team-a A100 Available = %d, want 0; the namespace quota must bind", got)
	}
}
