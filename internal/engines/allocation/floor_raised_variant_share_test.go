package allocation

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// Issue ev-shindin/llm-scaling-manager#495: the throughput floor raises decode to 100 tokens on a supply of
// 100 -- d4 80 of it, d2 20 -- while d2 holds 50 on its own, above its supply
// share of 20. How the analyzer spreads the floor over the variants decides
// what the optimizer prices each at: it takes the role's demand at each
// variant's fraction of the variants' sum (variantDemandShare).
//
//   - The water-fill (saturation.raiseVariantsToRoles): d2 keeps its 50, d4
//     takes the 50 left, the variants sum to the role, and d2 is priced at
//     its own occupancy.
//   - The larger of own figure and share, as before: d4 80, d2 50, summing to
//     130 against the role's 100, so d2 was priced at 100 x 50/130 = 38.5 --
//     23 % under what it holds.
func TestCostAware_AFlooredRoleIsPricedAtTheVariantsShares(t *testing.T) {
	decide := func(d4, d2 float64) map[string]domain.VariantDecision {
		result := &domain.AnalyzerResult{
			ModelID: "m", Namespace: "n", TotalDemand: 110,
			RoleDemand: map[string]float64{domain.RoleDecode: 100, domain.RolePrefill: 10},
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "d4", Role: domain.RoleDecode, ReplicaCount: 4,
					PerReplicaCapacity: 20, TotalDemand: d4, Utilization: d4 / 80},
				{VariantName: "d2", Role: domain.RoleDecode, ReplicaCount: 2,
					PerReplicaCapacity: 10, TotalDemand: d2, Utilization: d2 / 20},
				{VariantName: "p", Role: domain.RolePrefill, ReplicaCount: 1,
					PerReplicaCapacity: 20, TotalDemand: 10, Utilization: 0.5},
			},
		}
		req := ModelScalingRequest{
			ModelID: "m", Namespace: "n", Disaggregated: true,
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "d4", Role: domain.RoleDecode, CurrentReplicas: 4},
				{VariantName: "d2", Role: domain.RoleDecode, CurrentReplicas: 2},
				{VariantName: "p", Role: domain.RolePrefill, CurrentReplicas: 1},
			},
			Variants: []domain.VariantMetadata{
				{VariantName: "d4", ModelID: "m", Namespace: "n", Role: domain.RoleDecode, Cost: 1},
				{VariantName: "d2", ModelID: "m", Namespace: "n", Role: domain.RoleDecode, Cost: 1},
				{VariantName: "p", ModelID: "m", Namespace: "n", Role: domain.RolePrefill, Cost: 1},
			},
			CompositeSignal: NamedAnalyzerResult{
				Name: "saturation", Result: result,
				RoleCapacities: map[string]domain.RoleCapacity{
					domain.RoleDecode:  {Role: domain.RoleDecode, TotalSupply: 100, TotalAnticipatedSupply: 100, TotalDemand: 100},
					domain.RolePrefill: {Role: domain.RolePrefill, TotalSupply: 20, TotalAnticipatedSupply: 20, TotalDemand: 10},
				},
			},
		}
		out := map[string]domain.VariantDecision{}
		for _, d := range NewCostAwareOptimizer().Optimize(context.Background(), []ModelScalingRequest{req}, nil) {
			out[d.VariantName] = d
		}
		require.Contains(t, out, "d4")
		require.Contains(t, out, "d2")
		return out
	}

	waterFill := decide(50, 50)
	assert.InDelta(t, 50, waterFill["d2"].TotalDemand, 1e-9, "water-filled: the hot variant is priced at what it holds")
	assert.InDelta(t, 50, waterFill["d4"].TotalDemand, 1e-9)
	assert.InDelta(t, 100, waterFill["d4"].TotalDemand+waterFill["d2"].TotalDemand, 1e-9, "the decisions sum to the role")

	maxShare := decide(80, 50)
	assert.InDelta(t, 100*50.0/130, maxShare["d2"].TotalDemand, 1e-9,
		"the larger of own and share priced the hot variant under what it holds")
	assert.Less(t, maxShare["d2"].TotalDemand, 50.0)
}
