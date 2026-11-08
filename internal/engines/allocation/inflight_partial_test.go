package allocation

import (
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// Review 187 minor: a partial replica's placed pods were charged on their
// node whenever the partial list had changed since the snapshot -- all of
// them, the ones the snapshot already counted too. A gang of four 2-GPU pods:
// the leader on a1 is in the snapshot, a worker placed beside it since is
// not. Only that worker is charged on a1, and the two pods still unplaced are
// held: 6 GPUs, not 8.
func TestAPartialReplicaChargesOnlyItsNewlyPlacedPods(t *testing.T) {
	t0 := time.Unix(10_000, 0)
	nodes := racks([]string{"a"})
	a1 := nodes["a1"]
	a1.Used, a1.LWSGroup = 2, true // the leader, in the snapshot
	nodes["a1"] = a1
	shape := ReplicaShape{Variant: "ns/g", Accelerator: famHBM3, PodGPUs: []int{2, 2, 2, 2}, GPUs: 8, DomainKey: "rack"}
	placed := Placed{Partial: []domain.PartialReplica{{Node: "a1", PlacedGPUs: []int{2}}}}
	l := NewInFlightLedger()
	l.Admit("ns/g", shape, 1, 0, func() (Placed, bool) { return placed, true }, t0)
	l.holdOn(newNodeViewAt(nodes, t0), t0) // the leader is seen at t0

	placed = Placed{Partial: []domain.PartialReplica{{Node: "a1", PlacedGPUs: []int{2, 2}}}}
	v := newNodeViewAt(nodes, t0.Add(time.Second)) // shows the leader, not the worker
	l.holdOn(v, t0.Add(2*time.Second))
	total := 0
	for _, g := range v.DebitsExcept(nil) {
		total += g
	}
	if total != 6 {
		t.Fatalf("held %d GPUs (%v), want 6: the new worker's 2 and the two unplaced pods' 4", total, v.DebitsExcept(nil))
	}
}
