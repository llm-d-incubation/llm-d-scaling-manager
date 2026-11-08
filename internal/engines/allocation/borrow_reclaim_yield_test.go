package allocation

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// 186-3: a reclaim yields once. Queue a lent its 8 to b, whose model y holds
// 16 borrowed. Cycle 1: x grows 8 into a's lent nominal, y gives back one
// replica (2 -> 1). Cycle 2 reads y at 1 and x's replica still in flight,
// pods not created: the pass re-places it on its own reclaim entry
// (CreditGPUs), which is no new reclaim. Before, the yield summed the draw's
// GPUs, credit included, and lowered y again, 1 -> 0, for the same replica.
func TestReclaimYieldDoesNotThrash(t *testing.T) {
	record := NewBorrowInFlight()
	now := time.Now()
	run := func(yCurrent int) domain.VariantDecision {
		physical := physicalH100(24)
		physical.Pools["H100"] = ResourcePool{Limit: 24, Used: 8 * yCurrent}
		quota := borrowQuota(0, nsBorrow{ns: "a", guaranteed: 8, lentOut: 8},
			nsBorrow{ns: "b", guaranteed: 0, used: 8 * yCurrent})
		o := NewGreedyByScoreOptimizer()
		o.Nodes = NewNodeView(map[string]decision.NodeGPU{"n1": h100Node(8, nil), "n2": h100Node(8*(yCurrent-1), nil)})
		o.ReclaimCredit = record.Reclaims()
		ds := o.Optimize(context.Background(), []ModelScalingRequest{
			borrowModel("a", "x", 8, 0, 1, nil), borrowModel("b", "y", 8, yCurrent, 0, nil)},
			DeductReclaimed([]*ResourceConstraints{physical, quota}, record.Unreserved(now).InFlight))
		if got := addedGPUs(ds)["x"]; got != 8 {
			t.Fatalf("y at %d: x grew %d GPUs, want 8", yCurrent, got)
		}
		record.Trim(map[string]int{"a/x-v": decisionOf(t, ds, "x").TargetReplicas})
		record.RecordAt(o.ReclaimDraws(), now)
		return decisionOf(t, ds, "y")
	}
	if y := run(2); y.TargetReplicas != 1 {
		t.Fatalf("cycle 1: y's target is %d, want 1: Kueue preempts one borrowed replica for x", y.TargetReplicas)
	}
	if y := run(1); y.TargetReplicas != 1 {
		t.Fatalf("cycle 2: y's target is %d, want 1: x's replica is the same one, already yielded for", y.TargetReplicas)
	}
}

// yieldState is a pass that reclaimed gpus of H100 in cohort c for namespace
// a, beside namespace b holding past its guaranteed figure.
func yieldState(gpus, bHeldPast int) *borrowState {
	return &borrowState{
		reclaims: []BorrowDraw{{Namespace: "a", Cohort: "c", Accelerator: "H100", GPUs: gpus, Reclaim: true}},
		heldPast: map[string]map[string]int{"b": {"H100": bHeldPast}},
		cohorts:  map[string][]string{"b": {"c"}},
	}
}

// 186-3: a P/D borrower gives back whole pairs at its role ratio, and never
// goes below minReplicas.
//   - 2P:2D, 8 GPUs reclaimed: one prefill and one decode go (1P:1D). Before,
//     one variant at a time in name order took the decode alone (2P:1D).
//   - 1P:2D: one pair is the prefill and both decodes.
//   - a single-role borrower at minReplicas 1 keeps its replica. Before, it
//     went to 0.
func TestReclaimYieldKeepsRatioAndMinimum(t *testing.T) {
	one := 1
	pd := func(p, d int) ([]domain.VariantDecision, []ModelScalingRequest) {
		ds := []domain.VariantDecision{
			{Namespace: "b", ModelID: "pd", VariantName: "d", Role: "decode", AcceleratorName: "H100", GPUsPerReplica: 8,
				CurrentReplicas: d, TargetReplicas: d},
			{Namespace: "b", ModelID: "pd", VariantName: "p", Role: "prefill", AcceleratorName: "H100", GPUsPerReplica: 8,
				CurrentReplicas: p, TargetReplicas: p},
		}
		req := ModelScalingRequest{Namespace: "b", ModelID: "pd", Disaggregated: true, VariantStates: []domain.VariantReplicaState{
			{VariantName: "d", Role: "decode", CurrentReplicas: d, GPUsPerReplica: 8},
			{VariantName: "p", Role: "prefill", CurrentReplicas: p, GPUsPerReplica: 8},
		}}
		return ds, []ModelScalingRequest{req}
	}
	ds, reqs := pd(2, 2)
	yieldState(8, 32).yieldReclaimed(ds, reqs)
	if ds[0].TargetReplicas != 1 || ds[1].TargetReplicas != 1 {
		t.Errorf("2P:2D yielded to %dP:%dD, want 1P:1D: a whole pair goes", ds[1].TargetReplicas, ds[0].TargetReplicas)
	}
	ds, reqs = pd(1, 2)
	yieldState(16, 24).yieldReclaimed(ds, reqs)
	if ds[0].TargetReplicas != 0 || ds[1].TargetReplicas != 0 {
		t.Errorf("1P:2D yielded to %dP:%dD for 16 GPUs, want 0P:0D: its one pair goes whole", ds[1].TargetReplicas, ds[0].TargetReplicas)
	}
	// A unit far larger than the reclaim is not given: 3P:4D's smallest
	// pair is 7 replicas, 56 GPUs, for 8 reclaimed.
	ds, reqs = pd(3, 4)
	yieldState(8, 56).yieldReclaimed(ds, reqs)
	if ds[0].TargetReplicas != 4 || ds[1].TargetReplicas != 3 {
		t.Errorf("3P:4D yielded to %dP:%dD for 8 GPUs, want 3P:4D: its pair overshoots by 48", ds[1].TargetReplicas, ds[0].TargetReplicas)
	}
	single := []domain.VariantDecision{{Namespace: "b", ModelID: "y", VariantName: "y-v", AcceleratorName: "H100",
		GPUsPerReplica: 8, CurrentReplicas: 1, TargetReplicas: 1}}
	yieldState(8, 8).yieldReclaimed(single, []ModelScalingRequest{{Namespace: "b", ModelID: "y",
		VariantStates: []domain.VariantReplicaState{{VariantName: "y-v", CurrentReplicas: 1, GPUsPerReplica: 8, MinReplicas: &one}}}})
	if single[0].TargetReplicas != 1 {
		t.Errorf("a borrower at minReplicas 1 yielded to %d, want 1: no variant goes below its minReplicas", single[0].TargetReplicas)
	}
}

// KU3: a reclaim from a model this controller manages. Queue a (Any, nominal
// 8) lent its 8 to queue b, whose model y -- managed here too -- holds them
// borrowed on the one node. x in a grows into that nominal: Kueue preempts
// y's borrowed replica to admit it. The pass must lower y's target by the
// same replica, so that what it orders, x's 8 and y's, never exceeds the 8
// Kueue can admit. Before, y kept its target 1: its replica was recreated and
// waited gated for capacity that was gone. Control: b's borrower is not
// managed here, and x's reclaim lowers nothing.
func TestReclaimFromOwnBorrowerLowersItsTarget(t *testing.T) {
	for _, tc := range []struct {
		name    string
		managed bool
		wantY   int
	}{
		{"own borrower: y yields its replica", true, 0},
		{"control: borrower not managed here", false, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			physical := physicalH100(8)
			physical.Pools["H100"] = ResourcePool{Limit: 8, Used: 8}
			nss := []nsBorrow{{ns: "a", guaranteed: 8, lentOut: 8}}
			reqs := []ModelScalingRequest{borrowModel("a", "x", 8, 0, 1, nil)}
			if tc.managed {
				nss = append(nss, nsBorrow{ns: "b", guaranteed: 0, used: 8})
				reqs = append(reqs, borrowModel("b", "y", 8, 1, 0, nil))
			}
			o := NewGreedyByScoreOptimizer()
			o.Nodes = NewNodeView(map[string]decision.NodeGPU{"n1": h100Node(8, nil)})
			ds := o.Optimize(context.Background(), reqs, []*ResourceConstraints{physical, borrowQuota(0, nss...)})
			if got := addedGPUs(ds)["x"]; got != 8 {
				t.Fatalf("x grew %d GPUs, want 8: Kueue preempts the borrower for it", got)
			}
			if !tc.managed {
				return
			}
			y := decisionOf(t, ds, "y")
			if y.TargetReplicas != tc.wantY {
				t.Fatalf("y's target is %d, want %d: Kueue preempts its borrowed replica for x", y.TargetReplicas, tc.wantY)
			}
			total := 0
			for _, d := range ds {
				total += d.TargetReplicas * d.GPUsPerReplica
			}
			if total > 8 {
				t.Fatalf("the pass orders %d GPUs in all, more than the 8 Kueue can admit", total)
			}
			if steps := y.DecisionSteps; len(steps) == 0 || steps[len(steps)-1].Name != ReclaimYieldStepName ||
				y.Action != domain.ActionScaleDown {
				t.Fatalf("y's decision does not say why: action %q, steps %+v", y.Action, steps)
			}
			// Yielding is not scarcity: no limiter is named, so neither
			// llmsm_decisions_limited_total nor a ResourceConstrained event
			// counts it.
			if y.WasLimited || y.LimitedBy != "" {
				t.Fatalf("y's yield reads as limited (by %q): it is not a limiter's decision", y.LimitedBy)
			}
		})
	}
}
