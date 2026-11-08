package allocation

import (
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// regrowModel is model m in namespace a, of 8-GPU replicas on HBM3, with
// current replicas created and one replica more wanted.
func regrowModel(current int) ModelScalingRequest {
	return withSatEntry(&satEntryFixture{
		RequiredCapacity: 10000,
		VariantCapacities: []vcFixture{
			{VariantName: "m-v", AcceleratorName: famHBM3, Cost: 5, ReplicaCount: current, PerReplicaCapacity: 10000},
		},
	}, ModelScalingRequest{
		ModelID: "m", Namespace: "a", Priority: 1,
		VariantStates: []domain.VariantReplicaState{
			{VariantName: "m-v", CurrentReplicas: current, GPUsPerReplica: 8},
		},
	})
}

// The in-flight ledger holds the hole a variant's growth was ordered into
// until its pods hold a node, and the next pass restarts the variant's target
// from its current replicas. With its pods not created yet (current 2 of
// target 3), the pass re-places that very replica -- into the hole its own
// entry holds. One free 8-GPU node: without the release the pass found no
// hole, the target fell to 2, the entry ended, and the next pass ordered 3
// again.
func TestAVariantRePlacesIntoItsOwnHeldHole(t *testing.T) {
	t0 := time.Unix(80_000, 0)
	l := NewInFlightLedger()
	nodes := h100Nodes(0, 0, 8)
	shape := ReplicaShape{Variant: "a/m-v", Accelerator: famHBM3, GPUs: 8}
	pass := func(now time.Time) int {
		v := newNodeViewAt(nodes, now.Add(-time.Second))
		l.holdOn(v, now)
		l.ReleaseUncreated(v, map[string]int{"a/m-v": 2})
		o := NewGreedyByScoreOptimizer()
		o.Nodes = v
		target := 0
		for _, d := range o.Optimize(t.Context(), []ModelScalingRequest{regrowModel(2)}, physicalOf(nodes)) {
			target = d.TargetReplicas
		}
		l.Admit("a/m-v", shape, target, 2, nil, now)
		return target
	}
	got := make([]int, 0, 3)
	for i := range 3 {
		got = append(got, pass(t0.Add(time.Duration(i)*30*time.Second)))
	}
	for i, n := range got {
		if n != 3 {
			t.Fatalf("targets over three cycles %v, want 3 each: cycle %d found its own hole held", got, i+1)
		}
	}

	// Control: a replica whose pod exists (current 3) is not re-placed; its
	// hole stays held from every other variant.
	v := newNodeViewAt(nodes, t0)
	l.holdOn(v, t0.Add(time.Minute))
	l.ReleaseUncreated(v, map[string]int{"a/m-v": 3})
	if v.fits(replicaFit{acc: famHBM3, pods: []int{8}}, 1) != 0 {
		t.Fatal("a hole held for a replica whose pod exists was released")
	}
}
