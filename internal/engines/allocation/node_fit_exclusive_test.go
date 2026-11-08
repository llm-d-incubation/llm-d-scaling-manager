package allocation

import (
	"fmt"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// Whole-node review WN1 and WN2: an exclusive-topology LeaderWorkerSet
// (leaderworkerset.sigs.k8s.io/exclusive-topology) lands each group in one
// domain of its topology label, and a domain holds one group: its pods'
// anti-affinity refuses every other group's pods there. Racks of four 8-GPU
// nodes, gangs of two 8-GPU pods (16 GPUs).

// racks is 8-GPU HBM3 nodes, four per rack, labelled rack=<name>; used names
// the nodes a LeaderWorkerSet group's pod fills.
func racks(names []string, used ...string) map[string]decision.NodeGPU {
	out := map[string]decision.NodeGPU{}
	for _, r := range names {
		for i := 1; i <= 4; i++ {
			out[fmt.Sprintf("%s%d", r, i)] = decision.NodeGPU{Accelerator: famHBM3, Capacity: 8,
				Labels: map[string]string{"rack": r}}
		}
	}
	for _, n := range used {
		node := out[n]
		node.Used, node.LWSGroup = 8, true
		out[n] = node
	}
	return out
}

// gang is one 16-GPU exclusive-topology replica of two 8-GPU pods.
func gang(variant string) ReplicaShape {
	return ReplicaShape{Variant: variant, Accelerator: famHBM3, PodGPUs: []int{8, 8}, GPUs: 16, DomainKey: "rack"}
}

// WN1: gang 1 holds two nodes of rack a. Two nodes of rack a are free, and
// gang 2 fitted there by GPUs -- Pending for good, its anti-affinity
// refusing gang 1's rack. It goes to rack b, and a third gang finds no rack.
func TestAnExclusiveGangSkipsARackAnotherGroupHolds(t *testing.T) {
	v := NewNodeView(racks([]string{"a", "b"}, "a1", "a2"))
	if !v.PlaceReplicas([]ReplicaShape{gang("ns/g2")}, true) {
		t.Fatal("gang 2 found no rack: rack b is free")
	}
	debits := v.DebitsExcept(nil)
	if debits["a3"]+debits["a4"] != 0 || debits["b1"]+debits["b2"]+debits["b3"]+debits["b4"] != 16 {
		t.Fatalf("gang 2 placed %v, want 16 GPUs in rack b and none in rack a", debits)
	}
	if v.PlaceReplicas([]ReplicaShape{gang("ns/g3")}, false) {
		t.Fatal("a third gang fitted: rack a holds gang 1 and rack b gang 2")
	}
}

// WN1: two gangs ordered in one pass never share a rack, though one rack's
// four free nodes hold both by GPUs.
func TestTwoGangsInOnePassTakeTwoRacks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		racks []string
		want  bool
	}{
		{"one free rack: one gang", []string{"a"}, false},
		{"two free racks: both gangs", []string{"a", "b"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := NewNodeView(racks(tc.racks))
			if got := v.PlaceReplicas([]ReplicaShape{gang("ns/g1"), gang("ns/g2")}, false); got != tc.want {
				t.Fatalf("two gangs placed: %v, want %v", got, tc.want)
			}
			if got := v.fits(v.fitOf(gang("ns/g")), 4); got != len(tc.racks) {
				t.Fatalf("fits %d gangs, want one per rack (%d)", got, len(tc.racks))
			}
		})
	}
}

// WN1: a gang the in-flight ledger holds takes its rack from every later
// decision, as a placed one does.
func TestAHeldGangTakesItsRack(t *testing.T) {
	l := NewInFlightLedger()
	now := time.Now()
	l.Admit("ns/g1", gang("ns/g1"), 1, 0, nil, now)
	v := newNodeViewAt(racks([]string{"a", "b"}), now)
	l.holdOn(v, now)
	if !v.PlaceReplicas([]ReplicaShape{gang("ns/g2")}, true) {
		t.Fatal("gang 2 found no rack: one of two is free")
	}
	if v.PlaceReplicas([]ReplicaShape{gang("ns/g3")}, false) {
		t.Fatal("gang 3 shared a rack with the held gang 1 or with gang 2")
	}
}

// WN2: a gang's leader holds a1; its worker is not created yet
// (LeaderReady). PlacedReplicas counts the group unplaced, and the ledger
// held the whole 16-GPU gang anew -- rack a, with a1 full, no longer fits
// it, so it held two nodes of rack b, and a2, the node the worker needs, was
// free to anyone. The ledger holds the worker alone, in the leader's rack.
func TestAHalfPlacedGangHoldsItsWorkerInTheLeadersRack(t *testing.T) {
	t0 := time.Unix(10_000, 0)
	nodes := racks([]string{"a", "b"}, "a1")
	for _, n := range []string{"a3", "a4"} {
		node := nodes[n]
		node.Used = 8 // other tenants' pods, no group
		nodes[n] = node
	}
	placed := Placed{Partial: []domain.PartialReplica{{Node: "a1", PlacedGPUs: []int{8}}}}
	l := NewInFlightLedger()
	l.Admit("ns/g", gang("ns/g"), 1, 0, func() (Placed, bool) { return placed, true }, t0)
	l.holdOn(newNodeViewAt(nodes, t0), t0) // the leader's placement is seen

	v := newNodeViewAt(nodes, t0.Add(time.Second))
	l.holdOn(v, t0.Add(2*time.Second))
	debits := v.DebitsExcept(nil)
	if debits["a2"] != 8 {
		t.Fatalf("the worker's node a2 holds %d, want 8: the half-placed gang held %v", debits["a2"], debits)
	}
	if n := debits["b1"] + debits["b2"] + debits["b3"] + debits["b4"]; n != 0 {
		t.Fatalf("rack b holds %d for a gang whose leader is in rack a", n)
	}
}

// WN2: a count newer than the snapshot -- the snapshot does not show the
// leader on a1 yet -- holds the leader's GPUs on its node too.
func TestAHalfPlacedGangNewerThanTheSnapshotHoldsItsLeader(t *testing.T) {
	t0 := time.Unix(10_000, 0)
	nodes := racks([]string{"a"})
	placed := Placed{Partial: []domain.PartialReplica{{Node: "a1", PlacedGPUs: []int{8}}}}
	l := NewInFlightLedger()
	l.Admit("ns/g", gang("ns/g"), 1, 0, func() (Placed, bool) { return placed, true }, t0)
	v := newNodeViewAt(nodes, t0.Add(-time.Second))
	l.holdOn(v, t0)
	debits := v.DebitsExcept(nil)
	if debits["a1"] != 8 || debits["a1"]+debits["a2"]+debits["a3"]+debits["a4"] != 16 {
		t.Fatalf("held %v, want the leader's 8 on a1 and the worker's 8 beside it", debits)
	}
}
