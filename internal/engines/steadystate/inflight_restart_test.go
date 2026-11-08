package steadystate

import (
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// Review 187-1: only growth started an in-flight hold, and the ledger is not
// persisted. After a restart, a replica ordered before it -- its pod created,
// Pending, about to land on the one free node -- is at target = current, so
// no entry was started, and a wake took the node. The first sighting of a
// variant in a process starts its hold; a sighted variant does not.
//
// DefaultInFlight is process-wide: no t.Parallel.
func TestARestartKeepsAPendingReplicaNodeHeld(t *testing.T) {
	allocation.DefaultInFlight.Reset() // the restart: an empty ledger
	t.Cleanup(allocation.DefaultInFlight.Reset)
	now := time.Now()
	store := &decision.NodeGPUStore{}
	store.Publish(a100Nodes(8), now)
	physical := []*allocation.ResourceConstraints{{ProviderName: allocation.PhysicalLimiterName,
		Pools: map[string]allocation.ResourcePool{"A100": {Limit: 32, Used: 24}}}}
	wakeFits := func(at time.Time) bool {
		v, _ := allocation.NodeViewFor(physical, store, at)
		return v.PlaceReplicas([]allocation.ReplicaShape{{Variant: "cold/c", Accelerator: "A100", GPUs: 8}}, false)
	}
	if !wakeFits(now) {
		t.Fatal("setup: the free node does not fit a wake")
	}

	m := growingA100("ns", "m", 8)
	m.VariantStates[0].CurrentReplicas = 3
	m.VariantStates[0].FilledReplicas = 2
	m.VariantStates[0].HeldKnown = true
	at3 := []domain.VariantDecision{{VariantName: "m-v", Namespace: "ns", AcceleratorName: "A100", TargetReplicas: 3}}
	(&Engine{}).admitInFlight([]allocation.ModelScalingRequest{m}, at3, nil, nil, now)
	if wakeFits(now.Add(time.Second)) {
		t.Fatal("after a restart, a wake took the node a Pending replica was ordered into")
	}

	// A variant this process has sighted starts no hold at its current
	// replicas: its replica Pending past the TTL is not held again.
	allocation.DefaultInFlight.Reset()
	m.VariantStates[0].FilledReplicas = 3
	(&Engine{}).admitInFlight([]allocation.ModelScalingRequest{m}, at3, nil, nil, now)
	m.VariantStates[0].FilledReplicas = 2
	(&Engine{}).admitInFlight([]allocation.ModelScalingRequest{m}, at3, nil, nil, now)
	if allocation.DefaultInFlight.Holds("ns/m-v", now) {
		t.Fatal("a sighted variant at its current replicas started a hold of its own")
	}
}
