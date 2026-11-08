package allocation

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
)

// restrictedPool is one chunk of 16 free GPUs, 8 of them lent by a
// LowerPriority lender at 500, read for namespaces "serve" (X at 1000) and
// "lab" (Y at 0).
func restrictedPool(free int) *ResourceConstraints {
	const serving, canary = "kueue.x-k8s.io/serving", "kueue.x-k8s.io/canary"
	pool := borrowQuota(free,
		nsBorrow{ns: "serve", cap: config.QuotaUnlimited},
		nsBorrow{ns: "lab", cap: config.QuotaUnlimited})
	pool.SharedPools[0].Restricted = []PriorityLend{{Queue: "batch-cq", Below: 500, GPUs: 8}}
	pool.SharedPools[0].ClassPriority = map[string]int32{serving: 1000, canary: 0}
	return pool
}

// classed is model id of ns at the given priority class, wanting one 8-GPU
// replica.
func classed(ns, id, class string) ModelScalingRequest {
	m := borrowModel(ns, id, 8, 0, 1, nil)
	m.VariantStates[0].PriorityClass = class
	return m
}

// Review M1 / NEW-4: the K2-2 closure held for one pass only. Cycle 1, X at
// 1000 borrows the Any lender's 8 of a chunk of 16 (8 restricted to models
// the lender at 500 outranks). Cycle 2 reads the same pool -- X's pods not
// admitted yet, its draw in flight -- so 8 free, all of them restricted, and
// Y at 0 drew them. When the Any lender reclaims, Kueue preempts Y first, and
// the lender at 500 can never take back the 8 X holds: stranded. Y must not
// draw the restricted 8 while X's draw is in flight, nor once the read shows
// the cohort borrowing at 1000. Control: borrowed at 100, which the lender
// outranks, keeps it open.
func TestRestrictedPartStaysClosedAcrossCycles(t *testing.T) {
	const serving, canary = "kueue.x-k8s.io/serving", "kueue.x-k8s.io/canary"
	run := func(constraints []*ResourceConstraints, reqs ...ModelScalingRequest) (map[string]int, *GreedyByScoreOptimizer) {
		o := NewGreedyByScoreOptimizer()
		o.Nodes = NewNodeView(nil)
		return addedGPUs(o.Optimize(context.Background(), reqs, constraints)), o
	}

	t.Run("in flight: X's draw of cycle 1 closes it in cycle 2", func(t *testing.T) {
		record := NewBorrowInFlight()
		now := time.Now()
		got, o := run([]*ResourceConstraints{physicalH100(64), restrictedPool(16)}, classed("serve", "x", serving))
		if got["x"] != 8 {
			t.Fatalf("cycle 1: X borrowed %d, want 8", got["x"])
		}
		record.RecordAt(o.BorrowDraws(), now)
		cycle2 := DeductUnreservedAt([]*ResourceConstraints{physicalH100(64), restrictedPool(16)}, record.Unreserved(now))
		if got, _ := run(cycle2, classed("lab", "y", canary)); got["y"] != 0 {
			t.Fatalf("cycle 2: Y borrowed %d of the restricted 8 while X's draw at 1000 is in flight, want 0", got["y"])
		}
	})

	for _, tc := range []struct {
		name     string
		priority int32
		want     int
	}{
		{"by read: the cohort borrows 8 at 1000", 1000, 0},
		{"control: borrowed at 100, which the lender at 500 outranks", 100, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := restrictedPool(8)
			pool.SharedPools[0].Borrowed = 8
			pool.SharedPools[0].BorrowerPriority = &tc.priority
			if got, _ := run([]*ResourceConstraints{physicalH100(64), pool}, classed("lab", "y", canary)); got["y"] != tc.want {
				t.Fatalf("Y borrowed %d, want %d", got["y"], tc.want)
			}
		})
	}

	t.Run("by read: a borrower's priority not read closes it", func(t *testing.T) {
		pool := restrictedPool(8)
		pool.SharedPools[0].Borrowed = 8
		if got, _ := run([]*ResourceConstraints{physicalH100(64), pool}, classed("lab", "y", canary)); got["y"] != 0 {
			t.Fatalf("Y borrowed %d, want 0", got["y"])
		}
	})
}
