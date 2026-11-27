package allocation

import (
	"maps"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// growingH100 is model id at one replica of gpus H100 GPUs, asking for one
// more.
func growingH100(id string, gpus int, passive bool) ModelScalingRequest {
	v := id + "-v"
	return ModelScalingRequest{
		ModelID: id, Namespace: "ns", Priority: 1, Passive: passive,
		CompositeSignal: NamedAnalyzerResult{
			Name: domain.SaturationAnalyzerName, Remaining: 1000, Score: 1,
			Result: &domain.AnalyzerResult{
				TotalDemand:       2000,
				VariantCapacities: []domain.VariantCapacity{{VariantName: v, PerReplicaCapacity: 1000}},
			},
		},
		Variants:      []domain.VariantMetadata{{VariantName: v, AcceleratorName: famHBM3, Cost: 5}},
		VariantStates: []domain.VariantReplicaState{{VariantName: v, CurrentReplicas: 1, GPUsPerReplica: gpus}},
	}
}

// The fitted would-be pass fits a passive model to the nodes the active pass
// left, and consumes none of them: the view the cycle goes on with (the
// utilization share's, the warm pool's) is the active pass's, and a second
// fitted pass sees the same nodes. Without a node view it is not run.
func TestPassiveWouldBeFitted_FitsWithoutConsumingTheView(t *testing.T) {
	physical := &ResourceConstraints{ProviderName: PhysicalLimiterName,
		Pools: map[string]ResourcePool{famHBM3: {Limit: 32, Used: 16}}}
	cons := []*ResourceConstraints{physical}
	active := []ModelScalingRequest{growingH100("a", 8, false)}
	passive := []ModelScalingRequest{growingH100("p", 8, true)}

	o := NewGreedyByScoreOptimizer()
	// 16 free: the active model takes the whole node, the rest is two halves.
	o.Nodes = NewNodeView(h100Nodes(8, 4, 4))
	ds := o.Optimize(t.Context(), active, cons)
	must := func(cond bool, format string, args ...any) {
		t.Helper()
		if !cond {
			t.Fatalf(format, args...)
		}
	}
	must(len(ds) == 1 && ds[0].TargetReplicas == 2, "setup: the active model grows, got %+v", ds)
	left := maps.Clone(o.LastPass().h.nodes.debit)

	wouldBe := PassiveWouldBe(t.Context(), o, passive, cons, ds)
	must(len(wouldBe) == 1 && wouldBe[0].TargetReplicas == 2, "the unfitted would-be pass grows p, got %+v", wouldBe)
	for range 2 {
		fitted := PassiveWouldBeFitted(t.Context(), o, passive, cons, ds)
		must(len(fitted) == 1 && fitted[0].TargetReplicas == 1, "no node holds p's 8-GPU replica, got %+v", fitted)
		must(fitted[0].ShadowLimitedBy == NodeFitLimiterName && !fitted[0].WasLimited,
			"node fit is named only in ShadowLimitedBy, got %+v", fitted[0])
		must(maps.Equal(left, o.LastPass().h.nodes.debit), "the fitted pass consumed the active pass's view")
	}

	// Negative control: no node view, no fitted pass.
	o = NewGreedyByScoreOptimizer()
	ds = o.Optimize(t.Context(), active, cons)
	must(PassiveWouldBeFitted(t.Context(), o, passive, cons, ds) == nil, "a fitted pass ran without a node view")
	must(PassiveWouldBeFitted(t.Context(), NewCostAwareOptimizer(), passive, cons, ds) == nil,
		"a fitted pass ran under the cost-aware optimizer")
}
