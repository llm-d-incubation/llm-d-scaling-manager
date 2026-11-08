package scalefromzero

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// Issue #215: a wake decided in the instant before the steady-state engine
// had read the persisted in-flight ledger back did not see the holds it
// carries, and took the node an Unschedulable replica ordered before the
// restart was held for. Until the ledger is read back, a wake fitted to nodes
// waits; once it is, the wake sees the restored hold.
//
// The node store, the usage store and the in-flight ledger are process-wide:
// no t.Parallel.
func TestAWakeWaitsForTheRestoredInFlightHolds(t *testing.T) {
	publishUsage(t)
	publishNodes(t, 8)
	allocation.DefaultInFlight.ExpectRestore(time.Now())
	t.Cleanup(allocation.DefaultInFlight.Reset)
	e := physicalEngine(8)
	e.beginTick()
	if got := wakeOnce(t, e, "a", "cold", cand("cold-decode", "decode", wakeHBM3, 8, 1)); got != OutcomeAwaitingInFlight {
		t.Fatalf("a wake before the in-flight ledger was read back was judged (%s): it took the node a restored hold keeps", got)
	}

	// The steady-state engine reads the ledger back: an Unschedulable
	// replica's hold on the one free node.
	before := allocation.NewInFlightLedger()
	now := time.Now()
	before.Admit("b/warm-v", allocation.ReplicaShape{Variant: "b/warm-v", Accelerator: wakeHBM3, GPUs: 8}, 2, 1, nil, now)
	raw, err := before.MarshalInFlight(now)
	if err != nil {
		t.Fatal(err)
	}
	p, err := allocation.UnmarshalInFlight(raw)
	if err != nil {
		t.Fatal(err)
	}
	allocation.DefaultInFlight.RestoreInFlight(p, now, allocation.InFlightTTL)
	allocation.DefaultInFlight.MarkRestored()

	e.beginTick()
	if got := wakeOnce(t, e, "a", "cold", cand("cold-decode", "decode", wakeHBM3, 8, 1)); got != OutcomeNoCapacity {
		t.Fatalf("a wake after the read-back took the node a restored hold keeps: %s, want %s", got, OutcomeNoCapacity)
	}
}

// A ledger that is never read back holds wakes for inFlightRestoreWait, not
// for good: a parked model still wakes.
func TestAWakeStopsWaitingForAnUnreadLedger(t *testing.T) {
	publishUsage(t)
	publishNodes(t, 8)
	start := time.Now()
	allocation.DefaultInFlight.ExpectRestore(start)
	t.Cleanup(allocation.DefaultInFlight.Reset)
	e := physicalEngine(8)
	e.beginTick()
	if got := wakeOnce(t, e, "a", "cold", cand("cold-decode", "decode", wakeHBM3, 8, 1)); got != OutcomeAwaitingInFlight {
		t.Fatalf("setup: the wake did not wait (%s)", got)
	}
	allocation.DefaultInFlight.ExpectRestore(start.Add(-inFlightRestoreWait))
	e.beginTick()
	if got := wakeOnce(t, e, "a", "cold", cand("cold-decode", "decode", wakeHBM3, 8, 1)); got == OutcomeAwaitingInFlight || got == OutcomeNoCapacity {
		t.Fatalf("a wake still waited, or was refused, past inFlightRestoreWait: %s", got)
	}
}

// Once wakes have given up on the ledger they do not wait again, even if
// the restore is pending anew within the window.
func TestGivingUpOnTheLedgerIsSticky(t *testing.T) {
	publishUsage(t)
	publishNodes(t, 8)
	allocation.DefaultInFlight.ExpectRestore(time.Now().Add(-inFlightRestoreWait))
	t.Cleanup(allocation.DefaultInFlight.Reset)
	e := physicalEngine(8)
	e.beginTick()
	if got := wakeOnce(t, e, "a", "cold", cand("cold-decode", "decode", wakeHBM3, 8, 1)); got == OutcomeAwaitingInFlight {
		t.Fatalf("setup: the wake waited past inFlightRestoreWait (%s)", got)
	}
	allocation.DefaultInFlight.ExpectRestore(time.Now())
	t.Cleanup(allocation.DefaultInFlight.Reset)
	e.beginTick()
	if got := wakeOnce(t, e, "b", "cold2", cand("cold2-decode", "decode", wakeHBM3, 8, 1)); got == OutcomeAwaitingInFlight {
		t.Fatal("a wake waited again after wakes had given up on the ledger")
	}
}

// noopExecutor runs nothing: StartOptimizeLoop returns at once.
type noopExecutor struct{}

func (noopExecutor) Start(context.Context) {}

// The engines run only while the replica leads. A standby that waited for
// the lease longer than inFlightRestoreWait -- its process started half an
// hour ago -- still waits for the holds when it becomes leader: the clock
// starts in StartOptimizeLoop, not at the process start.
func TestTheRestoreWaitRunsFromLeadership(t *testing.T) {
	publishUsage(t)
	publishNodes(t, 8)
	allocation.DefaultInFlight.ExpectRestore(time.Now().Add(-30 * time.Minute))
	t.Cleanup(allocation.DefaultInFlight.Reset)
	e := physicalEngine(8)
	e.executor = noopExecutor{}
	e.StartOptimizeLoop(context.Background()) // this replica wins the lease now
	e.beginTick()
	if got := wakeOnce(t, e, "a", "cold", cand("cold-decode", "decode", wakeHBM3, 8, 1)); got != OutcomeAwaitingInFlight {
		t.Fatalf("a late leader's first wake did not wait for the holds (%s): the clock ran from the process start", got)
	}
}

// A wake the physical limiter does not bound builds no node view, reads no
// hold, and does not wait.
func TestAWakeNotBoundByNodesDoesNotWait(t *testing.T) {
	publishUsage(t)
	publishNodes(t, 8)
	allocation.DefaultInFlight.ExpectRestore(time.Now())
	t.Cleanup(allocation.DefaultInFlight.Reset)
	e := &Engine{gpuLimiter: okProvider{name: "cluster-quota", pools: map[string]allocation.ResourcePool{wakeHBM3: {Limit: 8}}}}
	e.beginTick()
	if got := wakeOnce(t, e, "a", "cold", cand("cold-decode", "decode", wakeHBM3, 8, 1)); got == OutcomeAwaitingInFlight || got == OutcomeNoCapacity {
		t.Fatalf("a wake under a quota alone waited, or was refused: %s", got)
	}
}

// A wake that waits is not a refusal: no llmsm_model_scaling_blocked reason is
// set for it.
func TestAWaitingWakeSetsNoBlockedReason(t *testing.T) {
	publishUsage(t)
	publishNodes(t, 8)
	allocation.DefaultInFlight.ExpectRestore(time.Now())
	t.Cleanup(allocation.DefaultInFlight.Reset)
	e := physicalEngine(8)
	e.beginTick()
	group := modelGroup{namespace: "a", modelID: "cold"}
	_, selected, outcome := e.wakeVerdict(context.Background(), group,
		[]Candidate{cand("cold-decode", "decode", wakeHBM3, 8, 1)}, coverage{})
	if outcome != OutcomeAwaitingInFlight || len(selected) != 0 {
		t.Fatalf("setup: outcome %s with %d selected, want %s and none", outcome, len(selected), OutcomeAwaitingInFlight)
	}
	if _, set := e.wakeBlocked.Load(group.key()); set {
		t.Fatal("a wake waiting for the in-flight holds set a scaling-blocked reason")
	}
}
