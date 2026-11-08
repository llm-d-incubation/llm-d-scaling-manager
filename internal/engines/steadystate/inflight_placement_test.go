package steadystate

import (
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// Review WN3 and the minor on guaranteed holes: a hole the ledger holds for
// growth the pass ordered off any Kueue flavor dropped its pod templates'
// placement, so it was held on whichever node fitted best first. A decode
// pinned to role=decode nodes, ordered into the decode node, held the
// prefill node instead, and a prefill was then refused the node it could use.
func TestAHeldHoleKeepsItsTemplatesPlacement(t *testing.T) {
	allocation.DefaultInFlight.Reset()
	t.Cleanup(allocation.DefaultInFlight.Reset)
	now := time.Now()
	store := &decision.NodeGPUStore{}
	store.Publish(map[string]decision.NodeGPU{
		"a": {Accelerator: "A100", Capacity: 8, Labels: map[string]string{"role": "prefill"}},
		"b": {Accelerator: "A100", Capacity: 8, Labels: map[string]string{"role": "decode"}},
	}, now)
	req := growingA100("ns", "m", 8)
	req.VariantStates[0].Placements = []domain.PodPlacement{{NodeSelector: map[string]string{"role": "decode"}}}
	decisions := []domain.VariantDecision{{VariantName: "m-v", Namespace: "ns", AcceleratorName: "A100", TargetReplicas: 2}}
	(&Engine{}).admitInFlight([]allocation.ModelScalingRequest{req}, decisions, nil, nil, now)

	physical := []*allocation.ResourceConstraints{{ProviderName: allocation.PhysicalLimiterName,
		Pools: map[string]allocation.ResourcePool{"A100": {Limit: 16}}}}
	view, _ := allocation.NodeViewFor(physical, store, now)
	prefill := allocation.ReplicaShape{Variant: "ns/p", Accelerator: "A100", GPUs: 8,
		Placements: []domain.PodPlacement{{NodeSelector: map[string]string{"role": "prefill"}}}}
	if !view.PlaceReplicas([]allocation.ReplicaShape{prefill}, false) {
		t.Fatal("the decode's held hole took the prefill node")
	}
	decode := prefill
	decode.Placements = req.VariantStates[0].Placements
	if view.PlaceReplicas([]allocation.ReplicaShape{decode}, false) {
		t.Fatal("the decode node is free to another decode: its hole is not held there")
	}
}
