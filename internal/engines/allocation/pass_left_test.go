package allocation

import (
	"context"
	"testing"
	"time"
)

// What an optimizer pass leaves for the warm pool and the utilization share,
// in whole 8-GPU units (review round 2). Shared state: no t.Parallel.

// Go minor 2: RepublishHeadroom republished the last pass's figures after a
// cycle that ran no pass on constraints. Each pass forgets the last.
func TestRepublishHeadroomNeedsThisCyclesPass(t *testing.T) {
	nodes := h100Nodes(8)
	o := NewGreedyByScoreOptimizer()
	decideOnNodes(o, nodes, physicalOf(nodes), famWanting("x", "m", famHBM3, 1, 8))
	o.Optimize(context.Background(), []ModelScalingRequest{famWanting("x", "m", famHBM3, 1, 8)}, nil)
	if o.RepublishHeadroom(nil, nil, time.Now()) {
		t.Fatal("republished a pass from an earlier cycle")
	}
}

// What the pass placed for a model is charged to it, so the utilization
// share takes back the placements of the models it plans, and nothing else.
func TestPassLeftTakesBackOnlyThePlannedModels(t *testing.T) {
	nodes := h100Nodes(8, 8)
	o := NewGreedyByScoreOptimizer()
	decideOnNodes(o, nodes, physicalOf(nodes), famWanting("x", "ma", famHBM3, 1, 8), famWanting("y", "mb", famHBM3, 1, 8))
	left := o.LastPass()
	all := 0
	for _, g := range left.NodeDebits(nil) {
		all += g
	}
	notA := 0
	for _, g := range left.NodeDebits(map[string]bool{"x/ma": true}) {
		notA += g
	}
	if all != 16 || notA != 8 {
		t.Fatalf("debits: %d in all, %d without x/ma, want 16 and 8", all, notA)
	}
	available, byNS := map[string]int{famHBM3: 16}, map[string]map[string]int(nil)
	left.Replay(available, byNS, map[string]bool{"x/ma": true})
	if available[famHBM3] != 8 {
		t.Fatalf("pools after the other model's spends: %d, want 8", available[famHBM3])
	}
}
