package allocation

import (
	"context"
	"slices"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// twoPoolCohort is cohort "c" with per-team pools: queue a-cq offers H100 on
// flavor "fa" (nodeLabels pool=a) to namespace a, queue b-cq on flavor "fb"
// (pool=b) to namespace b. Namespace a is counted, guaranteed 8, idle.
func twoPoolCohort(aFlavors []string) *ResourceConstraints {
	chunk := func(flavor, pool string) SharedPoolChunk {
		return SharedPoolChunk{Cohort: "c", Flavor: flavor, Resource: "nvidia.com/gpu",
			FlavorPlacement: kueue.FlavorPlacement{AcceleratorType: borrowH100, NodeLabels: map[string]string{"pool": pool}}}
	}
	c := borrowQuota(0, nsBorrow{ns: "a", guaranteed: 8, cap: 0, wn: 8})
	c.SharedPools = []SharedPoolChunk{chunk("fa", "a"), chunk("fb", "b")}
	b := c.NamespaceBorrow["a"]["H100"]
	b.Flavors = aFlavors
	c.NamespaceBorrow["a"]["H100"] = b
	c.NamespacePools["b"] = map[string]ResourcePool{"H100": {Limit: 8}}
	c.NamespaceBorrow["b"] = map[string]NamespaceBorrow{"H100": {Cap: 0, WithinNominal: 8, Flavors: []string{"fb"}}}
	c.NamespaceCohorts["b"] = []string{"c"}
	return c
}

// Review N2: the flavor a namespace's replicas are admitted on within nominal
// is its own queue's, not every flavor its cohort's chunks name. With per-team
// pools the cohort's chunks disagree (pool=a, pool=b), so the old union gave
// none, and a's guaranteed replica was fitted to b's free node, where Kueue
// never puts it. Whole 8-GPU nodes: a's node is full, b's is free.
func TestGuaranteedReplicaFitsItsOwnQueuesFlavor(t *testing.T) {
	nodes := func(aUsed int) map[string]decision.NodeGPU {
		return map[string]decision.NodeGPU{
			"na": h100Node(aUsed, map[string]string{"pool": "a"}),
			"nb": h100Node(0, map[string]string{"pool": "b"}),
		}
	}
	for _, tc := range []struct {
		name  string
		aUsed int
		want  int
	}{
		{"a's pool=a node is full: refused, not fitted on b's node", 8, 0},
		{"control: a's node is free: placed", 0, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			physical := physicalH100(16)
			physical.Pools["H100"] = ResourcePool{Limit: 16, Used: tc.aUsed}
			o := NewGreedyByScoreOptimizer()
			o.Nodes = NewNodeView(nodes(tc.aUsed))
			ds := o.Optimize(context.Background(), []ModelScalingRequest{borrowModel("a", "m", 8, 0, 1, nil)},
				[]*ResourceConstraints{physical, twoPoolCohort([]string{"fa"})})
			if got := addedGPUs(ds)["m"]; got != tc.want {
				t.Fatalf("a grew %d GPUs, want %d", got, tc.want)
			}
		})
	}

	// The wake reads the same flavor (W5).
	flavorOf := QueueFlavorOf([]*ResourceConstraints{twoPoolCohort([]string{"fa"})}, "a")
	if flavorOf == nil {
		t.Fatal("no queue flavor for a")
	}
	if f := flavorOf(borrowH100); f == nil || f.NodeLabels["pool"] != "a" {
		t.Fatalf("a's wake flavor is %+v, want pool=a", f)
	}
	if f := QueueFlavorOf([]*ResourceConstraints{twoPoolCohort([]string{"fa"})}, "b")(borrowH100); f == nil || f.NodeLabels["pool"] != "b" {
		t.Fatalf("b's wake flavor is %+v, want pool=b", f)
	}
}

// Review NEW-3: a queue that lists a peer's flavor at nominal 0, only to
// borrow on it, does not offer it to its own guaranteed replicas. a-cq lists
// fa at 16 and fb at 0 (to borrow b's pool); b-cq lists fb at 16. a's pool=a
// node is full and b's is free: a's guaranteed replica is refused, not fitted
// on pool b, where Kueue never admits it within nominal. Both read paths --
// counted (NamespaceBorrow) and read only (NominalFigures) -- name fa alone.
func TestZeroNominalFlavorIsNotTheQueuesOwn(t *testing.T) {
	entry := config.QuotaLimiterConfig{Name: "namespace-quota", Type: "quota", Scope: config.QuotaScopeNamespace,
		NamespaceQuotas: map[string]map[string]int{"a": {"H100": -1}},
		Kueue:           &config.KueueQuotaSource{Enabled: true}}
	q := NewQuotaInventoryWithSource(entry, &fakeQuotaSource{})
	rows := []config.ExternalBorrow{
		{Queue: "a-cq", Cohort: "c", Flavor: "fa", Resource: "nvidia.com/gpu", Accelerator: "H100", Nominal: 16, Guaranteed: 16, WithinNominal: 16},
		{Queue: "a-cq", Cohort: "c", Flavor: "fb", Resource: "nvidia.com/gpu", Accelerator: "H100", Nominal: 0, Guaranteed: 0, WithinNominal: 0},
	}
	b, ok := q.namespaceBorrow("a", "H100", 16, rows, nil)
	if !ok || !slices.Equal(b.Flavors, []string{"fa"}) {
		t.Fatalf("counted flavors %v (ok %v), want [fa]: fb is listed at nominal 0", b.Flavors, ok)
	}
	if got := rowFlavors(rowsOfType(rows, "H100")); !slices.Equal(got, []string{"fa"}) {
		t.Fatalf("read-only flavors %v, want [fa]", got)
	}
	physical := physicalH100(16)
	physical.Pools["H100"] = ResourcePool{Limit: 16, Used: 8}
	o := NewGreedyByScoreOptimizer()
	o.Nodes = NewNodeView(map[string]decision.NodeGPU{
		"na": h100Node(8, map[string]string{"pool": "a"}),
		"nb": h100Node(0, map[string]string{"pool": "b"}),
	})
	ds := o.Optimize(context.Background(), []ModelScalingRequest{borrowModel("a", "m", 8, 0, 1, nil)},
		[]*ResourceConstraints{physical, twoPoolCohort(b.Flavors)})
	if got := addedGPUs(ds)["m"]; got != 0 {
		t.Fatalf("a grew %d GPUs on pool b's node, want 0: Kueue admits a's guaranteed replica on fa (pool=a) only", got)
	}
}

// The flavors reach NamespaceBorrow from the namespace's own rows.
func TestNamespaceBorrowCarriesItsRowsFlavors(t *testing.T) {
	entry := config.QuotaLimiterConfig{Name: "namespace-quota", Type: "quota", Scope: config.QuotaScopeNamespace,
		NamespaceQuotas: map[string]map[string]int{"default": {"H100": -1}},
		Kueue:           &config.KueueQuotaSource{Enabled: true}}
	q := NewQuotaInventoryWithSource(entry, &fakeQuotaSource{})
	rows := []config.ExternalBorrow{
		{Queue: "a-cq", Cohort: "c", Flavor: "fa", Resource: "nvidia.com/gpu", Accelerator: "H100", Nominal: 8, Guaranteed: 8, WithinNominal: 8},
		{Queue: "a-cq", Cohort: "c", Flavor: "fa", Resource: "nvidia.com/gpu", Accelerator: "H100", Nominal: 8, Guaranteed: 8, WithinNominal: 8},
	}
	b, ok := q.namespaceBorrow("a", "H100", 8, rows, nil)
	if !ok || !slices.Equal(b.Flavors, []string{"fa"}) {
		t.Fatalf("flavors %v (ok %v), want [fa]", b.Flavors, ok)
	}
}
