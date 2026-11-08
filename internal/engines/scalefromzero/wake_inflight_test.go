package scalefromzero

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// Review round 2 (whole-node owner, MAJOR 3): the wake check held a woken
// replica's node hole for one 100 ms tick. Its pods take seconds to minutes
// to hold the node, and every tick in between saw the hole free: two cold
// models woke into one free 8-GPU node. The hole is held across ticks, and
// across both engines, until the pods land or the in-flight TTL passes.
//
// The node store, the usage store and the in-flight ledger are process-wide:
// no t.Parallel.

// Two cold models and one free 8-GPU node, over many ticks: exactly one wakes.
func TestOneFreeNodeWakesOneColdModelAcrossTicks(t *testing.T) {
	publishUsage(t)
	publishNodes(t, 8)
	e := physicalEngine(8)
	woke := 0
	for tick := range 50 {
		e.beginTick()
		for _, m := range []string{"first", "second"} {
			if tick > 0 && m == "first" {
				continue // awake now: no longer a scale-from-zero candidate
			}
			if got := wakeOnce(t, e, "ns-"+m, m, cand(m+"-decode", "decode", wakeHBM3, 8, 1)); got != OutcomeNoCapacity {
				woke++
			}
		}
	}
	if woke != 1 {
		t.Fatalf("one free 8-GPU node over 50 ticks woke %d models, want 1", woke)
	}
}

// A wake holds its hole from the steady-state engine's next pass too: the
// pass's node view (allocation.NodeViewFor) holds the ledger.
func TestAWakeHoldsItsNodeFromTheSteadyStatePass(t *testing.T) {
	publishUsage(t)
	publishNodes(t, 8)
	physical := []*allocation.ResourceConstraints{{ProviderName: allocation.PhysicalLimiterName,
		Pools: map[string]allocation.ResourcePool{wakeHBM3: {Limit: 8}}}}
	pass := func() int {
		o := allocation.NewGreedyByScoreOptimizer()
		o.Nodes, _ = allocation.NodeViewFor(physical, nil, time.Now())
		for _, d := range o.Optimize(context.Background(), []allocation.ModelScalingRequest{growing("b", "warm", 8)}, physical) {
			return d.TargetReplicas
		}
		return 0
	}
	if got := pass(); got != 2 {
		t.Fatalf("setup: before any wake the pass grew warm to %d, want 2", got)
	}
	e := physicalEngine(8)
	e.beginTick()
	if got := wakeOnce(t, e, "a", "cold", cand("cold-decode", "decode", wakeHBM3, 8, 1)); got == OutcomeNoCapacity {
		t.Fatalf("setup: the wake was refused (%s)", got)
	}
	if got := pass(); got != 1 {
		t.Fatalf("the steady-state pass grew warm to %d into the hole a wake holds, want 1", got)
	}
}

// The other way: growth the steady-state engine ordered holds its hole from a
// wake. (The engine admits it with allocation.DefaultInFlight.Admit.)
func TestSteadyStateGrowthHoldsItsNodeFromAWake(t *testing.T) {
	publishUsage(t)
	publishNodes(t, 8)
	allocation.DefaultInFlight.Admit("b/warm-v", allocation.ReplicaShape{Variant: "b/warm-v", Accelerator: wakeHBM3, GPUs: 8},
		2, 1, nil, time.Now())
	e := physicalEngine(8)
	e.beginTick()
	if got := wakeOnce(t, e, "a", "cold", cand("cold-decode", "decode", wakeHBM3, 8, 1)); got != OutcomeNoCapacity {
		t.Fatalf("a wake took the hole growth was ordered into: %s, want %s", got, OutcomeNoCapacity)
	}
}

// growing is a one-variant model in namespace ns at one replica of gpus GPUs,
// asking for one more.
func growing(ns, model string, gpus int) allocation.ModelScalingRequest {
	v := model + "-v"
	return allocation.ModelScalingRequest{
		ModelID: model, Namespace: ns, Priority: 1,
		CompositeSignal: allocation.NamedAnalyzerResult{
			Name: domain.SaturationAnalyzerName, Remaining: 1000, Score: 1,
			Result: &domain.AnalyzerResult{
				TotalDemand:       2000,
				VariantCapacities: []domain.VariantCapacity{{VariantName: v, PerReplicaCapacity: 1000}},
			},
		},
		Variants:      []domain.VariantMetadata{{VariantName: v, AcceleratorName: wakeHBM3, Cost: 5}},
		VariantStates: []domain.VariantReplicaState{{VariantName: v, CurrentReplicas: 1, GPUsPerReplica: gpus}},
	}
}
