package allocation

import (
	"context"
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// pdRescaleRequest is a P/D model with four one-GPU replicas per role on
// A100, prefill demand worth one replica and decode demand worth six, and the
// composite holds holds.
func pdRescaleRequest(holds map[string]RoleHold) ModelScalingRequest {
	return ModelScalingRequest{
		ModelID:   "m",
		Namespace: "ns",
		Priority:  1,
		CompositeSignal: NamedAnalyzerResult{
			Name: CompositeSignalName,
			Live: true,
			Result: &domain.AnalyzerResult{
				ModelID:    "m",
				Namespace:  "ns",
				RoleDemand: map[string]float64{domain.RolePrefill: 100, domain.RoleDecode: 600},
				VariantCapacities: []domain.VariantCapacity{
					{VariantName: "p", Role: domain.RolePrefill, ReplicaCount: 4, PerReplicaCapacity: 100},
					{VariantName: "d", Role: domain.RoleDecode, ReplicaCount: 4, PerReplicaCapacity: 100},
				},
			},
			RoleCapacities: map[string]domain.RoleCapacity{
				domain.RolePrefill: {Role: domain.RolePrefill, TotalDemand: 100, TotalSupply: 400},
				domain.RoleDecode:  {Role: domain.RoleDecode, TotalDemand: 600, TotalSupply: 400},
			},
			Holds: holds,
		},
		VariantStates: []domain.VariantReplicaState{
			{VariantName: "p", Role: domain.RolePrefill, CurrentReplicas: 4, GPUsPerReplica: 1},
			{VariantName: "d", Role: domain.RoleDecode, CurrentReplicas: 4, GPUsPerReplica: 1},
		},
		Variants: []domain.VariantMetadata{
			{VariantName: "p", Role: domain.RolePrefill, AcceleratorName: "A100", Cost: 1},
			{VariantName: "d", Role: domain.RoleDecode, AcceleratorName: "A100", Cost: 1},
		},
		Disaggregated: true,
	}
}

func rescaleTargets(req ModelScalingRequest, targetGPUs, free int) map[string]int {
	out := map[string]int{}
	for _, d := range NewGreedyByScoreOptimizer().rescaleModelDecisions(context.Background(), req, nil, "A100", targetGPUs, &free) {
		out[d.VariantName] = d.TargetReplicas
	}
	return out
}

// TestRescaleKeepsAHeldRole: rescale splits a model's GPUs by role demand and
// reclaims or fills each role toward its share. A role the composite holds is
// neither reclaimed nor filled: its demand is a figure the policy does not size
// on, and with it rescale shed prefill to fund decode.
func TestRescaleKeepsAHeldRole(t *testing.T) {
	free := rescaleTargets(pdRescaleRequest(nil), 8, 0)
	assert.Less(t, free["p"], 4, "unheld, prefill is reclaimed toward its share")

	both := map[string]RoleHold{domain.RolePrefill: {NoScaleUp: true, NoScaleDown: true}}
	assert.Equal(t, 4, rescaleTargets(pdRescaleRequest(both), 8, 0)["p"])

	down := map[string]RoleHold{domain.RolePrefill: {NoScaleDown: true}}
	assert.Equal(t, 4, rescaleTargets(pdRescaleRequest(down), 8, 0)["p"])

	// The held role keeps its GPUs in the split too: decode gets what is left
	// of the target. Split as if prefill would shrink to its share, decode
	// was given 7 of 8 and, with 3 GPUs free, the model grew to 11.
	got := rescaleTargets(pdRescaleRequest(down), 8, 3)
	assert.Equal(t, 8, got["p"]+got["d"], "the model stays at its target: %v", got)
}

// TestRescaleFreezesAModelWithAPinnedRole: a role held both ways freezes the
// model's rescale, as the share optimizer freezes it. Split around it, decode
// was filled toward 7 of a target of 8 while prefill kept its 4, and a target
// below prefill's 4 cut decode to its floor.
func TestRescaleFreezesAModelWithAPinnedRole(t *testing.T) {
	both := map[string]RoleHold{domain.RolePrefill: {NoScaleUp: true, NoScaleDown: true}}
	for _, tc := range []struct{ target, free int }{{8, 8}, {3, 0}, {16, 8}} {
		got := rescaleTargets(pdRescaleRequest(both), tc.target, tc.free)
		assert.Equal(t, 4, got["p"], "target %d", tc.target)
		assert.Equal(t, 4, got["d"], "target %d", tc.target)
	}
}

// TestRescaleInputHoldsAPinnedModelAtWhatItHas: a model rescale freezes is
// given exactly its GPUs in the water-fill, so the rest of the budget goes to
// the others. Weighed on its demand it was set a share it never took, and the
// GPUs sat idle while another model stayed short.
func TestRescaleInputHoldsAPinnedModelAtWhatItHas(t *testing.T) {
	free, _ := rescaleInputsForGroup([]ModelScalingRequest{pdRescaleRequest(nil)}, "A100", 16)
	assert.Equal(t, 7, free[0].CapGPUs, "unheld: prefill 1 + decode 6 from demand")

	both := map[string]RoleHold{domain.RolePrefill: {NoScaleUp: true, NoScaleDown: true}}
	in, sum := rescaleInputsForGroup([]ModelScalingRequest{pdRescaleRequest(both)}, "A100", 16)
	assert.Equal(t, 8, in[0].FloorGPUs)
	assert.Equal(t, 8, in[0].CapGPUs)
	assert.Equal(t, 8, sum, "it contends for what it holds")
}

// TestRescaleWeighsAOneWayHeldModelOnDemand: a role held one way does not pin
// the model. Its water-fill input still follows demand, and its other role
// still moves.
func TestRescaleWeighsAOneWayHeldModelOnDemand(t *testing.T) {
	up := map[string]RoleHold{domain.RolePrefill: {NoScaleUp: true}}
	in, _ := rescaleInputsForGroup([]ModelScalingRequest{pdRescaleRequest(up)}, "A100", 16)
	assert.Equal(t, 7, in[0].CapGPUs, "prefill 1 + decode 6 from demand, not the 8 it holds")
	assert.Greater(t, rescaleTargets(pdRescaleRequest(up), 16, 8)["d"], 4, "decode still fills")
}

// TestRoundFillsToReplicas: on 8-GPU pods a model given 8 more GPUs, split
// +1 and +7 between its roles, adds one decode pod; split as GPUs, neither
// role reached a pod and the 8 GPUs stayed free every cycle.
func TestRoundFillsToReplicas(t *testing.T) {
	req := pdRescaleRequest(nil)
	for i := range req.VariantStates {
		req.VariantStates[i].GPUsPerReplica = 8
	}
	records := buildVariantRecords(req, req.CompositeSignal.Result)
	stateMap := buildStateMap(req.VariantStates)
	roles := []string{domain.RoleDecode, domain.RolePrefill}
	cur := map[string]int{domain.RolePrefill: 16, domain.RoleDecode: 16}

	tgt := map[string]int{domain.RolePrefill: 17, domain.RoleDecode: 23}
	roundFillsToReplicas(records, stateMap, roles, tgt, cur, maps.Clone(tgt), initTargets(req.VariantStates))
	assert.Equal(t, map[string]int{domain.RolePrefill: 16, domain.RoleDecode: 24}, tgt)

	// A cut too small for any pod stays unspent rather than overshooting.
	tgt = map[string]int{domain.RolePrefill: 19, domain.RoleDecode: 19}
	roundFillsToReplicas(records, stateMap, roles, tgt, cur, maps.Clone(tgt), initTargets(req.VariantStates))
	assert.Equal(t, map[string]int{domain.RolePrefill: 16, domain.RoleDecode: 16}, tgt)

	// One-GPU replicas are left as split.
	one := pdRescaleRequest(nil)
	tgt = map[string]int{domain.RolePrefill: 5, domain.RoleDecode: 7}
	roundFillsToReplicas(buildVariantRecords(one, one.CompositeSignal.Result), buildStateMap(one.VariantStates),
		roles, tgt, map[string]int{domain.RolePrefill: 4, domain.RoleDecode: 4}, maps.Clone(tgt), initTargets(one.VariantStates))
	assert.Equal(t, map[string]int{domain.RolePrefill: 5, domain.RoleDecode: 7}, tgt)
}

// TestRoundFillsToReplicasUsesTheVariantThatFills: the share is rounded to
// what fillRole places, not to the cheapest variant's pod. With the 8-GPU
// variant at its maximum and a 4-GPU one free, +4 is one 4-GPU replica;
// rounded on the 8-GPU pod it was nothing.
func TestRoundFillsToReplicasUsesTheVariantThatFills(t *testing.T) {
	one := 1
	req := ModelScalingRequest{
		ModelID: "m", Namespace: "ns",
		CompositeSignal: NamedAnalyzerResult{Result: &domain.AnalyzerResult{VariantCapacities: []domain.VariantCapacity{
			{VariantName: "d8", Role: domain.RoleDecode, ReplicaCount: 1, PerReplicaCapacity: 800},
			{VariantName: "d4", Role: domain.RoleDecode, ReplicaCount: 1, PerReplicaCapacity: 300},
		}}},
		VariantStates: []domain.VariantReplicaState{
			{VariantName: "d8", Role: domain.RoleDecode, CurrentReplicas: 1, GPUsPerReplica: 8, MaxReplicas: &one},
			{VariantName: "d4", Role: domain.RoleDecode, CurrentReplicas: 1, GPUsPerReplica: 4},
		},
		Variants: []domain.VariantMetadata{
			{VariantName: "d8", Role: domain.RoleDecode, AcceleratorName: "A100", Cost: 1},
			{VariantName: "d4", Role: domain.RoleDecode, AcceleratorName: "A100", Cost: 1},
		},
	}
	records := buildVariantRecords(req, req.CompositeSignal.Result)
	tgt := map[string]int{domain.RoleDecode: 16}
	roundFillsToReplicas(records, buildStateMap(req.VariantStates), []string{domain.RoleDecode},
		tgt, map[string]int{domain.RoleDecode: 12}, maps.Clone(tgt), initTargets(req.VariantStates))
	assert.Equal(t, 16, tgt[domain.RoleDecode])
}

// TestRescaleGivesNoRolePodsBeyondItsDemand: what a role at its maxReplicas
// cannot place is not handed to the other role as whole pods. Prefill is at
// its max of one 8-GPU pod with demand for three; decode has one pod and
// demand for two. At a model target of 32 the leftover was 16, and decode
// went to three pods, more than at a target of 40.
func TestRescaleGivesNoRolePodsBeyondItsDemand(t *testing.T) {
	one := 1
	req := pdRescaleRequest(nil)
	req.CompositeSignal.Result.RoleDemand = map[string]float64{domain.RolePrefill: 300, domain.RoleDecode: 150}
	req.CompositeSignal.RoleCapacities = map[string]domain.RoleCapacity{
		domain.RolePrefill: {Role: domain.RolePrefill, TotalDemand: 300, TotalSupply: 100},
		domain.RoleDecode:  {Role: domain.RoleDecode, TotalDemand: 150, TotalSupply: 100},
	}
	for i := range req.CompositeSignal.Result.VariantCapacities {
		req.CompositeSignal.Result.VariantCapacities[i].ReplicaCount = 1
	}
	for i := range req.VariantStates {
		req.VariantStates[i].CurrentReplicas = 1
		req.VariantStates[i].GPUsPerReplica = 8
	}
	req.VariantStates[0].MaxReplicas = &one
	for _, target := range []int{32, 36, 40} {
		got := rescaleTargets(req, target, 64)
		assert.Equal(t, 1, got["p"], "target %d: prefill is at its max", target)
		assert.Equal(t, 2, got["d"], "target %d: decode goes to its demand of two pods, no further", target)
	}
}

// TestRescaleGivesTheLeftoverToARoleWithNoCut: a role whose share landed on
// whole pods still takes what a role at its maxReplicas could not place.
// Prefill is at its max of one 8-GPU pod with demand for three; decode has
// one pod and demand for five. At a model target of 51 decode's share was 32,
// four pods exactly, so it took nothing of prefill's 11 and stayed a pod short.
func TestRescaleGivesTheLeftoverToARoleWithNoCut(t *testing.T) {
	one := 1
	req := pdRescaleRequest(nil)
	req.CompositeSignal.Result.RoleDemand = map[string]float64{domain.RolePrefill: 300, domain.RoleDecode: 500}
	req.CompositeSignal.RoleCapacities = map[string]domain.RoleCapacity{
		domain.RolePrefill: {Role: domain.RolePrefill, TotalDemand: 300, TotalSupply: 100},
		domain.RoleDecode:  {Role: domain.RoleDecode, TotalDemand: 500, TotalSupply: 100},
	}
	for i := range req.CompositeSignal.Result.VariantCapacities {
		req.CompositeSignal.Result.VariantCapacities[i].ReplicaCount = 1
	}
	for i := range req.VariantStates {
		req.VariantStates[i].CurrentReplicas = 1
		req.VariantStates[i].GPUsPerReplica = 8
	}
	req.VariantStates[0].MaxReplicas = &one
	for _, target := range []int{48, 50, 51, 52} {
		got := rescaleTargets(req, target, 64)
		assert.Equal(t, 1, got["p"], "target %d: prefill is at its max", target)
		assert.Equal(t, 5, got["d"], "target %d: decode reaches its demand of five pods", target)
	}
}

// TestRescaleSizesOnAnotherFramesDemand: another analyzer that sizes a role
// raises its demand, at the full utilization rescale sizes to, instead of
// flooring it. Prefill holds four GPUs at 100 each with demand 100; a frame
// reading demand 300 of the same supply keeps three of them, so prefill's
// demand is three GPUs and the model's water-fill cap grows by two. A
// frame that agrees changes nothing.
func TestRescaleSizesOnAnotherFramesDemand(t *testing.T) {
	frame := func(demand float64) map[string]OtherFrames {
		return map[string]OtherFrames{domain.RolePrefill: {Supply: 400, Frames: []FrameReading{{Supply: 400, Demand: demand, Ratio: 1}}}}
	}
	req := pdRescaleRequest(nil)
	in, _ := rescaleInputsForGroup([]ModelScalingRequest{req}, "A100", 16)
	alone := in[0]

	req.CompositeSignal.OtherFrames = frame(100)
	in, _ = rescaleInputsForGroup([]ModelScalingRequest{req}, "A100", 16)
	assert.Equal(t, alone, in[0], "an agreeing frame changes nothing")

	req.CompositeSignal.OtherFrames = frame(300)
	assert.Equal(t, 3, roleDemandGPUs(req.CompositeSignal, buildVariantRecords(req, req.CompositeSignal.Result),
		buildStateMap(req.VariantStates), "A100", domain.RolePrefill))
	in, _ = rescaleInputsForGroup([]ModelScalingRequest{req}, "A100", 16)
	assert.Equal(t, alone.CapGPUs+2, in[0].CapGPUs)
	assert.Equal(t, alone.FloorGPUs, in[0].FloorGPUs, "as demand, not as a floor")

	held := pdRescaleRequest(map[string]RoleHold{domain.RolePrefill: {NoScaleDown: true}})
	in, _ = rescaleInputsForGroup([]ModelScalingRequest{held}, "A100", 16)
	assert.Equal(t, 4, in[0].HeldGPUs, "a role that may not shrink keeps all 4")
}

// TestRescaleFillsWholePods: rescale rounds a role's share to whole pods.
// With 8-GPU pods, 2 each, demand weighing the 40-GPU target 17:23, the
// split was +1 prefill and +7 decode, neither a pod, and the 8 free GPUs went
// unused every cycle; rounded, decode gets one pod.
func TestRescaleFillsWholePods(t *testing.T) {
	req := pdRescaleRequest(nil)
	req.CompositeSignal.Result.RoleDemand = map[string]float64{domain.RolePrefill: 1700, domain.RoleDecode: 2300}
	req.CompositeSignal.RoleCapacities = map[string]domain.RoleCapacity{
		domain.RolePrefill: {Role: domain.RolePrefill, TotalDemand: 1700, TotalSupply: 200},
		domain.RoleDecode:  {Role: domain.RoleDecode, TotalDemand: 2300, TotalSupply: 200},
	}
	for i := range req.VariantStates {
		req.VariantStates[i].GPUsPerReplica, req.VariantStates[i].CurrentReplicas = 8, 2
	}
	for i := range req.CompositeSignal.Result.VariantCapacities {
		req.CompositeSignal.Result.VariantCapacities[i].ReplicaCount = 2
	}
	got := rescaleTargets(req, 40, 8)
	assert.Equal(t, 2, got["p"])
	assert.Equal(t, 3, got["d"], "8 free GPUs buy one decode pod")
}

// TestRescaleDoesNotFillAHeldRole: a role held against growth is not filled,
// though its share and the free GPUs would allow it.
func TestRescaleDoesNotFillAHeldRole(t *testing.T) {
	req := pdRescaleRequest(nil)
	assert.Greater(t, rescaleTargets(req, 16, 8)["d"], 4, "unheld, decode fills")

	up := map[string]RoleHold{domain.RoleDecode: {NoScaleUp: true}}
	assert.Equal(t, 4, rescaleTargets(pdRescaleRequest(up), 16, 8)["d"])
}
