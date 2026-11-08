package allocation

import (
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// borrowBenchNodes is a large GPU cluster: n nodes of 8 H100s, every other one
// full, each carrying the given number of labels (the flavor's pool label among
// them) and one tolerated taint.
func borrowBenchNodes(n, labels int) map[string]decision.NodeGPU {
	out := make(map[string]decision.NodeGPU, n)
	for i := range n {
		l := map[string]string{"pool": "h100", "zone": fmt.Sprintf("z%d", i%4), "rdma": "true"}
		for j := len(l); j < labels; j++ {
			l[fmt.Sprintf("example.com/label-%02d", j)] = fmt.Sprintf("value-%d-%d", i%7, j)
		}
		out[fmt.Sprintf("node-%05d", i)] = decision.NodeGPU{
			Accelerator: borrowH100, Capacity: 8, Used: 8 * (i % 2), Labels: l,
			Taints: []corev1.Taint{{Key: "dedicated", Value: "gpu", Effect: corev1.TaintEffectNoSchedule}},
		}
	}
	return out
}

// benchTemplates is a pod template that pins RDMA nodes in two zones.
var benchTemplates = []domain.PodPlacement{{
	NodeSelector: map[string]string{"rdma": "true"},
	RequiredNodeAffinity: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
		MatchExpressions: []corev1.NodeSelectorRequirement{
			{Key: "zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"z0", "z1"}},
			{Key: "example.com/label-10", Operator: corev1.NodeSelectorOpExists},
		},
	}}},
}}

// benchTolerations tolerates borrowBenchNodes' taint.
var benchTolerations = []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "gpu", Effect: corev1.TaintEffectNoSchedule}}

// A clone shares the nodes and the eligibility cache but not the debits: what
// one view places is invisible to the other, and a probe debits nothing.
func TestNodeViewCloneSharesNodesNotDebits(t *testing.T) {
	v := NewNodeView(borrowBenchNodes(4, 5)) // node-00000 and node-00002 are free
	c := v.Clone()
	one := replicaFit{acc: "H100", pods: []int{8}, tolerations: benchTolerations}
	if !c.place(one) {
		t.Fatal("the clone placed nothing")
	}
	if got := v.freeOn("node-00000"); got != 8 {
		t.Fatalf("the original sees the clone's debit: node-00000 free %d", got)
	}
	if got := c.freeOn("node-00000"); got != 0 {
		t.Fatalf("the clone lost its debit: node-00000 free %d", got)
	}
	if v.fits(one, 3) != 2 {
		t.Fatal("the original's free nodes are wrong")
	}
	if got := v.freeOn("node-00002"); got != 8 {
		t.Fatalf("a probe debited: node-00002 free %d", got)
	}
	if !c.place(one) || c.fits(one, 1) != 0 {
		t.Fatal("after two commits the clone must have no free node left")
	}
}

// A placement the placer rolls back takes its owner's charge back with its
// debit: the share reads what each owner placed after the pass.
func TestRolledBackPlacementChargesNoOwner(t *testing.T) {
	v := NewNodeView(map[string]decision.NodeGPU{"n1": h100Node(0, nil), "n2": h100Node(0, nil)})
	prev := v.setOwner("ns/m")
	defer v.setOwner(prev)
	r := replicaFit{acc: "H100", pods: []int{8}}
	if !v.place(r) {
		t.Fatal("premise: an empty node holds 8")
	}
	saved := v.saveDebits()
	if !v.place(r) {
		t.Fatal("premise: the second empty node holds 8")
	}
	v.restoreDebits(saved)
	if got := v.DebitsExcept(nil); got["n1"]+got["n2"] != 8 {
		t.Errorf("after a roll-back the owners are charged %v, want 8 GPUs on one node", got)
	}
	if v.fits(r, 2) != 1 {
		t.Error("the rolled-back placement still holds its node")
	}
}

// BenchmarkBorrowPlacementCycle is one optimizer pass's borrowed placement on
// a 2000-node cluster with 80 labels per node: 50 models, each saving the
// state before a tentative unit and placing 4 borrowed 8-GPU replicas on the
// cohort's flavor nodes; every third unit is rolled back.
func BenchmarkBorrowPlacementCycle(b *testing.B) {
	snap := borrowBenchNodes(2000, 80)
	c := borrowQuota(1<<20, nsBorrow{ns: "b", guaranteed: 0, used: 0, cap: -1})
	c.SharedPools[0].NodeLabels = map[string]string{"pool": "h100"}
	constraints := []*ResourceConstraints{c}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		bs := newBorrowState(constraints, NewNodeView(snap))
		for m := range 50 {
			p := bs.newModelPlacer("b", fmt.Sprintf("m%d", m), map[string]int{"H100": 0}, map[string]int{}, nil)
			r := replicaFit{acc: "H100", pods: []int{8}, tolerations: benchTolerations, templates: benchTemplates}
			saved := bs.save(p.guaranteed)
			for range 4 {
				if placed, _ := p.placeReplica("v", r, 8, nil, 1); !placed {
					b.Fatal("a replica did not place")
				}
			}
			if m%3 == 0 {
				bs.restore(saved, p.guaranteed)
			}
		}
	}
}
