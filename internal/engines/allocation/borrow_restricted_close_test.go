package allocation

import (
	"context"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
)

// Review K2-2: an Any lender lends 8 and a LowerPriority lender at 500 lends
// 8 into one chunk (16 free, 8 of them restricted). Serving X at 1000 draws
// the unrestricted 8; canary Y at 0 then drew the restricted 8, which the
// lender at 500 outranks. When the Any lender reclaims, Kueue preempts the
// lowest priority first -- Y -- and the LowerPriority lender can never take
// back the 8 X holds: it is stranded. Once a model the LowerPriority lender
// does not outrank draws from the chunk, its restricted part closes to every
// model for the pass, whatever the placement order. 8-GPU units.
func TestRestrictedPartClosesOnceAnOutrankingModelDraws(t *testing.T) {
	const serving, canary = "kueue.x-k8s.io/serving", "kueue.x-k8s.io/canary"
	run := func(yFirst bool) map[string]int {
		pool := borrowQuota(16,
			nsBorrow{ns: "serve", cap: config.QuotaUnlimited},
			nsBorrow{ns: "lab", cap: config.QuotaUnlimited})
		pool.SharedPools[0].Restricted = []PriorityLend{{Queue: "batch-cq", Below: 500, GPUs: 8}}
		pool.SharedPools[0].ClassPriority = map[string]int32{serving: 1000, canary: 0}
		x := borrowModel("serve", "x", 8, 0, 1, nil)
		x.VariantStates[0].PriorityClass = serving
		y := borrowModel("lab", "y", 8, 0, 1, nil)
		y.VariantStates[0].PriorityClass = canary
		// At equal priority the optimizer places in request order.
		reqs := []ModelScalingRequest{x, y}
		if yFirst {
			reqs = []ModelScalingRequest{y, x}
		}
		o := NewGreedyByScoreOptimizer()
		o.Nodes = NewNodeView(nil)
		return addedGPUs(o.Optimize(context.Background(), reqs, []*ResourceConstraints{physicalH100(64), pool}))
	}
	for _, tc := range []struct {
		name   string
		yFirst bool
		winner string
	}{
		{"X placed first: Y may not take the restricted 8", false, "x"},
		{"Y placed first: X may not take the restricted 8", true, "y"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := run(tc.yFirst)
			if got["x"] > 0 && got["y"] > 0 {
				t.Fatalf("X borrowed %d and Y %d: Y holds the LowerPriority lender's 8 while X holds the Any lender's, "+
					"so an Any reclaim takes Y and the lender at 500 cannot take X's back", got["x"], got["y"])
			}
			if got[tc.winner] != 8 {
				t.Fatalf("borrowed %v, want the first placed, %s, to hold one 8-GPU replica", got, tc.winner)
			}
		})
	}
}
