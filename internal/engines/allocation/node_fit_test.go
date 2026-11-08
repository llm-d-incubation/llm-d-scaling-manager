package allocation

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// Review A3, from the whole-node P/D owner: the physical limiter counted GPUs
// per pool, never per node. Sixteen free H100s as 4+4+8 ordered an 8+8 P/D
// pair, and the prefill sat Pending forever. A replica must fit a node; every
// case below is in whole 8-GPU units.

// The node-fit tests read and write process-wide state (DefaultInFlight, which
// NodeViewFor applies; decision.DefaultHeadroom, which a pass publishes to), so
// none of them runs in parallel (no t.Parallel).

// h100Nodes is one 8-GPU H100 node per free figure, holding 8-free in use.
func h100Nodes(free ...int) map[string]decision.NodeGPU {
	out := make(map[string]decision.NodeGPU, len(free))
	for i, f := range free {
		out[fmt.Sprintf("n%02d", i)] = decision.NodeGPU{Accelerator: famHBM3, Capacity: 8, Used: 8 - f}
	}
	return out
}

// pdWanting is a P/D model in namespace on HBM3, one replica of each role of
// 8 GPUs, whose prefill and decode each want `more` replicas more.
func pdWanting(namespace, model string, more int) ModelScalingRequest {
	demand := float64(1+more) * 10000
	return withSatEntry(&satEntryFixture{
		ModelID: model, Namespace: namespace,
		RequiredCapacity: 2 * demand,
		RoleCapacities: map[string]domain.RoleCapacity{
			"prefill": {Role: "prefill", RequiredCapacity: demand, TotalDemand: demand},
			"decode":  {Role: "decode", RequiredCapacity: demand, TotalDemand: demand},
		},
		VariantCapacities: []vcFixture{
			{VariantName: model + "-prefill", AcceleratorName: famHBM3, Cost: 5, Role: "prefill", ReplicaCount: 1, PerReplicaCapacity: 10000},
			{VariantName: model + "-decode", AcceleratorName: famHBM3, Cost: 5, Role: "decode", ReplicaCount: 1, PerReplicaCapacity: 10000},
		},
	}, ModelScalingRequest{
		ModelID: model, Namespace: namespace, Disaggregated: true, Priority: 1,
		VariantStates: []domain.VariantReplicaState{
			{VariantName: model + "-prefill", CurrentReplicas: 1, GPUsPerReplica: 8, Role: "prefill"},
			{VariantName: model + "-decode", CurrentReplicas: 1, GPUsPerReplica: 8, Role: "decode"},
		},
	})
}

// physicalOf is the physical limiter's constraints for nodes, as the inventory
// would build them: one pool per accelerator, its capacity and use summed.
func physicalOf(nodes map[string]decision.NodeGPU) []*ResourceConstraints {
	pools := map[string]ResourcePool{}
	for _, n := range nodes {
		p := pools[n.Accelerator]
		p.Limit += n.Capacity
		p.Used += n.Used
		pools[n.Accelerator] = p
	}
	return []*ResourceConstraints{{ProviderName: PhysicalLimiterName, Pools: pools}}
}

// optimizeOnNodes runs the greedy optimizer over the physical limiter of nodes,
// with the node view the engine builds for it, and returns the targets.
func optimizeOnNodes(nodes map[string]decision.NodeGPU, requests ...ModelScalingRequest) (map[string]int, *NodeView) {
	o := NewGreedyByScoreOptimizer()
	view := NewNodeView(nodes)
	o.Nodes = view
	out := map[string]int{}
	for _, d := range o.Optimize(context.Background(), requests, physicalOf(nodes)) {
		out[d.VariantName] = d.TargetReplicas
	}
	return out, view
}

func TestPDPairOrdersNothingWhenNoNodeHoldsEachRole(t *testing.T) {
	got, _ := optimizeOnNodes(h100Nodes(4, 4, 8), pdWanting("x", "pd", 3))
	if got["pd-prefill"] != 1 || got["pd-decode"] != 1 {
		t.Fatalf("16 free as 4+4+8: ordered prefill %d decode %d, want 1 and 1 (one 8-GPU hole holds one role, not the pair)",
			got["pd-prefill"], got["pd-decode"])
	}
}

func TestPDPairOrdersOnePairWhenTwoNodesHoldIt(t *testing.T) {
	got, view := optimizeOnNodes(h100Nodes(8, 8), pdWanting("x", "pd", 3))
	if got["pd-prefill"] != 2 || got["pd-decode"] != 2 {
		t.Fatalf("16 free as 8+8: ordered prefill %d decode %d, want 2 and 2 (one pair more)",
			got["pd-prefill"], got["pd-decode"])
	}
	if left := view.fits(replicaFit{acc: famHBM3, pods: []int{1}}, 16); left != 0 {
		t.Errorf("both 8-GPU holes were ordered into, but the view still holds %d free GPUs", left)
	}
}

func TestSingleRoleOrdersOnlyWhatNodesHold(t *testing.T) {
	got, _ := optimizeOnNodes(h100Nodes(4, 4, 8), famWanting("x", "m", famHBM3, 3, 8))
	if got["m-v"] != 1 {
		t.Fatalf("16 free as 4+4+8, 8-GPU replicas: ordered %d, want 1", got["m-v"])
	}
}

// The view is consumed in order: a hole one model's replica took is not free
// to the next model in the same pass.
func TestOneHoleServesOneModelPerPass(t *testing.T) {
	got, _ := optimizeOnNodes(h100Nodes(8, 4, 4),
		famWanting("x", "ma", famHBM3, 1, 8), famWanting("y", "mb", famHBM3, 1, 8))
	if got["ma-v"]+got["mb-v"] != 1 {
		t.Fatalf("one 8-GPU hole: ordered ma %d + mb %d replicas, want 1 in all", got["ma-v"], got["mb-v"])
	}
}

// A replica made of several pods places each pod on a node: two 4-GPU pods fit
// 4+4, where one 8-GPU pod does not.
func TestAMultiPodReplicaPlacesEachPod(t *testing.T) {
	req := famWanting("x", "m", famHBM3, 2, 8)
	req.VariantStates[0].PodGPUs = []int{4, 4}
	got, _ := optimizeOnNodes(h100Nodes(4, 4, 4), req)
	if got["m-v"] != 1 {
		t.Fatalf("12 free as 4+4+4, replicas of two 4-GPU pods: ordered %d, want 1", got["m-v"])
	}
}

// Without a node view -- no physical limiter, or the physical limiter skipped
// -- the pool alone bounds, as before.
func TestWithoutANodeViewThePoolAloneBounds(t *testing.T) {
	nodes := h100Nodes(4, 4, 8)
	out := map[string]int{}
	for _, d := range NewGreedyByScoreOptimizer().Optimize(context.Background(),
		[]ModelScalingRequest{famWanting("x", "m", famHBM3, 3, 8)}, physicalOf(nodes)) {
		out[d.VariantName] = d.TargetReplicas
	}
	if out["m-v"] != 2 {
		t.Fatalf("no node view: ordered %d, want 2 (16 pooled GPUs)", out["m-v"])
	}
}

// The node view exists only when the physical limiter bounds the decision:
// a quota alone, or the physical provider skipped, fits by pools alone.
func TestNodeViewForFollowsThePhysicalLimiter(t *testing.T) {
	store := &decision.NodeGPUStore{}
	quota := &ResourceConstraints{ProviderName: "tenant-quota", Pools: map[string]ResourcePool{"H100": {Limit: 8}}}
	physical := &ResourceConstraints{ProviderName: PhysicalLimiterName, Pools: map[string]ResourcePool{famHBM3: {Limit: 8}}}

	if v, reason := NodeViewFor([]*ResourceConstraints{quota}, store, time.Now()); v != nil || reason != "" {
		t.Errorf("a quota alone: view %v reason %q, want none", v, reason)
	}
	if v, reason := NodeViewFor([]*ResourceConstraints{quota, physical}, store, time.Now()); v != nil || reason == "" {
		t.Errorf("physical limiter without a node snapshot: view %v reason %q, want none, and a reason", v, reason)
	}
	store.Publish(h100Nodes(8), time.Now())
	if v, reason := NodeViewFor([]*ResourceConstraints{quota, physical}, store, time.Now()); v == nil || reason != "" {
		t.Errorf("physical limiter with a snapshot: view %v reason %q, want a view", v, reason)
	}
	if v, _ := NodeViewFor([]*ResourceConstraints{quota}, store, time.Now()); v != nil {
		t.Error("the physical provider skipped (absent from the constraints): node fit must be skipped too")
	}
}
