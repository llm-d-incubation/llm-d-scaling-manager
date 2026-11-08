package allocation

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// Review 187-2: a LeaderWorkerSet's leader and worker templates were both
// required of every pod. A gang whose leader is pinned to pool=leader and
// whose workers to pool=worker then fitted on no node at all, on every path
// that fits a replica. The leader pod is judged by the leader template, the
// workers by the worker template.

// pools is 8-GPU HBM3 nodes labelled pool=<pool>, each fully free.
func pools(leaders, workers int) map[string]decision.NodeGPU {
	out := map[string]decision.NodeGPU{}
	for i := range leaders {
		out["leader"+string(rune('a'+i))] = decision.NodeGPU{Accelerator: famHBM3, Capacity: 8,
			Labels: map[string]string{"pool": "leader"}}
	}
	for i := range workers {
		out["worker"+string(rune('a'+i))] = decision.NodeGPU{Accelerator: famHBM3, Capacity: 8,
			Labels: map[string]string{"pool": "worker"}}
	}
	return out
}

// pooledGang is a 24-GPU group of three 8-GPU pods, its leader pinned to
// pool=leader and its workers to pool=worker.
func pooledGang() ReplicaShape {
	return ReplicaShape{Variant: "ns/g", Accelerator: famHBM3, PodGPUs: []int{8, 8, 8}, GPUs: 24,
		Placements: []domain.PodPlacement{
			{NodeSelector: map[string]string{"pool": "leader"}},
			{NodeSelector: map[string]string{"pool": "worker"}},
		}}
}

func TestALeaderAndItsWorkersAreJudgedByTheirOwnTemplates(t *testing.T) {
	v := NewNodeView(pools(1, 2))
	if !v.PlaceReplicas([]ReplicaShape{pooledGang()}, true) {
		t.Fatal("a gang with its leader on pool=leader and workers on pool=worker fits no node")
	}
	d := v.DebitsExcept(nil)
	if d["leadera"] != 8 || d["workera"]+d["workerb"] != 16 {
		t.Fatalf("placed %v, want the leader on the leader node and the workers on the worker nodes", d)
	}

	// Control: each template is still judged. Two leader nodes and one
	// worker node hold three pods by GPUs, but only one worker.
	if NewNodeView(pools(2, 1)).PlaceReplicas([]ReplicaShape{pooledGang()}, false) {
		t.Fatal("a worker was placed on a pool=leader node")
	}

	// A replica placed as one pod, its layout unknown: which pod it is cannot
	// be told, and neither template is judged.
	one := pooledGang()
	one.PodGPUs, one.GPUs = nil, 8
	if !NewNodeView(pools(0, 1)).PlaceReplicas([]ReplicaShape{one}, false) {
		t.Fatal("a replica whose pods cannot be told apart was judged by both templates")
	}
}
