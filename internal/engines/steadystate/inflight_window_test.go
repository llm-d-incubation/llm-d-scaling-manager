package steadystate

import (
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// Review WN5: the pass's placements reached the in-flight ledger only at the
// end of the cycle (admitInFlight), after the utilization share, scale-to-
// zero and the borrowing releases. A wake in that window built its node view
// from the snapshot and the ledger, saw the node the pass had just chosen
// free, and took it: two replicas ordered into one 8-GPU node. The pass's
// placements are admitted right after the optimizer; the end of the cycle
// trims them to its final targets.
//
// DefaultInFlight is process-wide: no t.Parallel.
func TestAWakeDuringTheCycleDoesNotTakeThePassNode(t *testing.T) {
	allocation.DefaultInFlight.Reset()
	t.Cleanup(allocation.DefaultInFlight.Reset)
	t.Cleanup(decision.DefaultHeadroom.Reset)
	now := time.Now()
	store := &decision.NodeGPUStore{}
	store.Publish(a100Nodes(8), now)
	physical := []*allocation.ResourceConstraints{{ProviderName: allocation.PhysicalLimiterName,
		Pools: map[string]allocation.ResourcePool{"A100": {Limit: 16, Used: 8}}}}
	// What a wake fits against: the snapshot less the ledger's holds.
	wakeFits := func() bool {
		v, _ := allocation.NodeViewFor(physical, store, time.Now())
		return v.PlaceReplicas([]allocation.ReplicaShape{{Variant: "cold/c", Accelerator: "A100", GPUs: 8}}, false)
	}
	if !wakeFits() {
		t.Fatal("setup: the free node does not fit a wake before the cycle")
	}

	e := &Engine{Config: shadowConfig(t, "")}
	o := allocation.NewGreedyByScoreOptimizer()
	o.Nodes, _ = allocation.NodeViewFor(physical, store, now)
	reqs := []allocation.ModelScalingRequest{growingA100("ns", "m", 8)}
	decisions := e.decideV2(t.Context(), o, reqs, physical, nil)
	if len(decisions) != 1 || decisions[0].TargetReplicas != 2 {
		t.Fatalf("setup: the pass decided %+v, want m grown to 2 into the free node", decisions)
	}
	// The window: the cycle has not committed its targets yet.
	if wakeFits() {
		t.Fatal("a wake between the optimizer and the end of the cycle took the node the pass chose")
	}

	// A release then lowers the target: the hold goes with it.
	decisions[0].TargetReplicas = 1
	e.admitInFlight(reqs, decisions, e.cyclePlanned, nil, time.Now())
	if !wakeFits() {
		t.Fatal("the cycle released its growth, but the node is still held")
	}
}

// The commit sets back what the provisional admission replaced: a target the
// pass raised and a release lowered again keeps the clock the earlier
// growth had, so a replica Pending for good is not held anew every cycle.
func TestACommitKeepsTheEarlierClock(t *testing.T) {
	allocation.DefaultInFlight.Reset()
	t.Cleanup(allocation.DefaultInFlight.Reset)
	t0 := time.Unix(10_000, 0)
	shape := allocation.ReplicaShape{Variant: "ns/m-v", Accelerator: "A100", GPUs: 8}
	allocation.DefaultInFlight.Admit("ns/m-v", shape, 2, 1, nil, t0)
	later := t0.Add(allocation.InFlightTTL - time.Minute)
	mark := allocation.DefaultInFlight.Provisional([]allocation.InFlightAdmission{
		{Key: "ns/m-v", Shape: shape, Want: 3, PlacedNow: 1}}, later)
	allocation.DefaultInFlight.Commit(mark, []allocation.InFlightAdmission{
		{Key: "ns/m-v", Shape: shape, Want: 2, PlacedNow: 1}}, later)
	if allocation.DefaultInFlight.Holds("ns/m-v", t0.Add(allocation.InFlightTTL+time.Second)) {
		t.Fatal("the hold outlived its first TTL: the provisional raise restarted the clock")
	}
}
