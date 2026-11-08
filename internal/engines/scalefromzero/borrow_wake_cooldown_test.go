package scalefromzero

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// Review AR1: a wake into nominal its queue lent out places without a node
// hole, Kueue preempting the borrowers. When no node frees, the governor's
// safety net releases it after the placement timeout and starts the model's
// cooldown. The wake did not read that cooldown: it woke the model again
// without a hole at once, and Kueue preempted a peer for it every placement
// timeout. During the cooldown the wake needs a real node hole; with one, it
// still wakes.
//
// The node store, the usage store, the in-flight ledger and the published
// cooldowns are process-wide: no t.Parallel.
func TestAReleasedNoHoleWakeNeedsAHoleDuringItsCooldown(t *testing.T) {
	ctx := context.Background()
	publishUsage(t)
	allocation.DefaultBorrowInFlight.Reset()
	t.Cleanup(allocation.DefaultBorrowInFlight.Reset)
	t.Cleanup(func() { allocation.DefaultBorrowCooldowns.Publish(nil) })

	// b's queue lent 16 to a borrower whose pods fill both 8-GPU nodes.
	limiter := allocation.NewCompositeLimiter("composite", []allocation.Limiter{
		okProvider{name: allocation.PhysicalLimiterName, pools: map[string]allocation.ResourcePool{wakeHBM3: {Limit: 16, Used: 16}}},
		lenderProvider{lentOut: 16},
	})
	wake := func(free ...int) []Candidate {
		t.Helper()
		publishNodes(t, free...)
		allocation.DefaultBorrowInFlight.Reset()
		e := &Engine{gpuLimiter: limiter}
		e.beginTick()
		_, selected, _ := e.decideWake(ctx, modelGroup{namespace: "b", modelID: "m"},
			[]Candidate{cand("m-decode", "decode", wakeHBM3, 8, 1)}, coverage{})
		return selected
	}

	allocation.DefaultBorrowCooldowns.Publish(nil)
	if got := wake(0, 0); len(got) != 1 {
		t.Fatalf("setup: with no cooldown the lender's wake woke %v, want its decode without a hole", names(got))
	}

	g := allocation.NewBorrowGovernor(allocation.DefaultBorrowSettings())
	g.MarkPreemptedIn("b/m", []string{"co"}, time.Now())
	allocation.DefaultBorrowCooldowns.Publish(g)
	if got := wake(0, 0); len(got) != 0 {
		t.Fatalf("in its cooldown the model woke %v without a node hole, want none", names(got))
	}
	if got := wake(0, 8); len(got) != 1 {
		t.Fatalf("in its cooldown with a free 8-GPU node the model woke %v, want its decode", names(got))
	}
	// Another model of the namespace is not cooling down.
	publishNodes(t, 0, 0)
	e := &Engine{gpuLimiter: limiter}
	e.beginTick()
	if _, got, _ := e.decideWake(ctx, modelGroup{namespace: "b", modelID: "other"},
		[]Candidate{cand("other-decode", "decode", wakeHBM3, 8, 1)}, coverage{}); len(got) != 1 {
		t.Fatalf("a model not in cooldown woke %v, want its decode without a hole", names(got))
	}
}
