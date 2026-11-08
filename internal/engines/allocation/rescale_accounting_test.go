package allocation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// TestRescaleCapsEachRoleAtItsCeiling (#218): a P/D model's water-fill cap is
// each role's demand capped at the role's maxReplicas, summed. Prefill at its
// maximum of two 8-GPU pods with demand worth 70 pods counted 560 GPUs
// toward the cap, though it can reach only 16.
func TestRescaleCapsEachRoleAtItsCeiling(t *testing.T) {
	req := pdRescaleRequest(nil)
	two := 2
	req.CompositeSignal.Result.RoleDemand = map[string]float64{domain.RolePrefill: 7000, domain.RoleDecode: 600}
	req.CompositeSignal.RoleCapacities[domain.RolePrefill] = domain.RoleCapacity{
		Role: domain.RolePrefill, TotalDemand: 7000, TotalSupply: 200}
	for i := range req.VariantStates {
		req.VariantStates[i].GPUsPerReplica, req.VariantStates[i].CurrentReplicas = 8, 2
		if req.VariantStates[i].Role == domain.RolePrefill {
			req.VariantStates[i].MaxReplicas = &two
		}
	}
	in, _ := rescaleInputsForGroup([]ModelScalingRequest{req}, "A100", 1000)
	require.Len(t, in, 1)
	assert.Equal(t, 16+6*8, in[0].CapGPUs, "prefill at its ceiling of 16, decode at its demand of 6 pods")
}

// TestRescaleWeighsTheDemandItCaps (#218): the water-fill weight is the same
// raised demand the cap is sized on. A model whose own demand is 400 while
// another analyzer keeps 800 weighs as one saturation reads at 800; weighed
// on its own 400 it was given 3 of 12 GPUs, not 4.
func TestRescaleWeighsTheDemandItCaps(t *testing.T) {
	other := aggRescaleRequest("o", 1600)
	framed := aggRescaleRequest("m", 400)
	framed.CompositeSignal.OtherFrames = map[string]OtherFrames{
		domain.RoleBoth: {Supply: 2000, Frames: []FrameReading{{Supply: 2000, Demand: 800, Ratio: 1}}},
	}
	alone := aggRescaleRequest("m", 800)

	got, _ := rescaleInputsForGroup([]ModelScalingRequest{framed, other}, "A100", 12)
	want, _ := rescaleInputsForGroup([]ModelScalingRequest{alone, other}, "A100", 12)
	require.Len(t, got, 2)
	assert.Equal(t, want[0].CapGPUs, got[0].CapGPUs, "setup: the cap already reads the frame")
	assert.InDelta(t, 800, got[0].Demand, 1e-9)

	targets, _ := computeRescaleTargets(got, 12)
	assert.Equal(t, 4, targets[modelKey(framed)])
	assert.Equal(t, 8, targets[modelKey(other)])
}
