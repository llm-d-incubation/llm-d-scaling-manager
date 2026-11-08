package steadystate

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// A borrowed replica whose pod exists but waits at Kueue's admission gate is
// held once on each axis: its hole on a node by the in-flight node ledger
// (allocation.DefaultInFlight), and its GPUs in the cohort's pool by the
// gated-pod deduction -- not also by the borrowed draw it came from, which
// resolves once the pod exists, and not by the ledger on the pool or by the
// deduction on the nodes. In whole 8-GPU units: Kueue reports 24 free in the
// pool and three of the flavor's 8-GPU nodes are free; model ma in namespace
// a borrowed one replica last cycle, now one gated pod. A second borrower, mb
// in namespace b, sees 16 in the pool and two holes.
//
// It uses the process-wide ledger and headroom stores, so it does not run in
// parallel.
func TestAGatedBorrowedReplicaIsDebitedOnce(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(20_000, 0)
	allocation.DefaultInFlight.Reset()
	t.Cleanup(func() {
		allocation.DefaultInFlight.Reset()
		decision.DefaultHeadroom.Reset()
	})

	flavor := kueue.FlavorPlacement{AcceleratorType: "H100", NodeLabels: map[string]string{"pool": "a"}}
	poolA := map[string]string{"pool": "a"}
	nodes := map[string]decision.NodeGPU{
		"n1": {Accelerator: "H100", Capacity: 8, Labels: poolA},
		"n2": {Accelerator: "H100", Capacity: 8, Labels: poolA},
		"n3": {Accelerator: "H100", Capacity: 8, Labels: poolA},
	}
	store := &decision.NodeGPUStore{}
	store.Publish(nodes, now)

	constraints := func() []*allocation.ResourceConstraints {
		quota := &allocation.ResourceConstraints{
			ProviderName:    "namespace-quota",
			NamespaceScoped: true,
			NamespacePools: map[string]map[string]allocation.ResourcePool{
				"a": {"H100": {Limit: 0}}, "b": {"H100": {Limit: 0}},
			},
			NamespaceBorrow: map[string]map[string]allocation.NamespaceBorrow{
				"a": {"H100": {Cap: config.QuotaUnlimited}}, "b": {"H100": {Cap: config.QuotaUnlimited}},
			},
			NamespaceCohorts: map[string][]string{"a": {"c"}, "b": {"c"}},
			SharedPools: []allocation.SharedPoolChunk{{Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu",
				FlavorPlacement: flavor, Free: 24, ReadAt: now,
				Lenders: []kueue.LenderState{{Queue: "lender"}}}},
		}
		physical := &allocation.ResourceConstraints{ProviderName: allocation.PhysicalLimiterName,
			Pools: map[string]allocation.ResourcePool{"H100": {Limit: 24}}}
		return []*allocation.ResourceConstraints{physical, quota}
	}

	// ma's pod: created, gated, holding no node.
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	gpus := corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")}
	gated := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "ma-v-0", Namespace: "a"},
		Spec: corev1.PodSpec{
			SchedulingGates: []corev1.PodSchedulingGate{{Name: kueue.AdmissionGate}},
			Containers:      []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{Requests: gpus, Limits: gpus}}},
		},
	}
	cfg := config.NewTestConfig()
	cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
		"default": {Limiters: []config.QuotaLimiterConfig{{Type: "gpu-inventory", Name: "gpu"}}},
	})
	e := &Engine{Config: cfg, client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(gated).Build()}

	// Last cycle: ma borrowed one replica, recorded as a draw in flight and,
	// on the nodes, admitted to the ledger on its flavor.
	e.borrowing.inFlight = allocation.NewBorrowInFlight()
	e.borrowing.inFlight.Record([]allocation.BorrowDraw{{Namespace: "a", ModelID: "ma", Variant: "ma-v",
		Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu", GPUs: 8, Replicas: 1, Target: 1, Placement: &flavor}})
	allocation.DefaultInFlight.Admit("a/ma-v", allocation.ReplicaShape{Variant: "a/ma-v", Accelerator: "H100", GPUs: 8,
		Flavor: &flavor}, 1, 0, nil, now.Add(-time.Minute))

	// This cycle ma's pod exists (current 1): the draw resolves, the gated
	// pod is what holds the pool now.
	ma := allocation.ModelScalingRequest{ModelID: "ma", Namespace: "a",
		VariantStates: []domain.VariantReplicaState{{VariantName: "ma-v", CurrentReplicas: 1, DesiredReplicas: 1, GPUsPerReplica: 8}}}
	mb := borrowingRequest("b", "mb", 3)
	cs := e.prepareBorrowing(ctx, constraints(), []allocation.ModelScalingRequest{ma, mb}, nil, now)

	if got := e.borrowing.inFlight.Entries(); len(got) != 0 {
		t.Fatalf("the draw is still in flight once its pod exists: %+v", got)
	}
	var free int
	for _, c := range cs {
		for _, p := range c.SharedPools {
			free += p.Free
		}
	}
	if free != 16 {
		t.Fatalf("the cohort pool reads %d free, want 16: Kueue's 24 less the gated pod's 8, once", free)
	}
	view, reason := allocation.NodeViewFor(cs, store, now)
	if view == nil {
		t.Fatalf("no node view: %s", reason)
	}
	probe := view.Clone()
	holes := 0
	for probe.PlaceReplicas([]allocation.ReplicaShape{{Variant: "b/mb-v", Accelerator: "H100", GPUs: 8, Flavor: &flavor}}, true) {
		holes++
	}
	if holes != 2 {
		t.Fatalf("the flavor's nodes hold %d free 8-GPU holes, want 2: three less the gated replica's, once", holes)
	}

	o := allocation.NewGreedyByScoreOptimizer()
	o.Nodes = view
	var target int
	for _, d := range o.Optimize(ctx, []allocation.ModelScalingRequest{mb}, cs) {
		if d.VariantName == "mb-v" {
			target = d.TargetReplicas
		}
	}
	if target != 2 {
		t.Fatalf("mb, asking 3 borrowed replicas, was ordered %d, want 2: 16 in the pool and two holes", target)
	}
}

// borrowingRequest is a one-variant H100 model of 8-GPU replicas, at none,
// whose demand asks for want replicas.
func borrowingRequest(ns, id string, want int) allocation.ModelScalingRequest {
	v := id + "-v"
	return allocation.ModelScalingRequest{
		ModelID: id, Namespace: ns, Priority: 1,
		CompositeSignal: allocation.NamedAnalyzerResult{
			Name: domain.SaturationAnalyzerName,
			Result: &domain.AnalyzerResult{ModelID: id, Namespace: ns,
				VariantCapacities: []domain.VariantCapacity{{VariantName: v, ReplicaCount: 0, PerReplicaCapacity: 10000}},
			},
			RequiredCapacity: float64(want) * 10000, Remaining: float64(want) * 10000, Live: true,
		},
		Variants:      []domain.VariantMetadata{{VariantName: v, AcceleratorName: "H100", Cost: 5}},
		VariantStates: []domain.VariantReplicaState{{VariantName: v, CurrentReplicas: 0, GPUsPerReplica: 8}},
	}
}
