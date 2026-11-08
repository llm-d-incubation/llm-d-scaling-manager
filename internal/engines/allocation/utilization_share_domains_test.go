package allocation

import (
	"maps"
	"math"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// oneRack is four free 8-GPU nodes in rack r1: room for two 16-GPU groups.
func oneRack() map[string]ShareNode {
	out := map[string]ShareNode{}
	for _, n := range []string{"n1", "n2", "n3", "n4"} {
		out[n] = ShareNode{Free: 8, Labels: map[string]string{"rack": "r1"}}
	}
	return out
}

// The node view places one exclusive-topology group per domain (#187); the
// share's fit did not. Two 16-GPU LeaderWorkerSet groups of two 8-GPU pods,
// one rack with 32 free GPUs: the share planned both into it, and the second
// stayed Pending on the exclusive-topology anti-affinity. Control: without
// the topology key both fit.
func TestTwoGroupsTheShareFillsCannotShareAnExclusiveRack(t *testing.T) {
	gang := ShareReplicaFit{PodGPUs: []int{8, 8}, DomainKey: "rack"}
	nodes := oneRack()
	if !ShareFitPods(nodes, gang) {
		t.Fatal("setup: the first group does not fit the empty rack")
	}
	if ShareFitPods(nodes, gang) {
		t.Fatal("the share placed a second group into an exclusive rack its first group took")
	}
	free := 0
	for _, n := range nodes {
		free += n.Free
	}
	if free != 16 {
		t.Fatalf("%d GPUs free after one group, want 16: the refused one spent something", free)
	}

	plain := oneRack()
	for range 2 {
		if !ShareFitPods(plain, ShareReplicaFit{PodGPUs: []int{8, 8}}) {
			t.Fatal("control: two groups without a topology key do not fit 32 free GPUs")
		}
	}
}

// A rack where a LeaderWorkerSet group's pod already runs holds no new group,
// whatever is free there; nor does one the optimizer's pass or the in-flight
// ledger gave a group this cycle.
func TestTheShareDoesNotFillARackHoldingAGroup(t *testing.T) {
	gang := ShareReplicaFit{PodGPUs: []int{8, 8}, DomainKey: "rack"}
	nodes := oneRack()
	n4 := nodes["n4"]
	n4.Groups, n4.Free = []string{"ns/z-0"}, 0
	nodes["n4"] = n4
	if ShareFitPods(nodes, gang) {
		t.Fatal("the share placed a group into a rack another group runs in")
	}

	claimed := oneRack()
	ClaimShareDomains(claimed, map[string][]string{"rack": {"r1"}})
	if ShareFitPods(claimed, gang) {
		t.Fatal("the share placed a group into a rack this cycle's pass gave a group")
	}
	if !ShareFitPods(claimed, ShareReplicaFit{PodGPUs: []int{8}, DomainKey: "rack"}) {
		t.Fatal("control: a one-pod replica is not bound to a domain, as in the node view")
	}
}

// A claim holds per label key, as NodeView.domains does: a model exclusive on
// "zone" in z1 refuses another zone group there, but not a group exclusive on
// "rack" in an empty rack of z1, which the node view places there.
func TestAShareClaimHoldsOnlyForItsOwnLabelKey(t *testing.T) {
	nodes := map[string]ShareNode{}
	for _, n := range []string{"n1", "n2", "n3", "n4"} {
		nodes[n] = ShareNode{Free: 8, Labels: map[string]string{"zone": "z1", "rack": "r1"}}
	}
	ClaimShareDomains(nodes, map[string][]string{"zone": {"z1"}})
	if !ShareFitPods(nodes, ShareReplicaFit{PodGPUs: []int{8, 8}, DomainKey: "rack"}) {
		t.Fatal("a claim on zone z1 refused a rack group in an empty rack of z1")
	}
	if ShareFitPods(nodes, ShareReplicaFit{PodGPUs: []int{8, 8}, DomainKey: "zone"}) {
		t.Fatal("control: a second zone group was placed in the claimed zone")
	}
}

// The pass's view reports the domains it gave a group, net of the models the
// share plans.
func TestTheNodeViewReportsTheDomainsItGaveAGroup(t *testing.T) {
	v := NewNodeView(racks([]string{"a", "b"}))
	v.setOwner("ns/gang")
	if !v.PlaceReplicas([]ReplicaShape{{Variant: "ns/m-v", Accelerator: famHBM3, PodGPUs: []int{8, 8}, GPUs: 16, DomainKey: "rack"}}, true) {
		t.Fatal("setup: the group does not fit")
	}
	if got := v.TakenDomains(nil); len(got["rack"]) != 1 || got["rack"][0] != "a" {
		t.Fatalf("taken domains %v, want rack a", got)
	}
	if got := v.TakenDomains(func(owner string) bool { return owner == "ns/gang" }); len(got) != 0 {
		t.Fatalf("taken domains %v for an excluded owner", got)
	}
}

// The share's fit judges a group's leader by the leader template and its
// worker by the workers', as the node view does. The leader is pinned to
// pool a and the worker to pool b; n0, first by name, is in neither.
func TestTheShareJudgesALeaderAndItsWorkersByTheirOwnTemplates(t *testing.T) {
	nodes := map[string]ShareNode{
		"n0": {Free: 8},
		"n1": {Free: 8, Labels: map[string]string{"pool": "a"}},
		"n2": {Free: 8, Labels: map[string]string{"pool": "b"}},
	}
	fit := ShareReplicaFit{PodGPUs: []int{8, 8}, Placements: []domain.PodPlacement{
		{NodeSelector: map[string]string{"pool": "a"}}, {NodeSelector: map[string]string{"pool": "b"}}}}
	search := shareNodeSearch{in: SharePlanInput{Nodes: maps.Clone(nodes), Placements: map[string][]domain.PodPlacement{"B": fit.Placements}},
		rc: "B", taken: map[string]int{}, used: map[shareUnitRef]bool{}}
	hole, ok := search.place(fit.PodGPUs, shareNodesInDomain(nodes, "", ""))
	if !ok || hole["n0"] != 8 || hole["n1"] != 0 || hole["n2"] != 0 {
		t.Fatalf("the set search left holes %v (placed %v), want the leader on n1 and the worker on n2", hole, ok)
	}
	if !ShareFitPods(nodes, fit) || nodes["n0"].Free != 8 || nodes["n1"].Free != 0 || nodes["n2"].Free != 0 {
		t.Fatalf("the fill left %v, want the leader on n1 and the worker on n2", nodes)
	}
}

// A donor set may fund a group into a rack whose only group is a donor group
// the set takes: once it goes, the rack holds no other group. A rack where a
// group's pod stays is refused, as the node view refuses it.
func TestADonorSetFreesARackOnlyWithItsGroup(t *testing.T) {
	plan := func(donorGroup string, n1Groups, n2Groups []string) SharePlan {
		nodes := map[string]ShareNode{
			"n1": {Labels: map[string]string{"rack": "r1"}, Groups: n1Groups},
			"n2": {Labels: map[string]string{"rack": "r1"}, Groups: n2Groups},
		}
		// Four 4-GPU donor pods: none holds a receiver pod pod-for-pod, so
		// only the node-aware set search funds it.
		units := []ShareUnit{{Pods: []SharePod{
			{Name: "ns/a-0-0", Node: "n1", GPUs: 4}, {Name: "ns/a-0-1", Node: "n1", GPUs: 4},
			{Name: "ns/a-0-2", Node: "n2", GPUs: 4}, {Name: "ns/a-0-3", Node: "n2", GPUs: 4}}, Group: donorGroup}}
		in := SharePlanInput{
			Roles: []ShareRole{
				{Key: "A", Weight: 1, Need: 4, Ceiling: 64, ReplicaGPUs: 16},
				{Key: "B", Weight: 1, Need: 64, Ceiling: 64, ReplicaGPUs: 16},
			},
			Held:       map[string]int{"A": 32, "B": 16},
			Thresholds: map[string]float64{"A": 0.8, "B": 0.8}, Budget: 48, Tolerance: 0.15,
			Give:  map[string]ShareVariant{"A": {Name: "a", GPUs: 16, PodGPUs: []int{4, 4, 4, 4}}},
			Grow:  map[string]ShareVariant{"B": {Name: "b", GPUs: 16, PodGPUs: []int{8, 8}}},
			Nodes: nodes, DonorUnits: map[string][]ShareUnit{"A": units},
			DomainKey: map[string]string{"B": "rack"}, PhysicalFree: math.MaxInt,
		}
		l := NewShareLedger()
		var p SharePlan
		for i := range 3 {
			if p = PlanShareTransfers(l, in, time.Unix(int64(30*i), 0), simTimings()); len(p.Started) > 0 {
				break
			}
		}
		return p
	}
	a, z := []string{"ns/a-0"}, []string{"ns/z-0"}
	if p := plan("ns/a-0", a, a); len(p.Started) != 1 {
		t.Fatalf("a donor group leaving its rack did not fund the receiver's group there: %+v", p.Started)
	}
	if p := plan("", z, z); len(p.Started) != 0 {
		t.Fatalf("the share funded a group into a rack where another group's pod stays: %+v", p.Started)
	}
	// TP4 workers packed two per node: n1 holds a pod of the donor group and
	// one of model Z's group. The donor leaving does not free n1.
	if p := plan("ns/a-0", []string{"ns/a-0", "ns/z-0"}, a); len(p.Started) != 0 {
		t.Fatalf("the share funded a group into a rack where another group shares a node with the donor: %+v", p.Started)
	}
}
