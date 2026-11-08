package allocation

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// figuresOf is queue "a"'s figures in cohort "c" of queues qs, as the reader
// computes them.
func figuresOf(t *testing.T, qs ...kueue.QueueState) kueue.QueueFigures {
	t.Helper()
	topo := kueue.Topology{Queues: map[string]kueue.QueueState{}}
	for _, q := range qs {
		q.Cohort, q.HasStatus = "c", true
		topo.Queues[q.Name] = q
	}
	figs := kueue.ComputeBorrowing(topo, false).Queues["a"]
	if len(figs) != 1 {
		t.Fatalf("queue a has %d figures, want 1", len(figs))
	}
	return figs[0]
}

// h100Queue is a queue of one H100 resource on flavor h100.
func h100Queue(name, reclaim string, r kueue.QueueResource) kueue.QueueState {
	return kueue.QueueState{Name: name, Reclaim: reclaim,
		Resources: map[kueue.ResourceKey]kueue.QueueResource{{Flavor: "h100", Resource: "nvidia.com/gpu"}: r}}
}

// decisionOf is the decision for model id.
func decisionOf(t *testing.T, ds []domain.VariantDecision, id string) domain.VariantDecision {
	t.Helper()
	for _, d := range ds {
		if d.ModelID == id {
			return d
		}
	}
	t.Fatalf("no decision for %s", id)
	return domain.VariantDecision{}
}

// Review N1: a lender's unused nominal that nobody borrows frees no node when
// it grows into it. Queue a (reclaimWithinCohort Any, nominal 32) runs two
// 8-GPU replicas; b runs one within its nominal 16; nobody borrows. The fourth
// node is NotReady, so the view lists three, all full. Kueue admits a's next
// replicas from the free pool and preempts nothing: no node frees, and a pod
// ordered without a hole stays Pending for good. The growth must be refused
// by node fit. Control: b borrows 16 of a's nominal on the same full nodes,
// and Kueue preempts it to admit a's two replicas.
func TestLenderGrowthIntoUnusedNominalNeedsANode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		peer     kueue.QueueResource
		nodes    int
		wantLent int
		want     int
	}{
		{"nobody borrows, a node NotReady: node fit refuses", kueue.QueueResource{Nominal: 16, Reserved: 8}, 3, 0, 0},
		{"control: b borrows 16 of a's nominal: Kueue preempts it", kueue.QueueResource{Nominal: 0, Reserved: 16, Borrowed: 16}, 4, 16, 16},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := figuresOf(t, h100Queue("a", kueue.ReclaimAny, kueue.QueueResource{Nominal: 32, Reserved: 16}),
				h100Queue("b", kueue.ReclaimNever, tc.peer))
			nodes := map[string]decision.NodeGPU{}
			for _, n := range []string{"n1", "n2", "n3", "n4"}[:tc.nodes] {
				nodes[n] = h100Node(8, nil)
			}
			physical := physicalH100(32)
			physical.Pools["H100"] = ResourcePool{Limit: 32, Used: 24}
			q := borrowQuota(f.CohortFree, nsBorrow{ns: "a", guaranteed: f.Guaranteed, used: 16, cap: 0, wn: f.WithinNominal,
				lentOut: f.LentOut, after: f.PreemptAfter})
			o := NewGreedyByScoreOptimizer()
			o.Nodes = NewNodeView(nodes)
			ds := o.Optimize(context.Background(), []ModelScalingRequest{borrowModel("a", "m", 8, 2, 2, nil)},
				[]*ResourceConstraints{physical, q})
			if got := addedGPUs(ds)["m"]; got != tc.want {
				t.Fatalf("a grew %d GPUs, want %d (LentOut %d)", got, tc.want, f.LentOut)
			}
			if f.LentOut != tc.wantLent {
				t.Fatalf("a's LentOut is %d, want %d (what b borrows)", f.LentOut, tc.wantLent)
			}
			if tc.want == 0 {
				if d := decisionOf(t, ds, "m"); !d.WasLimited || d.LimitedBy != NodeFitLimiterName {
					t.Fatalf("limited %v by %q, want node fit (%q)", d.WasLimited, d.LimitedBy, NodeFitLimiterName)
				}
			}
		})
	}
}

// Review N1, wide EP: the exemption covers a replica only whole. Queue a
// (Any, nominal 16, lendingLimit 8) keeps 8 and lends 8, which b borrows. A
// 16-GPU replica of a (two 8-GPU pods) is admitted on a's kept 8 and on the 8
// Kueue preempts b for: one node frees, and the other pod needs a hole no
// node has (two nodes hold 4 free each). Control: b borrows all 16 of a's
// nominal, and Kueue's preemption frees both nodes.
func TestWideReplicaReclaimedOnlyWhole(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b kueue.QueueResource
		want int
	}{
		{"8 borrowed, a 16-GPU replica: refused", kueue.QueueResource{Nominal: 16, LendingLimit: intPtr(8)},
			kueue.QueueResource{Nominal: 0, Reserved: 8, Borrowed: 8}, 0},
		{"control: 16 borrowed: placed", kueue.QueueResource{Nominal: 16},
			kueue.QueueResource{Nominal: 0, Reserved: 16, Borrowed: 16}, 16},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := figuresOf(t, h100Queue("a", kueue.ReclaimAny, tc.a), h100Queue("b", kueue.ReclaimNever, tc.b))
			physical := physicalH100(32)
			physical.Pools["H100"] = ResourcePool{Limit: 32, Used: 24}
			q := borrowQuota(f.CohortFree, nsBorrow{ns: "a", guaranteed: f.Guaranteed, cap: 0, wn: f.WithinNominal,
				lentOut: f.LentOut, after: f.PreemptAfter})
			o := NewGreedyByScoreOptimizer()
			o.Nodes = NewNodeView(map[string]decision.NodeGPU{
				"n1": h100Node(8, nil), "n2": h100Node(8, nil), "n3": h100Node(4, nil), "n4": h100Node(4, nil)})
			ds := o.Optimize(context.Background(), []ModelScalingRequest{borrowModel("a", "m", 16, 0, 1, []int{8, 8})},
				[]*ResourceConstraints{physical, q})
			if got := addedGPUs(ds)["m"]; got != tc.want {
				t.Fatalf("a grew %d GPUs, want %d (LentOut %d, PreemptAfter %d)", got, tc.want, f.LentOut, f.PreemptAfter)
			}
		})
	}
}

// Review NEW-2 (M2): a wide replica is exempt from the node hole only when
// Kueue preempts for all of it. Namespace a has 8 of PreemptAfter left (growth
// Kueue admits from the free pool, preempting nothing) and peers borrow 16 of
// its nominal. A 16-GPU replica (two 8-GPU pods) takes the 8 first: that pod
// needs a hole, and one node of four is down, so the view holds one 8-GPU
// hole -- n3's -- and n1 and n2 are full. Exempted whole, the replica was
// ordered and half its gang waited Pending for a hole no preemption frees.
// Steady placer and wake alike. Control: nothing of After left.
func TestWideReplicaWithAfterLeftNeedsAHole(t *testing.T) {
	nodes := func() map[string]decision.NodeGPU {
		return map[string]decision.NodeGPU{"n1": h100Node(8, nil), "n2": h100Node(8, nil), "n3": h100Node(0, nil)}
	}
	for _, tc := range []struct {
		name  string
		after int
		want  int
	}{
		{"After 8, LentOut 16, one node down: refused", 8, 0},
		{"control: After 0, LentOut 16: Kueue preempts for it whole", 0, 16},
	} {
		t.Run(tc.name, func(t *testing.T) {
			physical := physicalH100(32)
			physical.Pools["H100"] = ResourcePool{Limit: 32, Used: 16}
			q := borrowQuota(0, nsBorrow{ns: "a", guaranteed: 32, cap: 0, wn: 32, lentOut: 16, after: tc.after})
			o := NewGreedyByScoreOptimizer()
			o.Nodes = NewNodeView(nodes())
			ds := o.Optimize(context.Background(), []ModelScalingRequest{borrowModel("a", "m", 16, 0, 1, []int{8, 8})},
				[]*ResourceConstraints{physical, q})
			if got := addedGPUs(ds)["m"]; got != tc.want {
				t.Fatalf("steady: a grew %d GPUs, want %d (After %d, LentOut 16)", got, tc.want, tc.after)
			}
			placed, _ := NewNodeView(nodes()).PlaceReplicasReclaiming(
				[]ReplicaShape{{Variant: "a/m-v", Accelerator: "H100", GPUs: 16, PodGPUs: []int{8, 8}}}, true,
				map[string]Reclaim{"H100": {After: tc.after, GPUs: 16}})
			if placed != (tc.want > 0) {
				t.Fatalf("wake: placed %v, want %v (After %d, LentOut 16)", placed, tc.want > 0, tc.after)
			}
		})
	}
}

// Review N1: what a cycle grew into by Kueue's reclaim is held in flight, so
// the next cycle does not order replicas for the same borrowed GPUs again
// before a read shows them. b borrows 8 of a's nominal; cycle 1 orders a's
// replica without a hole; cycle 2, the same read, must not order another.
func TestReclaimHeldAcrossCycles(t *testing.T) {
	f := figuresOf(t, h100Queue("a", kueue.ReclaimAny, kueue.QueueResource{Nominal: 8}),
		h100Queue("b", kueue.ReclaimNever, kueue.QueueResource{Nominal: 0, Reserved: 8, Borrowed: 8}))
	constraints := func() []*ResourceConstraints {
		physical := physicalH100(8)
		physical.Pools["H100"] = ResourcePool{Limit: 8, Used: 8}
		return []*ResourceConstraints{physical, borrowQuota(f.CohortFree, nsBorrow{ns: "a", guaranteed: f.Guaranteed,
			cap: 0, wn: f.WithinNominal, lentOut: f.LentOut, after: f.PreemptAfter})}
	}
	record := NewBorrowInFlight()
	run := func(model string) int {
		o := NewGreedyByScoreOptimizer()
		o.Nodes = NewNodeView(map[string]decision.NodeGPU{"n1": h100Node(8, nil)})
		ds := o.Optimize(context.Background(), []ModelScalingRequest{borrowModel("a", model, 8, 0, 1, nil)},
			DeductReclaimed(constraints(), record.Unreserved(time.Now()).InFlight))
		record.Record(o.ReclaimDraws())
		return addedGPUs(ds)[model]
	}
	if got := run("m1"); got != 8 {
		t.Fatalf("cycle 1 grew %d GPUs, want 8: Kueue preempts b for it", got)
	}
	if got := run("m2"); got != 0 {
		t.Fatalf("cycle 2 grew %d GPUs on the same 8 borrowed GPUs, want 0", got)
	}
}

// Review NEW-1: the SAME model across cycles, before its pods exist. b
// borrows 8 of a's nominal and the one node is full; cycle 1 orders m's
// replica without a hole, Kueue's to reclaim. Cycle 2 reads the same, m's
// pods not created: its own entry came off a's LentOut (DeductReclaimed), and
// the pass re-places it from current 0. Without the credit node fit refused
// it, Trim dropped the entry and cycle 3 ordered it again -- 3/2/3, a borrower
// preempted for nothing. With it m keeps its replica every cycle, and the
// record holds the one 8-GPU entry, not two.
func TestReclaimSameModelHeldAcrossCycles(t *testing.T) {
	f := figuresOf(t, h100Queue("a", kueue.ReclaimAny, kueue.QueueResource{Nominal: 8}),
		h100Queue("b", kueue.ReclaimNever, kueue.QueueResource{Nominal: 0, Reserved: 8, Borrowed: 8}))
	record := NewBorrowInFlight()
	now := time.Now()
	for cycle := 1; cycle <= 3; cycle++ {
		physical := physicalH100(8)
		physical.Pools["H100"] = ResourcePool{Limit: 8, Used: 8}
		constraints := []*ResourceConstraints{physical, borrowQuota(f.CohortFree, nsBorrow{ns: "a", guaranteed: f.Guaranteed,
			cap: 0, wn: f.WithinNominal, lentOut: f.LentOut, after: f.PreemptAfter})}
		o := NewGreedyByScoreOptimizer()
		o.Nodes = NewNodeView(map[string]decision.NodeGPU{"n1": h100Node(8, nil)})
		o.ReclaimCredit = record.Reclaims()
		ds := o.Optimize(context.Background(), []ModelScalingRequest{borrowModel("a", "m1", 8, 0, 1, nil)},
			DeductReclaimed(constraints, record.Unreserved(now).InFlight))
		if got := addedGPUs(ds)["m1"]; got != 8 {
			t.Fatalf("cycle %d grew m1 by %d GPUs, want 8: its own reclaimed replica, pods not created yet", cycle, got)
		}
		record.Trim(map[string]int{"a/m1-v": decisionOf(t, ds, "m1").TargetReplicas})
		record.RecordAt(o.ReclaimDraws(), now)
		if rs := record.Reclaims(); len(rs) != 1 || rs[0].GPUs != 8 {
			t.Fatalf("cycle %d: the record holds %+v, want the one 8-GPU reclaim", cycle, rs)
		}
	}
}

func intPtr(n int) *int { return &n }
