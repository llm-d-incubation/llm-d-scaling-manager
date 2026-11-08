package allocation

import (
	"context"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
)

// Review N-m1: two Kueue-backed entries read one namespace's queues and
// disagree on what its peers borrow: one says 8, the other 0 (a fresher read,
// after the borrower left). The placer took the larger and ordered a replica
// with no node hole; the wake and the uncounted path (reclaimFor, bs.lent)
// take the smaller. The steady placer now does too: with its one node full,
// the replica is refused. Control: both say 8.
func TestTwoEntriesTakeTheSmallerLentOut(t *testing.T) {
	for _, tc := range []struct {
		name   string
		second int
		want   int
	}{
		{"one entry says 8, the other 0: refused", 0, 0},
		{"control: both say 8: placed", 8, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			physical := physicalH100(8)
			physical.Pools["H100"] = ResourcePool{Limit: 8, Used: 8}
			a := borrowQuota(0, nsBorrow{ns: "r", guaranteed: 8, cap: 0, wn: 8, lentOut: 8})
			b := borrowQuota(0, nsBorrow{ns: "r", guaranteed: 8, cap: 0, wn: 8, lentOut: tc.second})
			b.ProviderName = "second-quota"
			o := NewGreedyByScoreOptimizer()
			o.Nodes = NewNodeView(map[string]decision.NodeGPU{"n1": h100Node(8, nil)})
			got := addedGPUs(o.Optimize(context.Background(), []ModelScalingRequest{borrowModel("r", "m", 8, 0, 1, nil)},
				[]*ResourceConstraints{physical, a, b}))
			if got["m"] != tc.want {
				t.Fatalf("grew %d GPUs, want %d", got["m"], tc.want)
			}
		})
	}
}
