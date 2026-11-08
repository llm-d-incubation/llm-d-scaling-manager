package allocation

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// Borrowed placement on the cycle's one node view, in whole 8-GPU units:
// node-fit attribution, the share's post-pass picture and the in-flight
// ledger. These tests touch the package's default stores (headroom,
// contention) through Optimize, so they do not run in parallel.

// flavorA is the h100 chunk's flavor, on the nodes labelled pool=a.
var flavorA = kueue.FlavorPlacement{AcceleratorType: borrowH100, NodeLabels: map[string]string{"pool": "a"}}

// borrowOnFlavorA is a namespace "a" that borrows freely from a chunk of free
// GPUs on flavorA, beside a physical limiter of 64.
func borrowOnFlavorA(free int) []*ResourceConstraints {
	q := borrowQuota(free, nsBorrow{ns: "a", cap: config.QuotaUnlimited})
	q.SharedPools[0].FlavorPlacement = flavorA
	return []*ResourceConstraints{physicalH100(64), q}
}

// decideBorrowing runs one pass on nodes and returns the decision of model
// ma's variant.
func decideBorrowing(o *GreedyByScoreOptimizer, nodes map[string]decision.NodeGPU, constraints []*ResourceConstraints) domain.VariantDecision {
	o.Nodes = NewNodeView(nodes)
	for _, d := range o.Optimize(context.Background(), []ModelScalingRequest{borrowModel("a", "ma", 8, 0, 1, nil)}, constraints) {
		if d.VariantName == "ma-v" {
			return d
		}
	}
	return domain.VariantDecision{}
}

// A borrowed replica the pools hold and no node of its flavor does is cut by
// node fit, and named so: the chunk has 16 free, the flavor's only node has 4.
// With the pool empty instead, the pool is named.
func TestABorrowedReplicaTheNodesRefuseIsNamedNodeFit(t *testing.T) {
	nodes := map[string]decision.NodeGPU{
		"n1": h100Node(4, map[string]string{"pool": "a"}),
		"n2": h100Node(0, map[string]string{"pool": "b"}),
	}
	d := decideBorrowing(NewGreedyByScoreOptimizer(), nodes, borrowOnFlavorA(16))
	if d.TargetReplicas != 0 || !d.WasLimited || d.LimitedBy != NodeFitLimiterName {
		t.Fatalf("16 free in the pool, 4 on the flavor's node: target %d limited %v by %q, want 0, limited by %q",
			d.TargetReplicas, d.WasLimited, d.LimitedBy, NodeFitLimiterName)
	}
	// Control: an empty pool is the pool's cut, whatever the nodes hold.
	nodes["n1"] = h100Node(0, map[string]string{"pool": "a"})
	d = decideBorrowing(NewGreedyByScoreOptimizer(), nodes, borrowOnFlavorA(0))
	if d.TargetReplicas != 0 || !d.WasLimited || d.LimitedBy == NodeFitLimiterName {
		t.Fatalf("control: an empty pool, a free flavor node: target %d limited %v by %q, want 0, limited by a pool",
			d.TargetReplicas, d.WasLimited, d.LimitedBy)
	}
}

// What the placer places on borrowed capacity is charged to its model on the
// pass's view, so the utilization share, planning after the pass, sees the
// hole taken (and takes it back only for a model it plans itself).
func TestTheShareSeesWhereABorrowedReplicaLanded(t *testing.T) {
	nodes := map[string]decision.NodeGPU{
		"n1": h100Node(0, map[string]string{"pool": "b"}),
		"n2": h100Node(0, map[string]string{"pool": "a"}),
	}
	o := NewGreedyByScoreOptimizer()
	if d := decideBorrowing(o, nodes, borrowOnFlavorA(16)); d.TargetReplicas != 1 {
		t.Fatalf("premise: one borrowed replica on the flavor's free node, got target %d", d.TargetReplicas)
	}
	left := o.LastPass()
	if got := left.NodeDebits(nil); got["n2"] != 8 || got["n1"] != 0 {
		t.Fatalf("the pass's node debits %v, want 8 on n2 (the flavor's node) and none on n1", got)
	}
	if got := left.NodeDebits(map[string]bool{"a/ma": true}); got["n2"] != 0 {
		t.Fatalf("a model the share plans keeps its borrowed placement charged: %v", got)
	}
}

// The in-flight ledger holds a borrowed replica's hole on its flavor's nodes,
// where the placer fitted it, not on the first free node of the accelerator;
// and keeps it there once the draw resolves and the shape comes without a
// flavor (the pods exist, gated, holding no node).
func TestTheLedgerHoldsABorrowedHoleOnItsFlavor(t *testing.T) {
	nodes := map[string]decision.NodeGPU{
		"n1": h100Node(0, map[string]string{"pool": "b"}),
		"n2": h100Node(0, map[string]string{"pool": "a"}),
	}
	t0 := time.Unix(1000, 0)
	l := NewInFlightLedger()
	flavor := flavorA
	borrowed := ReplicaShape{Variant: "a/ma-v", Accelerator: "H100", GPUs: 8, Flavor: &flavor}
	l.Admit("a/ma-v", borrowed, 1, 0, nil, t0)
	v := newNodeViewAt(nodes, t0)
	l.holdOn(v, t0)
	if v.freeOn("n1") != 8 || v.freeOn("n2") != 0 {
		t.Fatalf("held n1 %d free, n2 %d free; want the flavor's node n2 held and n1 free", v.freeOn("n1"), v.freeOn("n2"))
	}

	plain := borrowed
	plain.Flavor = nil
	l.Admit("a/ma-v", plain, 1, 0, nil, t0.Add(time.Second))
	v = newNodeViewAt(nodes, t0)
	l.holdOn(v, t0.Add(time.Second))
	if v.freeOn("n2") != 0 {
		t.Fatalf("after the draw resolved the hole moved off the flavor: n1 %d free, n2 %d free", v.freeOn("n1"), v.freeOn("n2"))
	}
}

// A replica a borrowing rule holds back (a placement hold, a cooldown, the
// physical limiter skipped) is not scarcity: its decision is not marked
// limited, though the pool and the flavor's node have room.
func TestABlockedBorrowerIsNotMarkedLimited(t *testing.T) {
	nodes := map[string]decision.NodeGPU{"n1": h100Node(0, map[string]string{"pool": "a"})}
	o := NewGreedyByScoreOptimizer()
	o.BorrowBlocked = func(_, _, _ string) bool { return true }
	d := decideBorrowing(o, nodes, borrowOnFlavorA(16))
	if d.TargetReplicas != 0 || d.WasLimited {
		t.Fatalf("a blocked borrower: target %d limited %v by %q, want 0 and not limited", d.TargetReplicas, d.WasLimited, d.LimitedBy)
	}
	// Control: unblocked, it borrows the replica.
	if d := decideBorrowing(NewGreedyByScoreOptimizer(), nodes, borrowOnFlavorA(16)); d.TargetReplicas != 1 {
		t.Fatalf("control: target %d, want 1", d.TargetReplicas)
	}
}

// The same for a model backed off in every cohort it could draw on (B12).
func TestABackedOffBorrowerIsNotMarkedLimited(t *testing.T) {
	nodes := map[string]decision.NodeGPU{"n1": h100Node(0, map[string]string{"pool": "a"})}
	o := NewGreedyByScoreOptimizer()
	o.BorrowBlockedIn = func(_, _, _ string) bool { return true }
	d := decideBorrowing(o, nodes, borrowOnFlavorA(16))
	if d.TargetReplicas != 0 || d.WasLimited {
		t.Fatalf("backed off in its only cohort: target %d limited %v by %q, want 0 and not limited", d.TargetReplicas, d.WasLimited, d.LimitedBy)
	}
}
