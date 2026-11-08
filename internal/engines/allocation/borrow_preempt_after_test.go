package allocation

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
)

// afterQuota is cohort "c" with 8 free GPUs in its one chunk: namespace a, a
// reclaiming queue at nominal 16 using 8, whose PreemptAfter is 8 -- kept of
// it as given, the rest covered by the pool -- and namespace b, which borrows
// with no cap.
func afterQuota(kept int) *ResourceConstraints {
	c := borrowQuota(8,
		nsBorrow{ns: "a", guaranteed: 16, used: 8, cap: 0, wn: 16, after: 8},
		nsBorrow{ns: "b", cap: config.QuotaUnlimited})
	ab := c.NamespaceBorrow["a"]["H100"]
	ab.PreemptKept = kept
	c.NamespaceBorrow["a"]["H100"] = ab
	return c
}

// Review M-m1: growth into what a reclaiming queue keeps unused takes nothing
// from the cohort's pool -- Kueue never counts it there. a grows one 8-GPU
// replica into its kept 8; b borrows the pool's 8 in the same pass. Debiting
// the kept part refused b. Control: a's 8 come from the pool, and b is
// refused.
func TestKeptGrowthTakesNothingFromThePool(t *testing.T) {
	for _, tc := range []struct {
		name string
		kept int
		want int
	}{
		{"a grows into its kept 8: b still borrows the pool's 8", 8, 8},
		{"control: a's 8 come from the pool: b gets none", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := NewGreedyByScoreOptimizer()
			o.Nodes = NewNodeView(map[string]decision.NodeGPU{"n1": h100Node(0, nil), "n2": h100Node(0, nil)})
			ds := o.Optimize(context.Background(), []ModelScalingRequest{
				borrowModel("a", "ma", 8, 1, 2, nil), borrowModel("b", "mb", 8, 0, 1, nil),
			}, []*ResourceConstraints{physicalH100(64), afterQuota(tc.kept)})
			got := addedGPUs(ds)
			if got["ma"] != 8 {
				t.Fatalf("a grew %d, want 8", got["ma"])
			}
			if got["mb"] != tc.want {
				t.Fatalf("b borrowed %d, want %d (a's PreemptKept %d)", got["mb"], tc.want, tc.kept)
			}
		})
	}

	// The wake records no pool part for growth into what its queue keeps.
	draws := WakeDraws([]*ResourceConstraints{afterQuota(8)}, "a", "ma",
		[]WakeReplica{{Variant: "ma-v", Accelerator: "H100", GPUs: 8}}, nil, nil)
	after := DeductUnreservedAt([]*ResourceConstraints{afterQuota(8)}, Unreserved{InFlight: draws})
	if free := after[0].SharedPools[0].Free; free != 8 {
		t.Fatalf("a wake into its kept 8 left the pool at %d, want 8 (entries %+v)", free, draws)
	}
}

// Review round 3 minors: steady growth into PreemptAfter -- the part the
// pool covers -- is held in the shared record, so the wake (or a queue that
// cannot reclaim reaching through the pool) does not take the same 8 in the
// same window; and DeductReclaimed lowers PreemptAfter by it, as it lowers
// LentOut by what was reclaimed. Before, only the reclaimed part was recorded,
// and PreemptAfter never moved.
func TestPreemptAfterGrowthIsRecorded(t *testing.T) {
	o := NewGreedyByScoreOptimizer()
	o.Nodes = NewNodeView(map[string]decision.NodeGPU{"n1": h100Node(0, nil)})
	ds := o.Optimize(context.Background(), []ModelScalingRequest{borrowModel("a", "ma", 8, 1, 2, nil)},
		[]*ResourceConstraints{physicalH100(64), afterQuota(0)})
	if got := addedGPUs(ds)["ma"]; got != 8 {
		t.Fatalf("a grew %d, want 8", got)
	}
	record := NewBorrowInFlight()
	now := time.Now()
	record.RecordAt(o.ReclaimDraws(), now)
	u := record.Unreserved(now)
	if free := DeductUnreservedAt([]*ResourceConstraints{afterQuota(0)}, u)[0].SharedPools[0].Free; free != 0 {
		t.Fatalf("the pool reads %d free for the next decision, want 0: a's growth took its 8 (record %+v)", free, u.InFlight)
	}
	if pa := DeductReclaimed([]*ResourceConstraints{afterQuota(0)}, u.InFlight)[0].NamespaceBorrow["a"]["H100"].PreemptAfter; pa != 0 {
		t.Fatalf("a's PreemptAfter reads %d for the next decision, want 0", pa)
	}
}
