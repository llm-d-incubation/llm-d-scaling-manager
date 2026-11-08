package allocation

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// aggRescaleRequest is an aggregated model with variant "a" on A100, two
// one-GPU replicas at 100 each, and demand.
func aggRescaleRequest(id string, demand float64) ModelScalingRequest {
	return ModelScalingRequest{
		ModelID:   id,
		Namespace: "ns",
		Priority:  1,
		CompositeSignal: NamedAnalyzerResult{
			Name: CompositeSignalName,
			Live: true,
			Result: &domain.AnalyzerResult{
				ModelID: id, Namespace: "ns", TotalDemand: demand,
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: id + "-a", ReplicaCount: 2, PerReplicaCapacity: 100},
				},
			},
		},
		VariantStates: []domain.VariantReplicaState{
			{VariantName: id + "-a", CurrentReplicas: 2, GPUsPerReplica: 1},
		},
		Variants: []domain.VariantMetadata{
			{VariantName: id + "-a", AcceleratorName: "A100", Cost: 2},
		},
	}
}

// withUnnamedVariant adds variant "x" to req: no accelerator name, cheaper
// than "a", two one-GPU replicas at 100 each.
func withUnnamedVariant(req ModelScalingRequest) ModelScalingRequest {
	r := req.CompositeSignal.Result
	r.VariantCapacities = append(r.VariantCapacities,
		domain.VariantCapacity{VariantName: "x", ReplicaCount: 2, PerReplicaCapacity: 100})
	req.VariantStates = append(req.VariantStates, domain.VariantReplicaState{VariantName: "x", CurrentReplicas: 2, GPUsPerReplica: 1})
	req.Variants = append(req.Variants, domain.VariantMetadata{VariantName: "x", Cost: 1})
	return req
}

// TestRescaleLeavesAModelWithAnUnnamedVariantToTheAdditivePath (ev-shindin/llm-scaling-manager#223): a
// variant with no accelerator name holds GPUs no budget counts, so rescale
// does not take its model; the additive path may shrink it and has no budget
// to grow it. Taken, the cheaper unnamed variant was filled first with GPUs
// no budget counted; frozen instead, it never shrank, and the named variant
// was sized for demand it already served.
func TestRescaleLeavesAModelWithAnUnnamedVariantToTheAdditivePath(t *testing.T) {
	run := func(m ModelScalingRequest) (map[string]bool, []domain.VariantDecision) {
		t.Helper()
		o := NewGreedyByScoreOptimizer()
		o.Rescale = RescaleFlags{Cluster: true}
		reqs := []ModelScalingRequest{m, aggRescaleRequest("o", 1000)}
		decisions, handled := o.applyRescale(context.Background(), reqs, nil, map[string]int{"A100": 2}, nil)
		return handled, decisions
	}
	named := aggRescaleRequest("m", 1000)
	handled, _ := run(named)
	require.True(t, handled[modelKey(named)], "setup: the group is contended and rescale takes m")

	m := withUnnamedVariant(aggRescaleRequest("m", 1000))
	handled, decisions := run(m)
	assert.False(t, handled[modelKey(m)], "m goes to the additive path")
	for _, d := range decisions {
		assert.NotEqual(t, "x", d.VariantName)
		assert.NotEqual(t, "m-a", d.VariantName)
	}
}
