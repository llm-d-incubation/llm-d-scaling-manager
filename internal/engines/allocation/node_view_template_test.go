package allocation

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// pinned is borrowModel with the replica's pod templates set.
func pinned(req ModelScalingRequest, templates ...domain.PodPlacement) ModelScalingRequest {
	req.VariantStates[0].Placements = templates
	return req
}

// A 2x8 group pinned by its own nodeSelector to RDMA nodes must not count as
// placed on the flavor's non-RDMA nodes: the scheduler would leave it Pending.
func TestBorrowedReplicaHonoursItsTemplatePlacement(t *testing.T) {
	ctx := context.Background()
	flavor := SharedPoolChunk{Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu", Free: 16,
		FlavorPlacement: kueue.FlavorPlacement{AcceleratorType: borrowH100, NodeLabels: map[string]string{"pool": "a"}}}
	quota := func() *ResourceConstraints {
		c := borrowQuota(16, nsBorrow{ns: "a", cap: config.QuotaUnlimited})
		c.SharedPools = []SharedPoolChunk{flavor}
		return c
	}
	plain := map[string]string{"pool": "a"}
	rdma := map[string]string{"pool": "a", "network": "rdma"}
	optimize := func(nodes map[string]decision.NodeGPU, req ModelScalingRequest) int {
		o := NewGreedyByScoreOptimizer()
		o.Nodes = NewNodeView(nodes)
		return addedGPUs(o.Optimize(ctx, []ModelScalingRequest{req}, []*ResourceConstraints{physicalH100(64), quota()}))["ma"]
	}
	group := func(templates ...domain.PodPlacement) ModelScalingRequest {
		return pinned(borrowModel("a", "ma", 16, 0, 1, []int{8, 8}), templates...)
	}
	freeNonRDMA := map[string]decision.NodeGPU{
		"n1": h100Node(0, plain), "n2": h100Node(0, plain),
		"r1": h100Node(8, rdma), "r2": h100Node(8, rdma),
	}
	freeRDMA := map[string]decision.NodeGPU{
		"n1": h100Node(8, plain), "n2": h100Node(8, plain),
		"r1": h100Node(0, rdma), "r2": h100Node(0, rdma),
	}

	bySelector := domain.PodPlacement{NodeSelector: map[string]string{"network": "rdma"}}
	byAffinity := domain.PodPlacement{RequiredNodeAffinity: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
		MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "network", Operator: corev1.NodeSelectorOpIn, Values: []string{"rdma"}}},
	}}}}

	for name, tmpl := range map[string]domain.PodPlacement{"nodeSelector": bySelector, "required affinity": byAffinity} {
		t.Run(name, func(t *testing.T) {
			if got := optimize(freeNonRDMA, group(tmpl)); got != 0 {
				t.Errorf("ordered %d GPUs onto non-RDMA nodes the template excludes", got)
			}
			if got := optimize(freeRDMA, group(tmpl)); got != 16 {
				t.Errorf("control: ordered %d GPUs with two RDMA nodes free, want 16", got)
			}
		})
	}
	t.Run("control: an unpinned group places on any flavor node", func(t *testing.T) {
		if got := optimize(freeNonRDMA, group()); got != 16 {
			t.Errorf("ordered %d, want 16", got)
		}
	})

	t.Run("a taint the template tolerates admits the node", func(t *testing.T) {
		taint := corev1.Taint{Key: "dedicated", Value: "rdma", Effect: corev1.TaintEffectNoSchedule}
		nodes := map[string]decision.NodeGPU{"r1": h100Node(0, plain, taint), "r2": h100Node(0, plain, taint)}
		if got := optimize(nodes, group()); got != 0 {
			t.Fatalf("premise: neither flavor nor template tolerates the taint, ordered %d", got)
		}
		tolerating := group()
		tolerating.VariantStates[0].Tolerations = []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpEqual,
			Value: "rdma", Effect: corev1.TaintEffectNoSchedule}}
		if got := optimize(nodes, tolerating); got != 16 {
			t.Errorf("the pod carries its template's toleration: ordered %d, want 16", got)
		}
	})
}

func TestTemplateAdmits(t *testing.T) {
	labels := map[string]string{"network": "rdma", "zone": "z1", "gen": "5"}
	req := func(key string, op corev1.NodeSelectorOperator, values ...string) corev1.NodeSelectorRequirement {
		return corev1.NodeSelectorRequirement{Key: key, Operator: op, Values: values}
	}
	affinity := func(terms ...[]corev1.NodeSelectorRequirement) *corev1.NodeSelector {
		ns := &corev1.NodeSelector{}
		for _, exprs := range terms {
			ns.NodeSelectorTerms = append(ns.NodeSelectorTerms, corev1.NodeSelectorTerm{MatchExpressions: exprs})
		}
		return ns
	}
	for _, tc := range []struct {
		name string
		p    domain.PodPlacement
		want bool
	}{
		{"empty", domain.PodPlacement{}, true},
		{"selector match", domain.PodPlacement{NodeSelector: map[string]string{"network": "rdma"}}, true},
		{"selector value differs", domain.PodPlacement{NodeSelector: map[string]string{"network": "tcp"}}, false},
		{"selector key missing", domain.PodPlacement{NodeSelector: map[string]string{"rack": "r1"}}, false},
		{"In", domain.PodPlacement{RequiredNodeAffinity: affinity([]corev1.NodeSelectorRequirement{req("zone", corev1.NodeSelectorOpIn, "z1", "z2")})}, true},
		{"In miss", domain.PodPlacement{RequiredNodeAffinity: affinity([]corev1.NodeSelectorRequirement{req("zone", corev1.NodeSelectorOpIn, "z2")})}, false},
		{"NotIn", domain.PodPlacement{RequiredNodeAffinity: affinity([]corev1.NodeSelectorRequirement{req("zone", corev1.NodeSelectorOpNotIn, "z1")})}, false},
		{"NotIn absent key", domain.PodPlacement{RequiredNodeAffinity: affinity([]corev1.NodeSelectorRequirement{req("rack", corev1.NodeSelectorOpNotIn, "r1")})}, true},
		{"Exists", domain.PodPlacement{RequiredNodeAffinity: affinity([]corev1.NodeSelectorRequirement{req("network", corev1.NodeSelectorOpExists)})}, true},
		{"DoesNotExist", domain.PodPlacement{RequiredNodeAffinity: affinity([]corev1.NodeSelectorRequirement{req("network", corev1.NodeSelectorOpDoesNotExist)})}, false},
		{"Gt", domain.PodPlacement{RequiredNodeAffinity: affinity([]corev1.NodeSelectorRequirement{req("gen", corev1.NodeSelectorOpGt, "4")})}, true},
		{"Lt", domain.PodPlacement{RequiredNodeAffinity: affinity([]corev1.NodeSelectorRequirement{req("gen", corev1.NodeSelectorOpLt, "4")})}, false},
		{"terms are ORed", domain.PodPlacement{RequiredNodeAffinity: affinity(
			[]corev1.NodeSelectorRequirement{req("zone", corev1.NodeSelectorOpIn, "z9")},
			[]corev1.NodeSelectorRequirement{req("network", corev1.NodeSelectorOpExists)})}, true},
		{"expressions are ANDed", domain.PodPlacement{RequiredNodeAffinity: affinity(
			[]corev1.NodeSelectorRequirement{req("network", corev1.NodeSelectorOpExists), req("zone", corev1.NodeSelectorOpIn, "z9")})}, false},
		{"selector and affinity both apply", domain.PodPlacement{NodeSelector: map[string]string{"network": "tcp"},
			RequiredNodeAffinity: affinity([]corev1.NodeSelectorRequirement{req("zone", corev1.NodeSelectorOpIn, "z1")})}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := templateAdmits(&tc.p, labels); got != tc.want {
				t.Errorf("templateAdmits = %t, want %t", got, tc.want)
			}
		})
	}
}

// A flavor placement debits the same view a plain placement reads: a hole a
// borrowed replica takes is gone for a guaranteed one, and the reverse; a nil
// view is "no node information".
func TestPlaceForSharesTheViewsDebits(t *testing.T) {
	pool := map[string]string{"pool": "a"}
	flavor := &kueue.FlavorPlacement{AcceleratorType: borrowH100, NodeLabels: pool}
	v := NewNodeView(map[string]decision.NodeGPU{"n1": h100Node(0, pool), "n2": h100Node(0, nil)})
	clone := v.Clone()
	borrowed := replicaFit{acc: "H100", pods: []int{8}, flavor: flavor}
	plain := replicaFit{acc: "H100", pods: []int{8}}
	if !v.place(borrowed) {
		t.Fatal("premise: the flavor's empty node fits 8")
	}
	if v.fits(borrowed, 1) != 0 {
		t.Error("the flavor's only node was taken, yet a second borrowed replica fits")
	}
	if !v.place(plain) || v.fits(plain, 1) != 0 {
		t.Error("a plain placement must see the borrowed debit: one node left, then none")
	}
	if clone.freeOn("n1") != 8 {
		t.Error("a debit on the view reached its earlier clone")
	}
	var nilView *NodeView
	if nilView.Clone() != nil || !nilView.place(replicaFit{acc: "H100", pods: []int{64}, flavor: flavor}) {
		t.Error("a nil view must copy to nil and fit everything")
	}
}
