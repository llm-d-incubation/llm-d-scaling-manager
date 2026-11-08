package allocation

import (
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
)

// The in-flight ledger, in whole 8-GPU units. Each test builds its own ledger;
// NodeViewFor reads DefaultInFlight, which these tests do not touch.

// viewAt is a view of one free 8-GPU node, its snapshot taken at at.
func viewAt(at time.Time) *NodeView {
	return newNodeViewAt(h100Nodes(8), at)
}

// eight is one 8-GPU replica.
var eight = ReplicaShape{Variant: "ns/m", Accelerator: famHBM3, GPUs: 8}

func TestAnAdmittedReplicaHoldsItsHoleUntilTheTTL(t *testing.T) {
	l := NewInFlightLedger()
	t0 := time.Unix(1000, 0)
	l.Admit("ns/m", eight, 1, 0, nil, t0)
	v := viewAt(t0)
	l.holdOn(v, t0.Add(InFlightTTL-time.Second))
	if v.fits(replicaFit{acc: famHBM3, pods: []int{8}}, 1) != 0 {
		t.Fatal("inside the TTL the admitted replica's hole is free")
	}
	v = viewAt(t0)
	l.holdOn(v, t0.Add(InFlightTTL))
	if v.fits(replicaFit{acc: famHBM3, pods: []int{8}}, 1) != 1 {
		t.Fatal("past the TTL the hole is still held")
	}
	// The same want admitted again does not restart the clock: a replica
	// Pending for good is not held every cycle anew.
	l.Admit("ns/m", eight, 1, 0, nil, t0.Add(InFlightTTL+time.Second))
	if l.Holds("ns/m", t0.Add(InFlightTTL+2*time.Second)) {
		t.Fatal("re-admitting the same target restarted the TTL")
	}
	// More replicas do.
	l.Admit("ns/m", eight, 2, 0, nil, t0.Add(InFlightTTL+time.Second))
	if !l.Holds("ns/m", t0.Add(InFlightTTL+2*time.Second)) {
		t.Fatal("a higher target did not admit the growth")
	}
}

// A replica whose pods land is released, but only on a snapshot taken after
// they were seen landing: an older snapshot does not count them on the node
// either, and releasing early would show the hole free twice.
func TestALandedReplicaIsReleasedOnANewerSnapshot(t *testing.T) {
	l := NewInFlightLedger()
	t0 := time.Unix(1000, 0)
	placed, ok := 0, true
	l.Admit("ns/m", ReplicaShape{Variant: "ns/m", Accelerator: famHBM3, GPUs: 8}, 2, 0,
		func() (Placed, bool) { return Placed{Whole: placed}, ok }, t0)
	held := func(snapshotAt, now time.Time) int {
		v := newNodeViewAt(h100Nodes(8, 8), snapshotAt)
		l.holdOn(v, now)
		return 2 - v.fits(replicaFit{acc: famHBM3, pods: []int{8}}, 2)
	}
	if got := held(t0, t0.Add(time.Second)); got != 2 {
		t.Fatalf("nothing landed: %d held, want 2", got)
	}
	placed = 1
	if got := held(t0.Add(time.Second), t0.Add(2*time.Second)); got != 2 {
		t.Fatalf("one landed after the snapshot: %d held, want 2", got)
	}
	if got := held(t0.Add(3*time.Second), t0.Add(3*time.Second)); got != 1 {
		t.Fatalf("one landed before the snapshot: %d held, want 1", got)
	}
	placed = 2
	if got := held(t0.Add(4*time.Second), t0.Add(4*time.Second)); got != 0 {
		t.Fatalf("both landed: %d held, want 0", got)
	}
	if l.Holds("ns/m", t0.Add(5*time.Second)) {
		t.Fatal("the entry outlived its replicas landing")
	}
}

// A target at or below what is placed ends the entry.
func TestATargetReachedEndsTheEntry(t *testing.T) {
	l := NewInFlightLedger()
	t0 := time.Unix(1000, 0)
	l.Admit("ns/m", eight, 3, 1, nil, t0)
	l.Admit("ns/m", eight, 1, 1, nil, t0.Add(time.Second))
	if l.Holds("ns/m", t0.Add(2*time.Second)) {
		t.Fatal("a target already placed still holds holes")
	}
}

// NodeViewFor holds what the default ledger holds.
func TestNodeViewForHoldsTheLedger(t *testing.T) {
	DefaultInFlight.Reset()
	t.Cleanup(DefaultInFlight.Reset)
	store := &decision.NodeGPUStore{}
	now := time.Now()
	store.Publish(h100Nodes(8), now)
	DefaultInFlight.Admit("ns/m", eight, 1, 0, nil, now)
	v, _ := NodeViewFor(physicalOf(h100Nodes(8)), store, now)
	if v.fits(replicaFit{acc: famHBM3, pods: []int{8}}, 1) != 0 {
		t.Fatal("the view does not hold the in-flight replica's hole")
	}
	if d := v.DebitsExcept(nil); d["n00"] != 8 {
		t.Fatalf("debits %v, want the hole charged on n00", d)
	}
}
