package steadystate

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// costAwareTargets builds the composite of named and runs the cost-aware
// optimizer on it, one-GPU replicas of the given costs and current counts.
func costAwareTargets(t *testing.T, named []allocation.NamedAnalyzerResult, costs map[string]float64,
	current map[string]int) map[string]int {
	t.Helper()
	composite := buildComposite(context.Background(), named, named, 0.85, 0.7)
	states := make([]domain.VariantReplicaState, 0, len(composite.Result.VariantCapacities))
	metas := make([]domain.VariantMetadata, 0, len(composite.Result.VariantCapacities))
	for _, vc := range composite.Result.VariantCapacities {
		states = append(states, domain.VariantReplicaState{VariantName: vc.VariantName, Role: vc.Role,
			CurrentReplicas: current[vc.VariantName], GPUsPerReplica: 1})
		metas = append(metas, domain.VariantMetadata{VariantName: vc.VariantName, ModelID: "m", Namespace: "ns",
			Role: vc.Role, Cost: costs[vc.VariantName], AcceleratorName: "H100"})
	}
	req := allocation.ModelScalingRequest{ModelID: "m", Namespace: "ns", CompositeSignal: composite,
		VariantStates: states, Variants: metas, Priority: 1}
	out := map[string]int{}
	for _, d := range allocation.NewCostAwareOptimizer().Optimize(context.Background(), []allocation.ModelScalingRequest{req}, nil) {
		out[d.VariantName] = d.TargetReplicas
	}
	return out
}

// TestCostAwareReleasesOnlyWhatEveryFrameCanSpare: the optimizer releases
// within the bound, not only the spare it reports. The release budget is the
// larger of the spare and the releasable capacity at a target above the
// boundary; capped only on the spare, it shed both b replicas and left
// saturation's 80 on 40 of supply, which the next cycle scaled back.
func TestCostAwareReleasesOnlyWhatEveryFrameCanSpare(t *testing.T) {
	sat := variantsEntry(domain.SaturationAnalyzerName, 80,
		domain.VariantCapacity{VariantName: "a", ReplicaCount: 4, PerReplicaCapacity: 10, TotalDemand: 40},
		domain.VariantCapacity{VariantName: "b", ReplicaCount: 2, PerReplicaCapacity: 80, TotalDemand: 40})
	other := variantsEntry("throughput", 25,
		domain.VariantCapacity{VariantName: "a", PerReplicaCapacity: 10},
		domain.VariantCapacity{VariantName: "b", PerReplicaCapacity: 10})

	got := costAwareTargets(t, []allocation.NamedAnalyzerResult{sat, other},
		map[string]float64{"a": 1, "b": 10}, map[string]int{"a": 4, "b": 2})

	satSupply := float64(got["a"])*10 + float64(got["b"])*80
	assert.GreaterOrEqual(t, satSupply, 80/0.7, "saturation keeps its boundary: targets %v", got)
	assert.Less(t, got["a"]+got["b"], 6, "and the release within the bound still happens: targets %v", got)
}

// TestCostAwareReleasesWithinAnotherFramesSpare: priced in saturation's
// frame, the release still stays within an analyzer that rates b four times
// higher. Unbounded, six b went and that analyzer ended at 0.77 of supply
// against its 0.7 boundary.
func TestCostAwareReleasesWithinAnotherFramesSpare(t *testing.T) {
	sat := variantsEntry(domain.SaturationAnalyzerName, 100,
		domain.VariantCapacity{VariantName: "a", ReplicaCount: 10, PerReplicaCapacity: 10, TotalDemand: 50},
		domain.VariantCapacity{VariantName: "b", ReplicaCount: 10, PerReplicaCapacity: 10, TotalDemand: 50})
	other := variantsEntry("throughput", 200,
		domain.VariantCapacity{VariantName: "a", PerReplicaCapacity: 10},
		domain.VariantCapacity{VariantName: "b", PerReplicaCapacity: 40})

	got := costAwareTargets(t, []allocation.NamedAnalyzerResult{sat, other},
		map[string]float64{"a": 1, "b": 10}, map[string]int{"a": 10, "b": 10})

	otherSupply := float64(got["a"])*10 + float64(got["b"])*40
	assert.GreaterOrEqual(t, otherSupply, 200/0.7, "throughput keeps its boundary: targets %v", got)
}

// TestCostAwareReleasesWithinTheBoundOnPD: the same on a P/D role, whose
// release budget is the role's own.
func TestCostAwareReleasesWithinTheBoundOnPD(t *testing.T) {
	pd := func(name string, prefill float64, vcs ...domain.VariantCapacity) allocation.NamedAnalyzerResult {
		e := variantsEntry(name, prefill+300, vcs...)
		e.Result.RoleDemand = map[string]float64{domain.RolePrefill: prefill, domain.RoleDecode: 300}
		return e
	}
	sat := pd(domain.SaturationAnalyzerName, 80,
		domain.VariantCapacity{VariantName: "pa", Role: domain.RolePrefill, ReplicaCount: 4, PerReplicaCapacity: 10, TotalDemand: 40},
		domain.VariantCapacity{VariantName: "pb", Role: domain.RolePrefill, ReplicaCount: 2, PerReplicaCapacity: 80, TotalDemand: 40},
		domain.VariantCapacity{VariantName: "d", Role: domain.RoleDecode, ReplicaCount: 2, PerReplicaCapacity: 300, TotalDemand: 300})
	other := pd("throughput", 25,
		domain.VariantCapacity{VariantName: "pa", Role: domain.RolePrefill, PerReplicaCapacity: 10},
		domain.VariantCapacity{VariantName: "pb", Role: domain.RolePrefill, PerReplicaCapacity: 10})

	got := costAwareTargets(t, []allocation.NamedAnalyzerResult{sat, other},
		map[string]float64{"pa": 1, "pb": 10, "d": 1}, map[string]int{"pa": 4, "pb": 2, "d": 2})

	satPrefill := float64(got["pa"])*10 + float64(got["pb"])*80
	assert.GreaterOrEqual(t, satPrefill, 80/0.7, "saturation's prefill keeps its boundary: targets %v", got)
}

// TestCompositePublishesTheReleaseBound: the release bound is published
// whenever another frame sizes the role, at the release target the optimizer
// sheds to, and no hold is added: the share optimizer and rescale read the
// bound as a GPU floor. Saturation alone publishes none.
func TestCompositePublishesTheReleaseBound(t *testing.T) {
	sat := variantsEntry(domain.SaturationAnalyzerName, 80,
		domain.VariantCapacity{VariantName: "a", ReplicaCount: 4, PerReplicaCapacity: 10, TotalDemand: 40},
		domain.VariantCapacity{VariantName: "b", ReplicaCount: 2, PerReplicaCapacity: 80, TotalDemand: 40})
	other := variantsEntry("throughput", 25,
		domain.VariantCapacity{VariantName: "a", PerReplicaCapacity: 10},
		domain.VariantCapacity{VariantName: "b", PerReplicaCapacity: 10})
	named := []allocation.NamedAnalyzerResult{sat, other}

	composite := buildComposite(context.Background(), named, named, 0.85, 0.7)

	assert.Nil(t, composite.Holds)
	assert.InDelta(t, 200-80/0.7, composite.SpareCapacity, 1e-9, "the spare is bounded at the boundary")
	assert.InDelta(t, 200-80/0.7375, composite.ReleaseBound[domain.RoleBoth], 1e-9, "the release at the release target")
	of := composite.OtherFrames[domain.RoleBoth]
	assert.Len(t, of.Frames, 1, "one other frame sizes the model")
	assert.InDelta(t, composite.ReleaseBound[domain.RoleBoth], of.Releasable(0.7375), 1e-9, "the bound is the frames' releasable")

	alone := buildComposite(context.Background(), named[:1], named[:1], 0.85, 0.7)
	assert.Nil(t, alone.Holds)
	assert.Nil(t, alone.ReleaseBound)
	assert.Nil(t, alone.OtherFrames)
}

// TestCostAwareReleasesWithinEveryFramesReleaseTarget: the release is bounded
// even where the bound does not lower the spare. Saturation's spare was 57.1,
// the other frame's 57.3, so nothing was published, and the release budget,
// the releasable 64.4 at the release target, dropped b from 80 to 16 and left
// throughput at 0.86, above its scale-up threshold.
func TestCostAwareReleasesWithinEveryFramesReleaseTarget(t *testing.T) {
	sat := variantsEntry(domain.SaturationAnalyzerName, 100,
		domain.VariantCapacity{VariantName: "a", ReplicaCount: 120, PerReplicaCapacity: 1, TotalDemand: 60},
		domain.VariantCapacity{VariantName: "b", ReplicaCount: 80, PerReplicaCapacity: 1, TotalDemand: 40})
	other := variantsEntry("throughput", 199,
		domain.VariantCapacity{VariantName: "a", PerReplicaCapacity: 6.0 / 7},
		domain.VariantCapacity{VariantName: "b", PerReplicaCapacity: 8})

	got := costAwareTargets(t, []allocation.NamedAnalyzerResult{sat, other},
		map[string]float64{"a": 1, "b": 10}, map[string]int{"a": 120, "b": 80})

	// Throughput's frame in saturation's units: a worth 1, b 8 x 7/6.
	nA, nB := 199/(6.0/7), 199.0/8
	demand := nA
	supply := float64(got["a"])*demand/nA + float64(got["b"])*demand/nB
	assert.LessOrEqual(t, demand/supply, 0.7375+1e-9, "throughput stays at or below its release target: targets %v", got)
}
