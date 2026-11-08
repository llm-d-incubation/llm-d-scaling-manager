package allocation

import (
	"context"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
)

// Review W1: a lender whose namespace is not counted (not selected) grows
// into the nominal its reclaiming queue lent out while a borrower's 8+8 P/D
// pair fills its two nodes. No node shows a hole and the physical pool reads
// full; Kueue preempts the borrower to admit the lender's replica, so it must
// be ordered. Whole 8-GPU nodes.
func TestUnselectedLenderGrowsIntoLentNominalWithoutAHole(t *testing.T) {
	for _, tc := range []struct {
		name    string
		lentOut int
		want    int
	}{
		{"its queue lent 16 a peer holds: the replica is ordered", 16, 8},
		{"control: nothing lent out, no hole: no replica", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := borrowQuota(0, nsBorrow{ns: "a", guaranteed: 16, used: 32, cap: config.QuotaUnlimited})
			q.NamespacePools["b"] = map[string]ResourcePool{"H100": {Limit: 16}}
			q.NamespaceCohorts["b"] = []string{"c"}
			if tc.lentOut > 0 {
				q.NamespaceNominal = map[string]map[string]NominalFigures{"b": {"H100": {LentOut: tc.lentOut, WithinNominal: 16}}}
			}
			physical := physicalH100(32)
			physical.Pools["H100"] = ResourcePool{Limit: 32, Used: 32}
			o := NewGreedyByScoreOptimizer()
			// The two nodes the borrower's pair fills, and its own two.
			o.Nodes = NewNodeView(map[string]decision.NodeGPU{
				"n1": h100Node(8, nil), "n2": h100Node(8, nil), "n3": h100Node(8, nil), "n4": h100Node(8, nil)})
			got := addedGPUs(o.Optimize(context.Background(), []ModelScalingRequest{borrowModel("b", "m", 8, 0, 1, nil)},
				[]*ResourceConstraints{physical, q}))
			if got["m"] != tc.want {
				t.Fatalf("lender grew %d GPUs, want %d", got["m"], tc.want)
			}
		})
	}
}
