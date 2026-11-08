package allocation

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// xWake is a wake's growth of x, in namespace a, by one 8-GPU replica into
// nominal its queue lent out in cohort c.
func xWake() []BorrowDraw {
	return []BorrowDraw{{Namespace: "a", ModelID: "x", Variant: "x-v", Cohort: "c", Flavor: "h100",
		Resource: "nvidia.com/gpu", Accelerator: "H100", GPUs: 8, Replicas: 1, Target: 1, Reclaim: true}}
}

// wakeYieldPass is one steady pass over y, in namespace b, holding 8 GPUs per
// replica borrowed from a's lent nominal, with the record's pending wake
// reclaims handed to it and reported yielded after it.
func wakeYieldPass(t *testing.T, record *BorrowInFlight, now time.Time, yCurrent int) domain.VariantDecision {
	t.Helper()
	physical := physicalH100(24)
	physical.Pools["H100"] = ResourcePool{Limit: 24, Used: 8 * yCurrent}
	quota := borrowQuota(0, nsBorrow{ns: "a", guaranteed: 8, lentOut: 8}, nsBorrow{ns: "b", guaranteed: 0, used: 8 * yCurrent})
	o := NewGreedyByScoreOptimizer()
	o.Nodes = NewNodeView(map[string]decision.NodeGPU{"n1": h100Node(8, nil), "n2": h100Node(8*(yCurrent-1), nil)})
	o.ReclaimCredit = record.Reclaims()
	o.WakeReclaims = record.PendingWakeReclaims(now)
	ds := o.Optimize(context.Background(), []ModelScalingRequest{borrowModel("b", "y", 8, yCurrent, 0, nil)},
		DeductReclaimed([]*ResourceConstraints{physical, quota}, record.Unreserved(now).InFlight))
	record.YieldedWakeReclaims(o.WakeReclaimsYielded())
	return decisionOf(t, ds, "y")
}

// A scale-from-zero wake that grows into nominal its queue lent out lowers
// this controller's own borrower, as a reclaim the steady pass orders does
// (yieldReclaimed), in 8-GPU units. Queue a lent its 8 to b, whose model y
// holds 16 borrowed. A wake grows x, in a, by one 8-GPU replica into that
// nominal: Kueue preempts one of y's replicas to admit it. The wake decides
// alone; the next steady pass lowers y 2 -> 1. The pass after it reads y at
// 1, x's replica still in flight: the same reclaim, already yielded for, so y
// stays at 1. Before, no pass yielded for a wake's reclaim: y kept its target
// 2, and its preempted replica was recreated to wait gated for GPUs gone.
func TestWakeReclaimLowersOwnBorrowerOnce(t *testing.T) {
	record := NewBorrowInFlight()
	now := time.Now()
	record.RecordWake(xWake(), now)
	y := wakeYieldPass(t, record, now, 2)
	if y.TargetReplicas != 1 {
		t.Fatalf("pass 1: y's target is %d, want 1: Kueue preempts one borrowed 8-GPU replica for the wake", y.TargetReplicas)
	}
	if steps := y.DecisionSteps; len(steps) == 0 || steps[len(steps)-1].Name != ReclaimYieldStepName {
		t.Fatalf("y's decision does not carry the %s step: %+v", ReclaimYieldStepName, steps)
	}
	if y := wakeYieldPass(t, record, now, 1); y.TargetReplicas != 1 {
		t.Fatalf("pass 2: y's target is %d, want 1: the wake's reclaim was yielded for once already", y.TargetReplicas)
	}
}

// Review #199 (6): the queue is reported yielded only by a pass that yielded
// for it, so a cycle that ran none (the cost-aware optimizer, a cycle that
// failed before its pass) loses nothing; and a wake whose entry the record
// dropped lowers nobody. Before, the queue was emptied when the cycle read
// it, before any pass, and handed even a dropped wake's reclaim to the pass.
func TestWakeReclaimSurvivesACycleWithoutAPass(t *testing.T) {
	record := NewBorrowInFlight()
	now := time.Now()
	record.RecordWake(xWake(), now)
	if got := record.PendingWakeReclaims(now); len(got) != 1 { // a cycle with no greedy pass reads it, and reports nothing
		t.Fatalf("pending = %+v, want x's reclaim", got)
	}
	if y := wakeYieldPass(t, record, now, 2); y.TargetReplicas != 1 {
		t.Fatalf("y's target is %d, want 1: the reclaim waited for a pass that yields for it", y.TargetReplicas)
	}

	cancelled := NewBorrowInFlight()
	cancelled.RecordWake(xWake(), now)
	// The record drops the wake's entry (its pods never came within
	// InFlightTTL); emulated by clearing the entries alone.
	cancelled.mu.Lock()
	clear(cancelled.entries)
	cancelled.mu.Unlock()
	if y := wakeYieldPass(t, cancelled, now, 2); y.TargetReplicas != 2 {
		t.Fatalf("y's target is %d, want 2: the wake's entry is gone, and it lowers nobody", y.TargetReplicas)
	}
}

// The record queues a wake's reclaim, not its draw on the cohort's pool, and
// forgets one older than InFlightTTL.
func TestPendingWakeReclaims(t *testing.T) {
	record := NewBorrowInFlight()
	now := time.Now()
	record.RecordWake([]BorrowDraw{
		{Namespace: "a", Variant: "x-v", Cohort: "c", GPUs: 8, Replicas: 1, Reclaim: true},
		{Namespace: "a", Variant: "w-v", Cohort: "c", GPUs: 8, Replicas: 1},
	}, now)
	got := record.PendingWakeReclaims(now)
	if len(got) != 1 || got[0].Variant != "x-v" || got[0].GPUs != 8 {
		t.Fatalf("pending = %+v, want x-v's 8-GPU reclaim alone", got)
	}
	record.YieldedWakeReclaims(got)
	if got := record.PendingWakeReclaims(now); got != nil {
		t.Fatalf("pending after yielded = %+v, want nothing: each reclaim is yielded for once", got)
	}
	record.RecordWake([]BorrowDraw{{Namespace: "a", Variant: "z-v", Cohort: "c", GPUs: 8, Replicas: 1, Reclaim: true}}, now)
	if got := record.PendingWakeReclaims(now.Add(InFlightTTL + time.Second)); got != nil {
		t.Fatalf("pending past InFlightTTL = %+v, want nothing", got)
	}
}
