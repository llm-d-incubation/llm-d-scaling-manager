package allocation

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// TestRescaleRunsOnlyWhenTheCapsExceedTheBudget: the contention gate reads
// the GPUs the models can take, their caps, not their demand. m's prefill is
// at its maximum of two 8-GPU pods with demand worth 70; decode needs 6 pods.
// b holds 20 GPUs inside its band. Read uncapped, m's 608 GPUs of demand made
// the group of 112 contended, and rescale cut b from 20 to 16 while 28 GPUs
// stayed free. Its caps, 64 and 16, fit, so the additive path decides.
func TestRescaleRunsOnlyWhenTheCapsExceedTheBudget(t *testing.T) {
	m := pdRescaleRequest(nil)
	two := 2
	m.CompositeSignal.Result.RoleDemand = map[string]float64{domain.RolePrefill: 7000, domain.RoleDecode: 600}
	m.CompositeSignal.Result.TotalDemand = 7600
	m.CompositeSignal.RoleCapacities[domain.RolePrefill] = domain.RoleCapacity{
		Role: domain.RolePrefill, TotalDemand: 7000, TotalSupply: 200}
	for i := range m.VariantStates {
		m.VariantStates[i].GPUsPerReplica, m.VariantStates[i].CurrentReplicas = 8, 2
		if m.VariantStates[i].Role == domain.RolePrefill {
			m.VariantStates[i].MaxReplicas = &two
		}
	}
	for i := range m.CompositeSignal.Result.VariantCapacities {
		m.CompositeSignal.Result.VariantCapacities[i].ReplicaCount = 2
	}
	b := aggRescaleRequest("b", 1600)
	b.VariantStates[0].CurrentReplicas = 20
	b.CompositeSignal.Result.VariantCapacities[0].ReplicaCount = 20
	reqs := []ModelScalingRequest{m, b}

	in, sum := rescaleInputsForGroup(reqs, "A100", 112)
	require.Len(t, in, 2)
	assert.Equal(t, in[0].CapGPUs+in[1].CapGPUs, sum, "the gate reads the caps")
	assert.LessOrEqual(t, sum, 112, "setup: what the models can take fits the budget")

	o := NewGreedyByScoreOptimizer()
	o.Rescale = RescaleFlags{Cluster: true}
	available := map[string]int{"A100": 60}
	decisions, handled := o.applyRescale(context.Background(), reqs, nil, available, nil)
	assert.Empty(t, handled, "uncontended: left to the additive path")
	for _, d := range decisions {
		assert.False(t, d.VariantName == "b-a" && d.TargetReplicas < 20, "b is not cut while GPUs are free: %+v", d)
	}
	assert.Equal(t, 60, available["A100"])
}
